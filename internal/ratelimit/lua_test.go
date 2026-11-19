package ratelimit

import (
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// The script is run here by a Lua 5.1 interpreter written in Go. Inside
// Envoy it is run by LuaJIT; the integration tests cover that.

// harness stands in for Envoy's request handle: headers are a plain table,
// and what the script says about the request is left in it too, under
// names no header has: "__md.<key>" for metadata, "__status" and "__body"
// for a response it sends itself. A header sent more than once is given as
// "name", "name#2", "name#3".
const harness = `
function __run(headers)
  local hd = {}
  function hd:get(k) return headers[k] end
  function hd:getNumValues(k)
    local n = 0
    if headers[k] then
      n = 1
      while headers[k .. "#" .. (n + 1)] do n = n + 1 end
    end
    return n
  end
  function hd:getAtIndex(k, i)
    if i == 0 then return headers[k] end
    return headers[k .. "#" .. (i + 1)]
  end
  function hd:replace(k, v) headers[k] = v end
  function hd:remove(k) headers[k] = nil end
  local md = {}
  function md:set(ns, k, v) headers["__md." .. k] = v; headers["__md_ns"] = ns end
  local info = {}
  function info:dynamicMetadata() return md end
  local handle = {}
  function handle:headers() return hd end
  function handle:streamInfo() return info end
  function handle:logErr(m) headers["__error"] = m end
  function handle:respond(h, body)
    headers["__status"] = h[":status"]
    headers["__body"] = body
    headers["__content-type"] = h["content-type"]
    headers["__retry-after"] = h["retry-after"]
  end
  envoy_on_request(handle)
  return headers
end
`

func load(t *testing.T, rules ...Rule) *lua.LState {
	t.Helper()
	return loadWith(t, Hosts{}, rules...)
}

func loadWith(t *testing.T, hosts Hosts, rules ...Rule) *lua.LState {
	t.Helper()
	if err := Validate(rules); err != nil {
		t.Fatal(err)
	}
	L := lua.NewState()
	t.Cleanup(L.Close)
	if err := L.DoString(Script(rules, hosts) + harness); err != nil {
		t.Fatalf("the generated script does not load: %v", err)
	}
	return L
}

// identify calls the script's own identify for a request with at most one
// Authorization header and no bucket in its host name.
func identify(t *testing.T, L *lua.LState, method, path, auth, copySource string) (op, ak, bucket, subject string) {
	t.Helper()
	var auths []string
	if auth != "" {
		auths = []string{auth}
	}
	op, ak, bucket, subject, _ = identifyAll(t, L, method, path, "", auths, copySource)
	return
}

// identifyAll calls the script's own identify. several says whether the
// request named more than one access key.
func identifyAll(t *testing.T, L *lua.LState, method, path, host string, auths []string, copySource string) (op, ak, bucket, subject string, several bool) {
	t.Helper()
	arg := func(s string) lua.LValue {
		if s == "" {
			return lua.LNil
		}
		return lua.LString(s)
	}
	list := L.NewTable()
	for _, a := range auths {
		list.Append(lua.LString(a))
	}
	fn := L.GetGlobal("__deltav").(*lua.LTable).RawGetString("identify")
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 5, Protect: true}, lua.LString(method), lua.LString(path), arg(host), list, arg(copySource)); err != nil {
		t.Fatal(err)
	}
	subject = "<none>"
	if s, ok := L.Get(-2).(lua.LString); ok {
		subject = string(s)
	}
	op, ak, bucket, several = L.Get(-5).String(), L.Get(-4).String(), L.Get(-3).String(), lua.LVAsBool(L.Get(-1))
	L.Pop(5)
	return
}

// run sends one request through envoy_on_request and returns the headers
// afterwards.
func run(t *testing.T, L *lua.LState, method, path string, headers map[string]string) map[string]string {
	t.Helper()
	in := L.NewTable()
	in.RawSetString(":method", lua.LString(method))
	in.RawSetString(":path", lua.LString(path))
	for k, v := range headers {
		in.RawSetString(k, lua.LString(v))
	}
	if err := L.CallByParam(lua.P{Fn: L.GetGlobal("__run"), NRet: 1, Protect: true}, in); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	L.Get(-1).(*lua.LTable).ForEach(func(k, v lua.LValue) { out[k.String()] = v.String() })
	L.Pop(1)
	if e := out["__error"]; e != "" {
		t.Fatalf("the script reported an error: %s", e)
	}
	return out
}

