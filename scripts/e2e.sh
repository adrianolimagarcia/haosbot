#!/usr/bin/env bash
# End-to-end verification: build the real binary and drive it against a mock
# OpenAI-compatible server.
#
# Usage: scripts/e2e.sh
#
# This is the only check that exercises config loading, session storage,
# provider HTTP streaming, tool dispatch and the agent loop through the actual
# compiled binary. Unit tests call library functions directly and cannot catch
# a wiring defect.
#
# NOTE: all temporary output stays on the project disk. /tmp on this machine is
# a 16 GB RAM-backed tmpfs and must not be used.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck disable=SC1091
. scripts/goenv.sh

VENV_PY="$ROOT/.tools/venv/bin/python"
if [[ ! -x "$VENV_PY" ]]; then
  echo "ERROR: reference venv missing at $VENV_PY" >&2
  echo "The e2e harness uses it to run the mock server." >&2
  echo "Create it with:" >&2
  echo "  python3 -m venv .tools/venv && .tools/venv/bin/pip install pydantic pydantic-settings filelock loguru tzlocal tzdata watchfiles pyyaml tiktoken rich httpx jinja2 rapidfuzz croniter json-repair chardet packaging" >&2
  exit 1
fi

mkdir -p "$ROOT/.tools/bench" "$ROOT/.tools/tmp"

echo "==> building binary"
CGO_ENABLED=0 go build -trimpath -o "$ROOT/.tools/bench/nanobot" ./cmd/nanobot

echo "==> running end-to-end scenarios"
exec "$VENV_PY" "$ROOT/compat/python/e2e_test.py"
