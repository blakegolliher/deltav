# Build prompt: Delta-V

You are building a proof of concept called Delta-V. Delta-v is the budget a spacecraft has for changing course; this tool steers S3 traffic.

Naming, used consistently everywhere:

- Project name in prose and UI titles: Delta-V.
- Binary: `deltav`. The short name `dv` must work identically. Build one binary and have the Makefile and packaging install `dv` as a symlink to it; help and usage text should print whichever name was invoked. Examples in this prompt use `deltav`, and every one is equally valid as `dv`.
- Go module and repo name: `deltav`.
- Environment variable prefix `DELTAV_`, Prometheus metric prefix `deltav_`, systemd unit `deltav.service`, default state directory `/var/lib/deltav`.

## What this is and why

VAST Data customers run S3 clients against a VAST cluster. We want client-side load balancing with no proxy machines in the data path. Each client host runs a stock Envoy bound to loopback; the S3 application points at `127.0.0.1:<port>`; Envoy spreads requests across the cluster's S3 endpoints directly.

`deltav` is the control plane for that fleet of Envoys. It reads healthy endpoints from the VAST REST API, serves them to every Envoy over xDS, collects telemetry back from every Envoy, and shows the operator what the fleet and the storage nodes are doing. The fleet may be dozens, hundreds or thousands of hosts, so nothing may depend on scraping or SSHing to client hosts. Everything flows over streams that Envoy opens to the control plane.

This is a POC. Favor a small amount of clear, working code over generality. Where this prompt makes a design choice, follow it unless you find it is wrong; if so, say why and propose the alternative before changing course.

## Deliverable

One Go binary, `deltav`, that is both the service and its client:

- `deltav serve` runs the daemon: xDS server, telemetry ingest, VAST pollers, HTTP API, embedded web UI, Prometheus `/metrics`.
- `deltav tui` is a terminal UI (Bubble Tea and Lip Gloss) that talks to a running daemon over its HTTP API.
- `deltav vast add|list|remove` attaches the daemon to one or several VAST clusters.
- `deltav target add|list|remove` defines what Envoys balance across (see Domain model).
- `deltav fleet list`, `deltav node list`, `deltav limit set`, `deltav node drain|undrain` inspect and control.
- `deltav bootstrap` prints an Envoy bootstrap file and a systemd unit for a client host.

The CLI, the TUI and the web UI are three clients of one HTTP API. Do not give any of them a private path into daemon state.

The client hosts run unmodified upstream Envoy plus the generated bootstrap. There is no custom agent on client hosts.

## Domain model

- **VAST cluster**: a management endpoint plus credentials.
- **Target**: one VAST cluster, one VIP pool, a port, and whether upstream TLS is used. Each target becomes one Envoy cluster and one loopback listener port on every client.
- **Endpoint**: a VIP from the target's VIP pool. S3 clients connect to VIPs, not to CNode IPs, and VIPs move between CNodes. Envoy balances across VIPs.
- **CNode**: the storage node currently hosting a VIP. Operators think in CNodes, so the control plane keeps a VIP-to-CNode map from the VAST API and joins telemetry to CNodes on its side. Show both: per-CNode rollups, with per-VIP rows beneath.
- **Fleet member**: one Envoy, identified by its xDS node ID (default to hostname).

For the POC every Envoy receives every target, each on its own local port.

## The VAST API

The file `docs/vast-5.3.openapi.json` is a Swagger 2.0 excerpt of the VAST REST API. I will place it in the repo. Read it before writing the client. What it establishes:

- Base path `/api`, HTTPS, bearer token in the `Authorization` header.
- `POST /token/` with `username` and `password` returns `access` and `refresh`. `POST /token/refresh/` with `refresh` returns a new `access`. `POST /token/{tenant_name}` is the tenant-scoped form.
- The `CNode` object carries `id`, `name`, `hostname`, `ip`, `ip1`, `ip2`, `mgmt_ip`, `state`, `display_state`, `enabled`, `is_mgmt`, `is_leader`, `build`, `os_version`.

