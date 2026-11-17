package server

import (
	"github.com/prometheus/client_golang/prometheus"

	"deltav/internal/fleet"
	"deltav/internal/metrics"
)

// collector exports the aggregator's cumulative totals at scrape time, so
// one scrape of the daemon covers the whole fleet. Rolling windows stay in
// the JSON API; Prometheus derives its own rates from these counters.
//
// Series are per target, endpoint (VIP) and CNode. Per-fleet-member series
// are off unless asked for: thousands of hosts would be too many.
type collector struct {
	s         *Server
	perMember bool
}

var (
	endpointLabels = []string{"target", "endpoint", "cnode"}

	descRequests = prometheus.NewDesc("deltav_endpoint_requests_total",
		"Requests finished, by status class (2xx, 3xx, 4xx, 5xx, or none if no response was sent). From access logs.",
		append(endpointLabels, "class"), nil)
	descResponses = prometheus.NewDesc("deltav_endpoint_responses_total",
		"Responses with the status codes tracked individually: 403 and 503. Also counted in their class.",
		append(endpointLabels, "code"), nil)
	descOverflow = prometheus.NewDesc("deltav_endpoint_overflow_total",
		"Requests an Envoy refused because a circuit breaker was full (response flag UO).",
		endpointLabels, nil)
	descBytes = prometheus.NewDesc("deltav_endpoint_bytes_total",
		"Body bytes: direction \"in\" is client to storage (uploads), \"out\" is storage to client.",
		append(endpointLabels, "direction"), nil)
	descDuration = prometheus.NewDesc("deltav_endpoint_request_duration_seconds",
		"Request duration as seen by Envoy, from the first request byte to the last response byte.",
		endpointLabels, nil)
	descInFlight = prometheus.NewDesc("deltav_endpoint_in_flight_requests",
		"Requests the fleet has in flight to the endpoint, from load reports. Idle keep-alive connections are not counted.",
		endpointLabels, nil)
	descLoadIssued = prometheus.NewDesc("deltav_endpoint_load_issued_total",
		"Requests issued to the endpoint, from load reports. Unaffected by access log sampling; omits each Envoy's first reporting interval for a target.",
		endpointLabels, nil)
	descLoadDone = prometheus.NewDesc("deltav_endpoint_load_completed_total",
		"Requests completed, by result (success or error), from load reports.",
		append(endpointLabels, "result"), nil)

	descMembers = prometheus.NewDesc("deltav_fleet_members",
		"Envoys known to the daemon, by connection state.", []string{"state"}, nil)
	descConfig = prometheus.NewDesc("deltav_fleet_config",
		"Connected Envoys by state of the last config push: acked, nacked or pending.", []string{"state"}, nil)
	descTargets = prometheus.NewDesc("deltav_target_endpoints",
		"Endpoints currently served to Envoys for the target.", []string{"target"}, nil)
	descEntries = prometheus.NewDesc("deltav_telemetry_access_log_entries_total",
		"Access log entries ingested.", nil, nil)
	descReports = prometheus.NewDesc("deltav_telemetry_load_reports_total",
		"Load reports ingested.", nil, nil)

	descMemberRequests = prometheus.NewDesc("deltav_member_requests_total",
		"Requests finished by one Envoy, by status class. Only with --metrics-per-member.",
		[]string{"node", "class"}, nil)
	descMemberBytes = prometheus.NewDesc("deltav_member_bytes_total",
		"Body bytes through one Envoy, by direction. Only with --metrics-per-member.",
		[]string{"node", "direction"}, nil)
)

// Describe sends nothing, which makes this an unchecked collector: its
// label values come and go with endpoints and Envoys.
func (c collector) Describe(chan<- *prometheus.Desc) {}

var classNames = [6]string{"none", "none", "2xx", "3xx", "4xx", "5xx"}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}

	targets := c.s.xds.Targets()
	cnodes := configuredEndpoints(targets)
	for _, t := range targets {
		gauge(descTargets, float64(len(t.Endpoints)), t.Name)
	}

	for _, e := range c.s.agg.EndpointTotals() {
		l := []string{e.Key.Target, e.Key.Endpoint, cnodes[e.Key]}
		with := func(extra string) []string { return append(l[:3:3], extra) }
		t := &e.Totals
		counter(descRequests, t.Class[0]+t.Class[1], with("none")...)
		for class := 2; class <= 5; class++ {
			counter(descRequests, t.Class[class], with(classNames[class])...)
		}
		counter(descResponses, t.S403, with("403")...)
		counter(descResponses, t.S503, with("503")...)
		counter(descOverflow, t.Overflow, l...)
		counter(descBytes, t.BytesIn, with("in")...)
		counter(descBytes, t.BytesOut, with("out")...)
		buckets, count := metrics.PrometheusBuckets(&t.Hist)
		ch <- prometheus.MustNewConstHistogram(descDuration, count, t.DurationSum.Seconds(), buckets, l...)
		gauge(descInFlight, float64(e.InFlight), l...)
		counter(descLoadIssued, e.Load.Issued, l...)
		counter(descLoadDone, e.Load.Success, with("success")...)
		counter(descLoadDone, e.Load.Errors, with("error")...)
	}

	counts := countFleet(c.s.fleet.List())
	gauge(descMembers, float64(counts.Connected), "connected")
	gauge(descMembers, float64(counts.Total-counts.Connected), "disconnected")
	gauge(descConfig, float64(counts.Acked), string(fleet.StateAcked))
	gauge(descConfig, float64(counts.Nacked), string(fleet.StateNacked))
	gauge(descConfig, float64(counts.Pending), string(fleet.StatePending))
	counter(descEntries, c.s.telemetry.Entries())
	counter(descReports, c.s.telemetry.Reports())

	if c.perMember {
		for node, t := range c.s.agg.MemberTotals() {
			counter(descMemberRequests, t.Class[0]+t.Class[1], node, "none")
			for class := 2; class <= 5; class++ {
				counter(descMemberRequests, t.Class[class], node, classNames[class])
			}
			counter(descMemberBytes, t.BytesIn, node, "in")
			counter(descMemberBytes, t.BytesOut, node, "out")
		}
	}
}
