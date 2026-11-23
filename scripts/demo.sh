#!/usr/bin/env bash
# Delta-V on one Linux host, with no VAST cluster: six fake S3 backends stand
# in for the VIPs of a mock VAST API, and the daemon, three real Envoys and a
# steady load run against them. Ctrl-C stops all of it and removes its files.
#
#   ./demo.sh                        from a release tarball
#   make build && scripts/demo.sh    from the repository
#
# It needs Envoy 1.40.2: $ENVOY_BIN if that is set; else the binary that
# `make envoy` fetched; else the release binary, fetched from Envoy's GitHub
# releases (about 100 MB) into ~/.cache/deltav and checked against its SHA-256.
#
# Ports, all on loopback, and the variable that moves each:
#   18080  web UI and API          DELTAV_DEMO_HTTP_PORT
#   18000  xDS and telemetry       DELTAV_DEMO_GRPC_PORT
#   9000   S3 through Envoy        DELTAV_DEMO_S3_PORT
#   9100   the six fake backends   DELTAV_DEMO_BACKEND_PORT (on 127.0.0.11 to .16)
set -euo pipefail

# Keep in step with the Makefile.
ENVOY_VERSION=1.40.2
ENVOY_SHA256_x86_64=1d64506e25c1844f146162ddb2ece7010fa4e784faddb40a079021491f1f03cc
ENVOY_SHA256_aarch_64=b9d6d1e06dd5838a3506de9f4deebad3de9a2ce6cadb87cd76222ca286213baf

die() { echo "demo: $*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "Linux only: the fake backends use the 127.x.y.z loopback range"

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ -x "$here/deltav" ]; then
	root=$here bin=$here                      # a release tarball
else
	root=$(dirname "$here") bin=$(dirname "$here")/bin   # the repository
fi
[ -x "$bin/deltav" ] && [ -x "$bin/dvlab" ] || die "no deltav and dvlab in $bin: run 'make build' first"
fixtures=$root/testdata/vast/v5.5.3
[ -r "$fixtures/vips.json" ] || die "the mock VAST API's fixtures are not in $fixtures"

http=${DELTAV_DEMO_HTTP_PORT:-18080}
grpc=${DELTAV_DEMO_GRPC_PORT:-18000}
s3=${DELTAV_DEMO_S3_PORT:-9000}
backend=${DELTAV_DEMO_BACKEND_PORT:-9100}

in_use() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null; }
in_use 127.0.0.1 "$http" && die "port $http is in use; set DELTAV_DEMO_HTTP_PORT to another"
in_use 127.0.0.1 "$grpc" && die "port $grpc is in use; set DELTAV_DEMO_GRPC_PORT to another"
in_use 127.0.0.1 "$s3" && die "port $s3 is in use; set DELTAV_DEMO_S3_PORT to another"
in_use 127.0.0.11 "$backend" && die "port $backend on 127.0.0.11 is in use; set DELTAV_DEMO_BACKEND_PORT to another"

# Envoy: the caller's, the repository's, or the pinned release, checked.
if [ -n "${ENVOY_BIN:-}" ]; then
	envoy=$ENVOY_BIN
	[ -x "$envoy" ] || die "ENVOY_BIN=$envoy is not an executable file"
