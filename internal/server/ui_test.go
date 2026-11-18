package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"golang.org/x/net/html"

	"deltav/internal/metrics"
	"deltav/internal/vast/vasttest"
)

// The web UI, through the daemon's HTTP listener, against a mock VAST.

// uiDaemon starts a daemon with the mock VAST's pool "main" as target s3,
// three Envoys in the fleet and some traffic on record.
func uiDaemon(t *testing.T) (*Server, *vasttest.Server) {
	t.Helper()
	m := mockVAST(t)
	s, _ := run(t, t.TempDir())
	addCluster(t, s, m)
	tj := addTarget(t, s, "s3", "main", 9000)
	connect(s, 0, 3)
	for i, e := range tj.Endpoints {
		var batch []metrics.Sample
		for n := 0; n < 50*(i+1); n++ {
			status := 200
			if n%25 == 24 {
				status = 503
			}
			batch = append(batch, metrics.Sample{
				Target: "s3", Endpoint: fmt.Sprintf("%s:%d", e.IP, e.Port), Status: status,
				Duration: 8 * time.Millisecond, BytesIn: 1 << 20, BytesOut: 4 << 20,
			})
		}
		s.agg.Record("envoy-000", batch)
	}
	return s, m
}

// page fetches a page as a browser would.
func page(t *testing.T, s *Server, path string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+s.HTTPAddr()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// post submits a form as the page's own script would.
func post(t *testing.T, s *Server, path string, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.HTTPAddr()+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+s.HTTPAddr())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

// wellFormed checks what a lenient browser would hide: tags that do not
// nest, and IDs used twice (the live update matches elements by ID).
func wellFormed(doc string) error {
	z := html.NewTokenizer(strings.NewReader(doc))
	var stack []string
	ids := map[string]bool{}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				return z.Err()
			}
			if len(stack) > 0 {
				return fmt.Errorf("never closed: %v", stack)
			}
			return nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if string(k) == "id" {
					if ids[string(v)] {
						return fmt.Errorf("id %q is used twice", v)
					}
					ids[string(v)] = true
				}
			}
			if tt == html.StartTagToken && !voidElements[tag] {
				stack = append(stack, tag)
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if len(stack) == 0 || stack[len(stack)-1] != string(name) {
				return fmt.Errorf("</%s> where the open elements are %v", name, stack)
			}
			stack = stack[:len(stack)-1]
		}
	}
}

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s: missing %q", what, w)
		}
	}
}

