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
   exists on both Debian and RHEL families. *Not yet exercised against a real Envoy.* (It was
   on 2026-11-19, and the trust-store half was wrong: see 70 and 71.)
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

### Milestone 7: limits and drain

29. **Over the limit is reported in both modes.** For each target with a limit the daemon adds up
    the fleet's requests in flight to that target's VIPs on each CNode and compares the sum with
    the limit. Alert mode stops there. Enforce mode reports too, because its share cannot go
    below 1 and the fleet can then exceed the limit (amendment 1). An Envoy's first load-report
    interval for a target is missing from the sums (amendment 20).
30. **The enforced share, as built.** `limit.Share` is amendment 1's formula with N the Envoys
    connected now and *k_max* taken over the endpoints being served. A recomputed share is pushed
    only if it differs from the one in force by more than a fifth and at least a minute after the
    last push; turning enforcement on or off, and any change an operator makes to the limit, is
    pushed at once. The API shows both the share in force and the one computed, so a held-back
    change is visible. While a fleet is being rolled out the share therefore lags by up to the
    interval; alerting covers that.
31. **A restart keeps the share.** The first snapshot after a restart is built before any Envoy
    has connected. Worked out afresh it would be the whole limit per Envoy, loosening every limit
    until the fleet was back and the interval had passed. The share in force is cached with the
    endpoints (amendment 24) and restored, and the debounce interval starts again at startup.
32. **Drain is immediate, and local.** A drained CNode's VIPs are left out of every target of its
    cluster. If that empties a target it does so at once: the three-poll hold of amendment 23 is
    for odd answers from VAST, not for an operator's decision. Nothing is changed on VAST, and the
    name must be a CNode the cluster has, so a typo cannot sit in the state doing nothing.
33. **Seen against real Envoy** (2026-11-18, Envoy 1.40.2). At its cap an Envoy makes requests
    wait and fails none. Removing the cap under load, which rebuilds Envoy's cluster (amendment
    12), fails nothing. Draining a CNode under load fails nothing. The cap itself holds only as
    far as amendment 34 says.
34. **The cap leaks by one connection per extra worker thread.** Envoy counts a host's
    connections across its worker threads, but a worker whose own pool to that host is empty may
    always open one, whatever the count ("always create one, to prevent pending streams being
    queued to this upstream with no way to be processed": `tryCreateNewConnection` in
    `source/common/conn_pool/conn_pool_base.cc`). An Envoy with *W* workers can therefore hold
    share + *W* − 1 connections to a VIP. Observed with a share of 3: one worker, 3 at most on
    every backend; two workers, 4 on some. So enforce mode is exact only for Envoys started with
    `--concurrency 1`. Envoy's default is one worker per core, and on a many-core client a small
    share then hardly binds. Alert mode counts what really happens either way. Amendment 40 is
    what was done about it.

### Milestone 5: web UI

35. **The web UI is an API client that happens to live in the daemon.** `internal/webui` is handed
    the API's `http.Handler` and nothing else, and calls it in process through the typed client
    the TUI uses over the network (`internal/apiclient`). A page cannot show or change anything
    the API does not offer. What the UI needed and the API lacked went into the API:
    `GET /api/v1/settings` (the enforce-mode debounce, so the page can state it).
36. **Pages are rendered on the server and kept current by a stream of markup.** Each page has one
    live region. `GET /live/<page>`, with the page's own parameters, renders that region again
    every 2 s and sends it as a server-sent event; htmx 4's SSE extension merges it into the page
    (a morph, which leaves focus, scroll position and half-typed form fields alone; forms are
    excluded from the merge altogether). Markup that has not changed is not sent. Window, sort,
    filter, page and opened rows are URL parameters: every view is a link, works without script,
    and the daemon decides what a page of the fleet is. The stream is the UI's own and not
    `/api/v1/events`, because htmx swaps markup and what is rendered depends on the page's
    parameters. Forms post to `/controls/...`, which make one API call each and answer with the
    page.