else
	case "$(uname -m)" in
	x86_64) arch=x86_64 sum=$ENVOY_SHA256_x86_64 ;;
	aarch64 | arm64) arch=aarch_64 sum=$ENVOY_SHA256_aarch_64 ;;
	*) die "Envoy has no release binary for $(uname -m); set ENVOY_BIN to one you built" ;;
	esac
	file=envoy-$ENVOY_VERSION-linux-$arch
	envoy=$root/.cache/envoy/$file
	if [ ! -f "$envoy" ]; then
		envoy=${XDG_CACHE_HOME:-$HOME/.cache}/deltav/$file
		if [ ! -f "$envoy" ]; then
			command -v curl >/dev/null || die "curl is needed to fetch Envoy; or set ENVOY_BIN"
			echo "fetching Envoy $ENVOY_VERSION, about 100 MB, to $envoy"
			mkdir -p "$(dirname "$envoy")"
			curl -fL --progress-bar -o "$envoy.part" \
				"https://github.com/envoyproxy/envoy/releases/download/v$ENVOY_VERSION/$file"
			mv "$envoy.part" "$envoy"
		fi
	fi
	echo "$sum  $envoy" | sha256sum -c - >/dev/null 2>&1 ||
		die "$envoy does not match its pinned SHA-256; delete it and run this again"
	chmod +x "$envoy"
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/deltav-demo.XXXXXX")
names=() pids=()
cleanup() {
	trap - EXIT INT TERM
	if [ ${#pids[@]} -gt 0 ]; then
		kill "${pids[@]}" 2>/dev/null || true
		wait 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
trap 'echo; echo "stopping the demo"; exit 130' INT TERM

start() { # name, command...
	local name=$1
	shift
	"$@" >"$work/$name.log" 2>&1 &
	names+=("$name") pids+=($!)
}
fail() { # message, log name
	echo "demo: $1" >&2
	[ -z "${2:-}" ] || tail -n 15 "$work/$2.log" >&2
	exit 1
}
wait_for() { # what, log name, command...
	local what=$1 log=$2 i
	shift 2
	for i in $(seq 1 150); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 0.1
	done
	fail "$what did not come up; the end of its log:" "$log"
}

# Any pair of keys: the fake backends verify signatures made with this one.
export AWS_ACCESS_KEY_ID=dvlab AWS_SECRET_ACCESS_KEY=dvlab-secret
export DELTAV_API=http://127.0.0.1:$http
deltav=$bin/deltav dvlab=$bin/dvlab

ips=127.0.0.11,127.0.0.12,127.0.0.13,127.0.0.14,127.0.0.15,127.0.0.16
start fakes3 "$dvlab" fakes3 --listen "${ips//,/:$backend,}:$backend" --latency 20ms --jitter 20ms
start mockvast "$dvlab" mockvast --fixtures "$fixtures" --pool-vips "main=$ips"
start deltav "$deltav" serve --state-dir "$work/state" \
	--grpc-addr "127.0.0.1:$grpc" --http-addr "127.0.0.1:$http"

wait_for "the mock VAST API" mockvast grep -q '^mock VAST API at https://' "$work/mockvast.log"
vast=$(sed -n 's|^mock VAST API at https://\([^ ]*\) .*|\1|p' "$work/mockvast.log")
wait_for "the daemon" deltav "$deltav" vast list

DELTAV_PASSWORD=mock-password "$deltav" vast add mock --host "$vast" --user admin --insecure >/dev/null ||
	fail "the daemon could not attach the mock VAST API; the end of its log:" deltav
"$deltav" target add s3 --cluster mock --pool main --port "$backend" --listen-port "$s3" >/dev/null ||
	fail "the daemon could not add the target; the end of its log:" deltav
"$deltav" bootstrap --xds-address "127.0.0.1:$grpc" >"$work/envoy.json"

# Three Envoys on one host share the listen port, as three client hosts
# would each have it to themselves.
for name in client-a client-b client-c; do
	start "envoy-$name" "$envoy" --config-path "$work/envoy.json" --service-node "$name" \
		--concurrency 2 --config-yaml 'node: {metadata: {deltav_workers: 2}}' \
		--disable-hot-restart --log-level warn
done
wait_for "Envoy" envoy-client-a in_use 127.0.0.1 "$s3"
sleep 1 # the other two

start loadgen "$dvlab" loadgen --endpoint "http://127.0.0.1:$s3" --bucket demo \
	--duration 24h --concurrency 8 --put-ratio 0.3

# How to call the CLI from where this was started, as briefly as it can be put.
cli=$deltav
case $deltav in "$PWD"/*) cli=./${deltav#"$PWD"/} ;; esac
[ "$http" = 18080 ] || cli="DELTAV_API=$DELTAV_API $cli"
cat <<EOF

Delta-V demo is up: three Envoys, six VIPs on the three CNodes of a mock VAST
cluster, and a steady load of PUTs and GETs.

  Web UI        http://127.0.0.1:$http/
                from another machine: ssh -L $http:127.0.0.1:$http <this host>
  Terminal UI   $cli tui
  S3 endpoint   http://127.0.0.1:$s3  (path-style; access key dvlab, secret dvlab-secret)

To try, in another terminal:

  $cli node list --vips
  $cli node drain cnode-3-11      # its VIPs stop taking requests; "undrain" brings them back
  $cli limit set s3 --per-cnode 2 # alert mode: CNodes over 2 requests in flight are flagged
  $cli rule set slow-gets --operation GetObject --per-second 20 --per total
  $cli rule list                  # what the rule lets through and refuses

Ctrl-C here stops everything.
EOF

# Run until interrupted; if a process stops by itself, say which and why.
wait -n || true
for i in "${!pids[@]}"; do
	kill -0 "${pids[$i]}" 2>/dev/null || fail "${names[$i]} stopped; the end of its log:" "${names[$i]}"
done
