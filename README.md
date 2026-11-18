# Delta-V

Delta-V is the control plane for a fleet of loopback Envoys that load-balance S3 traffic across
the VIPs of one or more VAST clusters, with no proxy machines in the data path. It serves
endpoints over xDS, collects telemetry from every Envoy over the streams Envoy opens, and shows
what the fleet and the CNodes are doing.

**Status: proof of concept; milestones 1 to 7 of 8 are done.** Still to come: packaging with a
quick start (8). The daemon reads healthy endpoints from VAST clusters (or takes a static list),
serves them to Envoy over xDS and tracks every connected Envoy; it ingests Envoy's access logs and
load reports and keeps rolling per-endpoint, per-CNode and per-Envoy stats, served by the
`/api/v1` endpoints below and `/metrics`; a web UI, a terminal UI and the CLI all read that API;
`deltav bootstrap` prints the client bootstrap; a test harness (`dvlab`) provides fake S3
backends, a mock VAST API and a load generator. See [docs/DESIGN.md](docs/DESIGN.md).

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

`go build ./cmd/deltav` also works on its own: the web UI's stylesheet is committed. After
changing a page template or `internal/webui/tailwind.css`, run `make css`, which fetches the
pinned Tailwind standalone CLI (no Node) and regenerates it.

## Attach a VAST cluster

```sh
deltav serve --state-dir /var/lib/deltav &          # the state file holds the VAST password, mode 0600

export DELTAV_PASSWORD='...'                         # or: --password-stdin < file
deltav vast add lab --host vast.example.com --user admin
deltav vast pools lab                                # pools, and how their VIPs spread over CNodes
deltav target add s3 --cluster lab --pool main --port 80 --listen-port 9000
deltav target list
```

Every Envoy then listens on `127.0.0.1:9000` and balances across the pool's VIPs that sit on
healthy CNodes. The daemon reads the cluster every 10 s with three GETs; it can issue nothing
but GETs and the token call. If VAST is unreachable, or the daemon restarts, Envoys keep being
served the last known endpoints. TLS verification of the VAST API is on unless the cluster is
added with `--insecure`.

## Limits and drain

```sh
deltav limit set s3 --per-cnode 400                  # alert: report CNodes over 400 connections
deltav limit set s3 --per-cnode 400 --mode enforce   # also cap every Envoy
deltav limit set s3 --per-cnode 0                    # remove the limit
deltav node drain cnode-3-11                         # withhold a CNode's VIPs from every Envoy
deltav node undrain cnode-3-11
```

A limit is the number of connections the whole fleet should hold to any one CNode for a target.
"Connections" are requests in flight, which is what Envoy reports: S3 over HTTP/1.1 holds one
request per connection, and idle keep-alive connections are not counted.

- **alert** (the default) reports CNodes over the limit in the API, the TUI and `/metrics`
  (`deltav_cnode_over_limit`). Nothing is enforced.
- **enforce** also gives each Envoy a cap on its connections to each VIP. Two things to know.
  The cap is each Envoy's own, so the limit is divided by the number of connected Envoys and by
  the most VIPs of the target that one CNode hosts, rounded down but never below 1: CNodes
  hosting fewer VIPs get less than the limit, and once Envoys times VIPs exceeds the limit the
  fleet can exceed it (alerting still reports that). At its cap an Envoy makes requests for that
  VIP wait; it does not send them elsewhere. And changing the cap makes every Envoy reconnect its
  connections for the target, so as Envoys come and go a new cap is pushed only when it differs
  by more than a fifth and not within a minute of the last (`serve --limit-min-change`,
  `--limit-min-interval`). A change you make yourself is pushed at once.

  The cap is exact only for an Envoy started with `--concurrency 1`. Each further worker thread
  can hold one more connection to a VIP than the cap allows, and Envoy starts one worker per
  core unless told otherwise, so on a many-core client a small cap hardly binds. Alert mode
  reports the real numbers either way.

Drain is Delta-V's own setting: nothing changes on the VAST cluster. Envoys stop sending a
drained CNode new requests at once, and requests in flight finish. Limits and drains survive a
daemon restart.

## Watch it