// Every row of the table, given the smallest request that satisfies it, is
// classified as that row's operation: the script holds the same table, in
// the same order.
func TestEveryRowOfTheTable(t *testing.T) {
	L := load(t)
	paths := []string{"/", "/bkt", "/bkt/some/key"}
	names := map[string]bool{}
	for _, r := range opTable {
		var q []string
		for _, k := range r.query {
			q = append(q, k+"=x")
		}
		if r.key != "" {
			q = append(q, r.key+"="+r.value)
		}
		path := paths[r.level]
		if len(q) > 0 {
			path += "?" + strings.Join(q, "&")
		}
		copySource := ""
		if r.copy {
			copySource = "other/key"
		}
		want := r.op
		if want == "" {
			want = OpUnknown
		}
		if got, _, _, _ := identify(t, L, r.method, path, "", copySource); got != want {
			t.Errorf("%s %s (copy source %q) = %s, want %s", r.method, path, copySource, got, want)
		}
		names[want] = true
	}
	if n := len(Operations()); n != len(names)-1 || n < 100 { // names also holds Unknown
		t.Errorf("%d operations, %d distinct names in the table", n, len(names))
	}
}

func TestClassification(t *testing.T) {
	L := load(t)
	for _, c := range []struct{ method, path, copySource, want string }{
		{"GET", "/", "", "ListBuckets"},
		{"GET", "/bkt", "", "ListObjects"},
		{"GET", "/bkt/", "", "ListObjects"},
		{"GET", "/bkt?list-type=2&prefix=a/", "", "ListObjectsV2"},
		{"GET", "/bkt?list-type=1", "", "ListObjects"},
		{"GET", "/bkt?prefix=a&list-type=2&max-keys=10", "", "ListObjectsV2"},
		{"GET", "/bkt?uploads", "", "ListMultipartUploads"},
		{"GET", "/bkt?versions&prefix=x", "", "ListObjectVersions"},
		{"GET", "/bkt?analytics&id=7", "", "GetBucketAnalyticsConfiguration"},
		{"GET", "/bkt?analytics", "", "ListBucketAnalyticsConfigurations"},
		{"GET", "/bkt/k", "", "GetObject"},
		{"GET", "/bkt/k?partNumber=3", "", "GetObject"},
		{"GET", "/bkt/k?versionId=abc", "", "GetObject"},
		{"GET", "/bkt/k?uploadId=1", "", "ListParts"},
		{"GET", "/bkt/k?uploads", "", OpUnknown},
		{"GET", "/bkt/k?tagging", "", "GetObjectTagging"},
		{"HEAD", "/bkt/k", "", "HeadObject"},
		{"HEAD", "/bkt", "", "HeadBucket"},
		{"PUT", "/bkt", "", "CreateBucket"},
		{"PUT", "/bkt/k", "", "PutObject"},
		{"PUT", "/bkt/a/b/c%20d", "", "PutObject"},
		{"PUT", "/bkt/k", "src/key", "CopyObject"},
		{"PUT", "/bkt/k?uploadId=1&partNumber=2", "", "UploadPart"},
		{"PUT", "/bkt/k?partNumber=2&uploadId=1", "src/key", "UploadPartCopy"},
		{"PUT", "/bkt/k?uploadId=1", "", "PutObject"}, // an upload ID with no part number is not a part
		{"PUT", "/bkt/k?tagging", "", "PutObjectTagging"},
		{"PUT", "/bkt?versioning", "", "PutBucketVersioning"},
		{"POST", "/bkt/k?uploads", "", "CreateMultipartUpload"},
		{"POST", "/bkt/k?uploads&uploadId=1", "", "CreateMultipartUpload"},
		{"POST", "/bkt/k?uploadId=1", "", "CompleteMultipartUpload"},
		{"POST", "/bkt?delete", "", "DeleteObjects"},
		{"POST", "/bkt", "", "PostObject"},
		{"POST", "/bkt/k?select&select-type=2", "", "SelectObjectContent"},
		{"POST", "/bkt/k?select&select-type=1", "", OpUnknown},
		{"DELETE", "/bkt/k", "", "DeleteObject"},
		{"DELETE", "/bkt/k?uploadId=1", "", "AbortMultipartUpload"},
		{"DELETE", "/bkt", "", "DeleteBucket"},
		{"OPTIONS", "/bkt/k", "", OpPreflight},
		{"PATCH", "/bkt/k", "", OpUnknown},
		{"POST", "/", "", OpUnknown},
		// Query keys are the bytes on the wire: another case, or an encoded
		// letter, is another key.
		{"GET", "/bkt/k?UploadId=1", "", "GetObject"},
		{"GET", "/bkt?upload%73", "", "ListObjects"},
	} {
		if got, _, _, _ := identify(t, L, c.method, c.path, "", c.copySource); got != c.want {
			t.Errorf("%s %s (copy source %q) = %s, want %s", c.method, c.path, c.copySource, got, c.want)
		}
	}
}

