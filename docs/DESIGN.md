# Delta-V design

Milestone 1. This confirms the build prompt's design except for the amendments below. Each
amendment was checked against the Envoy v1.40 API protos (go-control-plane `envoy` v1.40.0)
or the Envoy v1.40.2 source. Items marked **[confirm]** need your OK before I build them.

**Pins.** Envoy 1.40.2 · go-control-plane v0.14.0 + `envoy` API v1.40.0 · Go ≥ 1.26 (the API
module requires it; `go.mod` selects toolchain go1.27.4, which `go build` fetches on its own) ·
cobra 1.10 · Prometheus client 1.25 · Bubble Tea / Lip Gloss v2.1.1, Bubbles v2.2.1 (`charm.land`)
· htmx 4.0.1 + its `hx-sse` extension, vendored · Tailwind standalone CLI 4.3.5.

## Confirmed shape

- One binary with two listeners. **gRPC** (`:18000`) carries everything Envoy opens: ADS, access
  log (ALS) and load reporting (LRS). **HTTP** (`127.0.0.1:18080`) carries `/api/v1`, SSE, the
  web UI and `/metrics`. The CLI, TUI and UI use only `/api/v1`.
- Every Envoy gets the same snapshot. The fleet registry comes from ADS stream callbacks. If the
  daemon goes down, Envoys keep their last accepted config, so traffic keeps flowing; only
  telemetry is lost until it returns.
- State: one JSON file in the state directory, mode 0600, written atomically (temp file and
  rename). Metrics: in memory, in 10 s slots of fixed log-bucket histograms that merge into
  1 m / 5 m / 1 h windows, one rollup per endpoint and one per fleet member (not the
  cross-product). No dependency is needed for this.
- VAST access: a read-only `RoundTripper` allows only `GET` and the `POST`s to `/api/token/...`.
  Because it sits in the transport, redirected requests are checked too.

## Amendments

1. **The limit share must account for VIPs per CNode.** `per_host_thresholds` applies per
   *endpoint* (per VIP), with one value per cluster. A CNode hosting *k* VIPs therefore gets
   *k* × the threshold from each Envoy. Share = `max(1, floor(L / (N × k_max)))`, where *k_max* is
   the largest number of the target's VIPs on any one CNode. Two consequences, both explained in
   the UI: CNodes hosting fewer VIPs are under-used, and once N × k_max > L, the minimum of 1 lets
   the fleet exceed L (alert mode still flags this). At the threshold, Envoy queues requests and
   then returns 503 with flag `UO`. `LEAST_REQUEST` does not route around a full host.
2. **Set explicit cluster circuit breakers.** By default Envoy allows 1024 connections, pending
   requests and requests per cluster per Envoy. That would silently cap a busy client. Set high
   defaults, configurable per target.
3. **Outlier detection never fires on small pools as specified.** Envoy ejects only if
   `(ejected+1)/hosts ≤ max_ejection_percent`. With the default 10%, a pool of fewer than 10 VIPs
   never ejects anything. Set `max_ejection_percent: 34` (at most a third of the pool) and
   `always_eject_one_host: true`. Also set `split_external_local_origin_errors`, so connect
   failures and resets eject on their own counter. Risk: S3 `503 SlowDown` is backpressure, and
   ejecting on it shifts load onto the remaining VIPs. Use 5 consecutive errors and a short base
   ejection time, and revisit once we see VAST's behavior.
4. **Drain needs `ignore_health_on_host_removal: true`.** With active health checks on, Envoy
   keeps an endpoint removed from EDS until it fails a check. A drained CNode's VIPs still pass TCP
   checks, so they would keep taking traffic. In-flight transfers still finish.
5. **Health checks are a fleet-scale load.** Every Envoy checks every VIP: 1,000 Envoys × 16 VIPs
   at 5 s is 3,200 TCP connects/s to the cluster. Default to 15 s with jitter and keep the 60 s
   `no_traffic_interval`. VAST's own CNode state is the primary signal; health checks are backup.
6. **Upstream TLS needs an explicit SNI and CA.** Envoy verifies upstream certificates only when
   it has a validation context, and `auto_sni` would send the client's Host (`127.0.0.1`). Each TLS
   target gets a server name and a CA bundle, with verification on by default and a per-target
   opt-out (as for the VAST API).
7. **LRS "connections" are in-flight requests.** In the Envoy source, `total_requests_in_progress`
   is the per-host `rq_active` gauge. Idle keep-alive sockets are invisible to it, so it is labeled
   that way. The upstream idle timeout drops from 1 h to 5 min to bound the sockets it can't count.
   `per_endpoint_stats` (which would report real `cx_active`) can't be combined with LRS.
8. **ALS volume and timing.** One entry per request means 1,000 hosts × 1,000 req/s = 1 M
   entries/s into one process. For the POC: unsampled by default, ingest rate measured with the load
   generator, and a per-target sampling knob (exact counts then come from LRS). Bytes are counted
   when a request completes, so a 10 GB GET appears all at once and the 1 m throughput window spikes
   for large objects. If that matters, Envoy's `access_log_flush_interval` can emit periodic entries
   (deltas keyed by stream ID); off for now.
9. **Web UI auth off loopback [confirm].** A browser's `EventSource` cannot send an
   `Authorization` header. Proposal: a one-field form trades the *same* bearer token for an
   `HttpOnly`, `SameSite=Strict` cookie, and mutating requests also check `Origin`. This adds no new
   credential, but it touches your "ask before adding auth" rule.
10. **S3 path, header and timeout settings, stated explicitly.** Route `timeout: 0s`. Stream idle
    timeout 1 h. `normalize_path: false`, `merge_slashes: false` and
    `path_with_escaped_slashes_action: KEEP_UNCHANGED`, all set explicitly because the defaults
    can be overridden at runtime. `proxy_100_continue: true` (without it, Envoy strips `Expect`,
    which breaks a signature that covers it). Router `suppress_envoy_headers: true`. No retry policy
    at all: SDKs retry, and stacked retries amplify load.
11. **One bootstrap for the whole fleet.** The client systemd unit passes `--service-node %H`, so
    the node ID is the hostname and the bootstrap file has nothing host-specific.
12. **CDS pushes re-warm clusters.** Each snapshot bumps every resource version, so a warming
    cluster gets EDS immediately. Envoy's EDS cache, used only after the 15 s
    `initial_fetch_timeout`, is the fallback. The old cluster serves while the new one warms; this is
    verified in milestone 7.
13. **Prometheus semantics.** `/metrics` exports cumulative counters and histograms (rolling windows
    stay in the JSON API). Series per target, VIP and CNode are on; series per fleet member are off by
    default, since thousands of hosts would be too many.

## Known gaps (POC)

mTLS on the xDS and telemetry streams; daemon HA; per-group targeting of Envoys; long-term metric
storage. The gRPC port is unauthenticated, so anything that can reach it can read config (VIP
lists) and inject telemetry, but it cannot change any other Envoy's config.
