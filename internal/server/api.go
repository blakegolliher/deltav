package server

import (
	"net/http"

	"deltav/internal/fleet"
	"deltav/internal/xds"
)

// fleetResponse is GET /api/v1/fleet. ConfigVersion is the snapshot the
// daemon currently offers; a member is in sync when its own config_version
// equals it. Paging, sort and filter arrive with the rest of the API in
// milestone 3.
type fleetResponse struct {
	ConfigVersion string         `json:"config_version"`
	Total         int            `json:"total"`
	Connected     int            `json:"connected"`
	Members       []fleet.Member `json:"members"`
}

func (s *Server) handleFleet(w http.ResponseWriter, _ *http.Request) {
	members := s.fleet.List()
	resp := fleetResponse{ConfigVersion: s.xds.Version(), Total: len(members), Members: members}
	for _, m := range members {
		if m.Connected {
			resp.Connected++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type endpointJSON struct {
	IP    string `json:"ip"`
	Port  uint32 `json:"port"`
	CNode string `json:"cnode,omitempty"`
}

type targetJSON struct {
	Name       string         `json:"name"`
	ListenPort uint32         `json:"listen_port"`
	TLS        bool           `json:"tls"`
	Endpoints  []endpointJSON `json:"endpoints"`
}

type targetsResponse struct {
	ConfigVersion string       `json:"config_version"`
	Targets       []targetJSON `json:"targets"`
}

func (s *Server) handleTargets(w http.ResponseWriter, _ *http.Request) {
	resp := targetsResponse{ConfigVersion: s.xds.Version(), Targets: []targetJSON{}}
	for _, t := range s.xds.Targets() {
		resp.Targets = append(resp.Targets, targetToJSON(t))
	}
	writeJSON(w, http.StatusOK, resp)
}

func targetToJSON(t xds.TargetSpec) targetJSON {
	out := targetJSON{Name: t.Name, ListenPort: t.ListenPort, TLS: t.TLS != nil, Endpoints: []endpointJSON{}}
	for _, e := range t.Endpoints {
		out.Endpoints = append(out.Endpoints, endpointJSON{IP: e.IP, Port: e.Port, CNode: e.CNode})
	}
	return out
}