func TestUIPages(t *testing.T) {
	s, m := uiDaemon(t)
	cnodes := m.PoolCNodes("main")
	tj := target(t, s, "s3")

	pages := map[string][]string{
		"/": {
			`<title>Overview · Delta-V</title>`, `hx-sse:connect="/live/overview"`, `hx-swap="innerMorph"`,
			`<p class="tile-value">3</p>`, "of 3 seen in the last hour", `<span class="name">s3</span>`, "127.0.0.1:9000",
			"pool main on lab", "6 of 6 VIPs", "No target has a limit", `class="spark"`, `aria-current="page">Overview<`,
		},
		"/?window=5m":    {`hx-sse:connect="/live/overview?window=5m"`, `href="/cnodes?window=5m"`, `aria-current="true">5m<`},
		"/?window=bogus": {`hx-sse:connect="/live/overview"`, `aria-current="true">1m<`},
		"/cnodes": {
			`<title>CNodes · Delta-V</title>`, `hx-sse:connect="/live/cnodes"`, cnodes[0], cnodes[2], cnodes[4], "Active",
			`aria-expanded="false"`, `href="/cnodes?open=lab%2F`,
		},
		"/fleet": {
			`<title>Fleet · Delta-V</title>`, "Envoys 1 to 3 of 3", "envoy-000", "envoy-002", "Pending", `aria-sort="ascending"`,
			"3 connected of 3 seen in the last hour", `name="q"`,
		},
		"/controls": {
			`<title>Controls · Delta-V</title>`, "Connection limit per CNode", "No limit", `action="/controls/limit"`, `hx-post="/controls/limit"`,
			"Drain a CNode", `hx-confirm="Drain ` + cnodes[0], `<code>` + m.Host() + `</code>`, "Certificate not verified",
			"The cap is each Envoy's own", "by more than 20%", "no sooner than a minute", "Each worker thread of an Envoy can hold one connection more than the cap", "DELTAV_ENVOY_WORKERS",
			"Attach a cluster", "Add a target", `<option value="main">`, "every 60 minutes",
		},
	}
	for path, wants := range pages {
		code, hdr, body := page(t, s, path)
		if code != http.StatusOK {
			t.Errorf("%s: status %d", path, code)
			continue
		}
		mustContain(t, path, body, wants...)
		if err := wellFormed(body); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		if strings.Contains(body, vasttest.Password) {
			t.Errorf("%s shows the VAST password", path)
		}
		if hdr.Get("Cache-Control") != "no-store" || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'self'") ||
			hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: response headers %v", path, hdr)
		}
		if strings.Contains(body, "style=") || regexp.MustCompile(`<script[^>]*>[^<]`).MatchString(body) || regexp.MustCompile(`\son[a-z]+="`).MatchString(body) {
			t.Errorf("%s has inline style or script, which its own content security policy forbids", path)
		}
	}

	// Opening a CNode's row shows its VIPs; the row's link now closes it.
	open := "lab/" + cnodes[0]
	_, _, body := page(t, s, "/cnodes?open="+url.QueryEscape(open))
	mustContain(t, "open CNode", body, `aria-expanded="true"`, `class="detail"`, "VIPs on "+cnodes[0],
		`hx-sse:connect="/live/cnodes?open=`+url.QueryEscape(open)+`"`)
	vips := 0
	for _, e := range tj.Endpoints {
		row := fmt.Sprintf("<code>%s:%d</code>", e.IP, e.Port)
		if (e.CNode == cnodes[0]) != strings.Contains(body, row) {
			t.Errorf("VIP %s (on %s): shown = %v", e.IP, e.CNode, strings.Contains(body, row))
		}
		if e.CNode == cnodes[0] {
			vips++
		}
	}
	if vips != 2 {
		t.Fatalf("the fixture should put two VIPs on %s", cnodes[0])
	}
	if !regexp.MustCompile(`aria-expanded="true"[^>]*`).MatchString(body) || !strings.Contains(body, `href="/cnodes" hx-get="/cnodes"`) {
		t.Error("the open row's link should lead back to the page with no row open")
	}
	if err := wellFormed(body); err != nil {
		t.Errorf("open CNode: %v", err)
	}

	if code, _, _ := page(t, s, "/nope"); code != http.StatusNotFound {
		t.Errorf("/nope: status %d", code)
	}
	for _, f := range []string{"app.css", "app.js", "htmx.min.js", "hx-sse.min.js"} {
		code, hdr, body := page(t, s, "/static/"+f)
		if code != http.StatusOK || len(body) < 1000 || hdr.Get("Content-Type") == "" || hdr.Get("Cache-Control") != "no-cache" {
			t.Errorf("/static/%s: %d, %d bytes, %v", f, code, len(body), hdr)
		}
	}
	// With the version the pages link to, a static file may be kept for good.
	_, _, home := page(t, s, "/")
	v := regexp.MustCompile(`/static/app\.css\?v=([0-9a-f]+)`).FindStringSubmatch(home)
	if v == nil {
		t.Fatal("the page does not link a versioned stylesheet")
	}
	if _, hdr, _ := page(t, s, "/static/app.css?v="+v[1]); !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Errorf("versioned asset: Cache-Control %q", hdr.Get("Cache-Control"))
	}
	if code, _, _ := page(t, s, "/static/..%2Fwebui.go"); code != http.StatusNotFound {
		t.Errorf("a path outside static/: status %d", code)
	}
}