The daemon serves a web UI on its HTTP address, `http://127.0.0.1:18080/` by default. It has four
pages, kept current without reloading:

- **Overview**: Envoys connected and their config state; requests, megabytes and 403/503 share
  across all targets; CNodes over a limit; then a row per target.
- **CNodes**: state as VAST reports it, VIPs hosted, requests in flight against each limit,
  rates, latency and a five-minute sparkline. Open a row for its VIPs.
- **Fleet**: every Envoy with version, last seen, config state (and Envoy's reason if it
  rejected the config) and request rate. Search, filter, sort and paging are done by the daemon,
  so the page is the same size with five thousand Envoys as with five.
- **Controls**: set or remove a target's limit, drain and undrain CNodes, attach and detach
  VAST clusters, add and remove targets.

It follows the system's light or dark setting; the button in the top bar overrides that. From
another machine, tunnel to it: `ssh -L 18080:127.0.0.1:18080 <daemon host>`, then open
`http://localhost:18080/`.

A daemon bound to anything but loopback requires a token (`DELTAV_API_TOKEN`), and the UI then
asks for it once and keeps a 12-hour session cookie. Use a long random token, and put TLS in
front of the daemon if the network is not trusted: it speaks plain HTTP. A daemon with no token
answers only to loopback names (`127.0.0.1`, `localhost`), and in both cases a request that
changes anything is refused if a browser says it comes from another site's page.

```sh
deltav tui                    # live terminal UI
deltav node list --vips       # CNodes with state, VIPs, requests in flight, rates and latency
deltav fleet list --sort request_rate --desc --limit 20
deltav fleet list --state nacked     # Envoys that rejected the config, with Envoy's reason
```

The terminal UI has four views, switched with `1`-`4` or `tab`: **Overview** (per target:
endpoints, requests in flight, request and byte rates, 403s and 503s, latency, a five-minute
sparkline), **CNodes** (the same per CNode; `enter` shows its VIPs), **Fleet** (every Envoy; `/`
filters, `s` sorts, `a` and `c` narrow by config state and connection, `pgup`/`pgdn` page; the
daemon does the paging, so it stays quick with thousands of Envoys) and **Controls** (attached
clusters, targets and CNodes; `e` sets a target's limit, `m` switches it between alert and
enforce, `d` drains or undrains a CNode). `w` cycles the window between 1m, 5m and 1h; `q` quits. It follows the
daemon's event stream and uses the terminal's own colors, so it suits light and dark themes.

All three take `--api` (default `http://127.0.0.1:18080`, or `DELTAV_API`) and, for a daemon
that requires it, `DELTAV_API_TOKEN`. The list commands take `--json`.

## Try it without a VAST cluster

On Linux every `127.x.y.z` address is loopback, so fake backends can share one port the way VIPs do.

```sh
make build envoy                      # also fetches and verifies the pinned Envoy binary
export AWS_ACCESS_KEY_ID=dvlab AWS_SECRET_ACCESS_KEY=dvlab-secret   # any pair; the fake verifies it

bin/dvlab fakes3 --listen 127.0.0.11:9100,127.0.0.12:9100,127.0.0.13:9100 &
bin/deltav serve --state-dir ./state --static-target s3:9000:127.0.0.11:9100,127.0.0.12:9100,127.0.0.13:9100 &
bin/deltav bootstrap --xds-address 127.0.0.1:18000 > bootstrap.json
.cache/envoy/envoy-1.40.2-linux-x86_64 -c bootstrap.json --service-node "$(hostname)" &

curl -s 127.0.0.1:18080/api/v1/fleet  # the Envoy, its version and whether it accepted the config
bin/dvlab loadgen --endpoint http://127.0.0.1:9000 --bucket lab --requests 1000 \
    --special-keys --expect --check   # through Envoy; fails if Host, path, Expect or body changed
```

