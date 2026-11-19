//go:build integration && live

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"deltav/internal/lab"
)

// odd is one way of asking for the same object: what goes on the wire, and
// the readings of it a server might sign over.
type odd struct {
	name   string
	target string   // request target, written as it is
	host   string   // Host header; "" is the listener's own address
	canon  []string // canonical URIs to try, each its own request
	query  string   // signed query string, without the "?"
	auth   string   // how the Authorization header is altered; see send
}

var (
	s3Code  = regexp.MustCompile(`<Code>([^<]*)</Code>`)
	xmlRoot = regexp.MustCompile(`^(?:<\?xml[^>]*>\s*)?<([A-Za-z]+)`)
)

// TestLiveRulesReadRequestsAsVASTDoes asks real VAST for one object in every
// odd way we know of, and then checks that a rule which should refuse them
// all does. A rule works on deltav's reading of a request: its bucket, its
// key, its access key, its operation. If VAST reads one of these requests
// differently and serves it, the rule can be walked around.
//
// It writes one small object under deltav-live/ and deletes it.
func TestLiveRulesReadRequestsAsVASTDoes(t *testing.T) {
	s, listen, keys, bucket := liveStack(t)
	self := fmt.Sprintf("127.0.0.1:%d", listen)
	key := fmt.Sprintf("deltav-live/probe-%d", time.Now().UnixNano())
	content := "deltav path probe " + key
	sum := sha256.Sum256([]byte(content))

	signed := func(method, host, canon, query, hash string) [][2]string {
		req, err := http.NewRequest(method, "http://"+host+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host, req.URL.RawQuery = host, query
		lab.SignAs(req, *keys, hash, time.Now(), canon)
		var out [][2]string
		for name, vals := range req.Header {
			out = append(out, [2]string{name, vals[0]})
		}
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
		return out
	}
	plain := "/" + bucket + "/" + key
	put, body := rawRequest(t, listen, http.MethodPut, plain, self, signed(http.MethodPut, self, plain, "", hex.EncodeToString(sum[:])), content)
	if put != http.StatusOK {
		t.Fatalf("writing the probe object: %d %s", put, body)
	}
	t.Cleanup(func() {
		for _, name := range []string{"probe-place", "probe-key", "probe-operation"} {
			req, _ := http.NewRequest(http.MethodDelete, "http://"+s.HTTPAddr()+"/api/v1/rules/"+name, nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			status, body := rawRequest(t, listen, http.MethodDelete, plain, self, signed(http.MethodDelete, self, plain, "", lab.EmptyPayloadHash), "")
			if status == http.StatusNoContent || status == http.StatusOK {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("the probe object %s was not deleted: %d %s", key, status, body)
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	})

	enc := func(c byte) string { return fmt.Sprintf("%%%02X", c) }
	first := enc(bucket[0]) + bucket[1:]
	firstKey := enc(key[0]) + key[1:]
	shapes := []odd{
		{name: "plain", target: plain, canon: []string{plain}},
		{name: "bucket letter encoded", target: "/" + first + "/" + key, canon: []string{"/" + first + "/" + key, plain}},
		{name: "bucket letter encoded twice", target: "/%25" + first[1:] + "/" + key, canon: []string{"/%25" + first[1:] + "/" + key, "/" + first + "/" + key, plain}},
		{name: "empty segment before the bucket", target: "/" + plain, canon: []string{"/" + plain, plain}},
		{name: "slash after the bucket encoded", target: "/" + bucket + "%2F" + key, canon: []string{"/" + bucket + "%2F" + key, plain}},
		{name: "two slashes after the bucket", target: "/" + bucket + "//" + key, canon: []string{"/" + bucket + "//" + key, plain}},
		{name: "dot segment before the bucket", target: "/." + plain, canon: []string{"/." + plain, plain}},
		{name: "dot-dot before the bucket", target: "/x/.." + plain, canon: []string{"/x/.." + plain, plain}},
		{name: "dot segment in the key", target: "/" + bucket + "/./" + key, canon: []string{"/" + bucket + "/./" + key, plain}},
		{name: "dot-dot in the key", target: "/" + bucket + "/x/../" + key, canon: []string{"/" + bucket + "/x/../" + key, plain}},
		{name: "key letter encoded", target: "/" + bucket + "/" + firstKey, canon: []string{"/" + bucket + "/" + firstKey, plain}},
		{name: "query: x-id", target: plain, query: "x-id=GetObject", canon: []string{plain}},
		{name: "query: unknown parameter", target: plain, query: "deltav-probe=1", canon: []string{plain}},
		{name: "query: partNumber", target: plain, query: "partNumber=1", canon: []string{plain}},
		{name: "query: response-content-type", target: plain, query: "response-content-type=text%2Fplain", canon: []string{plain}},
		{name: "decoy Authorization header first", target: plain, canon: []string{plain}, auth: "decoy-first"},
		{name: "decoy Authorization header second", target: plain, canon: []string{plain}, auth: "decoy-second"},
		{name: "decoy Credential field first", target: plain, canon: []string{plain}, auth: "decoy-field"},
		{name: "credential= in lower case", target: plain, canon: []string{plain}, auth: "lower"},
		{name: "CREDENTIAL= in upper case", target: plain, canon: []string{plain}, auth: "upper"},
		{name: "spaces around the = of Credential", target: plain, canon: []string{plain}, auth: "spaced"},
		{name: "algorithm in lower case", target: plain, canon: []string{plain}, auth: "algorithm-lower"},
		{name: "no space after the algorithm", target: plain, canon: []string{plain}, auth: "no-space"},
		{name: "fields parted by spaces, no commas", target: plain, canon: []string{plain}, auth: "no-commas"},
		{name: "a field named XCredential first", target: plain, canon: []string{plain}, auth: "decoy-lookalike"},
		{name: "presigned", target: plain, canon: []string{plain}, auth: "presigned"},
		{name: "presigned, parameter name encoded", target: plain, canon: []string{plain}, auth: "presigned-encoded"},
		{name: "presigned, with a decoy Authorization header", target: plain, canon: []string{plain}, auth: "presigned-decoy"},
	}
	if up := strings.ToUpper(bucket); up != bucket {
		shapes = append(shapes, odd{name: "bucket in upper case", target: "/" + up + "/" + key, canon: []string{"/" + up + "/" + key, plain}})
	}
	// Addressed by host name: the bucket is in the Host header and not in
	// the path. Only if the cluster's S3 name is known.
	if name := os.Getenv("DELTAV_LAB_S3_ENDPOINT"); name != "" {
		name = strings.TrimPrefix(strings.TrimPrefix(name, "https://"), "http://")
		name, _, _ = strings.Cut(name, "/")
		_, parent, _ := strings.Cut(name, ".")
		hosted := func(what, host, target string) odd {
			return odd{name: "host name: " + what, target: target, host: host, canon: []string{target}}
		}
		shapes = append(shapes,
			hosted("the cluster's own, bucket in the path", name, plain),
			hosted("bucket before the cluster's", bucket+"."+name, "/"+key),
			hosted("the same in upper case", strings.ToUpper(bucket+"."+name), "/"+key),
			hosted("the same with a port", bucket+"."+name+":80", "/"+key),
			hosted("the same with a dot at the end", bucket+"."+name+".", "/"+key),
			hosted("the same, and the bucket in the path too", bucket+"."+name, plain),
			hosted("bucket before the cluster's parent domain", bucket+"."+parent, "/"+key),
			hosted("bucket before a name VAST has never heard of", bucket+".s3.deltav.invalid", "/"+key),
			hosted("the bucket alone", bucket, "/"+key),
			hosted("a label and the bucket before the cluster's", "x."+bucket+"."+name, "/"+key),
			hosted("bucket and a label before the cluster's parent domain", bucket+".x."+parent, "/"+key),
			hosted("the cluster's parent domain, bucket in the path", parent, plain),
			hosted("a label before the cluster's parent domain, bucket in the path", "x."+parent, plain),
		)
	}

	decoy := "AWS4-HMAC-SHA256 Credential=AKIADELTAVDECOY00000/" + time.Now().UTC().Format("20060102") +
		"/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + strings.Repeat("0", 64)
	send := func(o odd, canon string) (int, string) {
		host := o.host
		if host == "" {
			host = self
		}
		target := o.target
		if o.query != "" {
			target += "?" + o.query
		}
		if strings.HasPrefix(o.auth, "presigned") {
			query := lab.Presign(http.MethodGet, host, canon, *keys, time.Now(), 5*time.Minute)
			var headers [][2]string
			switch o.auth {
			case "presigned-encoded":
				query = strings.Replace(query, "X-Amz-Credential=", "X-Amz-%43redential=", 1)
			case "presigned-decoy":
				headers = [][2]string{{"Authorization", decoy}}
			}
			return rawRequest(t, listen, http.MethodGet, target+"?"+query, host, headers, "")
		}
		var headers [][2]string
		for _, h := range signed(http.MethodGet, host, canon, o.query, lab.EmptyPayloadHash) {
			if h[0] != "Authorization" {
				headers = append(headers, h)
				continue
			}
			switch o.auth {
			case "decoy-first":
				headers = append(headers, [2]string{"Authorization", decoy}, h)
			case "decoy-second":
				headers = append(headers, h, [2]string{"Authorization", decoy})
			case "decoy-field":
				field := strings.SplitN(strings.TrimPrefix(decoy, "AWS4-HMAC-SHA256 "), ",", 2)[0]
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "AWS4-HMAC-SHA256 ", "AWS4-HMAC-SHA256 "+field+", ", 1)})
			case "lower":
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "Credential=", "credential=", 1)})
			case "upper":
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "Credential=", "CREDENTIAL=", 1)})
			case "spaced":
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "Credential=", "Credential = ", 1)})
			case "algorithm-lower":
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "AWS4-HMAC-SHA256", "aws4-hmac-sha256", 1)})
			case "no-space":
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "AWS4-HMAC-SHA256 ", "AWS4-HMAC-SHA256", 1)})
			case "no-commas":
				headers = append(headers, [2]string{"Authorization", strings.ReplaceAll(h[1], ", ", " ")})
			case "decoy-lookalike":
				field := strings.SplitN(strings.TrimPrefix(decoy, "AWS4-HMAC-SHA256 "), ",", 2)[0]
				headers = append(headers, [2]string{"Authorization", strings.Replace(h[1], "AWS4-HMAC-SHA256 ", "AWS4-HMAC-SHA256 X"+field+", ", 1)})
			default:
				headers = append(headers, h)
			}
		}
		return rawRequest(t, listen, http.MethodGet, target, host, headers, "")
	}

	// round asks in every way and returns the ways that were served the
	// object itself.
	round := func(what string) []string {
		var served []string
		for _, o := range shapes {
			for i, canon := range o.canon {
				status, body := send(o, canon)
				reading := "signed as sent"
				if i > 0 {
					reading = "signed as " + canon
				}
				if len(o.canon) == 1 {
					reading = ""
				}
				outcome := fmt.Sprint(status)
				switch {
				case status == http.StatusOK && body == content:
					outcome += " the object"
					served = append(served, strings.TrimSpace(o.name+" "+reading))
				case s3Code.MatchString(body):
					outcome += " " + s3Code.FindStringSubmatch(body)[1]
				case status == 0:
					outcome = body
				case status == http.StatusOK:
					// Its kind only: a listing would put names in the log.
					outcome += " something else"
					if m := xmlRoot.FindStringSubmatch(body); m != nil {
						outcome += ": " + m[1]
					}
				}
				t.Logf("%-24s %-46s %-44s %s", what, o.name, reading, outcome)
			}
		}
		return served
	}
	// until waits for the plain request to be served, or refused.
	until := func(want bool, what string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			status, body := send(shapes[0], plain)
			if (status == http.StatusOK && body == content) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the plain request answered %d", what, status)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	until(true, "with no rule")
	open := round("no rule")
	t.Logf("VAST serves the object asked for in %d of these ways", len(open))

	for _, rule := range []struct {
		name, about string
		body        map[string]any
	}{
		{"probe-place", "its bucket and prefix", map[string]any{"bucket": []string{bucket}, "prefix": []string{"deltav-live/"}, "blocked": true}},
		{"probe-key", "its access key", map[string]any{"access_key": []string{keys.AccessKey}, "blocked": true}},
		{"probe-operation", "its operation", map[string]any{"operation": []string{"GetObject"}, "blocked": true}},
	} {
		sendAPI(t, s, http.MethodPut, "/api/v1/rules/"+rule.name, rule.body)
		until(false, "blocked by "+rule.about)
		for _, way := range round("blocked by " + rule.about) {
			t.Errorf("a rule that blocks the object by %s let it through when asked for this way: %s", rule.about, way)
		}
		sendAPI(t, s, http.MethodDelete, "/api/v1/rules/"+rule.name, nil)
		until(true, "after removing the rule on "+rule.about)
	}
}