func TestUILimitAndDrainShow(t *testing.T) {
	s, m := uiDaemon(t)
	cnodes := m.PoolCNodes("main")
	hot := cnodes[0]

	// Three Envoys, two VIPs per CNode, limit 5: the share is 1 and even
	// that cannot hold the fleet to the limit.
	setLimit(t, s, "s3", 5, "enforce")
	inFlight(s, target(t, s, "s3"), map[string]uint64{hot: 4})
	_, _, home := page(t, s, "/")
	mustContain(t, "overview over limit", home, `<p class="tile-value">1</p>`, "8 of 5, s3", "5, enforced", hot+" at 8", "Over")
	_, _, cn := page(t, s, "/cnodes")
	mustContain(t, "cnodes over limit", cn, `class="meter meter-over"`, "8 of 5, s3", `aria-label="8 of 5"`)
	if strings.Count(cn, `class="meter meter-over"`) != 1 || strings.Count(cn, `class="meter meter-ok"`) != 2 {
		t.Error("one CNode should be over its limit and two within it")
	}
	_, _, ctl := page(t, s, "/controls")
	mustContain(t, "controls with a limit", ctl, "5 per CNode,", "enforced", "Each Envoy's cap is 1 per VIP: 5 ÷ 3 Envoys ÷ 2 VIPs on one CNode.",
		"Cannot hold", "3 Envoys × 2 VIPs is more than the limit", hot+" has 8 in flight", `value="5"`, `<option value="enforce" selected>`, "Remove limit",
		"Workers unknown", "3 connected Envoys do not say how many worker threads they run")
	for what, doc := range map[string]string{"overview": home, "cnodes": cn, "controls": ctl} {
		if err := wellFormed(doc); err != nil {
			t.Errorf("%s with a limit: %v", what, err)
		}
	}

	// A drained CNode is shown as drained, not as unhealthy, and offers undrain.
	if code, body := call(t, s, "POST", "/api/v1/vast/lab/cnodes/"+cnodes[2]+"/drain", nil); code != http.StatusNoContent {
		t.Fatalf("drain: %d %s", code, body)
	}
	_, _, cn = page(t, s, "/cnodes")
	mustContain(t, "cnodes with a drain", cn, "Drained")
	_, _, home = page(t, s, "/")
	mustContain(t, "overview with a drain", home, "4 of 6 VIPs", "2 withheld")
	_, _, ctl = page(t, s, "/controls")
	if !regexp.MustCompile(`name="cnode" value="` + cnodes[2] + `">\s*<input type="hidden" name="drained" value="false">\s*<button class="btn">Undrain</button>`).MatchString(ctl) {
		t.Error("the drained CNode should offer Undrain")
	}
	if strings.Count(ctl, ">Drain</button>") != 2 || strings.Count(ctl, ">Undrain</button>") != 1 {
		t.Error("want two Drain buttons and one Undrain")
	}
}

