#!/usr/bin/env bash
# Measure real binary characteristics of nanobot-go: size, startup time and
# resident memory.
#
# Usage:  scripts/bench.sh [--json]
#
# Design notes:
#   - Build output goes to the project disk, never /tmp (which is tmpfs here).
#   - RSS is read from /proc/<pid>/status VmHWM (peak) and VmRSS (current),
#     which are kernel-reported and not estimates.
#   - Every number printed is measured on this machine. Nothing is assumed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck disable=SC1091
. scripts/goenv.sh

OUT_DIR="$ROOT/.tools/bench"
mkdir -p "$OUT_DIR"

JSON=0
[[ "${1:-}" == "--json" ]] && JSON=1

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
BIN="$OUT_DIR/nanobot"
echo "==> building"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/nanobot

SIZE_BYTES=$(stat -c%s "$BIN")
SIZE_HUMAN=$(numfmt --to=iec --suffix=B "$SIZE_BYTES" 2>/dev/null || echo "${SIZE_BYTES}B")

# ---------------------------------------------------------------------------
# Startup time: time to run `version`, which loads no config and no network.
# ---------------------------------------------------------------------------
echo "==> measuring startup"
STARTUP_MS=$(python3 - "$BIN" <<'PY'
import subprocess, sys, time
bin_path = sys.argv[1]
runs = 20
best = None
for _ in range(runs):
    t0 = time.perf_counter()
    subprocess.run([bin_path, "version"], stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL, check=False)
    dt = (time.perf_counter() - t0) * 1000.0
    best = dt if best is None else min(best, dt)
print(f"{best:.2f}")
PY
)

# ---------------------------------------------------------------------------
# Peak RSS.
#
# A process that exits in ~1 ms cannot be sampled by an external poller, so
# this measures `selftest --hold`, which builds the real runtime (bus, tool
# registry, prompt builder, runner) and stays alive while /proc is sampled.
# Sampling a short-lived process silently reports 0, which would be a
# fabricated number.
# ---------------------------------------------------------------------------
measure_rss_hold() {
  local hold="$1"
  "$BIN" selftest --hold "$hold" >/dev/null 2>&1 &
  local pid=$!
  local hwm=0
  local deadline=$(( SECONDS + 15 ))
  while kill -0 "$pid" 2>/dev/null; do
    if [[ -r "/proc/$pid/status" ]]; then
      local v
      v=$(awk '/VmHWM/ {print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)
      [[ -n "$v" && "$v" -gt "$hwm" ]] && hwm=$v
    fi
    [[ $SECONDS -gt $deadline ]] && break
    sleep 0.02
  done
  wait "$pid" 2>/dev/null || true
  echo "$hwm"
}

echo "==> measuring peak RSS (runtime built, process held open)"
RSS_KB=$(measure_rss_hold 1200ms)
if [[ -z "$RSS_KB" || "$RSS_KB" -eq 0 ]]; then
  echo "ERROR: RSS sampling failed (got '${RSS_KB:-empty}')." >&2
  echo "Refusing to report a memory figure that was not actually measured." >&2
  exit 1
fi
RSS_MB=$(python3 -c "print(f'{$RSS_KB/1024:.2f}')")

echo "==> in-process heap report"
SELFTEST_OUT="$OUT_DIR/selftest.txt"
"$BIN" selftest >"$SELFTEST_OUT" 2>&1 || true

# ---------------------------------------------------------------------------
# Goroutine/thread count is not measurable from outside; report binary-level
# facts only, plus the library benchmark summary if available.
# ---------------------------------------------------------------------------
echo "==> running library benchmarks (may take a moment)"
BENCH_FILE="$OUT_DIR/bench.txt"
go test -bench=. -benchmem -benchtime=200x ./benchmarks/ >"$BENCH_FILE" 2>&1 || true

if [[ "$JSON" == "1" ]]; then
  python3 - "$SIZE_BYTES" "$STARTUP_MS" "$RSS_KB" <<'PY'
import json, sys
size, startup, rss = sys.argv[1:4]
print(json.dumps({
    "binary_bytes": int(size),
    "startup_ms_best_of_20": float(startup),
    "peak_rss_kb": int(rss),
}, indent=2))
PY
  exit 0
fi

cat <<EOF

================ nanobot-go binary profile ================
binary size          : $SIZE_HUMAN ($SIZE_BYTES bytes)
startup (best of 20) : ${STARTUP_MS} ms
peak RSS (runtime)   : ${RSS_MB} MB (${RSS_KB} KB)
===========================================================

Notes:
  * Built with CGO_ENABLED=0, -trimpath, -ldflags="-s -w".
  * RSS is VmHWM from the kernel, not an estimate.
  * RSS measured on a process that built the bus, tool registry, prompt
    builder and runner, then idled. Channels and MCP servers add more.
  * Library benchmarks written to $BENCH_FILE

Library benchmark summary:
EOF
cat "$SELFTEST_OUT"
grep -E '^Benchmark' "$BENCH_FILE" || echo "  (no benchmark output)"
