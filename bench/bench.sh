#!/usr/bin/env bash
# adbdav benchmark — reproduces every number the README quotes.
#
# It measures the WebDAV layer against the device layer, because that is the
# only comparison that tells you where time goes: adbdav can only ever be as
# fast as adb, and every bug worth fixing showed up as a gap between the two.
#
#   ./bench/bench.sh [-n runs] [-d device-dir] [-o out.md]
#
# Read-only: it lists, stats, reads and hashes. It never writes to the device.
set -euo pipefail

cd "$(dirname "$0")/.."

RUNS=5
DIR=""
OUT=""
while getopts "n:d:o:h" opt; do
  case "$opt" in
    n) RUNS=$OPTARG ;;
    d) DIR=$OPTARG ;;
    o) OUT=$OPTARG ;;
    h) sed -n '2,12p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done

have() { command -v "$1" >/dev/null 2>&1; }
have curl || { echo "bench: curl is required" >&2; exit 1; }
have perl || { echo "bench: perl is required (high-resolution clock)" >&2; exit 1; }

ADB=$(command -v adb || echo /opt/homebrew/share/android-commandlinetools/platform-tools/adb)
[ -x "$ADB" ] || { echo "bench: no adb" >&2; exit 1; }
[ -x ./adbdav ] || { echo "bench: build first — make build" >&2; exit 1; }

now()    { perl -MTime::HiRes=time -e 'printf "%.6f", time'; }
elapsed(){ perl -e 'printf "%.3f", $ARGV[1]-$ARGV[0]' "$1" "$(now)"; }
# Three decimals is the honest precision here: the clock is a shell round
# trip and the device is a phone over USB.
r3() { [ "$1" = "-" ] && { echo -; return; }; perl -e 'printf "%.3f", $ARGV[0]' "$1"; }

# Median of the numbers on stdin. A mean would be dominated by the one run
# that collided with whatever else the phone was doing.
median() { sort -n | awk '{v[NR]=$1} END{ if(NR==0){print "-"; exit} print (NR%2) ? v[(NR+1)/2] : (v[NR/2]+v[NR/2+1])/2 }'; }

# ---------------------------------------------------------------- the device
ROOT=/sdcard
SERIAL=$("$ADB" devices | awk 'NR>1 && $2=="device" {print $1; exit}')
[ -n "$SERIAL" ] || { echo "bench: no device attached" >&2; exit 1; }

# A directory with enough entries to make a listing meaningful, and the
# largest file under 256 MB to read. Both are chosen, never created.
if [ -z "$DIR" ]; then DIR=$ROOT; fi
# toybox find rejects `-size -256M`, so the size filter is done here: the
# largest file between 8 MB and 256 MB, which is big enough to measure
# throughput and small enough not to make the benchmark take minutes.
PICK=$("$ADB" exec-out "find -H $DIR -maxdepth 3 -type f -exec stat -Lc '%s|%n' {} + 2>/dev/null" \
       | tr -d '\r' | awk -F'|' '$1>8388608 && $1<268435456' | sort -t'|' -k1 -rn | head -1)
