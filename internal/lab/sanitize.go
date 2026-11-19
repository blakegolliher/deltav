package lab

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// Sanitizer scrubs identifying values out of VAST API responses so they
// can be committed as test fixtures. Field names, structure, numbers and
// booleans are left exactly as returned; only string values (and object
// keys that hold addresses) change, and the same real value always maps to
// the same replacement across every document, so relations between
// documents (a VIP's pool, a VIP's CNode) survive.
//
// What it replaces:
//   - IPv4 addresses: mapped, in address order, onto the documentation
//     ranges 192.0.2.0/24, 198.51.100.0/24 and 203.0.113.0/24;
//   - IPv6 addresses (2001:db8::/32), MAC addresses and UUIDs;
//   - the cluster's own name and DNS domain, wherever they appear;
//   - by field name: secrets, serial numbers, GUIDs, tenant and user
//     names, host names, cluster, chassis and DNS domain names, free-text
//     descriptions, and VIP pool names;
//   - any host name, serial, chassis or cluster name found that way, where
//     it turns up again inside another string (a display name, a title).
//
// It cannot know what a free-form field holds. Sanitize also reports, per
// field, how many values each rule rewrote; read that report (it contains
// no values) and the output before committing a fixture.
type Sanitizer struct {
	cluster string // the cluster's short host name
	domain  string // its DNS domain
	// Keep lists values that are generic and stay as they are, such as the
	// tenant "default".
	Keep map[string]bool

	maps  map[string]*valueMap
	known [][2]string // second pass: real identifier -> replacement, longest first
}

// NewSanitizer prepares a sanitizer for responses from the cluster whose
// management host is host, for example "vast1.lab.example.net".
func NewSanitizer(host string) *Sanitizer {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "https://"), "/"))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	s := &Sanitizer{Keep: map[string]bool{"default": true, "main": true}}
	if net.ParseIP(host) == nil {
		s.cluster, s.domain, _ = strings.Cut(host, ".")
	}
	return s
}

// valueMap assigns replacements to real values. It is filled in two passes
// so numbering follows the sorted real values, not the order of discovery:
// the output is then the same from one probe to the next.
type valueMap struct {
	format func(n int) (string, error)
	less   func(a, b string) bool
	seen   map[string]bool
	out    map[string]string
}

func (m *valueMap) get(v string) string {
	if m.out == nil {
		m.seen[v] = true
		return v
	}
	return m.out[v]
}

func (m *valueMap) assign() error {
	keys := make([]string, 0, len(m.seen))
	for k := range m.seen {
		keys = append(keys, k)
	}
	if m.less != nil {
		sort.Slice(keys, func(i, j int) bool { return m.less(keys[i], keys[j]) })
	} else {
		sort.Strings(keys)
	}
	m.out = make(map[string]string, len(keys))
	for i, k := range keys {
		v, err := m.format(i + 1)
		if err != nil {
			return err
		}
		m.out[k] = v
	}
	return nil
}

func numbered(prefix string) func(int) (string, error) {
	return func(n int) (string, error) { return fmt.Sprintf("%s-%d", prefix, n), nil }
}

// docIPv4 returns the nth documentation address (RFC 5737).
func docIPv4(n int) (string, error) {
	nets := []string{"192.0.2", "198.51.100", "203.0.113"}
	if n < 1 || n > len(nets)*254 {
		return "", fmt.Errorf("more than %d distinct IPv4 addresses: the documentation ranges are exhausted", len(nets)*254)
	}
	return fmt.Sprintf("%s.%d", nets[(n-1)/254], (n-1)%254+1), nil
}

