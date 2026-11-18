package ratelimit

import (
	"fmt"
	"sort"
	"strings"
)

// The part of rate limiting that runs inside Envoy, for every request, is
// one small Lua script: it works out the S3 operation, the access key, the
// bucket and the key, and decides which rules the request matches.
//
// A request that matches a blocked rule is answered by the script itself.
// For each other rule it matches, the script fills the next free slot: three
// request headers that name the rule, what the rule counts the request
// under, and the status a refusal should carry. Envoy's rate limit quota
// filters, one per slot, each look at their own slot's headers and nothing
// else (internal/xds). The headers are removed before the request goes on to
// VAST, and any a client sends itself are discarded first, so a client
// cannot choose what it is counted as.
//
// Slots, and not a filter per rule, so that the listener never has to change
// when the rules do: Envoy applies a changed listener to new connections
// only, and a rule must reach the ones that are open. The script travels
// with the route, which Envoy applies to the next request on every
// connection. The price is that one request can be limited by at most Slots
// rules: if it matches more, the first Slots in the order of the rules
// count, and the script says so in the request's metadata.
//
// Lua, because recognizing 110 S3 operations and matching prefixes is a page
// of script and a wall of matcher configuration; Envoy's release binaries
// include it. The script is generated: the operation table and the rules are
// data in it, written as string literals that cannot break out, whatever a
// rule's values contain.

// Slots is how many rules can limit one request.
const Slots = 4

// Names shared by the script and the daemon.
const (
	// MetadataNamespace is where the script leaves notes about a request
	// in Envoy's dynamic metadata, which reaches the daemon in the access
	// log: MetadataRefusedBy names the blocked rule that refused it, and
	// MetadataSkipped is set when it matched more rules than there are
	// slots.
	MetadataNamespace = "deltav"
	MetadataRefusedBy = "refused_by"
	MetadataSkipped   = "rules_skipped"
	// For a request that matches any rule, the script also notes what it
	// took the request to be, so that a refusal can be told from the
	// daemon's side: the rules it was counted under (comma-separated, in
	// slot order), its operation, access key and bucket. The last two are
	// percent-encoded as in a count's name, and cut at 200 bytes.
	MetadataRules     = "rules"
	MetadataOperation = "operation"
	MetadataAccessKey = "access_key"
	MetadataBucket    = "bucket"
)

// RefusedDetails is the response code details Envoy 1.40 gives a request
// its rate limit quota filter refused (filter.cc).
const RefusedDetails = "rate_limited_by_quota"

// Slot headers. j runs from 1 to Slots.
func SlotKeyHeader(j int) string    { return fmt.Sprintf("x-deltav-rl-%d", j) }
func SlotRuleHeader(j int) string   { return fmt.Sprintf("x-deltav-rr-%d", j) }
func SlotStatusHeader(j int) string { return fmt.Sprintf("x-deltav-rs-%d", j) }

// SlotHeaders returns every header the script may set, for the route to
// remove before a request goes upstream.
func SlotHeaders() []string {
	var out []string
	for j := 1; j <= Slots; j++ {
		out = append(out, SlotKeyHeader(j), SlotRuleHeader(j), SlotStatusHeader(j))
	}
	return out
}