// The fleet page asks the daemon for one page of rows, so it costs the same
// with five thousand Envoys as with five.
func TestUIFleetAtScale(t *testing.T) {
	s, _ := run(t, "")
	connect(s, 0, 5000)
	rows := regexp.MustCompile(`<tr id="envoy-`)

	started := time.Now()
	code, _, body := page(t, s, "/fleet")
	t.Logf("fleet page with 5,000 Envoys: %d bytes in %v", len(body), time.Since(started).Round(time.Millisecond))
	if code != http.StatusOK || len(rows.FindAllString(body, -1)) != 100 || len(body) > 300<<10 {
		t.Fatalf("first page: status %d, %d rows, %d bytes", code, len(rows.FindAllString(body, -1)), len(body))
	}
	mustContain(t, "first page", body, "Envoys 1 to 100 of 5,000", "5,000 connected of 5,000", "envoy-000", "envoy-099",
		`<span class="btn" aria-disabled="true">Previous</span>`, `href="/fleet?offset=100"`, `rel="next"`)
	if strings.Contains(body, "envoy-100<") {
		t.Error("the first page holds a row of the second")
	}
	if err := wellFormed(body); err != nil {
		t.Error(err)
	}

	_, _, body = page(t, s, "/fleet?offset=4900")
	// Node IDs sort as text, so the last of them is envoy-999.
	mustContain(t, "last page", body, "Envoys 4,901 to 5,000 of 5,000", "envoy-999<", `href="/fleet?offset=4800"`,
		`<span class="btn" aria-disabled="true">Next</span>`, `hx-sse:connect="/live/fleet?offset=4900"`)

	_, _, body = page(t, s, "/fleet?limit=25&offset=50")
	if n := len(rows.FindAllString(body, -1)); n != 25 {
		t.Errorf("limit=25: %d rows", n)
	}
	mustContain(t, "page of 25", body, "Envoys 51 to 75 of 5,000", `href="/fleet?limit=25&amp;offset=75"`, `href="/fleet?limit=25&amp;offset=25"`)

	// Filter and sort are the daemon's; the page's links keep them, and a
	// change of either starts again at the first page.
	_, _, body = page(t, s, "/fleet?q=envoy-4242&offset=0")
	mustContain(t, "filtered", body, "Envoys 1 to 1 of 1 that match", "envoy-4242", `value="envoy-4242"`, "Clear filters")
	_, _, body = page(t, s, "/fleet?sort=last_seen&order=desc&state=pending&offset=200")
	mustContain(t, "sorted", body, `aria-sort="descending"`, "Envoys 201 to 300 of 5,000 that match",
		`href="/fleet?sort=last_seen&amp;state=pending"`,                   // the same column again: the other way, from the top
		`href="/fleet?order=desc&amp;sort=request_rate&amp;state=pending"`, // busiest first
		`href="/fleet?offset=300&amp;order=desc&amp;sort=last_seen&amp;state=pending"`,
		`<input type="hidden" name="sort" value="last_seen">`, `<input type="hidden" name="state" value="pending">`)
	_, _, body = page(t, s, "/fleet?state=nacked")
	mustContain(t, "no match", body, "No Envoy matches", "Clear the filters")
	_, _, body = page(t, s, "/fleet?offset=99999")
	mustContain(t, "past the end", body, "past the end of the list", `href="/fleet"`)
	// What the page does not know is dropped, not passed on to the API or
	// into the page's links.
	code, _, body = page(t, s, "/fleet?sort=evil&order=sideways&limit=7&state=x&connected=maybe&junk=1")
	if code != http.StatusOK || strings.Contains(body, "evil") || strings.Contains(body, "junk") || !strings.Contains(body, "Envoys 1 to 100 of 5,000") {
		t.Errorf("unknown parameters: status %d", code)
	}
}