func TestIdentity(t *testing.T) {
	L := load(t)
	const v4 = "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20261118/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc"
	for _, c := range []struct{ what, method, path, auth, ak, bucket, subject string }{
		{"SigV4 header", "GET", "/data/a/b.bin", v4, "AKIAEXAMPLE1", "data", "a/b.bin"},
		{"SigV4 with the parts in another order", "GET", "/data/k", "AWS4-HMAC-SHA256 SignedHeaders=host,Credential=AKIAOTHER/20261118/us-east-1/s3/aws4_request,Signature=abc", "AKIAOTHER", "data", "k"},
		{"SigV2 header", "GET", "/data/k", "AWS AKIAV2KEY:c2lnbmF0dXJl", "AKIAV2KEY", "data", "k"},
		{"presigned SigV4", "GET", "/data/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAPRE%2F20261118%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=abc", "", "AKIAPRE", "data", "k"},
		{"presigned SigV4, slashes not encoded", "GET", "/data/k?X-Amz-Credential=AKIAPRE/20261118/us-east-1/s3/aws4_request", "", "AKIAPRE", "data", "k"},
		{"presigned SigV2", "GET", "/data/k?AWSAccessKeyId=AKIAOLD&Expires=1&Signature=x", "", "AKIAOLD", "data", "k"},
		{"credential= in lower case, which VAST accepts", "GET", "/data/k", "AWS4-HMAC-SHA256 credential=AKIALOWER/20261118/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc", "AKIALOWER", "data", "k"},
		{"CREDENTIAL= in upper case, likewise", "GET", "/data/k", "AWS4-HMAC-SHA256 CREDENTIAL=AKIAUPPER/20261118/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc", "AKIAUPPER", "data", "k"},
		{"spaces around the =", "GET", "/data/k", "AWS4-HMAC-SHA256 Credential = AKIASPACED/20261118/us-east-1/s3/aws4_request", "AKIASPACED", "data", "k"},
		{"a presigned parameter in another case", "GET", "/data/k?x-amz-credential=AKIAPRE%2F20261118", "", "AKIAPRE", "data", "k"},
		{"a presigned parameter with its name encoded", "GET", "/data/k?X-Amz-%43redential=AKIAPRE%2F20261118", "", "AKIAPRE", "data", "k"},
		{"the same key in the header and the query", "GET", "/data/k?X-Amz-Credential=AKIAEXAMPLE1%2Fx", v4, "AKIAEXAMPLE1", "data", "k"},
		{"anonymous", "GET", "/data/k", "", "-", "data", "k"},
		{"another scheme", "GET", "/data/k", "Bearer abc.def", "-", "data", "k"},
		{"the key is decoded once", "PUT", "/data/a%20b/c%2Bd+e%252F", v4, "AKIAEXAMPLE1", "data", "a b/c+d+e%2F"},
		{"a listing: its prefix parameter", "GET", "/data?list-type=2&prefix=logs%2F2026+11%2F", v4, "AKIAEXAMPLE1", "data", "logs/2026 11/"},
		{"a listing with no prefix", "GET", "/data?list-type=2", v4, "AKIAEXAMPLE1", "data", ""},
		{"a bucket operation that is not a listing", "GET", "/data?tagging&prefix=logs/", v4, "AKIAEXAMPLE1", "data", "<none>"},
		{"the service level", "GET", "/", v4, "AKIAEXAMPLE1", "", "<none>"},
		{"a trailing slash is still the bucket", "GET", "/data/", v4, "AKIAEXAMPLE1", "data", ""},
		// The path is decoded, then split: what VAST does.
		{"the slash after the bucket encoded", "GET", "/data%2Fa/b.bin", v4, "AKIAEXAMPLE1", "data", "a/b.bin"},
		{"the bucket encoded", "GET", "/%64ata/k", v4, "AKIAEXAMPLE1", "data", "k"},
		{"the bucket encoded twice is another name", "GET", "/%2564ata/k", v4, "AKIAEXAMPLE1", "%64ata", "k"},
		{"two slashes after the bucket: the key starts with one", "GET", "/data//k", v4, "AKIAEXAMPLE1", "data", "/k"},
		{"dot segments are part of the key", "GET", "/data/x/../k", v4, "AKIAEXAMPLE1", "data", "x/../k"},
		{"two slashes at the start: the service, and no bucket", "GET", "//data/k", v4, "AKIAEXAMPLE1", "", "<none>"},
		{"the same with the second slash encoded", "GET", "/%2Fdata/k", v4, "AKIAEXAMPLE1", "", "<none>"},
	} {
		_, ak, bucket, subject := identify(t, L, c.method, c.path, c.auth, "")
		if ak != c.ak || bucket != c.bucket || subject != c.subject {
			t.Errorf("%s: access key %q, bucket %q, prefix subject %q; want %q, %q, %q", c.what, ak, bucket, subject, c.ak, c.bucket, c.subject)
		}
	}
	// VAST answers a path that starts "//" as a listing of buckets.
	if op, _, _, _ := identify(t, L, "GET", "//data/k", v4, ""); op != "ListBuckets" {
		t.Errorf("GET //data/k is taken for %s, want ListBuckets", op)
	}
}

