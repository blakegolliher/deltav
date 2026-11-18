# Open items

Bugs and unverified areas found by a review on 2026-11-18, most important first. Nothing here
has been fixed. Twelve of the sixteen bugs are in the request-rate rules, which were written the
same day; the older core (xDS, telemetry, VAST polling, connection limits) produced items 4 and 9.

How each was established: **seen** means observed on real Envoy 1.40.2, **reproduced** means
shown in a throwaway test, **read** means from reading the code only.

## High

### 1. A rule change does not reach connections that are already open

Adding or removing a rule, or changing what a rule matches, changes the listener's HTTP filter
chain (`internal/xds/ratelimit.go`). Envoy applies a changed filter chain to new connections only
and drains the old ones over its drain time, 10 minutes by default. A change to a rule's rate is
not affected: it travels as a share from the quota service.

- **Seen.** One keep-alive connection sent a PUT every 3 s. A rule blocking PutObject was added
  7 s in. The connection was still passing PUTs 50 s later and was never closed.
- **Fix: large, and needs a design decision.** The rules must reach Envoy without touching the
  listener. Directions: deliver the script through the route (RDS, `LuaPerRoute`) or ECDS, and
  keep a fixed set of quota filters in the listener that the script assigns matching rules to;
  refuse blocked rules in the script itself, so they hold with no daemon. A stopgap is a short
  `--drain-time-s` in the client unit.

### 2. Slow rules can be beaten by pausing

The quota service tells an Envoy to forget a count after six reports with no requests, which is
30 s, whatever the rule (`idleReports`, `internal/ratelimit/server.go:250`). The next request
opens the count again with a full burst.

- **Seen.** A rule of 6 GetObject per hour with a burst of 3 let 4 through, then 4 more after
  47 s of silence, then 4 more after another 47 s: 12 in 94 s.
- Any rule whose burst takes longer than 30 s to refill is affected, per-minute rules with a
  large burst included.
- **Fix: small.** Do not give a count up until it would be full again: idle for at least
  burst ÷ rate. Tollbooth's sweeper has the same rule.

### 3. An encoded or odd path slips past bucket filters and blocked rules

The script compares and counts the bucket as the raw bytes of the path, while it decodes the key
(`internal/ratelimit/lua.go:225`). `/arch%69ve/k` is therefore not bucket `archive`. Separately,
when the only thing a rule counts per is empty (no bucket: `GET /`, or a path starting `//`), the
script sets the rule's header to an empty string, which the quota filter does not match, so the
request is not limited at all (`lua.go:248`).

- **Reproduced** in the script. Whether VAST decodes the bucket, or accepts `//bucket/key`, has
  not been tried. S3 servers normally decode the path, and Envoy passes it byte for byte.
- **Fix: small.** Decode the bucket once, as the key is, and never emit an empty count name.
  Tollbooth takes the bucket raw too, so it may have the same gap.

### 4. One odd VAST answer plus two operator commands can empty a target

A pool that suddenly resolves to no endpoints is not believed until three polls agree
(DESIGN.md amendment 23). The counter is advanced by every rebuild of the config, not by polls
(`internal/server/poller.go:372`), and commands such as `rule set`, `limit set`, `node drain` and
`target add` rebuild it too. One empty answer and two commands within the poll interval push an
empty endpoint list, and every Envoy answers 503 for that target until the next good poll.

- **Read.**
- **Fix: small.** Count polls: advance the counter only when the cluster's view is newer.

## Medium

### 5. Low-rate rules shared by many Envoys admit several times their limit

Every Envoy that has a count open is given at least one token per hour and a burst of at least
one (`internal/ratelimit/share.go:96`, `burstShare`), and one request passes when a count is
first opened, before the share arrives. With 50 Envoys a rule of 20 per hour hands out 102 per
hour and a burst of 102. Nothing reports it, unlike the connection limit's "cannot hold".

- **Reproduced.** The extra first request was also **seen** (4 passed with a burst of 3).
- **Fix: medium.** Plan it with item 10: both are about what an idle Envoy is given. At least
  say in the rule's status when the Envoys holding a count outnumber what the rule allows.

### 6. Refused requests are counted as the target's own traffic and 503s

A refusal is a response Envoy makes itself, and it is logged and counted like any other. Under a
20 per second rule with 8 busy workers the overview showed 2,610 requests per second and 2,592
503s per second for the target, while about 20 per second reached the backend. The fleet list's
REQ/S is inflated the same way, VAST's own 503s cannot be told from Delta-V's, and every refusal
is also an access log entry sent to the daemon.

- **Seen.**
- **Fix: medium.** Envoy marks a quota refusal with the response code details
  `rate_limited_by_quota` (`filter.cc`). Count those apart, or filter them out of the access log
  and rely on the quota reports.

### 7. Open redirect after login