37. **A browser changes the threat model, so the HTTP listener has a guard** (`internal/server/
    auth.go`). (a) The login form of amendment 9 trades the token for a cookie that holds an
    expiry time and an HMAC of it keyed with the token: `HttpOnly`, `SameSite=Strict`, 12 hours,
    no session state in the daemon, void when the token changes. (b) A request that changes
    anything is refused if the browser says it comes from another origin (`Origin`,
    `Sec-Fetch-Site`). This also closes a hole older than the UI: with no token, any page open
    in a browser on the daemon's host could post to the loopback API, and a drain needs no body.
    (c) With no token the `Host` header must name loopback, which stops DNS rebinding. (d) The
    pages forbid inline script and style and anything from another origin (CSP). Names that
    reach a page from a VAST cluster or an Envoy are escaped by `html/template`. (e) The event
    stream format ends a line at a bare CR as well as at LF; line ends in the markup are
    normalised before framing, so a CR in such a name cannot forge an event. The gRPC port is
    unauthenticated (see Known gaps), so those names must be assumed hostile.
38. **Charts.** One data color; states are an icon and a word, never a color alone; a sparkline
    is the request rate in the last 29 ten-second slots, scaled to its own peak, with the range
    in its accessible label and the current figures in the cells beside it; a meter shows
    requests in flight against a limit and changes color at 80% and above 100%; numbers in
    columns are tabular. Light and dark values are chosen per role with `light-dark()`.
39. **Not yet seen in a browser.** The session that built the UI could not run one. Its tests
    cover the markup (well formed, IDs unique, every class has a style rule), the forms, the live
    stream, the login and the guards, and the whole stack was driven with real Envoy under load
    through `curl`. htmx's part (merging events, partial page swaps, confirmation prompts, the
    redirect to the login form when a session ends) was checked by reading the pinned htmx 4.0.1
    source, not by running it. Nobody has looked at the rendering.

### Milestone 8: packaging

40. **The share allows for worker threads, and each Envoy says how many it has.** Envoy does not
    report its thread count. The client unit therefore passes one variable,
    `DELTAV_ENVOY_WORKERS`, twice: as `--concurrency`, and as node metadata (`deltav_workers`)
    through `--config-yaml`, which Envoy merges into the shared bootstrap. The daemon takes the
    largest count any connected Envoy declares, *W*, and serves the threshold
    `max(1, floor(L / (N × k_max)) − (W − 1))`, so that threshold + *W* − 1, the most an Envoy
    can hold to a VIP, is the per-Envoy, per-VIP part of the limit. When N × k_max × W exceeds L
    no threshold can hold the limit; the status says so (`holds`). Envoys that declare nothing
    are counted (`undeclared_workers`) and treated as single-threaded, there being nothing else
    to go on. Seen against real Envoy started with the unit's own command line: limit 6 per
    CNode, two workers, threshold 2, and no backend above 3; the same without the declaration,
    threshold 3, and backends at 4. The unit's default of 2 workers is the one Istio uses for
    its sidecars; **it is a guess about the traffic one client carries, and the user's to set.**
41. **The units.** `deltav.service` runs the daemon under `DynamicUser` with `StateDirectory`
    as its only writable path. `deltav-envoy.service` runs Envoy with `--service-node %H`
    (amendment 11) and restarts it always. Both are files in `packaging/systemd`, installed by
    `make install` and printed by `deltav unit`, so the ones on a host can match its binary. The
    integration tests take Envoy's arguments from the client unit itself, filling in its
    variables as systemd does; systemd 252 was seen to expand the variable inside the quoted
    `--config-yaml` argument into one argument (a throwaway `systemd-run --user` of `printf`).
    Not tested: the units themselves under systemd. `systemd-analyze verify` accepts them, and
    nothing was installed on the dev host.
42. **Envoy's admin interface is off unless asked for.** `deltav bootstrap` used to give it
    `127.0.0.1:9901`. It has no authentication, and on a shared client host any local user
    could stop Envoy or read its config through it. `--admin-address` turns it on.

43. **The TUI at 80 by 24.** A table that does not fit the terminal leaves out whole columns,
    the least needed first (sparkline, byte rates, p50, addresses), and names them under the
    table; it used to cut every row at the right edge, which is where the limit and the failure
    counts are. Sentences, key hints and questions wrap, and a question's answer keys move to a
    line of their own before they would be cut off. The notes on limits, which do not fit under
    the Controls tables on a short terminal, have a screen of their own (`?`). The Fleet view
    shows each Envoy's declared worker threads. Checked by running the binary in tmux against
    the daemon with three real Envoys under load, at 80×24 and 150×40.

### After the milestones: request-rate rules

Asked for by the user on 2026-11-18: "rate limiting like tollbooth". Not in the build prompt.

