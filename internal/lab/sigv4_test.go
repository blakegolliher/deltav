package lab

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The worked examples from the AWS documentation, "Authenticating Requests:
// Using the Authorization Header (AWS Signature Version 4)". They use the
// documentation's example credentials, which are not real.
var (
	docCreds = Credentials{
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
	}
	docTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
)

const docHost = "examplebucket.s3.amazonaws.com"

func docRequest(t *testing.T, method, pathAndQuery string, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+docHost+pathAndQuery, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestSignMatchesAWSDocumentationExamples(t *testing.T) {
	for _, tc := range []struct {
		name, method, target, payloadHash string
		headers                           map[string]string
		wantSigned, wantSignature         string
	}{
		{
			name: "GET Object", method: "GET", target: "/test.txt", payloadHash: EmptyPayloadHash,
			headers:       map[string]string{"Range": "bytes=0-9"},
			wantSigned:    "host;range;x-amz-content-sha256;x-amz-date",
			wantSignature: "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "PUT Object", method: "PUT", target: "/test$file.text",
			payloadHash: "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			headers: map[string]string{
				"Date":                "Fri, 24 May 2013 00:00:00 GMT",
				"x-amz-storage-class": "REDUCED_REDUNDANCY",
			},
			wantSigned:    "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			wantSignature: "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name: "GET Bucket Lifecycle", method: "GET", target: "/?lifecycle", payloadHash: EmptyPayloadHash,
			wantSigned:    "host;x-amz-content-sha256;x-amz-date",
			wantSignature: "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "List Objects", method: "GET", target: "/?max-keys=2&prefix=J", payloadHash: EmptyPayloadHash,
			wantSigned:    "host;x-amz-content-sha256;x-amz-date",
			wantSignature: "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := docRequest(t, tc.method, tc.target, tc.headers)
			Sign(req, docCreds, tc.payloadHash, docTime)
			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
				"SignedHeaders=" + tc.wantSigned + ", Signature=" + tc.wantSignature
			if got := req.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization\n got %s\nwant %s", got, want)
			}

			// The server side must accept what the client side produced.
			req.Host = docHost
			secret := func(string) (string, bool) { return docCreds.SecretKey, true }
			if err := Verify(req, req.URL.EscapedPath(), secret, docTime, time.Minute); err != nil {
				t.Errorf("Verify rejected a correctly signed request: %v", err)
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	secret := func(k string) (string, bool) { return docCreds.SecretKey, k == docCreds.AccessKey }
	signed := func() *http.Request {
		req := docRequest(t, "GET", "/bucket/some/key?versionId=3", map[string]string{"X-Amz-Meta-Owner": "lab"})
		Sign(req, docCreds, EmptyPayloadHash, docTime)
		req.Host = docHost
		return req
	}
	verify := func(req *http.Request, now time.Time) error {
		return Verify(req, req.URL.EscapedPath(), secret, now, 15*time.Minute)
	}
	if err := verify(signed(), docTime.Add(10*time.Minute)); err != nil {
		t.Fatalf("baseline request rejected: %v", err)
	}

	for name, tc := range map[string]struct {
		mutate func(*http.Request)
		now    time.Time
		want   string
	}{
		"host rewritten":        {func(r *http.Request) { r.Host = "127.0.0.1:9000" }, docTime, "signature does not match"},
		"path changed":          {func(r *http.Request) { r.URL.Path = "/bucket/some/other" }, docTime, "signature does not match"},
		"query changed":         {func(r *http.Request) { r.URL.RawQuery = "versionId=4" }, docTime, "signature does not match"},
		"method changed":        {func(r *http.Request) { r.Method = "DELETE" }, docTime, "signature does not match"},
		"signed header changed": {func(r *http.Request) { r.Header.Set("X-Amz-Meta-Owner", "other") }, docTime, "signature does not match"},
		"signed header removed": {func(r *http.Request) { r.Header.Del("X-Amz-Meta-Owner") }, docTime, "is missing from the request"},
		"payload hash changed":  {func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", UnsignedPayload) }, docTime, "signature does not match"},
		"no authorization":      {func(r *http.Request) { r.Header.Del("Authorization") }, docTime, "no Authorization header"},
		"clock skew":            {func(*http.Request) {}, docTime.Add(16 * time.Minute), "too far from server time"},
		"unknown key": {func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), docCreds.AccessKey, "AKIAOTHER", 1))
		}, docTime, "unknown access key"},
		"wrong secret": {func(r *http.Request) {
			other := docRequest(t, "GET", "/bucket/some/key?versionId=3", map[string]string{"X-Amz-Meta-Owner": "lab"})
			Sign(other, Credentials{AccessKey: docCreds.AccessKey, SecretKey: "not-the-secret"}, EmptyPayloadHash, docTime)
			r.Header.Set("Authorization", other.Header.Get("Authorization"))
		}, docTime, "signature does not match"},
	} {
		req := signed()
		tc.mutate(req)
		if err := verify(req, tc.now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

// An added, unsigned header (a proxy's x-forwarded-proto, say) is allowed.
func TestVerifyIgnoresUnsignedHeaders(t *testing.T) {
	req := docRequest(t, "GET", "/bucket/key", nil)
	Sign(req, docCreds, EmptyPayloadHash, docTime)
	req.Host = docHost
	req.Header.Set("X-Forwarded-Proto", "http")
	secret := func(string) (string, bool) { return docCreds.SecretKey, true }
	if err := Verify(req, req.URL.EscapedPath(), secret, docTime, 0); err != nil {
		t.Fatalf("unsigned extra header broke verification: %v", err)
	}
}

func TestCanonicalURI(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "/",
		"/":                       "/",
		"/bucket/key":             "/bucket/key",
		"/bucket/a//b":            "/bucket/a//b",       // not merged
		"/bucket/a/./b/../c":      "/bucket/a/./b/../c", // not normalized
		"/bucket/a%2Fb":           "/bucket/a/b",        // an encoded slash is a slash in the key
		"/bucket/a%2fb":           "/bucket/a/b",
		"/bucket/space%20here":    "/bucket/space%20here",
		"/bucket/plus+literal":    "/bucket/plus%2Bliteral",
		"/bucket/plus%2Bencoded":  "/bucket/plus%2Bencoded",
		"/bucket/%E2%9C%93":       "/bucket/%E2%9C%93",
		"/bucket/pct%25":          "/bucket/pct%25",
		"/bucket/test$file.text":  "/bucket/test%24file.text",
		"/bucket/semi;colon,comm": "/bucket/semi%3Bcolon%2Ccomm",
		"/bucket/bad%zzescape":    "/bucket/bad%zzescape", // undecodable: left alone
	} {
		if got := canonicalURI(in); got != want {
			t.Errorf("canonicalURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "",
		"lifecycle":           "lifecycle=",
		"prefix=J&max-keys=2": "max-keys=2&prefix=J",
		"a=1&a=0&b":           "a=0&a=1&b=",
		"k=a+b&j=a%20b":       "j=a%20b&k=a%2Bb",
		"prefix=dir%2Fsub/&x": "prefix=dir%2Fsub%2F&x=",
	} {
		if got := canonicalQuery(in); got != want {
			t.Errorf("canonicalQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