// A request can name more than one access key. Which of them a server acts
// on is its own business (VAST: a presigned URL's before a header's, the
// first "credential=" in a header), so the script reports that there are
// several and leaves the choice to no one.
func TestSeveralAccessKeys(t *testing.T) {
	L := load(t)
	const real = "AWS4-HMAC-SHA256 Credential=AKIAREAL/20261118/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc"
	const decoy = "AWS4-HMAC-SHA256 Credential=AKIADECOY/20261118/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=000"
	for _, c := range []struct {
		what, path string
		auths      []string
		first      string
		several    bool
	}{
		{"one header", "/data/k", []string{real}, "AKIAREAL", false},
		{"two headers", "/data/k", []string{decoy, real}, "AKIADECOY", true},
		{"two headers joined into one, as a proxy may", "/data/k", []string{real + "," + decoy}, "AKIAREAL", true},
		{"two Credential fields in one header", "/data/k", []string{strings.Replace(real, "Credential=", "Credential=AKIADECOY/x, Credential=", 1)}, "AKIADECOY", true},
		{"a field whose name ends in Credential", "/data/k", []string{strings.Replace(real, "Credential=", "XCredential=AKIADECOY/x, Credential=", 1)}, "AKIADECOY", true},
		{"a presigned URL and a header", "/data/k?X-Amz-Credential=AKIAREAL%2Fx&X-Amz-Signature=abc", []string{decoy}, "AKIADECOY", true},
		{"a presigned URL with two credentials", "/data/k?X-Amz-Credential=AKIADECOY%2Fx&X-Amz-Credential=AKIAREAL%2Fx", nil, "AKIADECOY", true},
		{"both presigned forms", "/data/k?AWSAccessKeyId=AKIADECOY&X-Amz-Credential=AKIAREAL%2Fx", nil, "AKIADECOY", true},
		{"signature versions 2 and 4", "/data/k", []string{"AWS AKIADECOY:c2ln", real}, "AKIADECOY", true},
		{"the same key every time", "/data/k?X-Amz-Credential=AKIAREAL%2Fx", []string{real, real}, "AKIAREAL", false},
		{"none", "/data/k", nil, "-", false},
	} {
		_, ak, _, _, several := identifyAll(t, L, "GET", c.path, "", c.auths, "")
		if ak != c.first || several != c.several {
			t.Errorf("%s: access key %q, several %v; want %q, %v", c.what, ak, several, c.first, c.several)
		}
	}

	// With a rule that looks at access keys, such a request is refused: not
	// throttled, but told it is malformed. Nothing is counted.
	headers := map[string]string{"authorization": decoy, "authorization#2": real}
	for _, rule := range []Rule{
		{Name: "by-key", LimitPerSecond: 5, Per: []string{PerAccessKey}},
		{Name: "one-key", AccessKey: []string{"AKIAREAL"}, Blocked: true},
	} {
		L := load(t, rule)
		out := run(t, L, "GET", "/data/k", headers)
		if got := slots(out); got != "REFUSED by "+RefusedSeveralKeys+" (400)" {
			t.Errorf("rule %s: %q", rule.Name, got)
		}
		if out["__body"] != RefusalBody(400) || !strings.Contains(out["__body"], "<Code>InvalidRequest</Code>") || out["__retry-after"] != "" || out[SlotKeyHeader(1)] != "" {
			t.Errorf("rule %s: answered %v", rule.Name, out)
		}
		if out["__md."+MetadataOperation] != "GetObject" || out["__md."+MetadataBucket] != "data" {
			t.Errorf("rule %s: metadata %v", rule.Name, out)
		}
		// One key, named twice, is no doubt at all.
		if got := slots(run(t, L, "GET", "/data/k", map[string]string{"authorization": decoy, "authorization#2": decoy})); strings.HasPrefix(got, "REFUSED") {
			t.Errorf("rule %s: the same key twice: %q", rule.Name, got)
		}
	}
	// With no rule that looks at access keys it makes no difference whose
	// the request is, and it is treated as any other.
	L = load(t, Rule{Name: "gets", Operation: []string{"GetObject"}, LimitPerSecond: 5, Per: []string{PerBucket}})
	if got := slots(run(t, L, "GET", "/data/k", headers)); got != "gets=data" {
		t.Errorf("with no rule about access keys: %q", got)
	}
	if nameRE.MatchString(RefusedSeveralKeys) {
		t.Error("a rule could be given the name that marks these refusals")
	}
}

