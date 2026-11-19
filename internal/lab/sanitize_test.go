package lab

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The documents below are invented for this test. Their field names are
// NOT a statement about the VAST API; real shapes come from a probe.
const (
	rawCNodes = `[
	 {"id": 1, "name": "cnode-1", "hostname": "acme204-cn1.lab.acme.example.net", "ip": "172.16.3.10",
	  "mgmt_ip": "10.20.30.61", "guid": "f3a9c1d2-7b44-4c1e-9d0a-5e6f7a8b9c0d", "sn": "S4X9KLM0", "state": "ACTIVE",
	  "enabled": true, "build": "release-5.3.0.1234", "os_version": "9.4.0.17", "bmc_mac": "AC:1F:6B:00:AA:01",
	  "ipv6": "fd00:aa::61", "netmask": "255.255.255.0", "url": "https://ACME204.lab.acme.example.net/api/cnodes/1/"},
	 {"id": 2, "name": "cnode-2", "hostname": "acme204-cn2.lab.acme.example.net", "ip": "172.16.3.11",
	  "mgmt_ip": "10.20.30.62", "guid": "0b1c2d3e-4f50-4a61-8b72-c3d4e5f60718", "sn": "S4X9KLM1", "state": "ACTIVE",
	  "enabled": false, "build": "release-5.3.0.1234", "os_version": "9.4.0.17", "bmc_mac": "AC:1F:6B:00:AA:02",
	  "ipv6": "fd00:aa::62", "netmask": "255.255.255.0", "url": "https://acme204.lab.acme.example.net/api/cnodes/2/"}
	]`
	rawPools = `[
	 {"id": 2, "name": "main", "tenant_name": "default", "role": "PROTOCOLS", "ip_ranges": [["172.25.204.1", "172.25.204.3"]],
	  "description": "primary pool for Jordan's team", "created_by": "jordan.smith", "active": true},
	 {"id": 9, "name": "jordan-vip", "tenant_name": "blue-team", "role": "PROTOCOLS", "ip_ranges": [["172.25.204.9", "172.25.204.9"]],
	  "description": "", "created_by": "jordan.smith", "active": true, "secret_key": "main"}
	]`
	rawVIPs = `[
	 {"id": 11, "ip": "172.25.204.1", "vippool": "main", "cnode": "cnode-1", "tenant_name": "default"},
	 {"id": 12, "ip": "172.25.204.2", "vippool": "main", "cnode": "cnode-2", "tenant_name": "default"},
	 {"id": 13, "ip": "172.25.204.3", "vippool": "main", "cnode": "cnode-1", "tenant_name": "default"},
	 {"id": 14, "ip": "172.25.204.9", "vippool": "jordan-vip", "cnode": "cnode-2", "tenant_name": "blue-team"}
	]`
)

