package lab

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestReviewM3_SecretFieldRedactedWhateverItsType: a field whose name says
// it holds a secret must be redacted whether its value is a string, an
// object with innocuous child names, or a number, both in the output and in
// the field report (which must show the redact rule for that field).
func TestReviewM3_SecretFieldRedactedWhateverItsType(t *testing.T) {
	const in = `[{
		"id": 1,
		"name": "pool-a",
		"password": "hunter2-string",
		"token": {"id": "tok-id-visible", "value": "s3cr3t-nested-value"},
		"secret_key": 918273645546372819,
		"credentials": [{"kind": "s3", "data": "nested-in-array-secret"}]
	}]`
	dec := json.NewDecoder(strings.NewReader(in))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	clean, report, err := NewSanitizer("vast1.lab.example.net").Sanitize(map[string]any{"vippools": doc})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(clean["vippools"])
	for _, secret := range []string{"hunter2-string", "s3cr3t-nested-value", "918273645546372819", "nested-in-array-secret"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("secret value %q survived into the fixture", secret)
		}
	}

	// The field report: every secret-named field (or its children) should
	// show the redact rule.
	redacted := map[string]bool{}
	for _, r := range report {
		if r.Rules["redact"] > 0 {
			redacted[r.Path] = true
		}
		t.Logf("report %-28s types=%v rules=%v", r.Path, r.Types, r.Rules)
	}
	for _, field := range []string{".password", ".token", ".secret_key", ".credentials"} {
		hit := false
		for p := range redacted {
			if strings.HasPrefix(p, "[]"+field) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("field report shows no redaction under %q", "[]"+field)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out.String())
	}
}