// A bucket can be in the host name. VAST looks for it there when the host
// name is under the cluster's DNS suffix, and so must the script, or a rule
// about a bucket is avoided by naming the bucket in the other place.
func TestBucketInTheHostName(t *testing.T) {
	hosts := Hosts{Suffixes: []string{"DNS1.example.com."}, Pools: []string{"main", "Second", "", "main"}}
	L := loadWith(t, hosts)
	for _, c := range []struct{ what, host, path, bucket, subject string }{
		{"the pool's own name: the bucket is in the path", "main.dns1.example.com", "/data/k", "data", "k"},
		{"bucket before the pool's name", "data.main.dns1.example.com", "/k", "data", "k"},
		{"before another pool's name", "data.second.dns1.example.com", "/a/b", "data", "a/b"},
		{"with a port", "data.main.dns1.example.com:8080", "/k", "data", "k"},
		{"with a dot at the end", "data.main.dns1.example.com.", "/k", "data", "k"},
		{"with both", "data.main.dns1.example.com.:80", "/k", "data", "k"},
		{"the domain in upper case; the bucket is taken as written", "Data.MAIN.DNS1.EXAMPLE.COM", "/k", "Data", "k"},
		{"bucket before the suffix, no pool", "data.dns1.example.com", "/k", "data", "k"},
		{"then the path is all key, whatever it starts with", "data.dns1.example.com", "/data/k", "data", "data/k"},
		{"and is decoded like any other", "data.dns1.example.com", "/a%2Fb%20c", "data", "a/b c"},
		{"a bucket with a dot in it", "x.data.main.dns1.example.com", "/k", "x.data", "k"},
		{"a label that is no pool belongs to the bucket", "data.x.dns1.example.com", "/k", "data.x", "k"},
		{"the suffix alone", "dns1.example.com", "/data/k", "data", "k"},
		{"a name not under the suffix", "data.main.dns2.example.com", "/data/k", "data", "k"},
		{"a name that only ends like the suffix", "data.xdns1.example.com", "/data/k", "data", "k"},
		{"an address", "127.0.0.1:9000", "/data/k", "data", "k"},
		{"an IPv6 address", "[::1]:9000", "/data/k", "data", "k"},
		{"no host at all", "", "/data/k", "data", "k"},
		{"the bucket itself, addressed by host: a listing with no prefix", "data.main.dns1.example.com", "/", "data", ""},
		{"a listing addressed by host", "data.main.dns1.example.com", "/?list-type=2&prefix=logs/", "data", "logs/"},
	} {
		op, _, bucket, subject, _ := identifyAll(t, L, "GET", c.path, c.host, nil, "")
		if bucket != c.bucket || subject != c.subject {
			t.Errorf("%s (%s %s): bucket %q, subject %q (%s); want %q, %q", c.what, c.host, c.path, bucket, subject, op, c.bucket, c.subject)
		}
	}
	if op, _, _, _, _ := identifyAll(t, L, "GET", "/", "data.main.dns1.example.com", nil, ""); op != "ListObjects" {
		t.Errorf("GET / on a bucket's host name is taken for %s, want ListObjects", op)
	}
	// A target whose cluster's names are not known reads every request by
	// its path.
	if _, _, bucket, _, _ := identifyAll(t, load(t), "GET", "/k", "data.main.dns1.example.com", nil, ""); bucket != "k" {
		t.Errorf("with no names known: bucket %q", bucket)
	}

	// And so a rule about a bucket holds for both ways of naming it.
	L = loadWith(t, hosts,
		Rule{Name: "no-deletes", Operation: []string{"DeleteObject"}, Bucket: []string{"archive"}, Blocked: true},
		Rule{Name: "per-bucket", LimitPerSecond: 5, Per: []string{PerBucket}})
	for _, c := range []struct{ host, path, want string }{
		{"127.0.0.1:9000", "/archive/x", "REFUSED by no-deletes (503)"},
		{"archive.main.dns1.example.com", "/x", "REFUSED by no-deletes (503)"},
		{"archive.dns1.example.com:80", "/x", "REFUSED by no-deletes (503)"},
		{"main.dns1.example.com", "/archive/x", "REFUSED by no-deletes (503)"},
		{"other.main.dns1.example.com", "/archive/x", "per-bucket=other"},
		{"other.main.dns1.example.com", "/x", "per-bucket=other"},
	} {
		if got := slots(run(t, L, "DELETE", c.path, map[string]string{":authority": c.host})); got != c.want {
			t.Errorf("DELETE %s on %s: %q, want %q", c.path, c.host, got, c.want)
		}
	}
}

// slots says what the script did with a request: the rule and count name
// in each slot, in order, as "rule=key" (with "!429" if a refusal is to be
// a 429), or "REFUSED by rule (status)" if it answered the request itself.
func slots(out map[string]string) string {
	if st := out["__status"]; st != "" {
		return "REFUSED by " + out["__md."+MetadataRefusedBy] + " (" + st + ")"
	}
	var got []string
	for j := 1; j <= Slots+1; j++ {
		rule, key := out[SlotRuleHeader(j)], out[SlotKeyHeader(j)]
		if rule == "" && key == "" {
			continue
		}
		s := rule + "=" + key
		if st := out[SlotStatusHeader(j)]; st != "" {
			s += "!" + st
		}
		got = append(got, s)
	}
	return strings.Join(got, " ")
}

