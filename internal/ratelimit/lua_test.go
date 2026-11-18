package ratelimit

import (
	"sort"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// The script is run here by a Lua 5.1 interpreter written in Go. Inside
// Envoy it is run by LuaJIT; the integration tests cover that.

// harness stands in for Envoy's request handle: headers are a plain table.
const harness = `
function __run(headers)
  local hd = {}
  function hd:get(k) return headers[k] end
  function hd:replace(k, v) headers[k] = v end
  function hd:remove(k) headers[k] = nil end
  local handle = {}
  function handle:headers() return hd end
  function handle:logErr(m) headers["__error"] = m end
  envoy_on_request(handle)
  return headers
end
`

func load(t *testing.T, rules ...Rule) *lua.LState {
	t.Helper()
	if err := Validate(rules); err != nil {
		t.Fatal(err)
	}
	L := lua.NewState()
	t.Cleanup(L.Close)
	if err := L.DoString(Script(rules) + harness); err != nil {
		t.Fatalf("the generated script does not load: %v", err)
	}
	return L
}

// identify calls the script's own identify.
func identify(t *testing.T, L *lua.LState, method, path, auth, copySource string) (op, ak, bucket, subject string) {
	t.Helper()
	arg := func(s string) lua.LValue {
		if s == "" {
			return lua.LNil
		}
		return lua.LString(s)
	}
	fn := L.GetGlobal("__deltav").(*lua.LTable).RawGetString("identify")
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 4, Protect: true}, lua.LString(method), lua.LString(path), arg(auth), arg(copySource)); err != nil {
		t.Fatal(err)
	}
	subject = "<none>"
	if s, ok := L.Get(-1).(lua.LString); ok {
		subject = string(s)
	}
	op, ak, bucket = L.Get(-4).String(), L.Get(-3).String(), L.Get(-2).String()
	L.Pop(4)
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
		{"a header wins over the query", "GET", "/data/k?X-Amz-Credential=AKIAPRE%2Fx", v4, "AKIAEXAMPLE1", "data", "k"},
		{"anonymous", "GET", "/data/k", "", "-", "data", "k"},
		{"another scheme", "GET", "/data/k", "Bearer abc.def", "-", "data", "k"},
		{"the key is decoded once", "PUT", "/data/a%20b/c%2Bd+e%252F", v4, "AKIAEXAMPLE1", "data", "a b/c+d+e%2F"},
		{"a listing: its prefix parameter", "GET", "/data?list-type=2&prefix=logs%2F2026+11%2F", v4, "AKIAEXAMPLE1", "data", "logs/2026 11/"},
		{"a listing with no prefix", "GET", "/data?list-type=2", v4, "AKIAEXAMPLE1", "data", ""},
		{"a bucket operation that is not a listing", "GET", "/data?tagging&prefix=logs/", v4, "AKIAEXAMPLE1", "data", "<none>"},
		{"the service level", "GET", "/", v4, "AKIAEXAMPLE1", "", "<none>"},
		{"a trailing slash is still the bucket", "GET", "/data/", v4, "AKIAEXAMPLE1", "data", ""},
	} {
		_, ak, bucket, subject := identify(t, L, c.method, c.path, c.auth, "")
		if ak != c.ak || bucket != c.bucket || subject != c.subject {
			t.Errorf("%s: access key %q, bucket %q, prefix subject %q; want %q, %q, %q", c.what, ak, bucket, subject, c.ak, c.bucket, c.subject)
		}
	}
}