func ipv4Less(a, b string) bool {
	x, y := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

func (s *Sanitizer) reset() {
	mk := func(format func(int) (string, error), less func(a, b string) bool) *valueMap {
		return &valueMap{format: format, less: less, seen: map[string]bool{}}
	}
	s.maps = map[string]*valueMap{
		"ipv4": mk(docIPv4, ipv4Less),
		"ipv6": mk(func(n int) (string, error) { return fmt.Sprintf("2001:db8::%x", n), nil }, nil),
		"mac": mk(func(n int) (string, error) {
			return fmt.Sprintf("02:00:00:%02x:%02x:%02x", n>>16&0xff, n>>8&0xff, n&0xff), nil
		}, nil),
		"uuid":        mk(func(n int) (string, error) { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n), nil }, nil),
		"serial":      mk(numbered("SERIAL"), nil),
		"guid":        mk(numbered("guid"), nil),
		"tenant":      mk(numbered("tenant"), nil),
		"user":        mk(numbered("user"), nil),
		"host":        mk(numbered("host"), nil),
		"pool":        mk(numbered("pool"), nil),
		"box":         mk(numbered("box"), nil),
		"domain":      mk(numbered("domain"), nil),
		"suffix":      mk(func(n int) (string, error) { return fmt.Sprintf("dns%d.example.com", n), nil }, nil),
		"clustername": mk(func(n int) (string, error) { return fmt.Sprintf("cluster%d", n), nil }, nil),
	}
	s.known = nil
}

// knownMaps are the maps whose real values are distinctive enough to be
// searched for inside other strings. Tenant, user and pool names are not:
// one called "s3" or "test" would be replaced all over the place.
var knownMaps = []string{"host", "serial", "box", "clustername"}

// minKnown is the shortest identifier searched for inside other strings.
const minKnown = 5

func (s *Sanitizer) buildKnown() {
	for _, name := range knownMaps {
		for real, repl := range s.maps[name].out {
			if len(real) >= minKnown {
				s.known = append(s.known, [2]string{real, repl})
			}
		}
	}
	sort.Slice(s.known, func(i, j int) bool {
		if len(s.known[i][0]) != len(s.known[j][0]) {
			return len(s.known[i][0]) > len(s.known[j][0])
		}
		return s.known[i][0] < s.known[j][0]
	})
}

// Field-name rules, matched against the lower-cased name of the field a
// string value sits in. The first that matches wins.
var fieldRules = []struct {
	rule string // a value map's name, or "redact" or "text"
	doc  string // "" for every document, or the one document it applies to
	re   *regexp.Regexp
}{
	{"redact", "", regexp.MustCompile(`passw|secret|token|private|credential|access_key|certificate|(^|_)cert$|licen[cs]e`)},
	{"text", "", regexp.MustCompile(`description|comment|(^|_)notes?$`)},
	{"serial", "", regexp.MustCompile(`serial|(^|_)sn$|wwn|asset`)},
	{"guid", "", regexp.MustCompile(`guid|uuid`)},
	{"tenant", "", regexp.MustCompile(`tenant`)},
	{"user", "", regexp.MustCompile(`^user$|username|owner|created_by|modified_by|updated_by|creator|email`)},
	{"host", "", regexp.MustCompile(`hostname|^host$|fqdn|dns_name`)},
	{"clustername", "", regexp.MustCompile(`^cluster(_name)?$`)},
	// A chassis is named after its serial number.
	{"box", "", regexp.MustCompile(`^[cde]?box(_name)?$`)},
	{"domain", "", regexp.MustCompile(`^domain(_name)?$`)},
	// The DNS suffix under which a cluster's pools are named. Replaced whole:
	// replacing the parts of it that are known would leave the rest.
	{"suffix", "", regexp.MustCompile(`domain_suffix`)},
	// A VIP pool's name can be a person's or a project's.
	{"pool", "vippools", regexp.MustCompile(`^name$`)},
	{"pool", "vips", regexp.MustCompile(`^vippool(_name)?$`)},
}

// versionField marks fields whose dotted numbers are versions, not IPs.
var versionField = regexp.MustCompile(`version|build|release|firmware|bios`)