func TestRuleMatching(t *testing.T) {
	const ak1, ak2 = "AKIAEXAMPLE1", "AKIAEXAMPLE2"
	auth := func(ak string) map[string]string {
		return map[string]string{"authorization": "AWS4-HMAC-SHA256 Credential=" + ak + "/20261118/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc"}
	}
	L := load(t,
		// tollbooth's own example.
		Rule{Name: "uploads", Operation: []string{"PutObject", "UploadPart", "CompleteMultipartUpload"}, AccessKey: []string{ak1, ak2},
			Bucket: []string{"training-data", "checkpoints"}, Prefix: []string{"ckpt/", "logs/"}, LimitPerSecond: 2000, Burst: 4000, Per: []string{PerAccessKey, PerBucket}},
		// A family, counted in total.
		Rule{Name: "listings", Operation: []string{"List*"}, LimitPerMinute: 600, Per: []string{PerTotal}, Respond: 429},
		// Every operation, per access key and per client host.
		Rule{Name: "per-key", LimitPerSecond: 100, Per: []string{PerAccessKey, PerClient}},
		// Counted per prefix; the longest matching prefix is the one.
		Rule{Name: "by-prefix", Bucket: []string{"data"}, Prefix: []string{"a/", "a/b/", "odd, one\"]]\n--"[:9]}, LimitPerSecond: 5, Per: []string{PerPrefix}},
		// Blocked.
		Rule{Name: "no-deletes", Operation: []string{"DeleteObject", "DeleteObjects"}, Bucket: []string{"archive"}, Blocked: true},
	)
	marks := func(method, path string, headers map[string]string) string {
		t.Helper()
		return slots(run(t, L, method, path, headers))
	}
	for _, c := range []struct {
		what, method, path string
		headers            map[string]string
		want               string
	}{
		{"an upload that passes every filter of the first rule", "PUT", "/checkpoints/ckpt/step-9.pt", auth(ak1), "uploads=" + ak1 + ",checkpoints per-key=" + ak1},
		{"another key and bucket: its own count", "PUT", "/training-data/logs/x", auth(ak2), "uploads=" + ak2 + ",training-data per-key=" + ak2},
		{"an access key the rule does not list", "PUT", "/checkpoints/ckpt/x", auth("AKIAOTHER"), "per-key=AKIAOTHER"},
		{"a bucket the rule does not list", "PUT", "/scratch/ckpt/x", auth(ak1), "per-key=" + ak1},
		{"a key under none of the prefixes", "PUT", "/checkpoints/tmp/x", auth(ak1), "per-key=" + ak1},
		{"an operation the rule does not list", "GET", "/checkpoints/ckpt/x", auth(ak1), "per-key=" + ak1},
		{"a part of a multipart upload", "PUT", "/checkpoints/ckpt/x?partNumber=1&uploadId=u", auth(ak1), "uploads=" + ak1 + ",checkpoints per-key=" + ak1},
		{"a listing: the family, in total, refused with 429", "GET", "/checkpoints?list-type=2&prefix=ckpt/", auth(ak1), "listings=*!429 per-key=" + ak1},
		{"ListParts is in the family too", "GET", "/checkpoints/ckpt/x?uploadId=u", auth(ak1), "listings=*!429 per-key=" + ak1},
		{"anonymous requests are counted together", "GET", "/public/x", nil, "per-key=-"},
		{"the longest prefix is the one counted", "GET", "/data/a/b/c", auth(ak1), "per-key=" + ak1 + " by-prefix=a/b/"},
		{"the shorter prefix", "GET", "/data/a/x", auth(ak1), "per-key=" + ak1 + " by-prefix=a/"},
		{"a listing is matched by its prefix parameter", "GET", "/data?list-type=2&prefix=a/b/c", auth(ak1), "listings=*!429 per-key=" + ak1 + " by-prefix=a/b/"},
		{"a listing with a shorter prefix than the rule's does not match it", "GET", "/data?list-type=2&prefix=a", auth(ak1), "listings=*!429 per-key=" + ak1},
		{"a prefix with marks in it is counted under its encoding", "GET", "/data/odd,%20one%22/x", auth(ak1), "per-key=" + ak1 + " by-prefix=odd%2C%20one%22"},
		{"blocked: the script answers", "DELETE", "/archive/x", auth(ak1), "REFUSED by no-deletes (503)"},
		{"blocked, the batch form", "POST", "/archive?delete", auth(ak1), "REFUSED by no-deletes (503)"},
		{"not blocked elsewhere", "DELETE", "/data/x", auth(ak1), "per-key=" + ak1},
		// The bucket is what a server would take it to be, however the
		// client spells it.
		{"blocked, the bucket percent-encoded", "DELETE", "/arch%69ve/x", auth(ak1), "REFUSED by no-deletes (503)"},
		{"blocked, the whole bucket percent-encoded", "DELETE", "/%61%72%63%68%69%76%65/x", auth(ak1), "REFUSED by no-deletes (503)"},
		{"blocked, the slash after the bucket percent-encoded", "DELETE", "/archive%2Fx", auth(ak1), "REFUSED by no-deletes (503)"},
		{"behind a second slash there is no bucket: VAST answers for the service", "DELETE", "//archive/x", auth(ak1), "per-key=" + ak1},
		{"an encoded bucket is the same bucket to a filter", "PUT", "/check%70oints/ckpt/x", auth(ak1), "uploads=" + ak1 + ",checkpoints per-key=" + ak1},
	} {
		if got := marks(c.method, c.path, c.headers); got != c.want {
			t.Errorf("%s (%s %s): %q, want %q", c.what, c.method, c.path, got, c.want)
		}
	}

	// What a refusal by the script looks like to the client: S3's own
	// throttling answer.
	out := run(t, L, "DELETE", "/archive/x", auth(ak1))
	if out["__status"] != "503" || out["__content-type"] != "application/xml" || out["__retry-after"] != "1" ||
		!strings.Contains(out["__body"], "<Code>SlowDown</Code>") || out["__body"] != RefusalBody(503) {
		t.Errorf("a blocked request was answered %v", out)
	}
	// And what the daemon is told of a request that a rule counted or
	// refused, by way of the access log.
	if out["__md_ns"] != MetadataNamespace || out["__md."+MetadataRefusedBy] != "no-deletes" || out["__md."+MetadataOperation] != "DeleteObject" ||
		out["__md."+MetadataAccessKey] != ak1 || out["__md."+MetadataBucket] != "archive" {
		t.Errorf("metadata of a blocked request: %v", out)
	}
	out = run(t, L, "GET", "/data?list-type=2&prefix=a/b/c", auth(ak1))
	if out["__md."+MetadataRules] != "listings,per-key,by-prefix" || out["__md."+MetadataOperation] != "ListObjectsV2" || out["__md."+MetadataRefusedBy] != "" {
		t.Errorf("metadata of a counted request: %v", out)
	}

	// A client cannot mark itself: what it sends under the script's header
	// names is dropped, whether or not a rule matches.
	h := auth("AKIAOTHER")
	h["x-deltav-rl-1"], h["x-deltav-rr-1"], h["x-deltav-rs-1"] = "someone-else", "uploads", "429"
	h["x-deltav-rl-3"], h["x-deltav-rr-3"], h["x-deltav-rl-4"], h["x-deltav-rr-4"] = "someone-else", "per-key", "*", "listings"
	if got := marks("PUT", "/checkpoints/ckpt/x", h); got != "per-key=AKIAOTHER" {
		t.Errorf("with forged headers: %q", got)
	}
	// Nor can it grow a count's name without bound, or put anything in it
	// but plain characters.
	long := strings.Repeat("A", 5000)
	got := run(t, L, "GET", "/public/x", map[string]string{"authorization": "AWS " + long + ":sig"})
	if len(got["x-deltav-rl-1"]) != 200 || len(got["__md."+MetadataAccessKey]) != 200 {
		t.Errorf("a 5000-byte access key is counted under %d bytes and noted in %d", len(got["x-deltav-rl-1"]), len(got["__md."+MetadataAccessKey]))
	}
	got = run(t, L, "GET", "/b%C3%28%00/x", map[string]string{"authorization": "AWS a b,c\td\x01e\xff:sig"})
	if got["x-deltav-rl-1"] != "a%20b%2Cc%09d%01e%FF" || got["__md."+MetadataAccessKey] != "a%20b%2Cc%09d%01e%FF" || got["__md."+MetadataBucket] != "b%C3%28%00" {
		t.Errorf("odd bytes are counted under %q and noted as %q, bucket %q", got["x-deltav-rl-1"], got["__md."+MetadataAccessKey], got["__md."+MetadataBucket])
	}
	// Other headers are left alone, and a request no rule matches is not
	// touched at all.
	out = run(t, L, "GET", "/public/x", map[string]string{"x-amz-date": "20261118T000000Z", "host": "127.0.0.1:9000"})
	if out["x-amz-date"] != "20261118T000000Z" || out["host"] != "127.0.0.1:9000" || out[":path"] != "/public/x" {
		t.Errorf("the script changed a header that is not its own: %v", out)
	}
}