44. **The rules are tollbooth's; the rate is the fleet's.** A rule's vocabulary and semantics
    are tollbooth 0.1.0's: filters on operation, access key, bucket and prefix; a limit per
    second, minute or hour with a burst, or `blocked`; `per`; `respond` 503 or 429; every
    matching rule must allow; the same table decides which S3 operation a request is. What
    cannot be the same is the division. Tollbooth's nodes sit behind a balancer that spreads a
    tenant's traffic evenly, so each node enforces `rate / node_count`. Here an Envoy sits on
    every client host, and an access key is typically busy on a few of them: an even split would
    give each a sliver. So a rule's rate belongs to the whole fleet and is divided by demand
    (amendment 46). `per: client` gives each client host the whole rate; it stands where
    tollbooth has `source_ip`, which means nothing when the source is always loopback.
45. **How a request is limited** (as first built; replaced by 52 to 54). In each listener,
    ahead of the router: a Lua filter, then one
    rate limit quota filter per rule (`internal/xds/ratelimit.go`). The Lua script is generated
    from the rules (`internal/ratelimit/lua.go`): it finds the operation, the access key, the
    bucket and the key, decides which rules match, and for each sets a request header naming
    what that rule counts the request under. A rule's quota filter matches requests that carry
    its header and counts them in the bucket `{rule, key}`. One filter per rule because a quota
    filter puts a request in one bucket only, and every matching rule must allow. The headers
    are removed before VAST; any a client sends are dropped first, so a client cannot pick its
    own count. Rule values reach the script as byte-escaped literals. A script error lets the
    request through.
46. **How the rate is divided** (the idle share and the last sentence are replaced by 59 and
    60). Envoy's quota filter reports, per bucket, requests allowed and
    refused, and takes a token bucket in return, over one gRPC stream to the daemon
    (`internal/ratelimit/server.go`). No request waits on it. For each bucket the daemon
    divides the rule's rate among the Envoys that have it open: what each asks for (allowed
    plus refused per second) while that fits, an even split among the largest askers when it
    does not, and for an Envoy that asks for nothing a quarter of an even share, so its next
    request is not refused for want of any (`Divide`, max-min fair). The burst is divided in
    proportion. A share that changes by more than a tenth is pushed without waiting for that
    Envoy's next report.
47. **Envoy 1.40 as found** (source read at v1.40.2; the second and third were also hit).
    Usage is reported every 5 s whatever `reporting_interval` says, so that is how fast shares
    follow demand. A token bucket's `fill_interval` is read in whole seconds: half a second
    becomes zero and the rate unlimited, so rates are written as tokens per 1, 10, 100, 1000 or
    3600 s. A token bucket given as the behaviour before the first assignment is used with no
    limiter behind it; that behaviour must be allow-all or deny-all. `RequestsPerTimeUnit`
    assignments are not implemented. The filter and `RateLimitStrategy` are marked
    work-in-progress upstream and Envoy logs a warning saying so.
48. **Failing open** (the lifetimes are replaced by 56). A new bucket lets requests through
    until the daemon's first answer, a
    matter of milliseconds. A share lasts 20 s and is renewed every 5. If the daemon is gone,
    each Envoy then applies a rule's whole rate by itself for ten minutes, and after that the
    rule is not enforced until the daemon returns. Past 100,000 keys for one rule, new keys are
    not limited. Blocked rules refuse in all of these states. Tollbooth's reason holds here: a
    limiter that takes storage away when it is unwell is worse than none.
49. **Seen against real Envoy** (2026-11-18, two worker threads each): 20 GetObject per second
    per access key let 142 through in 6 s and refused 11,266 with `503 SlowDown`, `Retry-After:
    1` and S3's error document, while PUTs passed untouched; a second Envoy on the same port,
    and the two together let 259 through in 12 s; the rate raised under load, with no failed
    request; a blocked rule refused everything with the status it names; the backend never saw
    the script's headers; with the rules removed traffic was byte-exact again.
50. **Not built, or not as tollbooth has it** (its last two sentences are replaced by 52 and
    60). `Retry-After` is always 1: the wait is not known
    where the answer is made. The error document has no `Resource` or `RequestId`. No
    `source_ip` filter, no `rules test`, no observe-only rules. Virtual-host-style requests
    are not recognized (clients of a loopback address use path-style). Adding or removing a
    rule changes every listener's filter chain; changing one changes a filter's settings.
    Envoy's own memory of buckets is bounded only by the daemon abandoning idle ones.