What it does not establish: the excerpt has 14 paths and contains no list endpoint for CNodes, and nothing for VIP pools or VIP-to-CNode assignment. You need three read operations: list CNodes with state, list VIP pools, and map each VIP to its current CNode. I expect these at `GET /cnodes/`, `GET /vippools/` and `GET /vips/`, but that is unverified.

Handle this as follows:

1. Put all VAST access behind a small Go interface (`ListCNodes`, `ListVIPPools`, `ListVIPs`).
2. If I provide live cluster access (`VAST_HOST`, `VAST_USER`, `VAST_PASSWORD` in the environment), probe those endpoints with GETs, save sanitized responses under `testdata/` as fixtures, and build the client against the real shapes.
3. If you have no live access, build against a mock VAST server with fixtures you mark clearly as assumed, and list every assumption in `docs/ASSUMPTIONS.md`.
4. Never invent a field or endpoint and present it as real. If a needed endpoint is absent on the live cluster, stop and tell me.

Hard rule: against a real VAST, `deltav` issues only token calls and GETs. It must never call `add_cnodes`, `control_led`, `highlight`, or any other mutating endpoint. Enforce this in the HTTP client, not by convention.

Credentials: accept from flags, environment or a prompt; store in a state file with mode 0600; never log them or return them from the API. TLS verification is on by default, with a per-cluster opt-out for lab clusters with self-signed certificates.

## Control plane design

Use `github.com/envoyproxy/go-control-plane` with its snapshot cache, serving ADS over gRPC.

**Config pushed to each Envoy**

- One listener per target on `127.0.0.1:<port>` with the HTTP connection manager and router.
- One cluster per target, type EDS, load balancing `LEAST_REQUEST`.
- Endpoints from the target's healthy VIPs. A drained CNode's VIPs are removed.
- Outlier detection on consecutive 5xx and gateway failures, with conservative ejection limits so a bad deploy cannot eject the whole pool.
- Active health check, configurable per target, defaulting to a TCP connect check. Do not assume a VAST S3 health URL exists.

**S3 correctness. These matter; get them wrong and signatures or large transfers break.**

