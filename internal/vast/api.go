package vast

import "context"

// The types below hold the fields Delta-V reads, with the names the API
// uses. They were written against responses from a VAST 5.5.3 cluster; the
// sanitized responses are in testdata/vast/v5.5.3 and the tests decode
// them. Every other field the cluster returns is ignored.

// CNode is one storage node, from GET /api/cnodes/.
type CNode struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"` // e.g. "cnode-3-10"; VIPs refer to a CNode by this
	Hostname     string `json:"hostname"`
	IP           string `json:"ip"`
	MgmtIP       string `json:"mgmt_ip"`
	State        string `json:"state"`
	DisplayState string `json:"display_state"`
	Enabled      bool   `json:"enabled"`
	IsMgmt       bool   `json:"is_mgmt"`
	IsLeader     bool   `json:"is_leader"`
	Build        string `json:"build"`
	OSVersion    string `json:"os_version"`
}

// StateActive is the state of a CNode that is in service. It is the only
// value seen so far; see docs/ASSUMPTIONS.md.
const StateActive = "ACTIVE"

// Healthy reports whether the CNode should receive S3 traffic: it is
// enabled and ACTIVE. Any state Delta-V has not seen counts as unhealthy.
func (c CNode) Healthy() bool { return c.Enabled && c.State == StateActive }

// VIPPool is a pool of virtual IPs, from GET /api/vippools/. Pools belong
// to tenants; TenantName is empty for a pool with no tenant.
type VIPPool struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	TenantName string     `json:"tenant_name"`
	Role       string     `json:"role"` // e.g. "PROTOCOLS"
	Enabled    bool       `json:"enabled"`
	StartIP    string     `json:"start_ip"`
	EndIP      string     `json:"end_ip"`
	IPRanges   [][]string `json:"ip_ranges"` // [[first, last], ...]
}

// VIP is one virtual IP and where it is now, from GET /api/vips/. VIPPool
// and CNode are names, matching VIPPool.Name and CNode.Name. A VIP moves
// between CNodes; CNode is its current host.
type VIP struct {
	ID         int64  `json:"id"`
	IP         string `json:"ip"`
	VIPPool    string `json:"vippool"`
	CNode      string `json:"cnode"`
	TenantName string `json:"tenant_name"`
}

// API is everything Delta-V asks of a VAST cluster: three reads.
type API interface {
	ListCNodes(ctx context.Context) ([]CNode, error)
	ListVIPPools(ctx context.Context) ([]VIPPool, error)
	ListVIPs(ctx context.Context) ([]VIP, error)
}

var _ API = (*Client)(nil)

// ListCNodes returns every CNode. The cluster answers with one unpaginated
// JSON array.
func (c *Client) ListCNodes(ctx context.Context) ([]CNode, error) {
	var out []CNode
	return out, c.Get(ctx, "cnodes/", &out)
}

// ListVIPPools returns every VIP pool of every tenant the user can see.
func (c *Client) ListVIPPools(ctx context.Context) ([]VIPPool, error) {
	var out []VIPPool
	return out, c.Get(ctx, "vippools/", &out)
}

// ListVIPs returns every VIP with its pool and its current CNode.
func (c *Client) ListVIPs(ctx context.Context) ([]VIP, error) {
	var out []VIP
	return out, c.Get(ctx, "vips/", &out)
}