51. **The AWS CLI against a rule** (2.15.31, by hand on 2026-11-18, through real Envoy to the
    fake backend). A refusal reaches it as `503`, `content-type: application/xml`, `retry-after:
    1` and S3's error document, which it parses as `SlowDown`. In its default (standard) mode it
    retries twice, after jittered delays of under a second each, ignores `Retry-After`, and
    then fails with "reached max retries: 2". One `aws s3 cp --recursive` of 40 files against
    5 PutObject a second: 11 failed by default; none with `AWS_RETRY_MODE=adaptive` (9 s) or
    `AWS_MAX_ATTEMPTS=10` (13 s). It does not retry `429 TooManyRequests` at all: that code is
    not in botocore's list of throttling errors and 429 is not among the statuses it treats as
    transient. So `respond: 429` suits blocked rules and little else for AWS SDK clients. Not
    tried: multipart transfers and listings (the fake backend has neither), other SDKs, boto3.

52. **Rules travel in the route, not the listener** (replaces 45, and the last lines of 50).
    Envoy applies a changed listener to new connections only; the old ones live on under the
    old listener until they close or its drain time runs out, ten minutes by default. An S3
    client keeps its connections. Seen on 2026-11-18: a connection opened before a rule
    blocked PutObject was still passing PUTs 50 s later. So the listener now holds only what
    never changes: a Lua filter with no script, then four rate limit quota filters ("slots"),
    then the router (`internal/xds/ratelimit.go`). The route table is a resource of its own,
    fetched by name over ADS (RDS), and carries the rules: the generated script, as the Lua
    filter's per-route configuration. Envoy applies a new route table to the next request on
    every connection. A target with no rules turns the Lua filter and every slot off in its
    route, removes no header, and passes a request exactly as it came.
53. **Slots.** The script puts each rule a request matches in the next free slot, as three
    request headers: the rule's name, what it counts the request under, and `429` if a refusal
    is to be one. Slot *j*'s quota filter knows nothing of any rule; it counts a request that
    has slot *j*'s headers in the bucket `{rule, key}` they name, and refuses with the status
    they name. All slots share one bucket cache and one stream to the daemon, because Envoy
    keys those by server and domain (`filter_persistence.h`), so a rule's count is the same
    count whichever slot it lands in. The price: one request can be limited by at most
    `ratelimit.Slots` (4) rules. One that matches more is limited by the first four in the
    order of the rules, and the script notes it in the request's metadata. Four, because each
    slot is a filter every request under any rule passes through.
54. **Blocked rules are the script's.** A request that matches a blocked rule is answered by
    the script itself, wherever the rule comes in the order, with the same S3 error document
    the quota filters send. Nothing is asked of the daemon, so a blocked rule holds whether or
    not the daemon can be reached, and takes no slot. The daemon learns of each refusal from
    the access log (57).
55. **Config must render to the same bytes every time.** Found while doing 52: a filter's
    config packed with `anypb.New` writes a map's entries in whatever order Go hands them
    over, so a listener with a quota filter came out as different bytes from one render to the
    next. The snapshot then looked changed when it was not, and Envoy, which compares what it
    is sent with what it has, replaced the listener. Every message is now packed
    deterministically (`toAny`), and `TestRenderingIsStable` renders each resource fifty times.
56. **A share lasts ten minutes and is renewed every two and a half** (replaces 48). There is
    no fallback strategy any more: a slot cannot name a rule's rate, and the old fallback gave
    every Envoy the rule's whole rate. Instead, when the daemon is away each Envoy goes on
    with the share it has, and the shares still add up to the rule. After ten minutes without
    the daemon, and for a count first opened while it is away, the count is not limited. Seen
    on 2026-11-18 with two Envoys under 20 GetObject a second and the daemon stopped for 35 s
    of a 75 s run: 1,541 allowed, 20.5 a second, no failed connection, no warnings on restart.
57. **Refusals are not the target's requests.** A refusal is Envoy's own answer, and was being
    logged and counted as a request to the target and as one of its 503s: under a rule of 20
    a second the overview showed 2,610 requests and 2,592 503s a second for a backend seeing
    20. The daemon now tells a refusal by the quota filter's response code details
    (`rate_limited_by_quota`) or the script's note of the blocked rule, and counts it apart:
    not in a target's or a client host's requests, statuses, bytes or latency, but as
    `refused` beside them. Refusals still reach the daemon one by one, by the owner's
    decision, and the latest thousand are kept (`/api/v1/refusals`, `deltav rule refusals`)
    with the operation, access key and bucket the script noted. For a request over a limit
    that several rules counted, Envoy does not say which refused it; the record names them
    all.