func TestUIControlsForms(t *testing.T) {
	s, m := uiDaemon(t)
	cnodes := m.PoolCNodes("main")
	submit := func(what, path string, form url.Values, wantCode int, wants ...string) string {
		t.Helper()
		code, body := post(t, s, path, form)
		if code != wantCode {
			t.Errorf("%s: status %d, want %d", what, code, wantCode)
		}
		mustContain(t, what, body, wants...)
		if err := wellFormed(body); err != nil {
			t.Errorf("%s: %v", what, err)
		}
		if strings.Contains(body, vasttest.Password) {
			t.Errorf("%s: the answer shows the VAST password", what)
		}
		return body
	}
	ok, bad := http.StatusOK, http.StatusUnprocessableEntity

	// Limits.
	submit("set limit", "/controls/limit", url.Values{"target": {"s3"}, "per_cnode": {"400"}, "mode": {"enforce"}}, ok,
		`class="note note-ok" role="status"`, "s3: limit set to 400 connections per CNode and enforced.", "400 per CNode,", `<title>Controls · Delta-V</title>`)
	if l := target(t, s, "s3").Status.Limit; l == nil || l.PerCNode != 400 || l.Mode != "enforce" {
		t.Errorf("limit after the form: %+v", l)
	}
	submit("alert mode", "/controls/limit", url.Values{"target": {"s3"}, "per_cnode": {"300"}, "mode": {"alert"}}, ok, "nothing is enforced")
	for what, form := range map[string]url.Values{
		"not a number": {"target": {"s3"}, "per_cnode": {"lots"}, "mode": {"alert"}},
		"zero":         {"target": {"s3"}, "per_cnode": {"0"}, "mode": {"alert"}},
		"bad mode":     {"target": {"s3"}, "per_cnode": {"10"}, "mode": {"strict"}},
		"no target":    {"target": {"nope"}, "per_cnode": {"10"}, "mode": {"alert"}},
	} {
		submit(what, "/controls/limit", form, bad, `class="note note-bad" role="alert"`)
	}
	if l := target(t, s, "s3").Status.Limit; l == nil || l.PerCNode != 300 || l.Mode != "alert" {
		t.Errorf("refused forms changed the limit: %+v", l)
	}
	submit("clear limit", "/controls/limit/clear", url.Values{"target": {"s3"}}, ok, "s3: limit removed.", "No limit")
	if l := target(t, s, "s3").Status.Limit; l != nil {
		t.Errorf("limit after clearing: %+v", l)
	}

	// Drain.
	submit("drain", "/controls/drain", url.Values{"cluster": {"lab"}, "cnode": {cnodes[0]}, "drained": {"true"}}, ok,
		cnodes[0]+" is drained", "Nothing was changed on the VAST cluster", ">Undrain</button>")
	if n := len(target(t, s, "s3").Endpoints); n != 4 {
		t.Errorf("endpoints after draining through the form: %d, want 4", n)
	}
	submit("drain a CNode that does not exist", "/controls/drain", url.Values{"cluster": {"lab"}, "cnode": {"cnode-9-99"}, "drained": {"true"}}, bad, "cnode-9-99")
	submit("undrain", "/controls/drain", url.Values{"cluster": {"lab"}, "cnode": {cnodes[0]}, "drained": {"false"}}, ok, "served to Envoys again")
	if n := len(target(t, s, "s3").Endpoints); n != 6 {
		t.Errorf("endpoints after undraining: %d, want 6", n)
	}

	// Clusters. A refused form comes back open with what was typed, except
	// the password.
	body := submit("attach with a wrong password", "/controls/clusters", url.Values{
		"name": {"second"}, "host": {m.Host()}, "user": {vasttest.User}, "password": {"wrong-pass-7731"}, "insecure": {"true"},
	}, bad, `role="alert"`, "cannot read cluster", `name="name" required pattern="[A-Za-z0-9][A-Za-z0-9._-]*" value="second"`, `value="`+m.Host()+`"`,
		`name="insecure" value="true" checked`)
	if strings.Contains(body, "wrong-pass-7731") {
		t.Error("a refused form gave the password back")
	}
	if !regexp.MustCompile(`<details class="more" hx-morph-skip open>\s*<summary>Attach a cluster`).MatchString(body) {
		t.Error("the refused form should be shown open")
	}
	submit("attach", "/controls/clusters", url.Values{
		"name": {"second"}, "host": {m.Host()}, "user": {vasttest.User}, "password": {vasttest.Password}, "insecure": {"true"},
	}, ok, "Cluster second attached", `<span class="name">second</span>`)
	if n := len(getJSON[struct{ Clusters []clusterJSON }](t, s, "/api/v1/vast").Clusters); n != 2 {
		t.Errorf("clusters after the form: %d", n)
	}

	// Targets.
	submit("add a target on a taken port", "/controls/targets", url.Values{
		"name": {"other"}, "cluster": {"second"}, "pool": {"main"}, "port": {"80"}, "listen_port": {"9000"},
	}, bad, `role="alert"`, `value="other"`, `<option value="second" selected>`)
	submit("add a target with a port that is not one", "/controls/targets", url.Values{
		"name": {"other"}, "cluster": {"second"}, "pool": {"main"}, "port": {"80"}, "listen_port": {"99999"},
	}, bad, "Loopback port on the clients: enter a whole number from 1 to 65535")
	submit("add a target", "/controls/targets", url.Values{
		"name": {"other"}, "cluster": {"second"}, "pool": {"main"}, "port": {"80"}, "listen_port": {"9001"},
	}, ok, "Target other added. Every Envoy now listens on 127.0.0.1:9001 for it.", "http://127.0.0.1:9001", "Used by 1 target")
	if tj := target(t, s, "other"); len(tj.Endpoints) != 6 || tj.Cluster != "second" {
		t.Errorf("target added through the form: %+v", tj)
	}
	submit("detach a cluster in use", "/controls/clusters/remove", url.Values{"name": {"second"}}, bad, `role="alert"`)
	submit("remove the target", "/controls/targets/remove", url.Values{"name": {"other"}}, ok, "Target other removed.")
	submit("detach", "/controls/clusters/remove", url.Values{"name": {"second"}}, ok, "Cluster second detached.")
	if n := len(getJSON[struct{ Clusters []clusterJSON }](t, s, "/api/v1/vast").Clusters); n != 1 {
		t.Errorf("clusters after detaching: %d", n)
	}

	// A form is a POST; a link cannot trigger one.
	if code, _, _ := page(t, s, "/controls/drain?cluster=lab&cnode="+cnodes[0]+"&drained=true"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a form's address: status %d", code)
	}
	if m.Mutating() != 0 {
		t.Errorf("the mock VAST saw %d requests that change something", m.Mutating())
	}
}