func decodeDoc(t *testing.T, raw string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func sanitizeTestDocs(t *testing.T) (map[string]any, []FieldReport, string) {
	t.Helper()
	docs := map[string]any{"cnodes": decodeDoc(t, rawCNodes), "vippools": decodeDoc(t, rawPools), "vips": decodeDoc(t, rawVIPs)}
	out, report, err := NewSanitizer("acme204.lab.acme.example.net").Sanitize(docs)
	if err != nil {
		t.Fatal(err)
	}
	all, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return out, report, string(all)
}

func field(t *testing.T, doc any, index int, name string) any {
	t.Helper()
	return doc.([]any)[index].(map[string]any)[name]
}

func TestSanitizeRemovesIdentifyingValues(t *testing.T) {
	_, _, all := sanitizeTestDocs(t)
	for _, leak := range []string{
		"172.16.3.", "10.20.30.", "172.25.204.", // every real address
		"acme204", "ACME204", "lab.acme.example.net", // cluster name and domain
		"f3a9c1d2", "0b1c2d3e", "S4X9KLM", "AC:1F:6B", "fd00:aa", // GUIDs, serials, MACs, IPv6
		"jordan", "Jordan", "blue-team", // people, tenants, pool names
	} {
		if strings.Contains(all, leak) {
			t.Errorf("output still contains %q", leak)
		}
	}
}

func TestSanitizeKeepsStructureAndRelations(t *testing.T) {
	out, _, _ := sanitizeTestDocs(t)
	cnodes, pools, vips := out["cnodes"], out["vippools"], out["vips"]

	// Numbers, booleans, versions, netmasks and generic names are untouched.
	for name, want := range map[string]any{
		"id": json.Number("1"), "name": "cnode-1", "state": "ACTIVE", "enabled": true,
		"build": "release-5.3.0.1234", "os_version": "9.4.0.17", "netmask": "255.255.255.0",
	} {
		if got := field(t, cnodes, 0, name); got != want {
			t.Errorf("cnodes[0].%s = %v, want %v", name, got, want)
		}
	}
	if got := field(t, pools, 0, "name"); got != "main" {
		t.Errorf("the generic pool name main became %v", got)
	}
	if got := field(t, pools, 0, "tenant_name"); got != "default" {
		t.Errorf("the tenant default became %v", got)
	}

	// Addresses map onto documentation ranges in address order, so the
	// three consecutive VIPs stay consecutive and a range still spans them.
	want := map[string]string{
		"10.20.30.61": "192.0.2.1", "10.20.30.62": "192.0.2.2", "172.16.3.10": "192.0.2.3", "172.16.3.11": "192.0.2.4",
		"172.25.204.1": "192.0.2.5", "172.25.204.2": "192.0.2.6", "172.25.204.3": "192.0.2.7", "172.25.204.9": "192.0.2.8",
	}
	if got := field(t, cnodes, 1, "mgmt_ip"); got != want["10.20.30.62"] {
		t.Errorf("mgmt_ip = %v", got)
	}
	for i, real := range []string{"172.25.204.1", "172.25.204.2", "172.25.204.3", "172.25.204.9"} {
		if got := field(t, vips, i, "ip"); got != want[real] {
			t.Errorf("vips[%d].ip = %v, want %s", i, got, want[real])
		}
	}
	r := field(t, pools, 0, "ip_ranges").([]any)[0].([]any)
	if r[0] != "192.0.2.5" || r[1] != "192.0.2.7" {
		t.Errorf("pool range = %v", r)
	}

	// The same real value gets the same replacement in every document.
	poolName := field(t, pools, 1, "name")
	if poolName == "jordan-vip" || !strings.HasPrefix(poolName.(string), "pool-") || field(t, vips, 3, "vippool") != poolName {
		t.Errorf("pool name: vippools has %v, vips has %v", poolName, field(t, vips, 3, "vippool"))
	}
	if field(t, vips, 0, "vippool") != "main" || field(t, vips, 3, "cnode") != "cnode-2" {
		t.Errorf("vip relations changed: %v %v", field(t, vips, 0, "vippool"), field(t, vips, 3, "cnode"))
	}
	tenant := field(t, pools, 1, "tenant_name")
	if !strings.HasPrefix(tenant.(string), "tenant-") || field(t, vips, 3, "tenant_name") != tenant {
		t.Errorf("tenant: vippools has %v, vips has %v", tenant, field(t, vips, 3, "tenant_name"))
	}
	if a, b := field(t, pools, 0, "created_by"), field(t, pools, 1, "created_by"); a != b || !strings.HasPrefix(a.(string), "user-") {
		t.Errorf("created_by: %v and %v", a, b)
	}

	// Secrets and free text go entirely, even when the value looks generic.
	if got := field(t, pools, 1, "secret_key"); got != "REDACTED" {
		t.Errorf("secret_key = %v", got)
	}
	if got := field(t, pools, 0, "description"); got != "" {
		t.Errorf("description = %q", got)
	}
	// The cluster's name inside a URL, in either case.
	for i := 0; i < 2; i++ {
		if got := field(t, cnodes, i, "url"); got != "https://cluster1.example.com/api/cnodes/"+string(rune('1'+i))+"/" {
			t.Errorf("cnodes[%d].url = %v", i, got)
		}
	}
	if got := field(t, cnodes, 0, "ipv6"); got != "2001:db8::1" {
		t.Errorf("ipv6 = %v", got)
	}
	if got := field(t, cnodes, 0, "bmc_mac"); got != "02:00:00:00:00:01" {
		t.Errorf("bmc_mac = %v", got)
	}
}

// Each case here is a kind of leak found by reading the first real probe's
// output. The values are invented.
func TestSanitizeIdentifiersInsideOtherFields(t *testing.T) {
	cnodes := decodeDoc(t, `[
	 {"name": "cnode-1", "hostname": "rack7-cb1-cn-1", "sn": "ZZTEST0001", "cluster": "lab-east-7",
	  "box": "cbox-ZZBOX00A1", "cbox": "cbox-ZZBOX00A1",
	  "display_name": "cnode-1 (10.9.8.1) [rack7-cb1-cn-1] leader", "title": "cnode-1 (10.9.8.1) [rack7-cb1-cn-1]",
	  "note_field": "chassis cbox-ZZBOX00A1 holds ZZTEST0001 in lab-east-7"},
	 {"name": "cnode-2", "hostname": "rack7-cb1-cn-2", "sn": "ZZTEST0002", "cluster": "lab-east-7",
	  "box": "cbox-ZZBOX00A2", "cbox": "cbox-ZZBOX00A2",
	  "display_name": "cnode-2 (10.9.8.2) [rack7-cb1-cn-2]", "title": "cnode-2 (10.9.8.2) [rack7-cb1-cn-2]",
	  "note_field": ""}
	]`)
	pools := decodeDoc(t, `[
	 {"name": "main", "cluster": "lab-east-7", "domain_name": "", "start_ip": "10.5.5.70", "end_ip": "10.5.5.72",
	  "ranges_summary": "10.5.5.[70-72]", "ip_ranges": [["10.5.5.70", "10.5.5.72"]]},
	 {"name": "sam-pool", "cluster": "lab-east-7", "domain_name": "sam", "start_ip": "10.5.5.80", "end_ip": "10.5.5.80",
	  "ranges_summary": "10.5.5.80", "ip_ranges": [["10.5.5.80", "10.5.5.80"]]}
	]`)
	out, report, err := NewSanitizer("mgmt7.corp.example.net").Sanitize(map[string]any{"cnodes": cnodes, "vippools": pools})
	if err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(out)
	for _, leak := range []string{"rack7", "ZZTEST", "ZZBOX", "lab-east", "10.5.5", "10.9.8", `"sam"`, "sam-pool"} {
		if strings.Contains(string(all), leak) {
			t.Errorf("output still contains %q:\n%s", leak, all)
		}
	}
	c, p := out["cnodes"], out["vippools"]
	for name, want := range map[string]string{
		"hostname": "host-1", "sn": "SERIAL-1", "cluster": "cluster1", "box": "box-1", "cbox": "box-1",
		// 10.5.5.70, .72, .80 sort before 10.9.8.1 and .2.
		"display_name": "cnode-1 (192.0.2.4) [host-1] leader", "title": "cnode-1 (192.0.2.4) [host-1]",
		"note_field": "chassis box-1 holds SERIAL-1 in cluster1",
	} {
		if got := field(t, c, 0, name); got != want {
			t.Errorf("cnodes[0].%s = %v, want %v", name, got, want)
		}
	}
	if got := field(t, c, 1, "display_name"); got != "cnode-2 (192.0.2.5) [host-2]" {
		t.Errorf("cnodes[1].display_name = %v", got)
	}
	// The summary keeps its form and agrees with the range it summarizes.
	if got := field(t, p, 0, "ranges_summary"); got != "192.0.2.[1-2]" {
		t.Errorf("ranges_summary = %v", got)
	}
	if a, b := field(t, p, 0, "start_ip"), field(t, p, 0, "end_ip"); a != "192.0.2.1" || b != "192.0.2.2" {
		t.Errorf("start/end = %v %v", a, b)
	}
	if got := field(t, p, 1, "ranges_summary"); got != "192.0.2.3" {
		t.Errorf("single-address summary = %v", got)
	}
	if got := field(t, p, 0, "domain_name"); got != "" {
		t.Errorf("an empty domain name became %v", got)
	}
	if got := field(t, p, 1, "domain_name"); got != "domain-1" {
		t.Errorf("domain_name = %v", got)
	}
	// The report names what rewrote the free-form fields.
	for _, r := range report {
		if r.Doc == "cnodes" && r.Path == "[].display_name" && r.Rules["ipv4+known-name"] != 2 {
			t.Errorf("display_name report: %+v", r)
		}
		if r.Doc == "vippools" && r.Path == "[].ranges_summary" && r.Rules["ipv4"] != 2 {
			t.Errorf("ranges_summary report: %+v", r)
		}
	}
}

// A summary whose ends land in different documentation networks cannot
// keep the bracket form.
func TestSanitizeRangeSummaryAcrossNetworks(t *testing.T) {
	var ips []any
	for i := 1; i <= 253; i++ {
		ips = append(ips, "10.0.0."+itoa(i))
	}
	doc := map[string]any{"filler": ips, "ranges_summary": "10.0.1.[5-9]", "ends": []any{"10.0.1.5", "10.0.1.9"}}
	out, _, err := NewSanitizer("vast.example.net").Sanitize(map[string]any{"x": doc})
	if err != nil {
		t.Fatal(err)
	}
	if got := out["x"].(map[string]any)["ranges_summary"]; got != "192.0.2.254 - 198.51.100.1" {
		t.Errorf("ranges_summary = %v", got)
	}
}

func TestSanitizeIsDeterministic(t *testing.T) {
	_, _, a := sanitizeTestDocs(t)
	_, _, b := sanitizeTestDocs(t)
	if a != b {
		t.Error("two runs over the same input differ")
	}
}

// The report shows the shape of the data and which rule touched each field,
// without any value, so it can be read (and pasted) safely.
func TestSanitizeReport(t *testing.T) {
	_, report, _ := sanitizeTestDocs(t)
	find := func(doc, path string) FieldReport {
		t.Helper()
		for _, r := range report {
			if r.Doc == doc && r.Path == path {
				return r
			}
		}
		t.Fatalf("no report row for %s %s", doc, path)
		return FieldReport{}
	}
	if r := find("vips", "[].ip"); r.Count != 4 || r.Distinct != 4 || r.Rules["ipv4"] != 4 || strings.Join(r.Types, ",") != "string" {
		t.Errorf("vips [].ip: %+v", r)
	}
	if r := find("vips", "[].cnode"); r.Count != 4 || r.Distinct != 2 || len(r.Rules) != 0 {
		t.Errorf("vips [].cnode: %+v", r)
	}
	if r := find("vips", "[].vippool"); r.Distinct != 2 || r.Rules["pool"] != 1 {
		t.Errorf("vips [].vippool: %+v", r)
	}
	if r := find("vippools", "[].ip_ranges[][]"); r.Count != 4 || r.Rules["ipv4"] != 4 {
		t.Errorf("vippools [].ip_ranges[][]: %+v", r)
	}
	if r := find("cnodes", "[].id"); strings.Join(r.Types, ",") != "number" || r.Distinct != 2 {
		t.Errorf("cnodes [].id: %+v", r)
	}
	if r := find("cnodes", "[].url"); r.Rules["cluster+domain"] != 2 {
		t.Errorf("cnodes [].url: %+v", r)
	}
	var buf bytes.Buffer
	for _, r := range report {
		buf.WriteString(r.Path)
		for rule := range r.Rules {
			buf.WriteString(rule)
		}
	}
	for _, leak := range []string{"172.", "jordan", "acme"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("the report contains a value: %q", leak)
		}
	}
}

