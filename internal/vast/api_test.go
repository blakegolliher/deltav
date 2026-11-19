package vast

import (
	"context"
	"testing"

	"deltav/internal/vast/vasttest"
)

// fixtures are the sanitized responses of a real VAST 5.5.3 cluster.
const fixtures = "../../testdata/vast/v5.5.3"

func mockClient(t *testing.T) (*Client, *vasttest.Server) {
	t.Helper()
	srv, err := vasttest.New(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	c, err := New(Options{Host: srv.Host(), User: vasttest.User, Password: vasttest.Password, rootCAs: srv.CertPool()})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

// The typed list calls against the real response shapes.
func TestListCallsDecodeRealResponses(t *testing.T) {
	c, srv := mockClient(t)
	ctx := context.Background()

	cnodes, err := c.ListCNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cnodes) != 3 {
		t.Fatalf("%d CNodes, want 3", len(cnodes))
	}
	byName := map[string]CNode{}
	leaders, mgmt := 0, 0
	for _, n := range cnodes {
		byName[n.Name] = n
		if n.ID == 0 || n.Name == "" || n.Hostname == "" || n.IP == "" || n.MgmtIP == "" || n.Build == "" || n.OSVersion == "" {
			t.Errorf("CNode with an empty field: %+v", n)
		}
		if n.State != StateActive || n.DisplayState != "ACTIVE" || !n.Enabled || !n.Healthy() {
			t.Errorf("CNode %s: state %q/%q enabled %v", n.Name, n.State, n.DisplayState, n.Enabled)
		}
		if n.IsLeader {
			leaders++
		}
		if n.IsMgmt {
			mgmt++
		}
	}
	if leaders != 1 || mgmt != 1 {
		t.Errorf("%d leaders and %d management nodes, want one of each", leaders, mgmt)
	}

	pools, err := c.ListVIPPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 48 {
		t.Fatalf("%d pools, want 48", len(pools))
	}
	poolNames := map[string]VIPPool{}
	for _, p := range pools {
		poolNames[p.Name] = p
		if p.ID == 0 || p.Name == "" || p.Role == "" || !p.Enabled || p.StartIP == "" || p.EndIP == "" {
			t.Errorf("pool with an empty field: %+v", p)
		}
		if len(p.IPRanges) != 1 || len(p.IPRanges[0]) != 2 || p.IPRanges[0][0] != p.StartIP || p.IPRanges[0][1] != p.EndIP {
			t.Errorf("pool %s: ranges %v, start %s, end %s", p.Name, p.IPRanges, p.StartIP, p.EndIP)
		}
	}
	main, ok := poolNames["main"]
	if !ok || main.ID != 2 || main.TenantName != "default" || main.Role != "PROTOCOLS" || main.DomainName != "main" {
		t.Fatalf("pool main: %+v (found %v)", main, ok)
	}

	// The DNS suffix: with a pool's own label it makes the name clients use
	// for the pool, and it is where VAST looks for a bucket in a host name.
	dns, err := c.ListDNS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dns) != 1 || dns[0].ID != 1 || dns[0].Name != "dns" || dns[0].DomainSuffix != "dns1.example.com" || !dns[0].Enabled {
		t.Fatalf("DNS: %+v", dns)
	}

	vips, err := c.ListVIPs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vips) != 187 {
		t.Fatalf("%d VIPs, want 187", len(vips))
	}
	mainPerCNode := map[string]int{}
	for _, v := range vips {
		if v.ID == 0 || v.IP == "" {
			t.Errorf("VIP with an empty field: %+v", v)
		}
		// Every VIP names a pool and a CNode that exist: this is the
		// VIP-to-CNode map the whole design rests on.
		pool, ok := poolNames[v.VIPPool]
		if !ok {
			t.Errorf("VIP %s names pool %q, which is not in the pool list", v.IP, v.VIPPool)
		}
		if _, ok := byName[v.CNode]; !ok {
			t.Errorf("VIP %s names CNode %q, which is not in the CNode list", v.IP, v.CNode)
		}
		if v.TenantName != pool.TenantName {
			t.Errorf("VIP %s is in tenant %q, its pool in %q", v.IP, v.TenantName, pool.TenantName)
		}
		if v.VIPPool == "main" {
			mainPerCNode[v.CNode]++
		}
	}
	if len(mainPerCNode) != 3 || mainPerCNode["cnode-3-10"] != 2 || mainPerCNode["cnode-3-11"] != 2 || mainPerCNode["cnode-3-12"] != 2 {
		t.Errorf("pool main: VIPs per CNode %v, want two on each of three", mainPerCNode)
	}

	// One login, four GETs, nothing else.
	req := srv.Requests()
	if len(req) != 5 || req["POST /api/token/"] != 1 || req["GET /api/cnodes/"] != 1 || req["GET /api/vippools/"] != 1 || req["GET /api/vips/"] != 1 || req["GET /api/dns/"] != 1 {
		t.Errorf("requests: %v", req)
	}
	if srv.Mutating() != 0 {
		t.Errorf("%d mutating requests reached the cluster", srv.Mutating())
	}
}

