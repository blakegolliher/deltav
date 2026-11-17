# Delta-V

Delta-V is the control plane for a fleet of loopback Envoys that load-balance S3 traffic across
the VIPs of one or more VAST clusters, with no proxy machines in the data path. It serves
endpoints over xDS, collects telemetry from every Envoy over the streams Envoy opens, and shows
what the fleet and the CNodes are doing.

**Status: proof of concept, milestone 3 of 8.** The daemon serves a static endpoint list to Envoy
over xDS and tracks every connected Envoy; it ingests Envoy's access logs and load reports and
keeps rolling per-endpoint and per-Envoy stats, served by the `/api/v1` endpoints below and
`/metrics`; `deltav bootstrap` prints the client bootstrap; a test harness (`dvlab`) provides fake
S3 backends and a load generator. VAST discovery and the UIs come in later milestones.
See [docs/DESIGN.md](docs/DESIGN.md). A ten-minute quick start comes
with milestone 8.

The binary is `deltav`, also installed as `dv`; the two behave identically.

## Pinned versions

| Component | Version |
|---|---|
| Envoy (client hosts and test harness) | **1.40.2** |
| go-control-plane / Envoy API | v0.14.0 / v1.40.0 |
| Go | ≥ 1.26 (`go.mod` selects toolchain go1.27.4) |

## Build

```sh
make build        # bin/deltav and the bin/dv symlink
make test         # vet, license gate, tests
sudo make install # /usr/local/bin/deltav and dv
```

`go build ./cmd/deltav` also works on its own.

## Try it without a VAST cluster

On Linux every `127.x.y.z` address is loopback, so fake backends can share one port the way VIPs do.

```sh
make build envoy                      # also fetches and verifies the pinned Envoy binary
export AWS_ACCESS_KEY_ID=dvlab AWS_SECRET_ACCESS_KEY=dvlab-secret   # any pair; the fake verifies it

bin/dvlab fakes3 --listen 127.0.0.11:9100,127.0.0.12:9100,127.0.0.13:9100 &
bin/deltav serve --static-target s3:9000:127.0.0.11:9100,127.0.0.12:9100,127.0.0.13:9100 &
bin/deltav bootstrap --xds-address 127.0.0.1:18000 > bootstrap.json
.cache/envoy/envoy-1.40.2-linux-x86_64 -c bootstrap.json --service-node "$(hostname)" &

curl -s 127.0.0.1:18080/api/v1/fleet  # the Envoy, its version and whether it accepted the config
bin/dvlab loadgen --endpoint http://127.0.0.1:9000 --bucket lab --requests 1000 \
    --special-keys --expect --check   # through Envoy; fails if Host, path, Expect or body changed
```

`make integration` runs the same arrangement as tests, with real Envoy processes.

## HTTP API and metrics

The API is JSON under `/api/v1` on the HTTP listener (`127.0.0.1:18080` by default; a bearer token
is required when it is bound elsewhere). Stats take `?window=1m|5m|1h`.

| Path | What it returns |
|---|---|
| `/api/v1/overview` | Per target: endpoints served, requests in flight, request and byte rates, status counts with 403 and 503 broken out, p50/p90/p99. Fleet counts. |
| `/api/v1/targets` | The targets and endpoints being served to Envoys. |
| `/api/v1/endpoints` | The same stats per endpoint (VIP); `?target=` narrows it. |
| `/api/v1/fleet` | Envoys with version, last seen, config state and stats. `?limit=&offset=`, `?sort=node_id\|request_rate\|last_seen\|connected_at\|envoy_version\|config_state\|address`, `?order=asc\|desc`, `?q=` (substring), `?state=acked\|nacked\|pending`, `?connected=true\|false`. |
| `/api/v1/fleet/{node}` | One Envoy, every window. |
| `/api/v1/events` | Server-sent events: the overview, every `?interval=` (default 2s). |

"In flight" is the number of requests Envoys have outstanding to an endpoint, from load reports.
S3 over HTTP/1.1 holds one request per connection, so it is the count of busy connections; idle
keep-alive connections are not included.

`/metrics` exports the same data as cumulative Prometheus series, so one scrape covers the fleet:
`deltav_endpoint_requests_total{target,endpoint,cnode,class}`,
`deltav_endpoint_responses_total{...,code="403"|"503"}`, `deltav_endpoint_bytes_total{...,direction}`,
`deltav_endpoint_request_duration_seconds` (histogram), `deltav_endpoint_in_flight_requests`,
`deltav_endpoint_overflow_total`, `deltav_endpoint_load_issued_total`,
`deltav_endpoint_load_completed_total{...,result}`, `deltav_fleet_members{state}`,
`deltav_fleet_config{state}` and `deltav_target_endpoints{target}`. Per-Envoy series
(`deltav_member_*`) are off unless the daemon runs with `--metrics-per-member`.

## S3 clients

Clients point at `http://127.0.0.1:<listen port>` and **must use path-style addressing**
(`/bucket/key`): the host is a loopback address, so there is no bucket subdomain. Envoy passes the
`Host` header, the path and `Expect: 100-continue` through unchanged, so SigV4 signatures made for
the loopback address verify at the backend.

## Known gaps

These are out of scope for the POC:

- No mutual TLS on the xDS and telemetry streams. Anything that can reach the gRPC port can read
  config and send telemetry.
- No high availability for the daemon. Envoys keep their last config while it is down.
- No per-group targeting: every Envoy receives every target.
- No long-term metric storage. Windows are kept in memory for up to 1 h; scrape `/metrics` for
  history.

## Licensing

Delta-V is MIT-licensed; see [LICENSE](LICENSE). Third-party licenses are listed in [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md) and audited
in [docs/LICENSE_AUDIT.md](docs/LICENSE_AUDIT.md).