// An object keyed by an address or a name gives the report a path made of
// that key. The path is the key as it was rewritten, not as it came.
func TestSanitizeReportOfKeyedObjects(t *testing.T) {
	doc := decodeDoc(t, `{"10.20.30.41": {"state": "ACTIVE"}, "10.20.30.42": {"state": "ACTIVE"}, "plain": {"state": "x"}}`)
	out, report, err := NewSanitizer("vms.acme.example").Sanitize(map[string]any{"by_address": doc})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, r := range report {
		paths[r.Path] = true
		if strings.Contains(r.Path, "10.20.30.") {
			t.Errorf("the report has a real address in a path: %q", r.Path)
		}
	}
	keyed := out["by_address"].(map[string]any)
	if len(keyed) != 3 || !paths[".plain.state"] {
		t.Fatalf("sanitized %v, report paths %v", keyed, paths)
	}
	for k := range keyed {
		if !paths["."+k+".state"] {
			t.Errorf("no report row for the key %q as it was written out: %v", k, paths)
		}
	}
}

func TestSanitizeAddressEdgeCases(t *testing.T) {
	doc := decodeDoc(t, `{
	  "range": "10.1.1.5-10.1.1.9", "cidr": "10.1.1.0/24", "with_port": "10.1.1.5:443",
	  "five_parts": "1.2.3.4.5", "not_ip": "999.1.1.1", "loopback": "127.0.0.1", "any": "0.0.0.0",
	  "time": "2026-11-17T12:30:45Z", "10.1.1.5": "an address used as a key"
	}`)
	out, _, err := NewSanitizer("192.0.2.200").Sanitize(map[string]any{"x": doc})
	if err != nil {
		t.Fatal(err)
	}
	got := out["x"].(map[string]any)
	// 10.1.1.0, .5 and .9 are the three addresses, in that order.
	for k, want := range map[string]string{
		"range": "192.0.2.2-192.0.2.3", "cidr": "192.0.2.1/24", "with_port": "192.0.2.2:443",
		"five_parts": "1.2.3.4.5", "not_ip": "999.1.1.1", "loopback": "127.0.0.1", "any": "0.0.0.0",
		"time": "2026-11-17T12:30:45Z", "192.0.2.2": "an address used as a key",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestSanitizeRunsOutOfDocumentationAddresses(t *testing.T) {
	var ips []any
	for i := 0; i < 800; i++ {
		ips = append(ips, "10.0."+string(rune('0'+i/250))+"."+strings.TrimLeft(strings.Repeat("0", 3)+itoa(i%250+1), "0"))
	}
	_, _, err := NewSanitizer("vast.example.net").Sanitize(map[string]any{"x": ips})
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("got %v", err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A DNS suffix is a host name with parts the sanitizer knows (the cluster,
// its domain) and parts it does not. Replacing only the known parts would
// leave the others in the fixture.
func TestSanitizeDNSSuffixIsReplacedWhole(t *testing.T) {
	docs := map[string]any{"dns": decodeDoc(t, `[{"id": 1, "name": "dns", "domain_suffix": "west-acme204.lab.acme.example.net"}]`)}
	out, _, err := NewSanitizer("acme204.lab.acme.example.net").Sanitize(docs)
	if err != nil {
		t.Fatal(err)
	}
	got := out["dns"].([]any)[0].(map[string]any)["domain_suffix"]
	if got != "dns1.example.com" {
		t.Errorf("domain_suffix = %q, want dns1.example.com", got)
	}
}
