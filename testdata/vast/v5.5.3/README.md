# VAST API fixtures

Sanitized responses of three list calls on a VAST 5.5.3 lab cluster, read on 2026-11-17 with
`dvlab vastprobe` (GET only): `cnodes.json` (`GET /api/cnodes/`, 3 CNodes), `vippools.json`
(`GET /api/vippools/`, 48 pools) and `vips.json` (`GET /api/vips/`, 187 VIPs).

Field names, structure, numbers and booleans are exactly as the cluster returned them, with
object keys sorted. String values that identify the site were replaced by
`internal/lab/sanitize.go`, consistently across the three files, so a VIP still names its pool
and its CNode:

- IP addresses map, in address order, onto the documentation range `192.0.2.0/24`; IPv6 onto
  `2001:db8::/32`. Adjacent real addresses stay adjacent.
- GUIDs, serial numbers, chassis names, host names, the cluster's name and DNS domain, tenant
  names, pool names and pool DNS names are replaced by numbered placeholders. The generic names
  `main` and `default` are kept.
- States, roles, versions and VAST's own node names (`cnode-3-10`) are untouched.

To refresh them, export `VAST_HOST`, `VAST_USER` and `VAST_PASSWORD` and run
`bin/dvlab vastprobe --out testdata/vast/v5.5.3`. Read the field table it prints and the files
before committing: the sanitizer cannot know what a free-form field holds.
