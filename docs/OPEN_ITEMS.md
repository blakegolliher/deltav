# Open items

A review on 2026-11-18 found sixteen bugs, twelve of them in the request-rate rules written
that day. All sixteen were dealt with the same day: fourteen fixed, one made visible where it
cannot be fixed (5), one accepted and documented (11). The second half of this file says what
became of each. The first half is what is still open.

On 2026-11-19 the first run against a real VAST cluster passed, and then showed that the fix
for finding 3 had been half wrong: four kinds of request that VAST serves went past a rule
blocking them. Those are fixed and the live test now looks for more (DESIGN.md amendments 65
to 69). The same day TLS to the VIPs ran for the first time, on real Envoy and against the
cluster: it works, except that its default way of trusting a certificate had never been able
to (70 to 72).

A second review, on 2026-11-23 (an outside one, of the whole repository), made 26 findings,
a dependency note, a removal and five refactoring suggestions. Each finding was reproduced by
a test before anything changed; most are fixed, a few wait on the owner. Its section is at
the end of this file (amendments 74 to 76).

How things here were established: **seen** means observed on real Envoy 1.40.2, **tested**
means a unit test covers it, **read** means from reading code only.

## Still open

### Never run

- **TLS to a cluster with a certificate of its own.** TLS to the VIPs runs (amendment 70),
  but the one real cluster it ran against presents VAST's factory certificate. Verifying a
  certificate issued for the pool's own DNS name, by a public or a company authority, was
  done against test backends only. Client certificates towards VAST are not supported.
- **Rate rules on a second cluster.** How rules read a request (bucket in the path or the
  host name, which access key) was matched to one cluster: VAST 5.5, DNS service on, one DNS
  suffix. `make live` asks a cluster the same questions and fails if a rule can be avoided;
  it has been run against that one cluster only. Not tried anywhere: signature version 2,
  browser form uploads (their access key is in the body, so to a rule they have none), other
  methods than GET on the odd paths, a cluster with its DNS service off or with several
  suffixes (amendment 69).
- **The systemd units** have not been run under systemd.
- **The web UI** has been used by its owner in one browser. What this round added to it (a
  "Burst not kept" note on a rule, a "Left out" note on a target, new wording in two hints) has
  not been looked at in a browser by anyone: it is covered by the template tests only.
- **Rate rules with other clients.** The AWS CLI and boto3 have been run against the real
  cluster through rules: multipart transfers, copies, listings, presigned URLs, batch deletes,
  the bucket in the host name (amendment 73). No other SDK has, nor these two over TLS, nor
  anything at volume: the largest transfer was 64 MiB.

### Limits of TLS to the VIPs

- A TLS target must name a CA bundle file that exists at the same path on every client host,
  or verify nothing (71). Envoy cannot be told to use a host's own trust store, and Delta-V
  does not find the file for it.
- A request Envoy answers itself because VAST gave no answer is still counted among the
  target's 503s, and so among its VIPs'. The target says that it is happening and why (72);
  the counts do not tell the two kinds of 503 apart.

### Limits of rate rules that remain

By design, or by what Envoy's filter does. Each is said in the README or `rule set --help`.

- A prefix rule does not match a listing of a parent prefix, so the names of keys under a
  blocked prefix can still be listed from above it (73).
- A request refused by one rule has been counted, as let through, by the rules before it in
  the order (seen: 73).
- There can be at most 64 rules.

- A request that names more than one access key is refused with `400` while any rule looks at
  access keys (68). No ordinary client sends one.
- A target that is not on a VAST pool, or whose cluster's DNS settings cannot be read, has no
  host names to go by: a bucket named in the host name is then not seen by rules (67). The
  target's status says when the read fails.
- If the rule script itself fails on a request, the request goes through unlimited and Envoy
  logs the error. That holds for blocked rules too.

- At most 4 rules can limit one request (amendment 53). More slots are a constant away, at the
  price of a filter each on every request.
- The first request of a newly opened count passes before the daemon has answered. On each
  Envoy a burst of 3 therefore lets 4 through at first (seen).
- With more Envoys sharing a count than its burst, the burst is not kept (59). The rule says
  so; nothing makes it hold short of a larger burst or counting per client.
- What Envoys hold can add up to a tenth over a rule for the few seconds between two of their
  reports, while shares move (60).
- With the daemon away for more than ten minutes, limits are not enforced; and an Envoy cut
  off from the daemon keeps its last share that long even if the rule has since been lowered.