To try clusters, CNodes, limits and drain as well, put the mock VAST API in front of six fake
backends (it serves a real cluster's sanitized responses; pool `main` has six VIPs on three CNodes):

```sh
IPS=127.0.0.11,127.0.0.12,127.0.0.13,127.0.0.14,127.0.0.15,127.0.0.16
bin/dvlab fakes3 --listen ${IPS//,/:9100,}:9100 &
bin/dvlab mockvast --pool-vips main=$IPS &          # prints its address and the command to attach it
bin/deltav serve --state-dir ./state &
DELTAV_PASSWORD=mock-password bin/deltav vast add mock --host 127.0.0.1:<port it printed> --user admin --insecure
bin/deltav target add s3 --cluster mock --pool main --port 9100 --listen-port 9000
```

Then start Envoy and the load generator as above and open `http://127.0.0.1:18080/`.

`make integration` runs the same arrangement as tests, with real Envoy processes, including one
where a mock VAST (a real cluster's sanitized responses) supplies the endpoints. `make live
ENV=<file>` does it on real equipment: a real VAST cluster, real Envoy, and round trips against a
real bucket; see the Makefile.

## HTTP API and metrics

The API is JSON under `/api/v1` on the HTTP listener (`127.0.0.1:18080` by default; a bearer token
is required when it is bound elsewhere). Stats take `?window=1m|5m|1h`.

| Path | What it returns |
|---|---|
| `/api/v1/overview` | Per target: endpoints served, requests in flight, request and byte rates, status counts with 403 and 503 broken out, p50/p90/p99. Fleet counts. |
| `/api/v1/targets` | The targets and endpoints being served to Envoys, and how each VAST target resolved. `POST` adds one, `DELETE /api/v1/targets/{name}` removes it. |
| `/api/v1/vast` | Attached VAST clusters and the state of their polling. `POST` attaches one (the password is never returned), `DELETE /api/v1/vast/{name}` detaches it; `/api/v1/vast/{name}/pools` lists its VIP pools. |
| `/api/v1/cnodes` | Per CNode: state from VAST, whether it is drained, VIPs hosted, requests in flight against each target's limit, and the same stats, with a row per VIP beneath. |
| `/api/v1/targets/{name}/limit` | `PUT` sets a target's per-CNode limit and mode; `DELETE` removes it. |
| `/api/v1/vast/{cluster}/cnodes/{cnode}/drain` | `POST` drains a CNode; `DELETE` undrains it. |
| `/api/v1/endpoints` | The same stats per endpoint (VIP); `?target=` narrows it. |
| `/api/v1/fleet` | Envoys with version, last seen, config state and stats. `?limit=&offset=`, `?sort=node_id\|request_rate\|last_seen\|connected_at\|envoy_version\|config_state\|address`, `?order=asc\|desc`, `?q=` (substring), `?state=acked\|nacked\|pending`, `?connected=true\|false`. |
| `/api/v1/fleet/{node}` | One Envoy, every window. |
| `/api/v1/events` | Server-sent events: the overview, every `?interval=` (default 2s). |
| `/api/v1/settings` | How the daemon was started, as far as a UI needs it: the enforce-mode debounce, the VAST poll interval, whether a token is required. |

"In flight" is the number of requests Envoys have outstanding to an endpoint, from load reports.
S3 over HTTP/1.1 holds one request per connection, so it is the count of busy connections; idle
keep-alive connections are not included.

`/metrics` exports the same data as cumulative Prometheus series, so one scrape covers the fleet:
`deltav_endpoint_requests_total{target,endpoint,cnode,class}`,
`deltav_endpoint_responses_total{...,code="403"|"503"}`, `deltav_endpoint_bytes_total{...,direction}`,
`deltav_endpoint_request_duration_seconds` (histogram), `deltav_endpoint_in_flight_requests`,
`deltav_endpoint_overflow_total`, `deltav_endpoint_load_issued_total`,
`deltav_endpoint_load_completed_total{...,result}`, `deltav_fleet_members{state}`,
`deltav_fleet_config{state}`, `deltav_target_endpoints{target}`, and for limits and drain
`deltav_target_cnode_limit{target,mode}`, `deltav_target_limit_share{target}`,
`deltav_cnode_in_flight_requests{target,cluster,cnode}`, `deltav_cnode_over_limit{...}` and
`deltav_cnode_drained{cluster,cnode}`. Per-Envoy series
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
