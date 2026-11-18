package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestFormats(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{count(0), "0"}, {count(999), "999"}, {count(1000), "1,000"}, {count(uint64(1234567)), "1,234,567"},
		{count(uint32(4294967295)), "4,294,967,295"}, {count(int64(-12345)), "-12,345"},
		{rate(0), "0"}, {rate(0.01), "<0.1"}, {rate(0.44), "0.4"}, {rate(12.4), "12"}, {rate(999.4), "999"},
		{rate(1284), "1.3K"}, {rate(3.4e6), "3.4M"},
		{mbps(0), "0"}, {mbps(20e3), "<0.1"}, {mbps(12.34e6), "12.3"}, {mbps(1234.5e6), "1,235"},
		{millis(0), "–"}, {millis(0.64), "0.6 ms"}, {millis(291.5), "292 ms"}, {millis(1549), "1.5 s"},
		{share(0, 0), "–"}, {share(0, 10), "0"}, {share(1, 10000), "<0.1%"}, {share(4, 1000), "0.4%"}, {share(87, 610), "14%"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		0: "just now", 12 * time.Second: "12s ago", 3 * time.Minute: "3m ago", 5 * time.Hour: "5h ago", 72 * time.Hour: "3d ago",
	} {
		if got := ago(now, now.Add(-d)); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
	if got := ago(now, time.Time{}); got != "never" {
		t.Errorf("ago(zero) = %q", got)
	}
}

func TestSparkline(t *testing.T) {
	// No traffic is a flat baseline with no mark, not a line at the floor
	// with a dot on it.
	flat := string(sparkline(make([]float64, 29), "s3"))
	if !strings.Contains(flat, "spark-flat") || strings.Contains(flat, "spark-now") || strings.Contains(flat, "polyline") {
		t.Errorf("flat history: %s", flat)
	}
	if !strings.Contains(flat, "over the last 5 minutes: low 0, peak 0, latest 0") {
		t.Errorf("flat history's label: %s", flat)
	}

	hist := make([]float64, 29)
	for i := range hist {
		hist[i] = float64(i * 10)
	}
	hist[10] = 400 // the peak
	got := string(sparkline(hist, "s3"))
	for _, want := range []string{
		`role="img"`, `aria-label="s3, requests per second over the last 5 minutes: low 0, peak 400, latest 280"`,
		`<polyline class="spark-line" points="4,24 `, `<circle class="spark-now" cx="116" cy="10" r="3"/>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sparkline is missing %s:\n%s", want, got)
		}
	}
	// The peak touches the top padding and nothing leaves the box.
	for _, m := range regexp.MustCompile(`(-?[\d.]+),(-?[\d.]+)`).FindAllStringSubmatch(got, -1) {
		if strings.HasPrefix(m[1], "-") || strings.HasPrefix(m[2], "-") {
			t.Errorf("a point outside the drawing: %s", m[0])
		}
	}
	if !strings.Contains(got, " 44,4 ") {
		t.Errorf("the peak (point 10 of 29) should be at x=44, y=4:\n%s", got)
	}

	// The name is escaped: it can come from a VAST cluster.
	if hostile := string(sparkline(hist, `x"><script>alert(1)</script>`)); strings.Contains(hostile, "<script>") || strings.Contains(hostile, `x">`) {
		t.Errorf("an unescaped name reached the markup: %s", hostile)
	}
}

func TestMeter(t *testing.T) {
	for _, c := range []struct {
		used  uint64
		limit uint32
		class string
		width string
	}{
		{0, 400, "meter-ok", `width="0"`},
		{1, 400, "meter-ok", `width="6"`}, // a sliver is drawn wide enough to see
		{200, 400, "meter-ok", `width="48"`},
		{320, 400, "meter-near", `width="76.8"`},
		{400, 400, "meter-near", `width="96"`},
		{401, 400, "meter-over", `width="96"`},
		{9000, 400, "meter-over", `width="96"`},
	} {
		got := string(meter(c.used, c.limit))
		if !strings.Contains(got, c.class) || !strings.Contains(got, `class="meter-fill" `+c.width) {
			t.Errorf("meter(%d, %d): want %s and fill %s, got %s", c.used, c.limit, c.class, c.width, got)
		}
	}
	if got := string(meter(1234, 5000)); !strings.Contains(got, `aria-label="1,234 of 5,000"`) {
		t.Errorf("meter label: %s", got)
	}
}

// A name supplied by an Envoy or a VAST cluster must not be able to forge a
// server-sent event by carrying a line break of any kind.
func TestWriteEventFramesEveryLine(t *testing.T) {
	var b bytes.Buffer
	markup := "<p>one</p>\n   <p>two\revent: forged\r\rdata: <script>x</script></p>\r\n\n<p>three</p>"
	if err := writeEvent(&b, []byte(markup)); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.HasSuffix(got, "\n\n") || strings.Count(got, "\n\n") != 1 {
		t.Errorf("want exactly one event, ended by one blank line:\n%q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("a carriage return survived:\n%q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n\n"), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			t.Errorf("a line that is not data: %q", line)
		}
	}
	want := "data: <p>one</p>\ndata: <p>two\ndata: event: forged\ndata: data: <script>x</script></p>\ndata: <p>three</p>\n\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/": "/", "/fleet?state=nacked": "/fleet?state=nacked",
		"//evil.example/x": "/", "https://evil.example/": "/", "/\\evil.example": "/", "fleet": "/", "/a\r\nSet-Cookie: x": "/",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The vendored htmx files are upstream's, byte for byte: htmx.org 4.0.1 from
// the npm registry (dist/htmx.min.js and dist/ext/hx-sse.min.js). If one is
// replaced by a newer release, update the digest here and the version in
// scripts/third_party_static.md together.
func TestVendoredScriptsAreUnmodified(t *testing.T) {
	for name, want := range map[string]string{
		"static/htmx.min.js":   "a0abb04f2f45a1f89d1d4844023245537e5e6014c87b144553e0607b4cc32130",
		"static/hx-sse.min.js": "18ef2761356ec1fe0bb6d92612315b37517a7c1e9856b661c5ae81fef239d103",
	} {
		b, err := staticFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: SHA-256 %s, want %s", name, got, want)
		}
	}
	// What is shipped comes with its license.
	for _, f := range []string{"htmx/LICENSE", "tailwindcss/LICENSE"} {
		if b, err := os.ReadFile("../../third_party/static/" + f); err != nil || len(b) < 200 {
			t.Errorf("third_party/static/%s is missing or empty: %v", f, err)
		}
	}
}

// The committed stylesheet is generated. This catches a template or style
// change that was committed without running `make css`: every class a
// template or the Go code uses must have a rule, and every rule written in
// tailwind.css must have made it into app.css.
func TestStylesheetIsCurrent(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(css, []byte("/*! tailwindcss v")) || !bytes.Contains(css[:100], []byte("MIT License")) {
		t.Error("app.css must keep Tailwind's license banner as its first line")
	}
	has := func(class string) bool {
		return regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `([^a-zA-Z0-9_-]|$)`).Match(css)
	}

	used := map[string]bool{}
	classAttr := regexp.MustCompile(`class="([^"]*)"`)
	action := regexp.MustCompile(`\{\{[^}]*\}\}`)
	collect := func(src string) {
		for _, m := range classAttr.FindAllStringSubmatch(src, -1) {
			for _, c := range strings.Fields(action.ReplaceAllString(m[1], " ")) {
				used[c] = true
			}
		}
	}
	err = fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := templateFS.ReadFile(p)
		collect(string(b))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"views.go", "format.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		collect(string(b))
	}
	// Classes built from a value at run time, checked by their variants.
	for _, c := range []string{"ico-%s", "meter-%s", "note-ok{{else}}note-bad", "num{{if", "num"} {
		delete(used, c)
	}
	for _, c := range []string{"num", "flagged", "note-ok", "note-bad", "opened", "detail"} {
		used[c] = true
	}
	for kind := range icons {
		used["ico-"+kind] = true
	}
	for _, level := range []string{"ok", "near", "over"} {
		if level != "ok" { // "ok" is the unstyled default
			used["meter-"+level] = true
		}
	}
	var missing []string
	for c := range used {
		if !has(c) {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("classes with no rule in static/app.css (run `make css`, or the class is a typo): %s", strings.Join(missing, ", "))
	}

	src, err := os.ReadFile("tailwind.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*\.([a-z][a-z0-9-]*)[ ,{:.\[>]`).FindAllStringSubmatch(string(src), -1) {
		if !has(m[1]) {
			t.Errorf("tailwind.css defines .%s but static/app.css has no such rule: run `make css`", m[1])
		}
	}
}