58. **A count is kept until its tokens would be full again.** The daemon used to tell an
    Envoy to forget a count after 30 s of silence whatever the rule, and a forgotten count
    comes back with a full burst: 6 an hour with a burst of 3 let 12 through in 94 s. It now
    waits until the tokens that Envoy holds would have refilled anyway (burst divided by rate,
    and never under 30 s). Seen with Envoy: 12 an hour, burst 3; 4 passed, and after 40 s of
    silence none.
59. **What idle Envoys are held, and when a burst cannot hold** (replaces part of 46). An
    Envoy that has a count open and sends nothing is counted as asking for a tenth of the rate
    divided by the number of Envoys, where it was a quarter: however many sit idle, the busy
    keep nine tenths. The burst is divided to add up exactly while there are no more Envoys
    than the burst. Each Envoy must have a burst of at least one, so beyond that the bursts add
    up to the number of Envoys; a rule in that state says so (`holds: false`, with the
    number), as a connection limit does. Rates are written as tokens over up to a hundred
    years, so that a very small share is not rounded up to one an hour: 400 Envoys sharing 20
    an hour are given shares that add up to 20 an hour. Not changed: the first request of a
    newly opened count passes before the daemon has answered.
60. **The quota service's work** (replaces the last sentence of 46). A count's shares are
    worked out at most once a second however many Envoys report it, and at once when an Envoy
    joins or leaves a count that few have open. A report that changes nothing is answered to
    nobody. An Envoy is sent its share in answer to its own report if it differs by more than
    a hundredth from what it holds, and unasked if by more than a tenth. A count of a rule
    that is gone, or blocked, or past the 100,000-key bound, is answered with "forget it" and
    not left for the Envoy to report for ever. Demand is measured by the daemon's clock, not
    the whole seconds Envoy reports. A rule's status is worked out once a second for
    everyone who asks. A changed rule reaches the quota service before Envoy, so that the
    first report of its count finds the rule known; and one lock is held across a rule change
    and its undoing.
61. **The bucket is what a server would take it to be** (how, and the last sentence, are
    replaced by 66). The script compared the bucket as
    the bytes on the wire, while it decoded the key: `/arch%69ve/k` was not bucket `archive`,
    and slipped a blocked rule. It now decodes the bucket once and skips empty segments ahead
    of it. And a count's name is never empty: a rule counting per bucket counts a request
    that names none under `-`, where it used to let it through uncounted. How VAST itself
    reads such paths has not been tried.
62. **Counted before it is authenticated.** A request is counted for the access key it
    claims, since Envoy cannot check a signature. Requests VAST will reject therefore use up
    that key's limit. Sharing by demand bounds it (a host asking for everything gets an even
    share), tollbooth has the same property, and the owner accepted it on 2026-11-18. `per:
    access_key, client` confines it to one client host.
63. **Smaller repairs of 2026-11-18.** The hold on a target's last endpoints (23) counted
    every rebuild of the config, which an operator's commands also cause; it counts polls. A
    VIP whose address is not an IP address failed the whole snapshot; it is left out and
    counted in the target's status. The login form's `next` accepted a tab, which a browser
    drops, giving a redirect off the site. A limit under 0.0005 a second was written as 0. The
    fixture report's paths could hold a real address.
64. **Decided on 2026-11-18, and left as they are.** Two Envoy worker threads by default in
    the client unit (40), to be tuned when there is a reason. The S3 operation table stays
    under Apache-2.0 with tollbooth's notice. Outlier ejection on five consecutive 5xx (3, 17)
    is unchanged; the owner confirmed on 2026-11-19 that it is to stay so, VAST's own
    `503 SlowDown` included.

### The first run against a real cluster (2026-11-19)

65. **`make live` passes.** VAST 5.5, three CNodes, six VIPs in the pool, plain HTTP to port
    80, one Envoy with two workers. 200 round trips of 1 MiB (PUT, GET with the content
    checked, DELETE) through the loopback listener: all verified, in 2.4 s, p50 15 ms, p99
    38 ms, 196 to 208 requests to each CNode, and the daemon's counts per CNode add up to
    exactly what the load generator sent. So VAST accepts requests as Envoy forwards them:
    lower-case header names, `x-forwarded-proto` added, the path untouched. The run before
    it failed on a bucket name that did not exist, which is how it is known that a `404
    NoSuchBucket` from VAST comes back through Envoy as it is.
