#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# The load tests of docs/wiki/Load-testing.md: how many calls, cells and handsets one machine carries.
#
#   tools/load-test.sh build [OUT_DIR] [ARCH]   on the development machine: builds the three test binaries (default
#                                               ./loadtest-bin, arm64) with docker; copy OUT_DIR and this script to the machine
#   tools/load-test.sh calls KINDS N [N...]     on the machine under test: N calls at once, one step per N; KINDS is a comma
#                                               list of g722, pcma-wide, pcma-narrow (see the manual)
#   tools/load-test.sh cells                    on the machine under test: 100 and 300 simulated cells, and 500 handsets on 100
#                                               cells; needs OPENDECTCELL_TEST_DATABASE_URL (a throwaway PostgreSQL, see the manual)
#
# Settings (environment): LOADTEST_BIN (where the binaries are, default the script's directory or ./loadtest-bin),
# LOADTEST_SECONDS (measured time per step, default 20), LOADTEST_MAX_TEMP (stop above this many degrees, default 80),
# LOADTEST_COOL_TEMP (wait until the board is below this before a step, default 74; both need /sys/class/thermal).
# Never run it on a controller that carries real calls: it fills the machine on purpose.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)

die() { echo "load-test: $*" >&2; exit 1; }

bin_dir() {
	for d in "${LOADTEST_BIN:-}" "$HERE" "$PWD/loadtest-bin" "$HERE/loadtest-bin"; do
		[ -n "$d" ] && [ -x "$d/media.test" ] && { echo "$d"; return; }
	done
	die "media.test not found: set LOADTEST_BIN to the directory 'build' wrote"
}

temp() {
	if [ -r /sys/class/thermal/thermal_zone0/temp ]; then
		echo $(($(cat /sys/class/thermal/thermal_zone0/temp) / 1000))
	else
		echo 0
	fi
}

build() {
	out=${1:-$PWD/loadtest-bin}
	arch=${2:-arm64}
	command -v docker >/dev/null || die "docker is needed to build (or build by hand, see the manual)"
	image=$(sed -n 's/^FROM \(golang:[^ @]*\).*/\1/p' "$REPO/Dockerfile" | head -1)
	[ -n "$image" ] || die "no golang image in the Dockerfile"
	mkdir -p "$out"
	out=$(cd "$out" && pwd)
	echo "== building for linux/$arch with $image into $out"
	docker run --rm -v "$REPO":/src -v "$out":/out -w /src -e GOOS=linux -e GOARCH="$arch" "$image" sh -c '
		set -e
		go test -c -o /out/media.test ./internal/media
		go test -c -o /out/cell.test ./internal/cell
		go test -c -o /out/core.test ./internal/dect/core'
	cp "$HERE/load-test.sh" "$out/load-test.sh"
	ls -l "$out"
	echo "== copy $out to the machine under test and run ./load-test.sh there"
}

# Prints one line per RESULT line of a step.
summarise() {
	if command -v python3 >/dev/null; then
		python3 -c '
import sys, json
for l in sys.stdin:
    if not l.startswith("RESULT "):
        sys.stdout.write(l); continue
    r = json.loads(l[7:]); p = r["peers"]; s = p["Summary"]
    print("calls=%d %s engine_cores=%s per_call=%s machine_busy=%s%% rss=%sMiB loss(cell/sip)=%s/%s%% worst_p99_variation(cell/sip)=%s/%sms max_gap=%sms generator_late=%s/%s concealed(d/u)=%s/%s" % (
        r["calls"], r["kinds"], r["engine_cores"], r["engine_cores_per_call"], r["machine_busy_pct"], r["engine_rss_mib"],
        s["to_cell_lost_pct"], s["to_sip_lost_pct"], s["to_cell_worst_p99_delay_var_ms"], s["to_sip_worst_p99_delay_var_ms"],
        max(s["to_cell_max_gap_ms"], s["to_sip_max_gap_ms"]), p["GeneratorLateTicks"], p["TotalTicks"],
        r["down_concealed"], r["up_concealed"]))
'
	else
		cat
	fi
}

calls() {
	[ $# -ge 2 ] || die "usage: calls KINDS N [N...]"
	kinds=$1
	shift
	bin=$(bin_dir)
	max=${LOADTEST_MAX_TEMP:-80}
	cool=${LOADTEST_COOL_TEMP:-74}
	echo "# $(date -u +%FT%TZ) $(uname -m), $(nproc) cores, kinds $kinds"
	for n in "$@"; do
		i=0
		while [ "$(temp)" -gt "$cool" ] && [ $i -lt 24 ]; do sleep 5; i=$((i + 1)); done
		t=$(temp)
		[ "$t" -gt "$max" ] && { echo "stop: the board is at ${t} degrees"; break; }
		echo "== $n calls, board at ${t} degrees"
		OPENDECTCELL_LOAD_CALLS=$n OPENDECTCELL_LOAD_KINDS=$kinds OPENDECTCELL_LOAD_SECONDS=${LOADTEST_SECONDS:-20} \
			nice -n 5 "$bin/media.test" -test.run '^TestLoadEngine$' -test.v 2>&1 | grep -E '^RESULT |FAIL|panic|load_test' | summarise
		echo "   board at $(temp) degrees, load average $(cut -d' ' -f1 /proc/loadavg)"
		sleep 8
	done
}

cells() {
	[ -n "${OPENDECTCELL_TEST_DATABASE_URL:-}" ] || die "OPENDECTCELL_TEST_DATABASE_URL is not set (a throwaway PostgreSQL, see the manual)"
	bin=$(bin_dir)
	for n in 100 300; do
		echo "== $n simulated cells"
		OPENDECTCELL_SCALE_CELLS=$n nice -n 5 "$bin/cell.test" -test.run '^TestManyCellsStayConnected$' -test.v 2>&1 | grep -E 'scale_test|PASS|FAIL|panic'
	done
	echo "== 500 handsets on 100 cells"
	"$bin/core.test" -test.run '^TestStoreScale$' -test.v 2>&1 | grep -E 'scale_test|PASS|FAIL|panic'
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
build) build "$@" ;;
calls) calls "$@" ;;
cells) cells ;;
*) sed -n '4,17p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
