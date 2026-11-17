# Delta-V design

Milestone 1. This confirms the build prompt's design except for the amendments below. Each
amendment was checked against the Envoy v1.40 API protos (go-control-plane `envoy` v1.40.0)
or the Envoy v1.40.2 source. Decisions recorded 2026-11-17 are marked **[decided]**.

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
  rename). Metrics: in memory, in slots of fixed log-bucket histograms that add up to the
  1 m / 5 m / 1 h windows, one rollup per endpoint and one per fleet member (not the
  cross-product). No dependency is needed for this. Amendment 18 has the details.
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
6. **Upstream TLS needs an explicit SNI and trust source.** Envoy verifies upstream certificates
   only when it has a validation context, and `auto_sni` would send the client's Host
   (`127.0.0.1`). Each TLS target gets a server name, with verification on by default and a
   per-target opt-out (as for the VAST API). Trust comes from a CA file if the target names one,
   otherwise from the client host's own trust store (`system_root_certs`): no single bundle path
   exists on both Debian and RHEL families. *Not yet exercised against a real Envoy.*
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
9. **Web UI auth off loopback [decided: approved].** A browser's `EventSource` cannot send an
   `Authorization` header. Proposal: a one-field form trades the *same* bearer token for an
   `HttpOnly`, `SameSite=Strict` cookie, and mutating requests also check `Origin`. No new
   credential type.
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
    verified in milestone 7. Amendment 15 says how the push order this relies on is kept.
13. **Prometheus semantics.** `/metrics` exports cumulative counters and histograms (rolling windows
    stay in the JSON API). Series per target, VIP and CNode are on; series per fleet member are off by
    default, since thousands of hosts would be too many.

### Found while building milestone 2

Amendments 14 to 16 come from reading go-control-plane v0.14.0 and from protocol tests that play
Envoy's side of the ADS stream (`internal/server/ads_test.go`); 17 follows from amendment 3's
settings. The integration tests (`make integration`, real Envoy 1.40.2) confirm 14 and 15: a
rejected cluster is reported once with Envoy's reason, is not re-sent, and endpoint updates keep
flowing. They also show that Envoy adds exactly one request header, `x-forwarded-proto: http`. It
is unsigned, so SigV4 is unaffected; whether VAST reacts to it is to be checked on the live cluster.

14. **A rejected config must not be re-sent.** A NACK carries the version Envoy last accepted. The
    snapshot cache has no notion of a NACK: it sees a version that differs from the snapshot's and
    answers at once with the same resources, which Envoy rejects again, in a tight loop per Envoy.
    A callback (`xds.withNACKGuard`) rewrites a NACK's version to the one it rejected, so the cache
    waits for the next change. The fleet registry shows the NACK and Envoy's reason until then.
15. **The cache's ADS mode is off; push order is kept another way.** In ADS mode the cache
    withholds an EDS response, and drops the watch, whenever the snapshot holds endpoints the Envoy
    has not asked for. An Envoy that rejects one target's cluster never asks for that cluster's
    endpoints, so it would get no endpoint updates for any other target until it reconnected.
    Instead `xds.Server` publishes each version one type at a time (clusters, endpoints,
    listeners), and the server runs with `WithOrderedADS`; by default it reads each type from its
    own channel in random order. The order is best effort: a stream with an ACK in flight is
    answered when the ACK lands. So routes set `validate_clusters: false`; a listener that arrives
    before its cluster answers 503 for that moment instead of being rejected for good.
16. **Config versions are unique to a daemon run** (`<start time>.<counter>`). A reconnecting Envoy
    reports the version it holds and is sent config only if the daemon's differs, so a counter
    that restarted at 1 could leave Envoys on stale config after a daemon restart. An update whose
    rendered resources are identical to the current ones is not pushed at all.
17. **503s can eject the only path to a backend.** Amendment 3's risk is concrete: five consecutive
    503s from one VIP eject it. The harness's refusing backend therefore answers 403. It has also
    been seen: in one integration run a backend answering a random 15% of requests with 503 took
    17 requests and then nothing for the rest of the test, where the same backend had taken about
    600 in each of two earlier runs. Refusals return faster than successes, so they bunch up in
    completion order, and a run of five comes sooner than the rate suggests. A VIP that sheds load
    with `503 SlowDown` would be ejected the same way, pushing its load onto the others. Whether
    503 should count toward ejection is still open, and now has a reason to be decided before
    this meets a busy cluster.

### Milestone 3: telemetry