66. **A rule reads a request as VAST does** (replaces the how of 61). 61 made the script read
    the bucket "as a server would", by reasoning. Asked of the real server, the reasoning was
    half wrong, and four requests that VAST served went past a rule that blocked them. The
    live test now sends one object's GET in every unusual form we know of, with no rule and
    then under a rule blocking it by bucket and prefix, by access key, and by operation; any
    form VAST serves under a block fails the test. What VAST 5.5 was seen to do, and the
    script now does:
    - It decodes the path once and then splits it. `/b%2Fk` is key `k` in bucket `b`; the
      script took `b/k` for the bucket. A name encoded twice is not decoded twice.
    - A path that starts `//` has no bucket, and a GET of it lists buckets, whatever follows.
      61 skipped the empty segment and read on, which made a bucket listing look like a
      GetObject and would have let it past a rule on `ListBuckets`.
    - Nothing else is tidied: `/b//k` is key `/k`, dot segments are part of a key, a bucket
      name is matched as written.
    - It takes the bucket from the host name where it can (67).
    - It finds `credential=` in an `Authorization` header in any letter case; the script
      looked for `Credential=` and read anything else as no access key.
    - Given a presigned URL and an `Authorization` header together it acts on the URL; the
      script believed the header (68).
67. **The bucket in the host name.** VAST serves `GET /k` with `Host: b.<pool>.<suffix>` or
    `Host: b.<suffix>` as key `k` of bucket `b`, where `<suffix>` is `domain_suffix` of
    `GET /api/dns/` and `<pool>` a VIP pool's `domain_name`. `<pool>.<suffix>` itself, the
    suffix alone, and every name not under the suffix are read by path. The domain is
    compared without regard to case, port or a final dot; the bucket is taken as written.
    The daemon reads `dns/` with each poll (a fourth GET) and gives the script every suffix
    and every pool label, per target, saved with the endpoint cache so that a restart
    without VAST still knows them. A failed read of `dns/` fails nothing else: the names last
    read stay, and the target's status says so (`bucket_hosts_problem`). A client need not
    resolve such a name to send it; a `Host` header is enough, which is why this could not be
    left as a thing clients behind a loopback proxy do not do. Static targets have no cluster
    to ask and are read by path only.
68. **A request that names several access keys is refused.** Two `Authorization` headers, two
    `Credential` fields in one, a presigned URL with a header: VAST picks one by rules of its
    own (and rejected most such requests, but not all). The script collects every access key
    a request names. If there is more than one and any rule looks at access keys (a filter,
    or `per: access_key`), it answers `400 InvalidRequest` itself and counts the request as
    refused, under `(several access keys)` where a rule's name would stand. With no such rule
    the request is treated as any other. Mimicking VAST's choice would have been exact for
    this version and wrong without notice for the next.
69. **What reading as VAST does rests on.** One cluster, one version, DNS service enabled, one
    suffix. Not seen: signature version 2, browser form uploads (the access key is in the
    body, which the script does not read: such a request has no access key to a rule), other
    methods than GET on the odd paths, a cluster with DNS off or several suffixes. The live
    test is the instrument: run it against a cluster before trusting rules about buckets or
    access keys there. The script still lets a request through unlimited if the script
    itself fails, which is a choice for availability that a blocked rule inherits.
70. **TLS to the VIPs, run at last** (2026-11-19; replaces the last sentence of 6). On real
    Envoy against backends that speak only TLS (`TestTLSToTheBackends`): a target with the
    authority's file and the certificate's name passes every request, as does one whose name
    the certificate covers with a wildcard, and one with verification off. A name the
    certificate is not for, a name two labels under its wildcard, another authority's file,
    and the host's own bundle are each refused, every request, with nothing reaching a
    backend; the backends were asked (by SNI) for exactly the names the targets were given.
    On the lab's VAST 5.5, port 443, TLS 1.2: `make live` passes in full over TLS both
    unverified and verified, 200 of 200 round trips each, as over plain HTTP. The lab presents
    VAST's factory certificate (names `*.vastdata.com`, `vms.vastdata.com` and a bare `*`,
    signed by VAST's own root, the same on all six VIPs), so "verified" there means against
    that root and the name `vms.vastdata.com`. Asked for the pool's own DNS name the same
    setup refuses it, 8 requests of 8: a bare `*` covers nothing, and `*.vastdata.com` one
    label.