// A rule that counts per bucket counts a request that names no bucket too,
// under a name of its own: there is no request it lets through uncounted.
func TestNothingMatchedGoesUncounted(t *testing.T) {
	L := load(t, Rule{Name: "per-bucket", LimitPerSecond: 5, Per: []string{PerBucket}})
	for path, want := range map[string]string{
		"/":             "per-bucket=-",
		"//":            "per-bucket=-",
		"/?max-keys=1":  "per-bucket=-",
		"/data":         "per-bucket=data",
		"//data/k":      "per-bucket=-",
		"///data//k":    "per-bucket=-",
		"/data%2Fk":     "per-bucket=data",
		"/data//k":      "per-bucket=data",
		"/d%61ta/k":     "per-bucket=data",
		"/%2D/k":        "per-bucket=-",
		"/%20/k":        "per-bucket=%20",
		"/a%2Cb/k":      "per-bucket=a%2Cb",
		"/data/k?x=//y": "per-bucket=data",
	} {
		if got := slots(run(t, L, "GET", path, nil)); got != want {
			t.Errorf("GET %s: %q, want %q", path, got, want)
		}
	}
	out := run(t, L, "GET", "/", nil)
	if out[SlotKeyHeader(1)] == "" {
		t.Error("a count's name must never be empty: the quota filter would not see the request")
	}
}