// events reads server-sent events from a live stream.
type events struct {
	t      *testing.T
	sc     *bufio.Scanner
	cancel context.CancelFunc
}

func liveStream(t *testing.T, s *Server, path string) *events {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.HTTPAddr()+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%s: status %d, content type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	// Split at every line end the event stream format knows, as a browser does.
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		for i, c := range data {
			if c == '\n' {
				return i + 1, data[:i], nil
			}
			if c == '\r' {
				if i+1 < len(data) && data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				if i+1 < len(data) || atEOF {
					return i + 1, data[:i], nil
				}
			}
		}
		return 0, nil, nil
	})
	return &events{t: t, sc: sc, cancel: cancel}
}

// next returns the next event's data, failing the test on anything in the
// stream that is not a data line or a comment.
func (e *events) next() string {
	e.t.Helper()
	var data []string
	for e.sc.Scan() {
		line := e.sc.Text()
		switch {
		case line == "" && len(data) > 0:
			return strings.Join(data, "\n")
		case line == "" || strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		default:
			e.t.Fatalf("a line that is neither data nor a comment: %q", line)
		}
	}
	e.t.Fatalf("the stream ended: %v", e.sc.Err())
	return ""
}

// until reads events until one contains want.
func (e *events) until(want string) string {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ev := e.next(); strings.Contains(ev, want) {
			return ev
		}
	}
	e.t.Fatalf("no event contained %q", want)
	return ""
}

func TestUILiveStream(t *testing.T) {
	s, err := New(Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", UIInterval: 30 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(cancel)

	// The first event is the page as it stands; it is the same markup the
	// page itself was rendered with.
	ev := liveStream(t, s, "/live/fleet")
	first := ev.next()
	mustContain(t, "first event", first, "No Envoy has connected yet", `<section class="card"`)
	if strings.Contains(first, "<html") || strings.Contains(first, "topbar") {
		t.Error("a live event should carry only the page's changing part")
	}
	_, _, full := page(t, s, "/fleet")
	if i := strings.Index(full, `<section class="card"`); i < 0 || !strings.Contains(squash(full), squash(first)) {
		t.Error("the live event is not the markup the page was rendered with")
	}

	// A change shows up without asking.
	connect(s, 0, 1)
	got := ev.until("envoy-000")
	mustContain(t, "after an Envoy connected", got, "Envoys 1 to 1 of 1", "Pending")

	// A name with line breaks in it, as a hostile or broken Envoy might
	// send, stays inside its event: next() fails on any line it could forge.
	s.fleet.StreamOpen(9001, "")
	s.fleet.Request(9001, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{
		Id: "evil\revent: forged\r\rdata: <b>owned</b>\nretry: 1",
	}})
	got = ev.until("forged")
	mustContain(t, "hostile node ID", got, "&lt;b&gt;owned&lt;/b&gt;", "Envoys 1 to 2 of 2")
	if strings.Contains(got, "<b>owned") {
		t.Error("markup from a node ID reached the page unescaped")
	}

	// The stream takes the page's parameters.
	filtered := liveStream(t, s, "/live/fleet?q=envoy-000")
	mustContain(t, "filtered stream", filtered.next(), "Envoys 1 to 1 of 1 that match")

	if code, _, _ := page(t, s, "/live/nope"); code != http.StatusNotFound {
		t.Errorf("/live/nope: status %d", code)
	}

	// Open pages must not hold up a shutdown.
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	if d := time.Since(started); d > 3*time.Second {
		t.Errorf("stopping took %v with two live streams open", d)
	}
}