- A request is counted before VAST authenticates it (62). Accepted.
- An upload that is refused has its connection closed by Envoy (seen). A refused GET does not.
- For a refusal over a limit that several rules counted, the record cannot say which rule
  refused: Envoy does not tell.
- Envoy's rate limit quota filter is marked work-in-progress upstream, and every Envoy logs a
  warning about it for each of the four filters when it loads a listener, rules or no rules.
- Moving a running fleet from a build before amendment 52 replaces every listener once, and
  with that drains its open connections once.
- The gRPC port is unauthenticated (a known gap). Anyone who can reach it can shift shares
  with false usage reports, or add to the refusal record with false log entries.

### Waiting on the owner, from the review of 2026-11-23

- **A presigned URL's signature is in every access-log entry** Envoy sends the daemon (seen):
  Envoy's gRPC access logger sends the whole path, query included, and cannot be told not to.
  It crosses the unauthenticated gRPC port in clear. Header-signed requests are not affected.
- **A change that fails to publish is still answered 2xx** (limit, drain, delete target), and
  the status already shows it; it is published at the next successful poll (tested). No way
  to make publishing fail outside shutdown is known.
- **A drained CNode's history vanishes at once** from `/api/v1/cnodes`: its VIP rows, stats
  and sparkline (tested).
- `bootstrap --admin-address` accepts a non-loopback address for Envoy's unauthenticated admin
  interface (tested); the API token has no minimum length.

### Tidying the review turned up

- `internal/cli` has its own HTTP client and API types where `internal/apiclient` would do.
- The rate, MB/s, latency and age formatters exist three times (`tui`, `webui`, `cli`).

## What the review found, and what became of it

| # | Finding | Outcome | Established |
|---|---|---|---|
| 1 | A rule change did not reach connections already open: rules changed the listener, which Envoy applies to new connections only. | Fixed. Rules travel in the route (52 to 54). | Seen: one connection, rule added, changed and removed, each in effect on its next request (`TestRuleChangeReachesOpenConnections`). |
| 2 | Slow rules could be beaten by pausing: a count was given up after 30 s of silence and came back with a full burst. | Fixed. A count is kept until its tokens would have refilled (58). | Seen: 12 an hour, burst 3; 4 passed, then none after 40 s of silence (`TestSlowRuleHoldsThroughAPause`). |
| 3 | An encoded bucket name slipped past bucket filters and blocked rules; a request with no bucket was not counted by a per-bucket rule. | Fixed in the script (61), then fixed again against real VAST, which reads paths otherwise than 61 supposed (66 to 68). | Seen: of 54 unusual requests for one object, VAST serves 18, and a rule blocking the object refuses all 18 (`TestLiveRulesReadRequestsAsVASTDoes`). |
| 4 | One empty answer from VAST plus two operator commands emptied a target. | Fixed. The hold counts polls (63). | Tested. |
| 5 | Low-rate rules shared by many Envoys admitted several times their limit. | The rate is now exact: 400 Envoys on 20 an hour are given 20 an hour. The burst cannot be: each Envoy needs one. The rule says so (`holds`). | Tested. |
| 6 | Refused requests were counted as the target's own requests and 503s. | Fixed. Counted apart, and each kept on record (57). | Seen: 4,322 requests and no 5xx for the target, 27,489 refused beside them. |
| 7 | Open redirect after login through a tab in `next`. | Fixed (63). | Tested. |
| 8 | The quota service did work that grew with the square of the fleet, and rebuilt its status on every read. | Fixed (60), but not wholly until the review of 2026-11-23 (T1): each report still looked over every Envoy's part of the count. | Tested: 500 Envoys on one count, shares worked out 36 times over five rounds of reports, where it was once for each of some 3,000 reports; a steady round sends nothing. A round with 2,000 Envoys took 0.3 s, now 11 ms. |
| 9 | One unparsable VIP address blocked every config update. | Fixed. That VIP is left out and counted (63). | Tested. |
| 10 | A quarter of a rule's rate was held back for idle Envoys. | Now a tenth, however many are idle (59). | Tested. |
| 11 | A client can spend another access key's budget with requests VAST will reject. | Accepted by the owner, and said where rules are described (62). | By design. |
| 12 | Counts past the key bound, or of a removed rule, were never released by Envoy. | Fixed: answered with "forget it" (60). | Tested. |
| 13 | Demand was measured with Envoy's whole-second timer. | Fixed: the daemon's clock (60). | Tested. |
| 14 | A valid limit under 0.0005 a second was shown as 0. | Fixed (63). | Tested. |
| 15 | The fixture sanitizer's report could contain a real address. | Fixed (63). | Tested. |
| 16 | Two rule changes at once could lose one. | Fixed: one lock across a change and its undoing (60). | Read. |

