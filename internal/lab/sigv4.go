// Package lab holds the test harness tools behind cmd/dvlab: a fake S3
// backend and a load generator, with their own AWS Signature Version 4
// implementation so the harness needs no SDK.
//
// The signer and verifier follow the AWS documentation, "Authenticating
// Requests: Using the Authorization Header (AWS Signature Version 4)", and
// are checked against the worked examples published there.
package lab

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sigAlgorithm = "AWS4-HMAC-SHA256"
	amzDateFmt   = "20060102T150405Z"

	// UnsignedPayload is the x-amz-content-sha256 value for a body that is
	// not covered by the signature.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptyPayloadHash is the SHA-256 of an empty body.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Credentials sign requests for one region and service.
type Credentials struct {
	AccessKey string
	SecretKey string
	Region    string // default us-east-1
	Service   string // default s3
}

func (c Credentials) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c Credentials) service() string {
	if c.Service == "" {
		return "s3"
	}
	return c.Service
}

// Sign adds X-Amz-Date, X-Amz-Content-Sha256 and Authorization to req.
// Every header already set on req is signed, along with Host, so a proxy
// that alters any of them breaks the signature. payloadHash is the hex
// SHA-256 of the body, or UnsignedPayload.
func Sign(req *http.Request, c Credentials, payloadHash string, now time.Time) {
	now = now.UTC()
	req.Header.Set("X-Amz-Date", now.Format(amzDateFmt))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	signed := []string{"host"}
	for name := range req.Header {
		if lower := strings.ToLower(name); lower != "authorization" {
			signed = append(signed, lower)
		}
	}
	sort.Strings(signed)

	scope := now.Format("20060102") + "/" + c.region() + "/" + c.service() + "/aws4_request"
	canonical := canonicalRequest(req.Method, req.URL.EscapedPath(), req.URL.RawQuery, host, req.Header, signed, payloadHash)
	sig := signature(c.SecretKey, scope, now.Format(amzDateFmt), canonical)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, c.AccessKey, scope, strings.Join(signed, ";"), sig))
}

// Verify checks the SigV4 Authorization header of a request a server
// received. rawPath is the request target's path exactly as it arrived.
// secretFor returns the secret for an access key. A maxSkew above zero
// rejects requests whose X-Amz-Date is further than that from now.
//
// It verifies header signatures only: presigned URLs and the per-chunk
// signatures of streaming uploads are not checked.
func Verify(r *http.Request, rawPath string, secretFor func(accessKey string) (string, bool), now time.Time, maxSkew time.Duration) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return errors.New("no Authorization header")
	}
	alg, rest, _ := strings.Cut(auth, " ")
	if alg != sigAlgorithm {
		return fmt.Errorf("unsupported algorithm %q", alg)
	}
	params := map[string]string{}
	for _, kv := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok {
			params[k] = v
		}
	}
	cred := strings.Split(params["Credential"], "/")
	if len(cred) != 5 || cred[4] != "aws4_request" || params["SignedHeaders"] == "" || params["Signature"] == "" {
		return errors.New("malformed Authorization header")
	}
	accessKey, scope := cred[0], strings.Join(cred[1:], "/")
	secret, ok := secretFor(accessKey)
	if !ok {
		return fmt.Errorf("unknown access key %q", accessKey)
	}

	amzDate := r.Header.Get("X-Amz-Date")
	when, err := time.Parse(amzDateFmt, amzDate)
	if err != nil {
		return fmt.Errorf("missing or malformed X-Amz-Date %q", amzDate)
	}
	if when.Format("20060102") != cred[1] {
		return errors.New("credential scope date does not match X-Amz-Date")
	}
	if maxSkew > 0 {
		if d := now.Sub(when); d > maxSkew || d < -maxSkew {
			return fmt.Errorf("request time %s is too far from server time", amzDate)
		}
	}

	signed := strings.Split(params["SignedHeaders"], ";")
	hasHost := false
	for _, h := range signed {
		if h == "host" {
			hasHost = true
		} else if _, present := r.Header[http.CanonicalHeaderKey(h)]; !present {
			return fmt.Errorf("signed header %q is missing from the request", h)
		}
	}
	if !hasHost {
		return errors.New("the host header is not signed")
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		return errors.New("no X-Amz-Content-Sha256 header")
	}

	canonical := canonicalRequest(r.Method, rawPath, r.URL.RawQuery, r.Host, r.Header, signed, payloadHash)
	want := signature(secret, scope, amzDate, canonical)
	if subtle.ConstantTimeCompare([]byte(want), []byte(params["Signature"])) != 1 {
		return errors.New("signature does not match")
	}
	return nil
}

// canonicalRequest builds the SigV4 canonical request. signed must be
// lower-case and sorted.
func canonicalRequest(method, rawPath, rawQuery, host string, h http.Header, signed []string, payloadHash string) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(rawPath))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(rawQuery))
	b.WriteByte('\n')
	for _, name := range signed {
		b.WriteString(name)
		b.WriteByte(':')
		if name == "host" {
			b.WriteString(host)
		} else {
			vals := h[http.CanonicalHeaderKey(name)]
			for i, v := range vals {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(strings.Join(strings.Fields(v), " "))
			}
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signed, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

func signature(secret, scope, amzDate, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	toSign := sigAlgorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := []byte("AWS4" + secret)
	for _, part := range strings.Split(scope, "/") {
		key = hmacSHA256(key, part)
	}
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalURI is the S3 form of the canonical URI: the path decoded and
// then URI-encoded once, keeping "/" and without normalizing "//" or dot
// segments. A path that cannot be decoded is used as it is.
func canonicalURI(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return rawPath
	}
	return uriEncode(decoded, true)
}

// canonicalQuery sorts the query parameters and URI-encodes each name and
// value. "+" is a literal plus, as in a path, not a space.
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	pairs := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		if d, err := url.PathUnescape(k); err == nil {
			k = d
		}
		if d, err := url.PathUnescape(v); err == nil {
			v = d
		}
		pairs = append(pairs, uriEncode(k, false)+"="+uriEncode(v, false))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes every byte except the unreserved characters
// A-Z a-z 0-9 - . _ ~ and, if keepSlash, "/". Hex digits are upper-case.
func uriEncode(s string, keepSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}