FILE=${PICK#*|}
FSIZE=${PICK%%|*}
[ -n "$PICK" ] || { FILE=""; FSIZE=0; }
ENTRIES=$("$ADB" exec-out "find -H $DIR -maxdepth 1 -mindepth 1 2>/dev/null | wc -l" | tr -d '\r\n ')
# The report names the MODEL, never the serial: a serial identifies one
# physical phone and benchmark output gets pasted into public places.
MODEL=$("$ADB" -s "$SERIAL" exec-out "getprop ro.product.model" | tr -d '\r\n')

echo "device   : $SERIAL"
echo "listing  : $DIR ($ENTRIES entries)"
echo "read file: ${FILE:-none found} (${FSIZE} bytes)"
echo "runs     : $RUNS (median reported)"
echo

# ------------------------------------------------------------- the server
PORT=18099
start_server() { # $1 = cache TTL in ms
  ./adbdav serve -port "$PORT" -cache "$1" >/tmp/adbdav-bench.$$ 2>&1 &
  SRV=$!
  for _ in $(seq 1 50); do
    URL=$(awk '/^serving /{print $NF; exit}' /tmp/adbdav-bench.$$ 2>/dev/null || true)
    [ -n "${URL:-}" ] && return 0
    perl -e 'select undef,undef,undef,0.1'
  done
  echo "bench: server did not start" >&2; cat /tmp/adbdav-bench.$$ >&2; exit 1
}
stop_server() { [ -n "${SRV:-}" ] && kill "$SRV" 2>/dev/null || true; wait "${SRV:-}" 2>/dev/null || true; SRV=""; rm -f /tmp/adbdav-bench.$$; }
trap stop_server EXIT

# curl against the served root. %{time_total} is the whole request, which is
# what Finder waits for.
propfind() { # $1 = depth, $2 = path suffix
  curl -s -o /dev/null -w '%{time_total}' -X PROPFIND -H "Depth: $1" "$URL$2"
}

# A benchmark that measures failed requests reads as "impossibly fast", which
# is exactly how the first version of this script lied. Every phase asserts
# the status code before it times anything.
expect() { # $1 = expected code, $2.. = curl args
  local want=$1; shift
  local got
  got=$(curl -s -o /dev/null -w '%{http_code}' "$@")
  [ "$got" = "$want" ] || { echo "bench: expected HTTP $want, got $got from: $*" >&2; exit 1; }
}

# ------------------------------------------------------------ measurements
echo "running…"

# 1. the floor: what the device itself takes to answer the same listing.
DEV_LIST=$(for _ in $(seq 1 "$RUNS"); do
  t=$(now); "$ADB" exec-out "find -H $DIR -maxdepth 1 -mindepth 1 -exec stat -Lc '%F|%s|%Y|%n' {} + 2>/dev/null" >/dev/null; elapsed "$t"; echo
done | median)

# 2. one round trip, for scale: PROPFIND Depth:0 is a single stat.
start_server 0
echo "server   : $URL"
expect 207 -X PROPFIND -H "Depth: 1" "$URL/"
WD_STAT=$(for _ in $(seq 1 "$RUNS"); do propfind 0 /; echo; done | median)

# 3. the pathology, with the cache switched off entirely: webdav's walkFS
#    re-Stats every child after Readdir, so a listing costs one adb round
#    trip PER ENTRY. This is what the bridge does if you write the obvious
#    thing, and it is why the stat cache exists.
WD_NOCACHE=$(for _ in $(seq 1 3); do propfind 1 /; echo; done | median)
stop_server

# 4. cold, as a user meets it: a fresh server, nothing cached, one listing.
#    The server is restarted for every run because the cache is in-process —
#    timing a second request would silently measure the warm path, which is
#    the mistake the first version of this script made.
COLD=$(for _ in $(seq 1 "$RUNS"); do
  start_server 5000
  propfind 1 /
  echo
  stop_server
done | median)

# 5. warm: Finder issues PROPFINDs in bursts, so the second one through the
#    cache is the one a user actually feels.
start_server 5000
expect 207 -X PROPFIND -H "Depth: 1" "$URL/"
propfind 1 / >/dev/null
WD_WARM=$(for _ in $(seq 1 "$RUNS"); do propfind 1 /; echo; done | median)

# 6. reads. The HTTP GET is compared against `adb exec-out cat`, which is the
#    same bytes over the same cable with no WebDAV in the way.
GET_S="-"; GET_MBS="-"; ADB_MBS="-"; RANGE="-"; HASH="-"
if [ -n "$FILE" ] && [ "$FSIZE" -gt 0 ]; then
  REL=${FILE#"$ROOT"/}
  ENC=$(perl -MURI::Escape -e 'print uri_escape($ARGV[0], "^A-Za-z0-9\-._~/")' "$REL" 2>/dev/null || echo "$REL")
  expect 200 "$URL/$ENC"
  GET_S=$(for _ in $(seq 1 3); do
    curl -s -o /dev/null -w '%{time_total}' "$URL/$ENC"; echo
  done | median)
  GET_MBS=$(perl -e 'printf "%.1f", $ARGV[0]/1048576/$ARGV[1]' "$FSIZE" "$GET_S")
  A=$(for _ in $(seq 1 3); do
    t=$(now); "$ADB" exec-out "cat '$FILE' 2>/dev/null" >/dev/null; elapsed "$t"; echo
  done | median)
  ADB_MBS=$(perl -e 'printf "%.1f", $ARGV[0]/1048576/$ARGV[1]' "$FSIZE" "$A")
  RANGE=$(for _ in $(seq 1 "$RUNS"); do
    curl -s -o /dev/null -r 0-1048575 -w '%{time_total}' "$URL/$ENC"; echo
  done | median)
  HASH=$(for _ in $(seq 1 3); do
    t=$(now); "$ADB" exec-out "sha256sum '$FILE' 2>/dev/null" >/dev/null; elapsed "$t"; echo
  done | median)
fi
stop_server

# ---------------------------------------------------------------- report
for v in DEV_LIST COLD WD_WARM WD_NOCACHE WD_STAT GET_S RANGE HASH; do
  eval "$v=\$(r3 \"\$$v\")"
done
MB=$(perl -e 'printf "%.0f", $ARGV[0]/1048576' "${FSIZE:-0}")
report() {
  cat <<EOF
| Operation | Measured | Reference |
|---|---|---|
| **Listing a $ENTRIES-entry directory** — on the device itself (\`find\` + \`stat\` over \`adb\`) | ${DEV_LIST}s | the floor |
| &nbsp;&nbsp;↳ PROPFIND \`Depth: 1\`, cold (fresh server, nothing cached) | ${COLD}s | |
| &nbsp;&nbsp;↳ PROPFIND \`Depth: 1\`, warm | ${WD_WARM}s | |
| &nbsp;&nbsp;↳ PROPFIND \`Depth: 1\`, **\`-cache 0\`** — one \`adb\` round trip per entry | ${WD_NOCACHE}s | what a naive bridge costs |
| **Single \`stat\`** — PROPFIND \`Depth: 0\`, uncached | ${WD_STAT}s | one \`adb\` round trip |
| **Read ${MB} MB** — HTTP GET through the mount | ${GET_S}s (${GET_MBS} MB/s) | \`adb exec-out cat\`: ${ADB_MBS} MB/s |
| &nbsp;&nbsp;↳ first 1 MiB only — ranged GET | ${RANGE}s | |
| **Hash ${MB} MB on the device** — \`sha256sum\` for write verification | ${HASH}s | nothing transferred |

Device: ${MODEL:-unknown}. $RUNS runs each, median. Reads are of a ${MB} MB file.
Reproduce with \`./bench/bench.sh\`.
EOF
}
echo
report
if [ -n "$OUT" ]; then report > "$OUT"; echo "wrote $OUT"; fi