Found along the way, and not by the review:

- **A TLS target with no CA file was rejected by every Envoy** (2026-11-19): it relied on a
  setting Envoy does not implement. Never noticed, because TLS had never been run. Now refused
  when the target is added, with what to give in its place (71). And a target whose
  certificate cannot be verified said nothing but 503; it now says why (72).

- **Four ways round a rule, on real VAST** (2026-11-19): the slash after the bucket written
  `%2F`; the bucket named in the host name; `credential=` in lower case; a presigned URL sent
  with a second, false `Authorization` header. VAST served each, and a rule blocking the
  bucket or the access key did not apply. And `//bucket/key`, which VAST answers as a list of
  buckets, was read as a GetObject. Fixed (66 to 68), tested in the script, and seen fixed
  on the cluster.

- **A listener rendered to different bytes from one moment to the next** whenever it held a
  quota filter, so about half of all config rebuilds pushed a listener Envoy took for changed
  and replaced (55). Fixed and tested.
- **A new rule was unenforced for up to 5 s**: Envoy learned of it before the quota service
  did. The order is now the other way round (60).

Also checked again on real Envoy after the changes: the daemon stopped for 35 s of a 75 s run
under a rule of 20 a second on two Envoys let 1,541 through, 20.5 a second, with no failed
connection (56). The whole suite against Envoy is fourteen tests, all passing, and the two
against a real cluster pass as well, over plain HTTP and over TLS (65, 70).

Decided by the owner on 2026-11-19: outlier ejection stays as it is, VAST's own
`503 SlowDown` included (64).

## The review of 2026-11-23, and what became of it

Severity is the review's, then as found when reproduced. Every outcome below is tested; the
real-Envoy suite (fourteen tests) passes after them.

| Finding | Outcome |
|---|---|
| S1 gRPC port unauthenticated; presigned URLs in access logs | Waiting on the owner (above). The log exposure is seen on real Envoy. |
| S2 a redirect could take the VAST password, refresh token or access token elsewhere, http included; the CLI and API client followed redirects too (High → Medium) | Fixed (75). |
| S3 a failed DNS read on the first poll after a restart dropped the bucket host suffixes, and saved that (High → Medium) | Fixed (76). |
| S4 the example env file showed `$(...)`, which systemd passes as literal text (Medium) | Fixed: wording, and such a token is refused. |
| S5 VAST responses read without a size limit; a token at the cut survived redaction (Medium → Low) | Fixed (75). |
| S6 a live stream outlived its browser session (Medium → Low) | Fixed: the request ends with the session. |
| C1 a publish failing part-way defeated a rollback (High → Low: only at shutdown today) | Fixed (74). |
| C2 two runs in one second shared config versions (High → Low) | Fixed (74). |
| C3 a target served empty lost its listener across a restart (Medium → Low) | Fixed (76). |
| C4 a failed save of the restart cache was never retried (Medium) | Fixed (76). |
| C5 a poll overwrote a cluster attached or re-attached during it (Medium → Low) | Fixed (76). |
| C6 a 2xx can hide a change that failed to publish (High → Medium) | Waiting on the owner; failed rollbacks now say so (500). |
| C7 a limit given with a new target skipped the checks (Medium → Low) | Fixed: one normalization, also on load. |
| C8 one slow cluster held back every cluster's changes (Medium) | Fixed (76): up to four read at once. |
| T1 quota reports did work in the square of the fleet (Medium) | Fixed: 1000 Envoys, 91 ms → 5 ms a round (60, item 8 above). |
| T2 a superseded load-report stream could clear its successor's gauges (Medium → Low) | Fixed. |
| T3 load-only counters never expired (Medium → Low) | Fixed: each source expires on its own. |
| T4 a drained CNode's in-flight work disappears from its view (Medium → Low) | Waiting on the owner (above). |
| T5 the terminal UI could show an answer for an older page, filter or window (Medium) | Fixed; a failed change also keeps its error. |
| Smaller: `loadgen --check`, state-file version, sanitizer, admin bind, license gate, negative size, trailing JSON | All fixed but the admin bind (owner). The latency list of long `loadgen` runs is unbounded still. |
| `golang.org/x/net` | Bumped to v0.62.0: five HTTP/2 advisories reachable from the daemon, which the review had not found. |
| Dead code (`poller.poke`) | Removed. |

The review's refactoring suggestions (split the poller's `apply`, one HTTP client, fixed Lua in
its own file, in-process handler tests, a cached Controls page) are not taken up yet.