18. **Rolling windows, as built.** Each series keeps 10 s slots for five minutes and 1 min slots
    for an hour; the 1 m and 5 m windows sum the first, the 1 h window the second. A window is its
    whole slots plus the elapsed part of the current one, and is capped at the daemon's age, so
    rates are true from the first minute. Latency buckets grow by 2^(1/3) from 100 µs to 8.8 min,
    so percentiles are good to about ±13%. A series that is busy for a whole hour holds about
    35 KB; 5,000 busy Envoys are about 175 MB. Entries are counted when they arrive, not when the
    request ended (Envoy flushes every second), which avoids trusting client clocks.
19. **Requests with no endpoint are kept apart.** A request Envoy answers itself (no healthy host,
    circuit breaker overflow) has no upstream address. It is counted under the target with an
    empty endpoint, so it shows in the target's totals without being blamed on a VIP.
20. **Load reports replace, not add; and they start late.** Envoy leaves idle endpoints out of a
    report, so each report is taken as that Envoy's whole picture: an endpoint missing from it has
    nothing in flight. Counts go when the reporting stream closes, or after two minutes without a
    report. The request totals in load reports are independent of access logs and matched the
    backends exactly in the integration test, with one exception that is Envoy's design: it
    starts tracking a cluster at the first report tick after the cluster exists and resets the
    cluster's counters as it does (`load_stats_reporter_impl.cc`: "for any newly created cluster,
    we will always drop the data for the initial load report period"). So for up to one reporting
    interval (5 s by default) after an Envoy starts or first learns a target, its requests are
    missing from load-report totals and from the in-flight count. Access-log counts are not
    affected. Milestone 7's limit must tolerate that blind interval.
21. **Not done yet in this area.** The per-target access-log sampling knob (amendment 8) waits for
    target management in milestone 6. Ingest cost is measured for the aggregator alone (about
    125 ns per entry on the dev host); the rate through gRPC from a real fleet is not measured.

### Milestone 4: VAST

22. **The VAST API as found** (5.5.3; sanitized responses in `testdata/vast/v5.5.3`). `GET
    /api/cnodes/`, `/api/vippools/` and `/api/vips/` each return one unpaginated array. A VIP names
    its pool (`vippool`) and its current CNode (`cnode`) by name; that is the VIP-to-CNode map.
    Pools belong to tenants, so a target names a pool and, only if two tenants share the name, a
    tenant. A CNode is healthy if `enabled` and `state == "ACTIVE"`; the VIPs of any other CNode
    are left out. What this takes on trust is listed in `docs/ASSUMPTIONS.md`.
23. **An unreachable VAST changes nothing.** A failed poll keeps the last good answer, and the
    API shows the error and the age of the data. A target that suddenly resolves to no endpoints
    is not believed until three polls in a row agree: a pool does not normally lose every VIP at
    once, and Envoy's health checks already cover endpoints that are really gone.
24. **A restart changes nothing either.** The endpoints served are cached beside the state file,
    and the first snapshot is built from that cache before any listener opens. Without it, an
    Envoy reconnecting to a restarted daemon would be handed a config with its targets missing
    and would drop its listeners until the first poll. A target with neither a cache nor a
    successful poll is not served at all, not served empty.
25. **Per-CNode numbers follow the VIP.** Telemetry is per VIP; the CNode view groups VIPs by
    their current host and merges their histograms before taking percentiles. After a VIP moves,
    its whole window counts toward the CNode that has it now.
26. **Credentials.** A cluster is read once before it is stored, so wrong credentials or an
    untrusted certificate fail at `vast add`. The password is written to the state file (mode
    0600, replaced atomically) and is never returned by the API, logged, or put in an error.

### Milestone 6: CLI and TUI

27. **The TUI is driven by the event stream.** Each server-sent event carries the one-minute
    overview and prompts a fresh read of whatever the current view shows, so there is one clock
    (the daemon's) and no polling loop in the client. The fleet view asks the daemon for one
    screen of rows at a time, sorted and filtered there. Colors are the terminal's own sixteen
    plus bold, faint and reverse, so the UI follows the terminal's theme. The Controls view
    lists clusters and targets; changing limits and draining CNodes arrive with milestone 7.
    The order of the remaining milestones was changed by the user on 2026-11-17 to 6, 7, 5, 8.
28. **Sparklines** come from the 10-second slots the aggregator already keeps: the request rate
    in each of the last 29 complete slots, per target and per CNode, in the overview and CNode
    responses (`rate_history`).

## Known gaps (POC)

mTLS on the xDS and telemetry streams; daemon HA; per-group targeting of Envoys; long-term metric
storage. The gRPC port is unauthenticated, so anything that can reach it can read config (VIP
lists) and inject telemetry, but it cannot change any other Envoy's config.