// squash removes the white space the live stream drops, so that markup from
// the two sources can be compared.
func squash(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(strings.TrimLeft(line, " \t"))
	}
	return b.String()
}

// Names that reach the pages from outside (an Envoy's node ID, a CNode's
// state as VAST reports it) are text, whatever they contain.
func TestUIEscapesWhatOthersSupply(t *testing.T) {
	s, m := uiDaemon(t)
	cnodes := m.PoolCNodes("main")
	const hostile = `"><script>alert(1)</script><img src=x onerror=alert(2)>`
	s.fleet.StreamOpen(9002, hostile)
	s.fleet.Request(9002, &discoverygrpc.DiscoveryRequest{TypeUrl: resource.ClusterType, Node: &corev3.Node{Id: hostile, UserAgentName: hostile}})
	if err := m.SetCNode(cnodes[0], hostile, true); err != nil {
		t.Fatal(err)
	}
	s.poller.pollOnce(context.Background())

	for _, path := range []string{"/", "/cnodes", "/cnodes?open=" + url.QueryEscape(`lab/`+hostile), "/fleet", "/fleet?q=" + url.QueryEscape(hostile), "/controls"} {
		code, _, body := page(t, s, path)
		if code != http.StatusOK {
			t.Errorf("%s: status %d", path, code)
		}
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") || strings.Contains(body, `""><`) {
			t.Errorf("%s: unescaped markup from outside reached the page", path)
		}
		if err := wellFormed(body); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	_, _, fleet := page(t, s, "/fleet")
	mustContain(t, "fleet", fleet, "&lt;script&gt;alert(1)&lt;/script&gt;")
	_, _, cn := page(t, s, "/cnodes")
	mustContain(t, "cnodes", cn, "VAST reports &#34;&gt;&lt;script&gt;")
}