// One request can be limited by as many rules as there are slots. Past
// that the first ones in the order of the rules count, and the script says
// that some did not. A blocked rule refuses wherever it comes in the order.
func TestMoreRulesThanSlots(t *testing.T) {
	var rules []Rule
	for i := 1; i <= Slots+2; i++ {
		rules = append(rules, Rule{Name: "r" + string(rune('0'+i)), LimitPerSecond: float64(i), Per: []string{PerTotal}})
	}
	L := load(t, rules...)
	out := run(t, L, "GET", "/b/k", nil)
	if got := slots(out); got != "r1=* r2=* r3=* r4=*" || Slots != 4 {
		t.Errorf("with %d rules: %q", len(rules), got)
	}
	if out["__md."+MetadataSkipped] != "true" || out["__md."+MetadataRules] != "r1,r2,r3,r4" {
		t.Errorf("metadata with more rules than slots: %v", out)
	}
	L = load(t, rules[:Slots]...)
	if out := run(t, L, "GET", "/b/k", nil); out["__md."+MetadataSkipped] != "" || slots(out) != "r1=* r2=* r3=* r4=*" {
		t.Errorf("with exactly as many rules as slots: %v", out)
	}
	L = load(t, append(rules, Rule{Name: "stop", Operation: []string{"GetObject"}, Blocked: true, Respond: 429})...)
	out = run(t, L, "GET", "/b/k", nil)
	if got := slots(out); got != "REFUSED by stop (429)" || !strings.Contains(out["__body"], "<Code>TooManyRequests</Code>") {
		t.Errorf("a blocked rule after more rules than slots: %q, %v", got, out["__body"])
	}
	if got := slots(run(t, L, "PUT", "/b/k", nil)); got != "r1=* r2=* r3=* r4=*" {
		t.Errorf("a request the blocked rule does not match: %q", got)
	}
}

// Values in rules are data to the script, whatever they contain.
func TestScriptCannotBeBrokenByARule(t *testing.T) {
	hostile := []string{`"]] os.exit() --[[`, "a\\", "\"..error('x')..\"", "]==]", "k\tv", "ünïcode/"}
	r := Rule{Name: "odd", AccessKey: hostile, Bucket: hostile, Prefix: hostile, LimitPerSecond: 1, Per: []string{PerBucket, PerPrefix}}
	L := load(t, r)
	for _, v := range hostile {
		enc := strings.NewReplacer("%", "%25").Replace(v)
		path := "/" + v + "/" + enc + "tail"
		out := run(t, L, "GET", path, map[string]string{"authorization": "AWS " + v + ":sig"})
		if strings.Contains(v, "/") || strings.ContainsAny(v, ":") {
			continue // not expressible as one path segment or one SigV2 key; loading was the test
		}
		if out["x-deltav-rl-1"] == "" {
			t.Errorf("value %q: the request was not matched", v)
		}
	}
	if s := Script([]Rule{r}, Hosts{}); strings.Contains(s, "os.exit") && !strings.Contains(s, `os.exit\040\041`) && strings.Contains(s, `"]] os.exit()`) {
		t.Error("a rule's value appears in the script unescaped")
	}
}

func TestLuaString(t *testing.T) {
	for in, want := range map[string]string{
		"":         `""`,
		"abc/1.-_": `"abc/1.-_"`,
		"a;b{c}":   `"a\059b\123c\125"`,
		`a"b`:      `"a\034b"`,
		"a\nb":     `"a\010b"`,
		`\`:        `"\092"`,
		"é":        `"\195\169"`,
	} {
		if got := luaString(in); got != want {
			t.Errorf("luaString(%q) = %s, want %s", in, got, want)
		}
	}
}