func TestHealthy(t *testing.T) {
	for _, tc := range []struct {
		n    CNode
		want bool
	}{
		{CNode{State: "ACTIVE", Enabled: true}, true},
		{CNode{State: "ACTIVE", Enabled: false}, false},
		{CNode{State: "INACTIVE", Enabled: true}, false},
		{CNode{State: "", Enabled: true}, false},
		{CNode{State: "active", Enabled: true}, false}, // only the exact value seen counts
	} {
		if got := tc.n.Healthy(); got != tc.want {
			t.Errorf("%+v: Healthy() = %v", tc.n, got)
		}
	}
}

// The mock can change under the client's feet, as a cluster does.
func TestMockStateChanges(t *testing.T) {
	c, srv := mockClient(t)
	ctx := context.Background()
	if err := srv.SetCNode("cnode-3-11", "INACTIVE", true); err != nil {
		t.Fatal(err)
	}
	cnodes, err := c.ListCNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unhealthy := 0
	for _, n := range cnodes {
		if !n.Healthy() {
			unhealthy++
			if n.Name != "cnode-3-11" {
				t.Errorf("%s is unhealthy", n.Name)
			}
		}
	}
	if unhealthy != 1 {
		t.Fatalf("%d unhealthy CNodes, want 1", unhealthy)
	}

	vips, _ := c.ListVIPs(ctx)
	var ip string
	for _, v := range vips {
		if v.VIPPool == "main" && v.CNode == "cnode-3-11" {
			ip = v.IP
			break
		}
	}
	if err := srv.MoveVIP(ip, "cnode-3-12"); err != nil {
		t.Fatal(err)
	}
	vips, _ = c.ListVIPs(ctx)
	for _, v := range vips {
		if v.IP == ip && v.CNode != "cnode-3-12" {
			t.Errorf("VIP %s is on %s after the move", ip, v.CNode)
		}
	}

	// An expired token is renewed without the caller noticing; a failing
	// cluster is an error.
	srv.ExpireTokens(false)
	if _, err := c.ListVIPs(ctx); err != nil {
		t.Fatalf("after access token expiry: %v", err)
	}
	srv.ExpireTokens(true)
	if _, err := c.ListVIPs(ctx); err != nil {
		t.Fatalf("after access and refresh token expiry: %v", err)
	}
	if req := srv.Requests(); req["POST /api/token/refresh/"] != 2 || req["POST /api/token/"] != 2 {
		t.Errorf("token calls: %v", req)
	}
	srv.FailNext(1)
	if _, err := c.ListVIPs(ctx); err == nil {
		t.Fatal("a 500 from the cluster was not reported")
	}
	if srv.SetCNode("nope", "ACTIVE", true) == nil || srv.MoveVIP("192.0.2.250", "cnode-3-10") == nil || srv.MoveVIP(ip, "nope") == nil {
		t.Error("the mock accepted a change to something that does not exist")
	}
	if n := srv.RemovePoolVIPs("main"); n != 6 {
		t.Errorf("removed %d VIPs from main, want 6", n)
	}
	if srv.Mutating() != 0 {
		t.Errorf("%d mutating requests", srv.Mutating())
	}
}