71. **There is no "host's trust store" in Envoy** (replaces part of 6). 6 chose
    `system_root_certs` for a TLS target with no CA file. The API has the field; Envoy 1.40
    does not implement it (it is marked so in the API's source, which nobody read), and
    rejects a cluster that verifies names without a CA: every Envoy refused the whole update
    the first time one was tried. So this default never worked. A TLS target now needs a CA
    file, a path on the client hosts, or verification switched off; `target add` and the web
    form refuse it otherwise and name the usual bundle paths, and a target stored the old way
    is not served and says why. Not done: finding the host's bundle by itself. It would take
    a static secret defined where each Envoy starts, since one config serves the whole fleet
    and the path differs by distribution; an operator with a mixed fleet has to provide one
    path that exists everywhere.
72. **A target that gets no answer says why.** A wrong CA file or server name gives every
    request a 503 that Envoy made up. The client is told "upstream connect error", Envoy logs
    nothing at its usual level, and the daemon counted these as 503s of the VIPs, which reads
    as VAST throttling. Envoy's access log does carry the reason. The daemon now keeps, per
    target, the latest request an Envoy answered for want of an answer from the target (any
    5xx whose response code details are not `via_upstream`, leaving out rate rules and the
    connection limit, which are deliberate), in words, with how many there were in the last
    minute: `no_answer` on `/api/v1/targets`, shown by the CLI, the TUI and the web overview
    for five minutes after the last one. For a certificate rejected by name it gives the
    names the certificate is for, which Envoy supplies. The counts are unchanged: such a 503
    is still among the target's 503s.
73. **Real clients, real cluster, through rules** (2026-11-19, by hand; one Envoy with two
    workers on the lab's pool over plain HTTP). The AWS CLI 2.15.31 and boto3 1.41.3, both at
    their defaults, with a rule per operation whose limit nothing could reach, so that each
    rule's count says how the script classified what the client sent:
    - A 64 MiB `aws s3 cp` up is 1 CreateMultipartUpload, 8 UploadPart, 1
      CompleteMultipartUpload; down, 1 HeadObject and 8 ranged GetObject. A server-side copy
      is 1 HeadObject, 1 GetObjectTagging, 1 CreateMultipartUpload, 8 UploadPartCopy, 1
      CompleteMultipartUpload. `aws s3 rm --recursive` is a ListObjectsV2 and a DeleteObject
      for each key; boto3's `delete_objects` is one DeleteObjects for 303 keys. A presigned
      URL is a GetObject under the key that signed it. boto3 did the same things with the
      same counts, less the tagging read on a copy, which it does not make. Nothing was
      classified as anything else, and nothing as unknown: a rule on everything counted
      exactly the sum of the others.
    - boto3 with `addressing_style: virtual`, sent to Envoy as a client would be that
      resolves the cluster's S3 name to loopback: served, counted under the bucket named in
      the host, and refused by a block on that bucket (67, with a real SDK).
    - Under `UploadPart` at 2 a second with a burst of 2: the CLI at its defaults fails in 3 s
      (15 attempts at parts, 11 refused, "reached max retries: 2") and aborts the upload; no
      unfinished upload was left on the cluster. With adaptive retries and 10 attempts it
      finishes in 6 s (13 attempts, 5 refused). boto3 at its defaults finishes in 3 s (18
      attempts, 10 refused), in adaptive mode in 5 s (12 attempts, 4 refused).
    - Under `List*` at 1 a second, 300 keys at 50 a page: the CLI at its defaults fails on a
      page; adaptive, 13 s. boto3 at its defaults, 6 s.
    - A block on a prefix refuses reads, writes, deletes and listings of that prefix or a
      longer one, however the key is spelled; a listing of the parent prefix is not the
      rule's, and shows the names under it.
    What this did not show: any other SDK; these clients over TLS; anything at a rate that
    would stress Envoy or the cluster (the uploads were 64 MiB).

## Known gaps (POC)

mTLS on the xDS and telemetry streams; daemon HA; per-group targeting of Envoys; long-term metric
storage. The gRPC port is unauthenticated, so anything that can reach it can read config (VIP
lists) and inject telemetry, but it cannot change any other Envoy's config.