var (
	ipv4RE = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)
	macRE  = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)
	uuidRE = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	ipv6RE = regexp.MustCompile(`(?i)[0-9a-f:]*:[0-9a-f:]*:[0-9a-f:.]*`)
	// rangeRE matches the summary form of an address range, "10.1.2.[5-9]".
	rangeRE = regexp.MustCompile(`(\d{1,3}\.\d{1,3}\.\d{1,3})\.\[(\d{1,3})-(\d{1,3})\]`)
)

// FieldReport says what happened to one field of one document.
type FieldReport struct {
	Doc      string         // document name
	Path     string         // e.g. "[].ip" or "[].ranges[][]"
	Types    []string       // JSON types seen: string, number, bool, null, object, array
	Count    int            // how many values the field had
	Distinct int            // how many different values
	Rules    map[string]int // rule name -> values it rewrote
}

type fieldStats struct {
	types  map[string]bool
	count  int
	values map[string]bool
	rules  map[string]int
}

// Sanitize scrubs a set of decoded JSON documents (as produced by
// encoding/json with UseNumber, keyed by a short name such as "vips") and
// returns the scrubbed copies and a report. The documents are processed
// together so replacements are consistent across them.
func (s *Sanitizer) Sanitize(docs map[string]any) (map[string]any, []FieldReport, error) {
	s.reset()
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)

	// First pass: only collect the values each map will have to replace.
	for _, name := range names {
		s.walk(name, "", "", docs[name], nil)
	}
	for _, m := range s.maps {
		if err := m.assign(); err != nil {
			return nil, nil, err
		}
	}
	s.buildKnown()
	out := make(map[string]any, len(docs))
	var report []FieldReport
	for _, name := range names {
		stats := map[string]*fieldStats{}
		out[name] = s.walk(name, "", "", docs[name], stats)
		paths := make([]string, 0, len(stats))
		for p := range stats {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			st := stats[p]
			fr := FieldReport{Doc: name, Path: p, Count: st.count, Distinct: len(st.values), Rules: st.rules}
			for t := range st.types {
				fr.Types = append(fr.Types, t)
			}
			sort.Strings(fr.Types)
			report = append(report, fr)
		}
	}
	return out, report, nil
}

// walk returns a scrubbed copy of v. field is the name of the object field
// v sits in (inherited by array elements), path its place in the document.
func (s *Sanitizer) walk(doc, path, field string, v any, stats map[string]*fieldStats) any {
	note := func(kind, value, rule string) {
		if stats == nil {
			return
		}
		st := stats[path]
		if st == nil {
			st = &fieldStats{types: map[string]bool{}, values: map[string]bool{}, rules: map[string]int{}}
			stats[path] = st
		}
		st.types[kind] = true
		st.count++
		st.values[value] = true
		if rule != "" {
			st.rules[rule]++
		}
	}
	switch x := v.(type) {
	case map[string]any:
		note("object", "", "")
		out := make(map[string]any, len(x))
		for k, child := range x {
			// A key can itself be an address or a name of the cluster. The
			// report's path is made from the key as it goes out, never as
			// it came in: the report is meant to be safe to paste.
			newKey, _ := s.inline(k, false)
			out[newKey] = s.walk(doc, path+"."+newKey, k, child, stats)
		}
		return out
	case []any:
		note("array", "", "")
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = s.walk(doc, path+"[]", field, child, stats)
		}
		return out
	case string:
		clean, rule := s.scrub(doc, strings.ToLower(field), x)
		note("string", x, rule)
		return clean
	case json.Number:
		note("number", x.String(), "")
	case float64:
		note("number", fmt.Sprint(x), "")
	case bool:
		note("bool", fmt.Sprint(x), "")
	case nil:
		note("null", "", "")
	}
	return v
}

// scrub cleans one string value and names the rule that changed it.
func (s *Sanitizer) scrub(doc, field, v string) (string, string) {
	if v == "" {
		return v, ""
	}
	for _, r := range fieldRules {
		if (r.doc != "" && r.doc != doc) || !r.re.MatchString(field) {
			continue
		}
		switch r.rule {
		case "redact":
			return "REDACTED", "redact"
		case "text":
			return "", "text"
		}
		if s.Keep[v] {
			return v, "" // a generic name, such as the tenant "default"
		}
		return s.maps[r.rule].get(v), r.rule
	}
	return s.inline(v, versionField.MatchString(field))
}