- Do not rewrite the `Host` header. SigV4 signs it, and the server must see what the client signed.
- Document that clients must use path-style addressing, since the host is a loopback address.
- Disable the route timeout (Envoy's default is 15 seconds) and set generous idle timeouts. Multi-gigabyte PUTs and GETs must not be cut off.
- Do not normalize paths or merge slashes. Object keys may contain `//` and encoded characters.
- Enable `proxy_100_continue`. S3 SDKs send `Expect: 100-continue`.
- No request or response buffering filters. Stream both directions.
- No automatic retries on non-idempotent requests.

**Telemetry in. Implement both gRPC services in the daemon.**

- Access Log Service (`envoy.access_loggers.http_grpc`): one entry per request with upstream host, response code, response flags, duration, bytes in and out. Aggregate per endpoint and per fleet member: request rate, status counts with 403 and 503 broken out explicitly, latency p50, p90 and p99, bytes per second each way.
- Load Reporting Service: respond with `report_endpoint_granularity: true` and consume per-endpoint in-progress, success, error and issued counts. Treat in-progress requests as the connection count per endpoint. S3 over HTTP/1.1 holds one request per connection.

Keep metrics in memory in rolling windows (1 minute, 5 minutes, 1 hour). No external database. Expose the same aggregates at `/metrics` in Prometheus format so one scrape target covers the whole fleet.

**Fleet registry.** From xDS stream callbacks record node ID, Envoy version, connect time, last seen, config version, and whether the last push was ACKed or NACKed with the error text.

**Connection limit per CNode.** Adjustable per target, with two modes:

- `alert` (default): compare fleet-total connections per CNode with the limit and flag breaches in the API, UI, TUI and metrics. Nothing is enforced.
- `enforce`: push Envoy `circuit_breakers.per_host_thresholds` with `max_connections`. Two facts constrain this. The threshold is local to each Envoy, so compute each Envoy's share as the limit divided by the number of connected Envoys for that target, minimum 1. And the threshold lives on the Cluster resource, where an update drains and reconnects connection pools, so debounce: only push when the computed share changes by more than a set fraction, and never more often than a set interval. Explain both in the UI next to the control.

## Interfaces

**HTTP API.** JSON, versioned under `/api/v1`, plus a server-sent events stream for live updates. Binds to `127.0.0.1` by default. When bound to another address, require a bearer token.

**Web UI.** Embedded with `go:embed` and served by the daemon. Tailwind and modern CSS. No Node runtime and no Node build step required to compile the binary: use the Tailwind standalone CLI from the Makefile and commit the generated CSS so `go build` works alone. Server-rendered Go templates with htmx and SSE are the default; a small SPA is acceptable only if you justify it. Works in light and dark mode. Pages:

- Overview: per target, healthy endpoints, fleet size, total request rate, throughput, error rates, limit breaches.
- CNodes: state from VAST, VIPs hosted, connections against the limit, requests per second, MB per second, p50 and p99, 403 and 503 rates, sparklines. Expand a row for per-VIP detail.
- Fleet: every Envoy with version, last seen, config status, request rate. Must stay usable at several thousand rows: server-side paging, sort and filter.
- Controls: limit and mode per target, drain and undrain a CNode, VAST clusters and targets.

**TUI.** Bubble Tea and Lip Gloss. Same four views, keyboard driven, live updating from the SSE stream. Use the current stable major version of each library and check its docs rather than relying on memory.

## Testing without a VAST

Build a local harness so every milestone is verifiable on a laptop:

- A mock VAST API server driven by fixtures.
- A fake S3 backend: several tiny HTTP servers with configurable latency and a configurable rate of 403 and 503 responses.
- Real Envoy, via docker compose or a downloaded release binary. Do not mock Envoy.
- A load generator that sends path-style requests through the loopback listener.

Write integration tests that start the harness and assert on the API output. Unit-test the aggregator and the limit-share calculation.

## Milestones

Work in this order. At each one, run the tests, commit, and give me a short status before continuing.

1. Scaffold, dependency license audit, and a one-page `docs/DESIGN.md` confirming or amending this design.
2. Static endpoint list served over xDS to one real Envoy; a request through the listener reaches the fake S3 backend with the `Host` header intact.
3. Access log and load report ingest, the aggregator, `/api/v1`, and `/metrics`.
4. VAST client and poller against the mock, then against the live cluster if I have provided access.
5. Web UI.
6. CLI commands and the TUI.
7. Limit in alert and enforce modes; CNode drain.
8. `deltav bootstrap`, systemd units for daemon and client, and a README with a ten-minute quick start.

## Licensing and attribution

This may be shipped to customers by a storage vendor, so license hygiene is a requirement.

- Before adding any dependency, check its license. Permitted: Apache-2.0, MIT, BSD, ISC, MPL-2.0. Not permitted: GPL, AGPL, LGPL, SSPL, BSL, or anything unlicensed. If something you want falls outside the permitted list, ask me.
- Maintain `THIRD_PARTY_LICENSES.md` listing every dependency, its version, license and copyright holder. Preserve upstream LICENSE and NOTICE files where the license requires it, and note any file you modified from an upstream source.
- Do not copy or adapt code from MinIO sidekick (AGPL) or from service mesh control planes. Implement from the Envoy API documentation.

## Ground rules

- Verify Envoy API fields against the current Envoy v3 API docs and the go-control-plane version you pin. Field names and defaults change between releases; do not rely on memory.
- Pin Envoy to one recent stable release and record it in the README.
- Ask me before: adding a database, adding authentication beyond the bearer token, changing the single-binary constraint, or adding any VAST API call that is not a GET or a token call.
- If you are blocked or an assumption in this prompt turns out false, stop and say so. A clear report of what did not work is more useful than a workaround that hides it.
- Out of scope for the POC: mutual TLS on the xDS and telemetry streams, high availability for the daemon, per-group targeting of Envoys, long-term metric storage. List these in the README as known gaps.