`localPath` (`internal/webui/webui.go:414`) accepts a `next` value containing a tab. Browsers
remove tabs from addresses, so `/login?next=/%09/evil.example/x` sends someone to
`//evil.example/x` after they sign in with the real token.

- **Reproduced** (the `Location` header).
- **Fix: small.** Refuse any control character; parse the value and require a plain path.

### 8. The quota service does too much work at fleet scale

- A blocked rule's share is 0, and 0 also means "never sent", so every report for a blocked count
  re-sends the refusal to every Envoy holding it: quadratic in the number of Envoys
  (`server.go:318`). **Reproduced.**
- Shares for a count are recomputed on every Envoy's report, not once per interval. **Read.**
- `Status()` walks and sorts every count under the service's lock, and is called on every web UI
  tick per viewer, every `/metrics` scrape and every rules read. **Read.**
- **Fix: medium.**

### 9. One unparsable VIP address blocks every config update

VIP addresses from VAST go into endpoints unchecked (`poller.go`, `resolve`), and xDS rejects the
whole snapshot if one is not an IP address (`internal/xds/resources.go:336`). No target is
updated while the value is there, `target add` fails, and every rule change is rolled back.

- **Read.**
- **Fix: small.** Leave such a VIP out and say so in the target's status.

### 10. Up to a quarter of a rule's rate is held back for idle Envoys

`Divide` counts an Envoy that asks for nothing as asking for a quarter of an equal share. One
busy host therefore gets about 78% of a rule's rate while nine others merely have the count open.
`TestDivide` has this as an expected value (775 of 1,000).

- **From the algorithm.**
- **Fix: small to medium.** Cap what idle Envoys are held in total, or let it shrink with idle
  time. Fixing item 2 keeps idle counts open longer, which makes this matter more.

## Low

### 11. A client can spend another access key's budget

A request is counted before VAST authenticates it. Unsigned requests that carry someone else's
access key ID use up that key's rate across the fleet. Even sharing bounds the harm: a host
asking for everything gets an equal share, not all of it. Tollbooth has the same property.
**By design.** Document it, or suggest `per: access_key, client` where it matters.

### 12. Counts past the key cap, or for a removed rule, are never released

Both are answered "allow all" and not tracked (`server.go`, `report`), so the Envoy keeps them
and keeps reporting them. **Read. Fix: small:** answer with an abandon instead.

### 13. Demand is measured with Envoy's whole-second timer

Envoy reports the time since its last report in whole seconds, so a rate worked out from it can
be 25% off. **Read in Envoy's source. Fix: small:** use the daemon's own clock between reports.

### 14. A valid limit below 0.0005 per second is shown as "0 per second"

`amount` prints three decimals (`internal/ratelimit/rule.go:239`). **Reproduced. Fix: small.**

### 15. The fixture sanitizer's report can contain real addresses

The report's path is built from an object's real key (`internal/lab/sanitize.go:291`), though the
report is described as holding no values. It matters only for a response keyed by address or host
name. **Read. Fix: small.**

### 16. Two rule changes at the same moment can lose one

A rule change that cannot be served puts back the list it read beforehand
(`internal/server/api_rules.go`, `rulesChanged`), which undoes a change made in between.
**Read. Fix: small:** hold one lock across the change and its rollback.

## Not bugs, but unverified

- TLS to the VIPs has never run against real Envoy (DESIGN.md amendment 6).
- `make live` (real VAST, real bucket) has never run. It needs S3 keys and a bucket name.
- The systemd units have not been run under systemd.
- The web UI has been used by its owner; no one has checked it across browsers.
- Rate rules have been tried with the AWS CLI only, and not for multipart transfers or listings.
- The gRPC port is unauthenticated (a known gap). With rate rules, anyone who can reach it can
  also shift shares by sending false usage reports.

## Tidying the review also turned up

- `internal/cli` has its own HTTP client and API types where `internal/apiclient` would do.
- The rate, MB/s, latency and age formatters exist three times (`tui`, `webui`, `cli`).
- `server/api_rules.go` checks `errors.Is(err, ratelimit.ErrInvalid)` on an error that
  `Store.SetRule` has wrapped without it; the branch never runs, and the status is right anyway.

## Decisions waiting on the owner

- The client unit's default of 2 Envoy worker threads is a guess (amendment 40).
- The S3 operation table is adapted from tollbooth under Apache-2.0, with its LICENSE and NOTICE
  shipped; the owner of both may prefer to relicense the table.
- Whether `503 SlowDown` from VAST should count toward outlier ejection (amendments 3 and 17).
- Envoy writes HTTP/1 header names in lower case unless told to preserve case; not yet looked
  at on the wire against VAST.
- `x-forwarded-proto: http` reaches VAST on every request; check that VAST ignores it.

## Suggested order

Items 2, 3, 4, 7 and 9 are small and independent. Item 1 needs its design settled before any
code. Items 5 and 10 are the same arithmetic and belong in one change.