// RefusalBody is the body of a refused request: S3's own error document, so
// that every S3 client treats the refusal as S3 throttling.
func RefusalBody(status int) string {
	code := "SlowDown"
	if status == 429 {
		code = "TooManyRequests"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>` + code + `</Code><Message>Please reduce your request rate.</Message></Error>`
}

// luaString writes s as a Lua string literal. Every byte that is not a
// letter, a digit or one of a few plain marks becomes a decimal escape, so
// no value can end the literal or the line.
func luaString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte(" ._/-", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\%03d", c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func luaSet(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = "[" + luaString(v) + "]=true"
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func luaList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = luaString(v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Script returns the Lua source for the given rules, which must have been
// normalized.
func Script(rules []Rule) string {
	var b strings.Builder
	b.WriteString("-- Delta-V: recognizes each S3 request and marks the rate-limit rules it matches.\n")
	b.WriteString("-- Generated by the daemon from its rules. Do not edit.\n")

	// The operation table: ROWS[method][level+1] is a list of rows.
	b.WriteString("local ROWS = {}\n")
	b.WriteString("local function row(method, level, r) ROWS[method] = ROWS[method] or {{}, {}, {}}; local t = ROWS[method][level + 1]; t[#t + 1] = r end\n")
	for _, r := range opTable {
		fields := []string{"op=" + luaString(r.op)}
		if len(r.query) > 0 {
			fields = append(fields, "q="+luaList(r.query))
		}
		if r.key != "" {
			fields = append(fields, "k="+luaString(r.key), "v="+luaString(r.value))
		}
		if r.copy {
			fields = append(fields, "c=true")
		}
		fmt.Fprintf(&b, "row(%s, %d, {%s})\n", luaString(r.method), r.level, strings.Join(fields, ","))
	}
	var lists []string
	for op := range listOps {
		lists = append(lists, op)
	}
	sort.Strings(lists)
	fmt.Fprintf(&b, "local LIST = %s\n", luaSet(lists))

	fmt.Fprintf(&b, "local SLOTS = %d\n", Slots)
	fmt.Fprintf(&b, "local NS, REFUSED_BY, SKIPPED, MATCHED = %s, %s, %s, %s\n",
		luaString(MetadataNamespace), luaString(MetadataRefusedBy), luaString(MetadataSkipped), luaString(MetadataRules))
	fmt.Fprintf(&b, "local OPERATION, ACCESS_KEY, BUCKET = %s, %s, %s\n",
		luaString(MetadataOperation), luaString(MetadataAccessKey), luaString(MetadataBucket))
	fmt.Fprintf(&b, "local BODY = {[503]=%s,[429]=%s}\n", luaString(RefusalBody(503)), luaString(RefusalBody(429)))
	b.WriteString("local RULES = {\n")
	for _, r := range rules {
		fields := []string{"name=" + luaString(r.Name), fmt.Sprintf("status=%d", r.Respond)}
		if r.Blocked {
			fields = append(fields, "blocked=true")
		}
		if ops := r.Ops(); ops != nil {
			fields = append(fields, "ops="+luaSet(ops))
		}
		if len(r.AccessKey) > 0 {
			fields = append(fields, "aks="+luaSet(r.AccessKey))
		}
		if len(r.Bucket) > 0 {
			fields = append(fields, "buckets="+luaSet(r.Bucket))
		}
		if len(r.Prefix) > 0 {
			// Longest first: the longest prefix that matches is the one counted.
			prefixes := append([]string(nil), r.Prefix...)
			sort.SliceStable(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
			fields = append(fields, "prefixes="+luaList(prefixes))
		}
		// What the count is kept per, in a fixed order. The client host is
		// not in it: each Envoy is its own client, and the daemon keeps the
		// counts of such a rule apart per Envoy.
		var per []string
		for _, p := range []string{PerAccessKey, PerBucket, PerPrefix} {
			if r.Counts(p) {
				per = append(per, p)
			}
		}
		fields = append(fields, "per="+luaList(per))
		fmt.Fprintf(&b, "  {%s},\n", strings.Join(fields, ","))
	}
	b.WriteString("}\n")
	b.WriteString(luaLibrary)
	return b.String()
}

// luaLibrary is the fixed part of the script. It is Lua 5.1, which is what
// Envoy's LuaJIT runs.
const luaLibrary = `
local deltav = {}

-- Percent-decoding, once. A "+" stays a "+".
local function unescape(s)
  return (string.gsub(s, "%%(%x%x)", function(h) return string.char(tonumber(h, 16)) end))
end

-- Percent-encoding of everything that is not plainly safe in a header value
-- and as part of a comma-separated list.
local function escape(s)
  return (string.gsub(s, "[^%w%._~/%-]", function(c) return string.format("%%%02X", string.byte(c)) end))
end

-- A raw query string as a table of key to raw value. Keys are compared as
-- the bytes on the wire, as S3 does; the first of a repeated key wins.
local function scan(raw)
  local q = {}
  for pair in string.gmatch(raw, "[^&]+") do
    local eq = string.find(pair, "=", 1, true)
    local k, v = pair, ""
    if eq then k, v = string.sub(pair, 1, eq - 1), string.sub(pair, eq + 1) end
    if q[k] == nil then q[k] = v end
  end
  return q
end

-- The request path as bucket, raw key, level (0 service, 1 bucket, 2 key)
-- and raw query. Path-style addressing: the bucket is the first segment.
-- The bucket is percent-decoded, as the key is later: a server decodes the
-- path before it looks the bucket up, so "arch%69ve" must not pass for some
-- other bucket than "archive". Empty segments before the bucket are skipped
-- for the same reason: whatever a server makes of "//archive/k", a rule
-- about archive is the one to apply.
local function split(path)
  local p, raw = path, ""
  local qm = string.find(path, "?", 1, true)
  if qm then p, raw = string.sub(path, 1, qm - 1), string.sub(path, qm + 1) end
  local bucket, key = string.match(p, "^/+([^/]*)/?(.*)$")
  bucket, key = bucket or "", key or ""
  local level = 0
  if bucket ~= "" then
    level = 1
    if key ~= "" then level = 2 end
  end
  return unescape(bucket), key, level, raw
end

local function classify(method, level, q, copy)
  if method == "OPTIONS" then return "Preflight" end
  local rows = ROWS[method] and ROWS[method][level + 1]
  if rows then
    for _, r in ipairs(rows) do
      local ok = true
      if r.q then
        for _, k in ipairs(r.q) do
          if q[k] == nil then ok = false; break end
        end
      end
      if ok and r.k and q[r.k] ~= r.v then ok = false end
      if ok and r.c and not copy then ok = false end
      if ok then
        if r.op == "" then return "Unknown" end
        return r.op
      end
    end
  end
  return "Unknown"
end

-- The access key ID: from a SigV4 or SigV2 Authorization header, else from a
-- presigned URL's query. "-" if the request carries none.
local function access_key(auth, q)
  if auth then
    local ak = string.match(auth, "^AWS4%-HMAC%-SHA256.-Credential=([^/,%s]*)") or string.match(auth, "^AWS ([^:]*):")
    if ak and ak ~= "" then return ak end
  end
  local c = q["X-Amz-Credential"]
  if c then
    local ak = string.match(c, "^(.-)%%2[Ff]") or string.match(c, "^([^/]*)")
    if ak and ak ~= "" then return unescape(ak) end
  end
  local a = q["AWSAccessKeyId"]
  if a and a ~= "" then return unescape(a) end
  return "-"
end

-- What a request is: its operation, access key, bucket, and what a rule's
-- prefix is compared with (the key for an operation on a key, the prefix
-- parameter for a listing, nil for anything else).
function deltav.identify(method, path, auth, copy)
  local bucket, key, level, raw = split(path)
  local q = scan(raw)
  local op = classify(method, level, q, copy ~= nil and copy ~= "")
  local subject = nil
  if level == 2 then
    subject = unescape(key)
  elseif LIST[op] then
    subject = unescape((string.gsub(q["prefix"] or "", "+", " ")))
  end
  return op, access_key(auth, q), bucket, subject
end

-- The rules a request matches, in order, each with what that rule counts
-- the request under.
function deltav.match(op, ak, bucket, subject)
  local out = {}
  for _, r in ipairs(RULES) do
    local ok = (not r.ops or r.ops[op]) and (not r.aks or r.aks[ak]) and (not r.buckets or r.buckets[bucket])
    local prefix = nil
    if ok and r.prefixes then
      ok = false
      if subject then
        for _, p in ipairs(r.prefixes) do
          if string.sub(subject, 1, #p) == p then prefix = p; ok = true; break end
        end
      end
    end
    if ok then
      local parts = {}
      for _, per in ipairs(r.per or {}) do
        local v = ak
        if per == "bucket" then v = bucket elseif per == "prefix" then v = prefix end
        -- Never empty: a request with no bucket is counted under "-".
        if v == nil or v == "" then v = "-" end
        parts[#parts + 1] = escape(string.sub(v, 1, 200))
      end
      if #parts == 0 then parts[1] = "*" end
      out[#out + 1] = {rule = r, key = table.concat(parts, ",")}
    end
  end
  return out
end

-- What to do with a request: the blocked rule that refuses it, if any; else
-- what to put in the slots (a list of {rule, key}), and whether it matched
-- more rules than there are slots.
function deltav.decide(matches)
  local slots, skipped = {}, false
  for _, m in ipairs(matches) do
    if m.rule.blocked then return m.rule, {}, false end
    if #slots < SLOTS then slots[#slots + 1] = m else skipped = true end
  end
  return nil, slots, skipped
end

function envoy_on_request(handle)
  local headers = handle:headers()
  -- Whatever the client sent under these names is not to be believed.
  for j = 1, SLOTS do
    headers:remove("x-deltav-rl-" .. j)
    headers:remove("x-deltav-rr-" .. j)
    headers:remove("x-deltav-rs-" .. j)
  end
  -- A mistake in here must never cost a request: on any error the request
  -- goes on, unlimited.
  local refuse = nil
  local ok, err = pcall(function()
    local op, ak, bucket, subject = deltav.identify(
      headers:get(":method") or "", headers:get(":path") or "", headers:get("authorization"), headers:get("x-amz-copy-source"))
    local matches = deltav.match(op, ak, bucket, subject)
    if #matches == 0 then return end
    local slots, skipped
    refuse, slots, skipped = deltav.decide(matches)
    local names = {}
    for j, m in ipairs(slots) do
      headers:replace("x-deltav-rl-" .. j, m.key)
      headers:replace("x-deltav-rr-" .. j, m.rule.name)
      if m.rule.status == 429 then headers:replace("x-deltav-rs-" .. j, "429") end
      names[j] = m.rule.name
    end
    local md = handle:streamInfo():dynamicMetadata()
    md:set(NS, OPERATION, op)
    md:set(NS, ACCESS_KEY, escape(string.sub(ak, 1, 200)))
    md:set(NS, BUCKET, escape(string.sub(bucket, 1, 200)))
    if refuse then md:set(NS, REFUSED_BY, refuse.name) end
    if #names > 0 then md:set(NS, MATCHED, table.concat(names, ",")) end
    if skipped then md:set(NS, SKIPPED, "true") end
  end)
  if not ok then
    handle:logErr("deltav rate limit script: " .. tostring(err))
    return
  end
  if refuse then
    -- A blocked rule is refused here, at once, with nothing asked of the
    -- daemon: it holds whether or not the daemon can be reached. Outside
    -- the protected call above, because respond does not return.
    handle:respond(
      {[":status"] = tostring(refuse.status), ["content-type"] = "application/xml", ["retry-after"] = "1"},
      BODY[refuse.status] or BODY[503])
  end
end

-- For tests of the script outside Envoy.
__deltav = deltav
`