// inline replaces addresses, UUIDs and the cluster's own names inside a
// string, leaving the rest of it alone.
func (s *Sanitizer) inline(v string, isVersion bool) (string, string) {
	rules := map[string]bool{}
	sub := func(re *regexp.Regexp, rule string, ok func(match, before, after string) bool) {
		var b strings.Builder
		last := 0
		for _, loc := range re.FindAllStringIndex(v, -1) {
			m := v[loc[0]:loc[1]]
			if ok != nil && !ok(m, v[:loc[0]], v[loc[1]:]) {
				continue
			}
			b.WriteString(v[last:loc[0]])
			b.WriteString(s.maps[rule].get(m))
			last = loc[1]
			rules[rule] = true
		}
		b.WriteString(v[last:])
		v = b.String()
	}
	sub(uuidRE, "uuid", nil)
	sub(macRE, "mac", nil)
	if !isVersion {
		sub(ipv4RE, "ipv4", func(m, before, after string) bool {
			// Not part of a longer dotted number, a real address, and not
			// a netmask or a placeholder.
			if strings.HasSuffix(before, ".") || strings.HasPrefix(after, ".") || endsInDigit(before) || startsWithDigit(after) {
				return false
			}
			ip := net.ParseIP(m).To4()
			return ip != nil && ip[0] != 0 && ip[0] != 127 && ip[0] != 255
		})
		// "10.1.2.[5-9]": map both ends, and keep the short form if they
		// still share a prefix. This runs after the plain addresses so that
		// what it writes is not taken for input and mapped a second time.
		v = rangeRE.ReplaceAllStringFunc(v, func(m string) string {
			p := rangeRE.FindStringSubmatch(m)
			first, last := p[1]+"."+p[2], p[1]+"."+p[3]
			if net.ParseIP(first) == nil || net.ParseIP(last) == nil {
				return m
			}
			rules["ipv4"] = true
			a, b := s.maps["ipv4"].get(first), s.maps["ipv4"].get(last)
			ai, bi := strings.LastIndex(a, "."), strings.LastIndex(b, ".")
			if ai > 0 && bi > 0 && a[:ai] == b[:bi] {
				return a[:ai] + ".[" + a[ai+1:] + "-" + b[bi+1:] + "]"
			}
			return a + " - " + b
		})
	}
	sub(ipv6RE, "ipv6", func(m, _, _ string) bool {
		ip := net.ParseIP(m)
		return ip != nil && ip.To4() == nil && !ip.IsLoopback() && !ip.IsUnspecified()
	})
	if s.domain != "" {
		if i := strings.Index(strings.ToLower(v), s.domain); i >= 0 {
			v = replaceFold(v, s.domain, "example.com")
			rules["domain"] = true
		}
	}
	if s.cluster != "" && strings.Contains(strings.ToLower(v), s.cluster) {
		v = replaceFold(v, s.cluster, "cluster1")
		rules["cluster"] = true
	}
	for _, k := range s.known {
		if strings.Contains(v, k[0]) {
			v = strings.ReplaceAll(v, k[0], k[1])
			rules["known-name"] = true
		}
	}
	names := make([]string, 0, len(rules))
	for r := range rules {
		names = append(names, r)
	}
	sort.Strings(names)
	return v, strings.Join(names, "+")
}

func endsInDigit(s string) bool     { return s != "" && s[len(s)-1] >= '0' && s[len(s)-1] <= '9' }
func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// replaceFold replaces every case-insensitive occurrence of old (which is
// lower case) in s.
func replaceFold(s, old, repl string) string {
	var b strings.Builder
	lower := strings.ToLower(s)
	for {
		i := strings.Index(lower, old)
		if i < 0 {
			break
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
	b.WriteString(s)
	return b.String()
}