// Rate rules in the web UI: the form, the table, and the Overview's tile.
func TestUIRateRules(t *testing.T) {
	s, _ := uiDaemon(t)
	_, _, ctl := page(t, s, "/controls")
	mustContain(t, "controls without rules", ctl, "Request-rate rules", "No rate rules. Requests are not limited by rate.", "Add or replace a rule",
		`action="/controls/rules"`, `name="per" value="access_key"`, `<option value="503" selected>`)
	_, _, home := page(t, s, "/")
	mustContain(t, "overview without rules", home, "Refused by rate rules, per second", "No rate rules. Add one under")

	submit := func(what string, form url.Values, wantCode int, wants ...string) string {
		t.Helper()
		code, body := post(t, s, "/controls/rules", form)
		if code != wantCode {
			t.Errorf("%s: status %d, want %d", what, code, wantCode)
		}
		mustContain(t, what, body, wants...)
		if err := wellFormed(body); err != nil {
			t.Errorf("%s: %v", what, err)
		}
		return body
	}
	submit("a rule", url.Values{
		"name": {"uploads"}, "operation": {"putobject, UploadPart"}, "access_key": {"AKIAEXAMPLE1,AKIAEXAMPLE2"}, "bucket": {" training-data "},
		"prefix": {"ckpt/\r\nlogs, old/\n"}, "limit": {"2000"}, "unit": {"second"}, "burst": {"4000"}, "per": {"access_key", "bucket"}, "note": {"agreed <with> the tenant"},
	}, http.StatusOK, "Rule uploads is set and on its way to every Envoy.", `<span class="name">uploads</span>`,
		"At most 2000 PutObject, UploadPart calls per second from access keys AKIAEXAMPLE1 and AKIAEXAMPLE2, to bucket training-data, under prefixes ckpt/ and logs, old/, for each access key and each bucket, across the fleet.",
		"Up to 4000 may pass at once.", "Note: agreed &lt;with&gt; the tenant", "Nothing matches it now", `hx-confirm="Remove rule uploads?`)
	rules := getJSON[rulesResponse](t, s, "/api/v1/rules").Rules
	if len(rules) != 1 || strings.Join(rules[0].Prefix, "|") != "ckpt/|logs, old/" || rules[0].Burst != 4000 || rules[0].LimitPerSecond != 2000 {
		t.Fatalf("the rule as the API has it: %+v", rules)
	}
	submit("a blocked rule", url.Values{"name": {"no-deletes"}, "operation": {"DeleteObject"}, "bucket": {"archive"}, "blocked": {"true"}, "respond": {"429"}, "limit": {""}, "unit": {"second"}},
		http.StatusOK, "All DeleteObject calls to bucket archive are refused with 429 TooManyRequests.")
	submit("per hour", url.Values{"name": {"listings"}, "operation": {"List*"}, "limit": {"0.5"}, "unit": {"minute"}, "per": {"total"}},
		http.StatusOK, "At most 0.5 List* calls per minute, in total across the fleet.")

	// A form that cannot be a rule comes back open, as it was typed, with
	// the daemon's reason.
	body := submit("no per", url.Values{"name": {"bad"}, "operation": {"GetObject"}, "limit": {"5"}, "unit": {"hour"}, "prefix": {"a/\nb/"}, "note": {"keep me"}},
		http.StatusUnprocessableEntity, `role="alert"`, "say what the limit is counted per", `name="name" required pattern="[A-Za-z0-9_.\-]{1,64}" value="bad"`,
		`name="operation" value="GetObject"`, `<option value="hour" selected>`, `value="keep me"`, "a/\nb/</textarea>")
	if !regexp.MustCompile(`<details class="more" hx-morph-skip open>\s*<summary>Add or replace a rule`).MatchString(body) {
		t.Error("the refused form should be shown open")
	}
	submit("no limit", url.Values{"name": {"bad"}, "limit": {""}, "per": {"total"}}, http.StatusUnprocessableEntity, "Limit: enter how many requests")
	submit("an unknown operation", url.Values{"name": {"bad"}, "operation": {"WRITE"}, "limit": {"5"}, "per": {"total"}}, http.StatusUnprocessableEntity, "not an S3 operation")
	if n := len(getJSON[rulesResponse](t, s, "/api/v1/rules").Rules); n != 3 {
		t.Errorf("refused forms left %d rules, want 3", n)
	}

	// What Envoys report shows on both pages.
	s.quota.SetRules(s.store.Rules())
	_, _, home = page(t, s, "/")
	mustContain(t, "overview with idle rules", home, "Nothing refused", "by 3 rules")

	code, body := post(t, s, "/controls/rules/remove", url.Values{"name": {"listings"}})
	if code != http.StatusOK || !strings.Contains(body, "Rule listings removed.") || strings.Contains(body, `<span class="name">listings</span>`) {
		t.Errorf("removing a rule: %d", code)
	}
	if code, body := post(t, s, "/controls/rules/remove", url.Values{"name": {"listings"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "no rule") {
		t.Errorf("removing it again: %d", code)
	}
	// The live stream carries the rules too.
	ev := liveStream(t, s, "/live/controls")
	mustContain(t, "live controls", ev.next(), "Request-rate rules", `<span class="name">uploads</span>`, `<span class="name">no-deletes</span>`)
}
