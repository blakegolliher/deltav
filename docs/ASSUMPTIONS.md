# Assumptions about the VAST API

The VAST client was written against live responses from one cluster (VAST 5.5.3, read on
2026-11-17; sanitized copies in `testdata/vast/v5.5.3`), not against assumed fixtures. Every
field Delta-V reads was present there. What follows is what that one look could not establish.
None of it is presented as fact in the code; each item says what Delta-V does about it.

| Assumption | Why it is only an assumption | What Delta-V does |
|---|---|---|
| Other VAST versions return the same fields. | One cluster, one version. The build prompt's API excerpt was for 5.3 and was not available. | Unknown fields are ignored; a missing field decodes to its zero value, which makes a CNode unhealthy rather than healthy. |
| The three lists are never paginated. | 3 CNodes, 48 pools and 187 VIPs each came back as one array. Larger clusters were not seen. | Decodes one array. A paginated (object) response fails to decode and shows as a poll error, with the last good endpoints kept. |
| `ACTIVE` is the state of a CNode in service, and no other state serves S3. | It is the only `state` value seen. | Healthy means `enabled` and `state == "ACTIVE"`. Any other state removes that CNode's VIPs. |
| A VIP's `cnode` always names a CNode in the CNode list. | True for all 187 VIPs seen. | A VIP naming an unlisted CNode is left out. |
| A pool name is unique within a tenant. | 48 pools, 48 distinct names, 30 tenants. | A target may name the tenant; an ambiguous name is refused with a message saying so. |
| VAST moves a failed CNode's VIPs to other CNodes by itself. | Not observed; no CNode failed. | Does not depend on it: the VIPs of an unhealthy CNode are simply not served until it is healthy or they move. |
| A disabled pool should not be served. | Every pool seen was enabled. | A target on a disabled pool resolves to no endpoints (after the three-poll hold). |
| Access tokens can expire at any time. | The lifetime was not measured. | On a 401 the client refreshes the token, or logs in again, and retries once. |
| The user can see every tenant's pools. | The lab user was an administrator. | A pool the user cannot see is reported as not found. |
| VAST finds a bucket in a host name under the DNS suffix of `GET /api/dns/`, with or without a pool's `domain_name` before the suffix; `<pool>.<suffix>` itself and every other host name are read by path. | Seen on the one cluster by asking for an object under thirteen host names (2026-11-19). That cluster has one DNS object, enabled, and one suffix. What VAST does with its DNS service disabled, with several suffixes, or with a pool label that belongs to another tenant was not seen. | Rate rules read the bucket this way, using every suffix and every pool label the cluster lists. `make live` asks the same questions of whatever cluster it is pointed at and fails if a rule is avoided. |
| The user can read `GET /api/dns/`. | The lab user was an administrator. | If it cannot be read, nothing else fails: the names last read stay in force, the target's status says why they may be stale, and with none ever read a bucket named in a host name is not recognized by rules. |
| VAST takes a request's access key from a presigned URL's query before the `Authorization` header, and from the first `credential=` in the header in any letter case. | Seen on the one cluster with thirteen variations (2026-11-19). Signature version 2 and browser form uploads, which carry the key elsewhere, were not tried. | Rules do not depend on the order: a request naming more than one access key is refused while any rule looks at access keys. A form upload's key is in its body, which the script does not read; such a request counts as having no access key. |
