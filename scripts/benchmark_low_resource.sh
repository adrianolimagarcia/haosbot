#!/usr/bin/env bash
# Targeted low-resource harness for the canonical memory path.
#
# It intentionally runs one Go package with one build worker and one CPU
# runtime thread. RSS/CPU figures must be collected on the target device;
# this script does not fabricate a portable hardware result.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ -f scripts/goenv.sh ]]; then
	# shellcheck disable=SC1091
	. scripts/goenv.sh
fi

BENCHTIME="${NANOBOT_BENCHTIME:-3s}"
echo "==> Memory Fabric benchmark (GOMAXPROCS=1, go test -p 1)"
echo "==> bench time: $BENCHTIME"
GOMAXPROCS=1 go test -p 1 -tags sqlite_fts5 -run '^$' \
	-bench 'BenchmarkMemoryFabric(Append|ClaimAck)$' -benchmem \
	-benchtime="$BENCHTIME" ./internal/memoryfabric

cat <<'EOF'

Interpretation:
  * Compare RSS, CPU%, p50/p95 latency and database/WAL size on the target.
  * The +10 MB RAM and 200 MB disk budgets apply to optional new resources;
    they do not include the already-existing runtime baseline.
  * Keep vector lazy and use the graph-only/FTS fallback when the measured
    embedder cost exceeds the incremental budget.
EOF
