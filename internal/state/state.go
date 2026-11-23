// Package state is the daemon's persistent configuration: the VAST clusters
// it is attached to (with their credentials) and the targets it serves.
// Everything lives in one JSON file, mode 0600, replaced atomically.
//
// A second file caches what was last served for each target (its endpoints
// and any enforced share), so a restarted daemon can offer Envoys the same
// config at once, before (or without) reaching VAST.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"deltav/internal/ratelimit"
)

// Cluster is one VAST cluster: a management endpoint plus credentials.
type Cluster struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	User     string `json:"user"`
	Password string `json:"password"`
	Tenant   string `json:"tenant,omitempty"`
	// Insecure turns certificate verification off for this cluster.
	Insecure bool `json:"insecure,omitempty"`
}

// caBundlePaths says where a host's own CA bundle usually is.
const caBundlePaths = "/etc/ssl/certs/ca-certificates.crt on Debian and Ubuntu, /etc/pki/tls/certs/ca-bundle.crt on RHEL and Fedora"

// TLS is a target's upstream TLS setting. Verification needs CAFile, a path
// on the client hosts, unless Insecure.
type TLS struct {
	ServerName string `json:"server_name"`
	CAFile     string `json:"ca_file,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
}

// Limit modes.
const (
	ModeAlert   = "alert"   // report CNodes over the limit; enforce nothing
	ModeEnforce = "enforce" // also cap each Envoy's connections per VIP
)

// Limit is a target's per-CNode connection limit.
type Limit struct {
	PerCNode uint32 `json:"per_cnode"`
	Mode     string `json:"mode"`
}

// Drain names a CNode whose VIPs are withheld from Envoys.
type Drain struct {
	Cluster string `json:"cluster"`
	CNode   string `json:"cnode"`
}

// Target is what Envoys balance across: one VIP pool of one cluster, a
// port, and whether upstream TLS is used. It becomes one Envoy cluster and
// one loopback listener port on every client.
type Target struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	Pool    string `json:"pool"`
	// Tenant narrows Pool when two tenants each have a pool of that name.
	Tenant     string `json:"tenant,omitempty"`
	Port       uint32 `json:"port"`        // the S3 port on the VIPs
	ListenPort uint32 `json:"listen_port"` // the loopback port on every client
	TLS        *TLS   `json:"tls,omitempty"`
	Limit      *Limit `json:"limit,omitempty"` // nil: no limit
}

// Endpoint is one cached endpoint of a target.
type Endpoint struct {
	IP    string `json:"ip"`
	Port  uint32 `json:"port"`
	CNode string `json:"cnode,omitempty"`
}

type file struct {
	Version  int       `json:"version"`
	Clusters []Cluster `json:"clusters"`
	Targets  []Target  `json:"targets"`
	Drained  []Drain   `json:"drained,omitempty"`
	// Rules are the request-rate rules, in the order they are applied.
	Rules []ratelimit.Rule `json:"rules,omitempty"`
}

const (
	stateFile = "state.json"
	cacheFile = "served.json"
	// fileVersion is the version of the state file this build writes, and
	// the newest it opens: a newer file may hold what this build cannot
	// represent, and its next save would drop it. A field added within a
	// version is tolerated, and lost at the next save by a build without it.
	fileVersion = 1
)

// Errors the API maps to HTTP statuses.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// MaxRules bounds the request-rate rules: every rule is a filter each
// request passes through in every Envoy.
const MaxRules = 64

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Store holds the state in memory and writes every change through to disk.
// With no directory it keeps everything in memory only (tests).
type Store struct {
	dir string

	mu   sync.Mutex
	data file
}

// Open loads the state from dir, creating the directory (mode 0700) if it
// does not exist. An empty dir gives a store that is never written.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, data: file{Version: fileVersion}}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state directory: %w (choose another with --state-dir)", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Prove the directory is writable now, not at the first change.
		probe, err := os.CreateTemp(dir, ".write-test-*")
		if err != nil {
			return nil, fmt.Errorf("state directory: %w (choose another with --state-dir)", err)
		}
		probe.Close()
		os.Remove(probe.Name())
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("state file: %w", err)
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("state file %s: %w", filepath.Join(dir, stateFile), err)
	}
	if s.data.Version > fileVersion {
		return nil, fmt.Errorf("state file %s: written by a newer Delta-V (version %d); this build reads up to version %d",
			filepath.Join(dir, stateFile), s.data.Version, fileVersion)
	}
	// Rules are checked when they are set; a file edited by hand, or written
	// by a version that knew other operations, is checked again here.
	if err := ratelimit.Validate(s.data.Rules); err != nil {
		return nil, fmt.Errorf("state file %s: %w", filepath.Join(dir, stateFile), err)
	}
	return s, nil
}

// writeAtomic replaces dir/name with data: a temporary file in the same
// directory, synced, then renamed over the old one.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// save must be called with s.mu held.
func (s *Store) save() error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.dir, stateFile, append(b, '\n'))
}

// Clusters returns the clusters, sorted by name. They carry passwords: the
// caller must not hand them to an API response.
func (s *Store) Clusters() []Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Cluster(nil), s.data.Clusters...)
}

// Cluster returns one cluster by name.
func (s *Store) Cluster(name string) (Cluster, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.data.Clusters {
		if c.Name == name {
			return c, true
		}
	}
	return Cluster{}, false
}

// Targets returns the targets, sorted by name.
func (s *Store) Targets() []Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Target(nil), s.data.Targets...)
}

// AddCluster stores a new cluster.
func (s *Store) AddCluster(c Cluster) error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("%w: cluster name %q: use letters, digits, '.', '_' and '-'", ErrInvalid, c.Name)
	}
	if c.Host == "" || c.User == "" || c.Password == "" {
		return fmt.Errorf("%w: a cluster needs a host, a user and a password", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.data.Clusters {
		if have.Name == c.Name {
			return fmt.Errorf("%w: cluster %q already exists", ErrConflict, c.Name)
		}
	}
	old := s.data.Clusters
	s.data.Clusters = append(append([]Cluster(nil), old...), c)
	sort.Slice(s.data.Clusters, func(i, j int) bool { return s.data.Clusters[i].Name < s.data.Clusters[j].Name })
	if err := s.save(); err != nil {
		s.data.Clusters = old
		return err
	}
	return nil
}

// RemoveCluster deletes a cluster that no target uses.
func (s *Store) RemoveCluster(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.data.Targets {
		if t.Cluster == name {
			return fmt.Errorf("%w: cluster %q is used by target %q; remove the target first", ErrConflict, name, t.Name)
		}
	}
	old := s.data.Clusters
	kept := make([]Cluster, 0, len(old))
	for _, c := range old {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	if len(kept) == len(old) {
		return fmt.Errorf("%w: no cluster %q", ErrNotFound, name)
	}
	oldDrained := s.data.Drained
	var drained []Drain
	for _, d := range oldDrained {
		if d.Cluster != name {
			drained = append(drained, d)
		}
	}
	s.data.Clusters, s.data.Drained = kept, drained
	if err := s.save(); err != nil {
		s.data.Clusters, s.data.Drained = old, oldDrained
		return err
	}
	return nil
}

// AddTarget stores a new target. reserved maps listen ports that are
// already taken by something outside the store (static targets) to its
// name.
func (s *Store) AddTarget(t Target, reserved map[uint32]string) error {
	switch {
	case !nameRE.MatchString(t.Name):
		return fmt.Errorf("%w: target name %q: use letters, digits, '.', '_' and '-'", ErrInvalid, t.Name)
	case t.Pool == "":
		return fmt.Errorf("%w: a target needs a VIP pool", ErrInvalid)
	case t.Port == 0 || t.Port > 65535 || t.ListenPort == 0 || t.ListenPort > 65535:
		return fmt.Errorf("%w: port and listen port must be 1-65535", ErrInvalid)
	case t.TLS != nil && t.TLS.ServerName == "":
		return fmt.Errorf("%w: a TLS target needs a server name: Envoy would otherwise send the loopback address as SNI", ErrInvalid)
	case t.TLS != nil && !t.TLS.Insecure && t.TLS.CAFile == "":
		// Envoy cannot be told to use "the host's trust store"; a target
		// stored without a file would be rejected by every Envoy.
		return fmt.Errorf("%w: a TLS target needs the path of a CA bundle on the client hosts to verify the certificate against (a host's own is at %s), or verification switched off",
			ErrInvalid, caBundlePaths)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, c := range s.data.Clusters {
		found = found || c.Name == t.Cluster
	}
	if !found {
		return fmt.Errorf("%w: no cluster %q", ErrNotFound, t.Cluster)
	}
	if other, taken := reserved[t.ListenPort]; taken {
		return fmt.Errorf("%w: listen port %d is used by target %q", ErrConflict, t.ListenPort, other)
	}
	for _, have := range s.data.Targets {
		if have.Name == t.Name {
			return fmt.Errorf("%w: target %q already exists", ErrConflict, t.Name)
		}
		if have.ListenPort == t.ListenPort {
			return fmt.Errorf("%w: listen port %d is used by target %q", ErrConflict, t.ListenPort, have.Name)
		}
	}
	old := s.data.Targets
	s.data.Targets = append(append([]Target(nil), old...), t)
	sort.Slice(s.data.Targets, func(i, j int) bool { return s.data.Targets[i].Name < s.data.Targets[j].Name })
	if err := s.save(); err != nil {
		s.data.Targets = old
		return err
	}
	return nil
}

// RemoveTarget deletes a target.
func (s *Store) RemoveTarget(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.data.Targets
	kept := make([]Target, 0, len(old))
	for _, t := range old {
		if t.Name != name {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(old) {
		return fmt.Errorf("%w: no target %q", ErrNotFound, name)
	}
	s.data.Targets = kept
	if err := s.save(); err != nil {
		s.data.Targets = old
		return err
	}
	return nil
}

// SetTargetLimit sets a target's per-CNode connection limit, or removes it
// if l is nil or its PerCNode is 0.
func (s *Store) SetTargetLimit(name string, l *Limit) error {
	if l != nil && l.PerCNode == 0 {
		l = nil
	}
	if l != nil && l.Mode != ModeAlert && l.Mode != ModeEnforce {
		return fmt.Errorf("%w: limit mode %q: want %s or %s", ErrInvalid, l.Mode, ModeAlert, ModeEnforce)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.data.Targets {
		if t.Name != name {
			continue
		}
		if l != nil {
			copied := *l
			l = &copied
		}
		// A new slice: Targets() hands out the old one's elements.
		targets := append([]Target(nil), s.data.Targets...)
		targets[i].Limit = l
		prev := s.data.Targets
		s.data.Targets = targets
		if err := s.save(); err != nil {
			s.data.Targets = prev
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: no target %q", ErrNotFound, name)
}

// Rules returns the request-rate rules, in the order they are applied.
func (s *Store) Rules() []ratelimit.Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ratelimit.Rule(nil), s.data.Rules...)
}

// SetRule adds a rule, or replaces the rule of the same name where it
// stands. The rule is normalized first.
func (s *Store) SetRule(r ratelimit.Rule) error {
	if err := r.Normalize(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := append([]ratelimit.Rule(nil), s.data.Rules...)
	replaced := false
	for i := range rules {
		if rules[i].Name == r.Name {
			rules[i], replaced = r, true
		}
	}
	if !replaced {
		if len(rules) >= MaxRules {
			return fmt.Errorf("%w: there are already %d rules, which is the most there can be", ErrConflict, MaxRules)
		}
		rules = append(rules, r)
	}
	return s.setRules(rules)
}

// RemoveRule removes a rule.
func (s *Store) RemoveRule(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rules []ratelimit.Rule
	for _, r := range s.data.Rules {
		if r.Name != name {
			rules = append(rules, r)
		}
	}
	if len(rules) == len(s.data.Rules) {
		return fmt.Errorf("%w: no rule %q", ErrNotFound, name)
	}
	return s.setRules(rules)
}

// ReplaceRules puts back a list of rules as it was, after a change that
// could not be served.
func (s *Store) ReplaceRules(rules []ratelimit.Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setRules(append([]ratelimit.Rule(nil), rules...))
}

func (s *Store) setRules(rules []ratelimit.Rule) error {
	prev := s.data.Rules
	s.data.Rules = rules
	if err := s.save(); err != nil {
		s.data.Rules = prev
		return err
	}
	return nil
}

// Drained returns the drained CNodes.
func (s *Store) Drained() []Drain {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Drain(nil), s.data.Drained...)
}

// SetDrained drains or undrains a CNode of an attached cluster. Draining
// one already drained is not an error; undraining one that is not is.
func (s *Store) SetDrained(cluster, cnode string, drained bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, c := range s.data.Clusters {
		found = found || c.Name == cluster
	}
	if !found {
		return fmt.Errorf("%w: no cluster %q", ErrNotFound, cluster)
	}
	old := s.data.Drained
	var next []Drain
	was := false
	for _, d := range old {
		if d.Cluster == cluster && d.CNode == cnode {
			was = true
			continue
		}
		next = append(next, d)
	}
	switch {
	case drained:
		next = append(next, Drain{Cluster: cluster, CNode: cnode})
		sort.Slice(next, func(i, j int) bool {
			if next[i].Cluster != next[j].Cluster {
				return next[i].Cluster < next[j].Cluster
			}
			return next[i].CNode < next[j].CNode
		})
	case !was:
		return fmt.Errorf("%w: CNode %q of cluster %q is not drained", ErrNotFound, cnode, cluster)
	}
	s.data.Drained = next
	if err := s.save(); err != nil {
		s.data.Drained = old
		return err
	}
	return nil
}

// Cache is what a restarted daemon needs in order to serve Envoys exactly
// what they were being served: each target's endpoints, for a target with
// an enforced limit the per-Envoy share, and the host names under which the
// target's cluster reads a bucket from the host name.
type Cache struct {
	Endpoints map[string][]Endpoint      `json:"endpoints"`
	Shares    map[string]uint32          `json:"shares,omitempty"`
	Hosts     map[string]ratelimit.Hosts `json:"hosts,omitempty"`
}

// LoadCache returns the cache. An absent one is empty, and so is one that
// cannot be read or parsed; the error then says why, for the log: it is
// only a cache, but a restart without it is not the restart 24 promises.
func (s *Store) LoadCache() (Cache, error) {
	c := Cache{}
	var err error
	if s.dir != "" {
		path := filepath.Join(s.dir, cacheFile)
		var b []byte
		switch b, err = os.ReadFile(path); {
		case errors.Is(err, fs.ErrNotExist):
			err = nil
		case err == nil:
			if err = json.Unmarshal(b, &c); err != nil {
				c, err = Cache{}, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	if c.Endpoints == nil {
		c.Endpoints = map[string][]Endpoint{}
	}
	if c.Shares == nil {
		c.Shares = map[string]uint32{}
	}
	if c.Hosts == nil {
		c.Hosts = map[string]ratelimit.Hosts{}
	}
	return c, err
}

// SaveCache replaces the cache.
func (s *Store) SaveCache(c Cache) error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.dir, cacheFile, append(b, '\n'))
}