func TestRuleMatching(t *testing.T) {
	const ak1, ak2 = "AKIAEXAMPLE1", "AKIAEXAMPLE2"
	auth := func(ak string) map[string]string {
		return map[string]string{"authorization": "AWS4-HMAC-SHA256 Credential=" + ak + "/20261118/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc"}
	}
	L := load(t,
		// 1: tollbooth's own example.
		Rule{Name: "tenant-a-uploads", Operation: []string{"PutObject", "UploadPart", "CompleteMultipartUpload"}, AccessKey: []string{ak1, ak2},
			Bucket: []string{"training-data", "checkpoints"}, Prefix: []string{"ckpt/", "logs/"}, LimitPerSecond: 2000, Burst: 4000, Per: []string{PerAccessKey, PerBucket}},
		// 2: a family, counted in total.
		Rule{Name: "all-listings", Operation: []string{"List*"}, LimitPerMinute: 600, Per: []string{PerTotal}},
		// 3: every operation, per access key and per client host.
		Rule{Name: "per-key", LimitPerSecond: 100, Per: []string{PerAccessKey, PerClient}},
		// 4: counted per prefix; the longest matching prefix is the one.
		Rule{Name: "by-prefix", Bucket: []string{"data"}, Prefix: []string{"a/", "a/b/", "odd, one\"]]\n--"[:9]}, LimitPerSecond: 5, Per: []string{PerPrefix}},
		// 5: blocked.
		Rule{Name: "no-deletes", Operation: []string{"DeleteObject", "DeleteObjects"}, Bucket: []string{"archive"}, Blocked: true},
	)
	marks := func(method, path string, headers map[string]string) string {
		t.Helper()
		out := run(t, L, method, path, headers)
		var got []string
		for k, v := range out {
			if n, ok := strings.CutPrefix(k, "x-deltav-rl-"); ok {
				got = append(got, n+"="+v)
			}
		}
		sort.Strings(got)
		return strings.Join(got, " ")
	}
	for _, c := range []struct {
		what, method, path string
		headers            map[string]string
		want               string
	}{
		{"an upload that passes every filter of rule 1", "PUT", "/checkpoints/ckpt/step-9.pt", auth(ak1), "1=" + ak1 + ",checkpoints 3=" + ak1},
		{"another key and bucket: its own count", "PUT", "/training-data/logs/x", auth(ak2), "1=" + ak2 + ",training-data 3=" + ak2},
		{"an access key the rule does not list", "PUT", "/checkpoints/ckpt/x", auth("AKIAOTHER"), "3=AKIAOTHER"},
		{"a bucket the rule does not list", "PUT", "/scratch/ckpt/x", auth(ak1), "3=" + ak1},
		{"a key under none of the prefixes", "PUT", "/checkpoints/tmp/x", auth(ak1), "3=" + ak1},
		{"an operation the rule does not list", "GET", "/checkpoints/ckpt/x", auth(ak1), "3=" + ak1},
		{"a part of a multipart upload", "PUT", "/checkpoints/ckpt/x?partNumber=1&uploadId=u", auth(ak1), "1=" + ak1 + ",checkpoints 3=" + ak1},
		{"a listing: the family, in total", "GET", "/checkpoints?list-type=2&prefix=ckpt/", auth(ak1), "2=* 3=" + ak1},
		{"ListParts is in the family too", "GET", "/checkpoints/ckpt/x?uploadId=u", auth(ak1), "2=* 3=" + ak1},
		{"anonymous requests are counted together", "GET", "/public/x", nil, "3=-"},
		{"the longest prefix is the one counted", "GET", "/data/a/b/c", auth(ak1), "3=" + ak1 + " 4=a/b/"},
		{"the shorter prefix", "GET", "/data/a/x", auth(ak1), "3=" + ak1 + " 4=a/"},
		{"a listing is matched by its prefix parameter", "GET", "/data?list-type=2&prefix=a/b/c", auth(ak1), "2=* 3=" + ak1 + " 4=a/b/"},
		{"a listing with a shorter prefix than the rule's does not match it", "GET", "/data?list-type=2&prefix=a", auth(ak1), "2=* 3=" + ak1},
		{"a prefix with marks in it is counted under its encoding", "GET", "/data/odd,%20one%22/x", auth(ak1), "3=" + ak1 + " 4=odd%2C%20one%22"},
		{"blocked", "DELETE", "/archive/x", auth(ak1), "3=" + ak1 + " 5=*"},
		{"blocked, the batch form", "POST", "/archive?delete", auth(ak1), "3=" + ak1 + " 5=*"},
		{"not blocked elsewhere", "DELETE", "/data/x", auth(ak1), "3=" + ak1},
	} {
		if got := marks(c.method, c.path, c.headers); got != c.want {
			t.Errorf("%s (%s %s): marked %q, want %q", c.what, c.method, c.path, got, c.want)
		}
	}

	// A client cannot mark itself: what it sends under the script's header
	// names is dropped, whether or not the rule matches.
	h := auth("AKIAOTHER")
	h["x-deltav-rl-1"], h["x-deltav-rl-3"], h["x-deltav-rl-5"] = "someone-else", "someone-else", "*"
	if got := marks("PUT", "/checkpoints/ckpt/x", h); got != "3=AKIAOTHER" {
		t.Errorf("with forged headers: marked %q", got)
	}
	// Nor can it grow a count's name without bound, or put anything in it
	// but plain characters.
	long := strings.Repeat("A", 5000)
	got := run(t, L, "GET", "/public/x", map[string]string{"authorization": "AWS " + long + ":sig"})["x-deltav-rl-3"]
	if len(got) != 200 {
		t.Errorf("a 5000-byte access key is counted under %d bytes", len(got))
	}
	got = run(t, L, "GET", "/public/x", map[string]string{"authorization": "AWS a b,c\td\x01e:sig"})["x-deltav-rl-3"]
	if got != "a%20b%2Cc%09d%01e" {
		t.Errorf("an access key with odd bytes is counted under %q", got)
	}
	// Other headers are left alone.
	out := run(t, L, "GET", "/public/x", map[string]string{"x-amz-date": "20261118T000000Z", "host": "127.0.0.1:9000"})
	if out["x-amz-date"] != "20261118T000000Z" || out["host"] != "127.0.0.1:9000" || out[":path"] != "/public/x" {
		t.Errorf("the script changed a header that is not its own: %v", out)
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
	if s := Script([]Rule{r}); strings.Contains(s, "os.exit") && !strings.Contains(s, `os.exit\040\041`) && strings.Contains(s, `"]] os.exit()`) {
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
