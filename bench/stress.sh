#!/usr/bin/env bash
# adbdav stress test — concurrent listings and reads against one server.
#
#   ./bench/stress.sh [-s seconds] [-w workers]
#
# Finder does not issue one request at a time, and neither does Spotlight.
# This drives the server the way they do and reports anything that stalls.
# If a request takes longer than the stall threshold, the server is sent
# SIGQUIT so its goroutine dump lands in the log instead of being lost —
# a hang is only diagnosable if you catch it while it is hanging.
#
# Read-only: it lists and reads. It never writes to the device.
set -euo pipefail
cd "$(dirname "$0")/.."

SECS=60
WORKERS=4
STALL=60
while getopts "s:w:t:h" opt; do
  case "$opt" in
    s) SECS=$OPTARG ;;
    w) WORKERS=$OPTARG ;;
    t) STALL=$OPTARG ;;
    h) sed -n '2,12p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done

ADB=$(command -v adb || echo /opt/homebrew/share/android-commandlinetools/platform-tools/adb)
[ -x ./adbdav ] || { echo "stress: build first — make build" >&2; exit 1; }

LOG=${LOG:-/tmp/adbdav-stress.log}
: > "$LOG"
./adbdav serve -port 18097 -cache 5000 >>"$LOG" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null || true' EXIT
URL=""
for _ in $(seq 1 50); do
  URL=$(awk '/^serving /{print $NF; exit}' "$LOG" || true)
  [ -n "$URL" ] && break
  perl -e 'select undef,undef,undef,0.1'
done
[ -n "$URL" ] || { echo "stress: server did not start" >&2; cat "$LOG" >&2; exit 1; }
echo "server : $URL"

# The biggest file under 256 MB, same choice the benchmark makes.
PICK=$("$ADB" exec-out "find -H /sdcard -maxdepth 3 -type f -exec stat -Lc '%s|%n' {} + 2>/dev/null" \
       | tr -d '\r' | awk -F'|' '$1>8388608 && $1<268435456' | sort -t'|' -k1 -rn | head -1)
FILE=${PICK#*|}
ENC=$(perl -MURI::Escape -e 'print uri_escape($ARGV[0], "^A-Za-z0-9\-._~/")' "${FILE#/sdcard/}")
echo "file   : $((${PICK%%|*} / 1048576)) MB"
echo "running: $WORKERS workers, ${SECS}s, stall threshold ${STALL}s"

STALLED=0
# A stall is caught by curl's own timeout, not by watching the clock after
# the fact: -m makes the request fail at the threshold so the worker can say
# so while the server is still wedged.
hit() { # $1 = label, $2.. = curl args
  local label=$1; shift
  local t
  if ! t=$(curl -s -o /dev/null -m "$STALL" -w '%{time_total}' "$@"); then
    echo "STALL: $label exceeded ${STALL}s"
    kill -QUIT $SRV 2>/dev/null || true
    STALLED=1
    return 1
  fi
  case $t in 1[0-9].*|[2-9][0-9].*|[0-9][0-9][0-9]*) echo "slow: $label ${t}s" ;; esac
}

worker() {
  local n=$1 end=$((SECONDS + SECS)) ops=0
  while [ $SECONDS -lt $end ]; do
    hit "w$n propfind"   -X PROPFIND -H "Depth: 1" "$URL/" || return 1
    hit "w$n range"      -r 0-1048575 "$URL/$ENC"          || return 1
    if [ $((ops % 5)) = 0 ]; then
      hit "w$n full-get" "$URL/$ENC" || return 1
    fi
    ops=$((ops + 1))
  done
  echo "w$n: $ops rounds"
}

# Wait for the WORKERS, by pid. A bare `wait` also waits for the server,
# which never exits — the script then hangs after the last worker has
# finished and looks exactly like the stall it is supposed to detect.
rc=0
pids=()
for i in $(seq 1 "$WORKERS"); do worker "$i" & pids+=("$!"); done
for p in "${pids[@]}"; do wait "$p" || rc=1; done
if [ "$rc" != 0 ] || [ "$STALLED" != 0 ]; then
  echo "STALLED — goroutine dump in $LOG"
  exit 1
fi
echo "no stalls in ${SECS}s"
