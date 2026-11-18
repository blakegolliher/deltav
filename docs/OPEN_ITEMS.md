# Open items

A review on 2026-11-18 found sixteen bugs, twelve of them in the request-rate rules written
that day. All sixteen were dealt with the same day: fourteen fixed, one made visible where it
cannot be fixed (5), one accepted and documented (11). The second half of this file says what
became of each. The first half is what is still open.

How things here were established: **seen** means observed on real Envoy 1.40.2, **tested**
means a unit test covers it, **read** means from reading code only.

## Still open

### Never run

- **TLS to the VIPs** has never run through real Envoy (DESIGN.md amendment 6).
- **A real VAST cluster with a real bucket.** `make live` was run on 2026-11-18 and got as far
  as it could: the daemon read the cluster, Envoy took the config, and all 200 uploads of the
  run reached VAST through Envoy, spread over three CNodes and six VIPs. Every one was
  answered `404 NoSuchBucket`: the bucket name the test was given does not exist for the keys
  it was given. No object was created. It needs the right bucket name, and then tells us
  what is still not known about VAST behind Envoy: whether signatures verify with Envoy's
  lower-case header names and its `x-forwarded-proto: http`, and how VAST reads an encoded
  bucket name or a path that starts `//` (amendment 61).
- **The systemd units** have not been run under systemd.
- **The web UI** has been used by its owner in one browser. What this round added to it (a
  "Burst not kept" note on a rule, a "Left out" note on a target, new wording in two hints) has
  not been looked at in a browser by anyone: it is covered by the template tests only.
- **Rate rules with real clients** have been tried with the AWS CLI only, and not for multipart
  transfers or listings, which the fake backend lacks.

### Limits of rate rules that remain

By design, or by what Envoy's filter does. Each is said in the README or `rule set --help`.

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

### To confirm with the owner

- **Outlier ejection on VAST's own `503 SlowDown`.** Today five 5xx in a row eject a VIP for a
  while, which moves load onto the others when VAST is only asking clients to slow down. The
  answer given on 2026-11-18 could be read either way, so nothing was changed.

## What the review found, and what became of it

| # | Finding | Outcome | Established |
|---|---|---|---|
| 1 | A rule change did not reach connections already open: rules changed the listener, which Envoy applies to new connections only. | Fixed. Rules travel in the route (52 to 54). | Seen: one connection, rule added, changed and removed, each in effect on its next request (`TestRuleChangeReachesOpenConnections`). |
| 2 | Slow rules could be beaten by pausing: a count was given up after 30 s of silence and came back with a full burst. | Fixed. A count is kept until its tokens would have refilled (58). | Seen: 12 an hour, burst 3; 4 passed, then none after 40 s of silence (`TestSlowRuleHoldsThroughAPause`). |
| 3 | An encoded bucket name slipped past bucket filters and blocked rules; a request with no bucket was not counted by a per-bucket rule. | Fixed in the script (61). | Tested in the script. VAST's own reading of such paths: not tried. |
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

- **A listener rendered to different bytes from one moment to the next** whenever it held a
  quota filter, so about half of all config rebuilds pushed a listener Envoy took for changed
  and replaced (55). Fixed and tested.
- **A new rule was unenforced for up to 5 s**: Envoy learned of it before the quota service
  did. The order is now the other way round (60).

Also checked again on real Envoy after the changes: the daemon stopped for 35 s of a 75 s run
under a rule of 20 a second on two Envoys let 1,541 through, 20.5 a second, with no failed
connection (56). The whole suite against Envoy is twelve tests, all passing.
