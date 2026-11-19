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
| 8 | The quota service did work that grew with the square of the fleet, and rebuilt its status on every read. | Fixed (60). | Tested: 500 Envoys on one count, shares worked out 36 times over five rounds of reports, where it was once for each of some 3,000 reports; a steady round sends nothing. |
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
