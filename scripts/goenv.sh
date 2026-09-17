# Source this to get the pinned Go toolchain for this project.
# Usage: . scripts/goenv.sh
#
# NOTE: /tmp on this machine is a 16G tmpfs (RAM-backed) and is shared with
# other sessions. Go's build cache and work directory are therefore pinned to
# the project filesystem so a build can never exhaust system memory.
NANOBOT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
export GOROOT="$NANOBOT_ROOT/.tools/go"
export GOPATH="$NANOBOT_ROOT/.tools/gopath"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE="$NANOBOT_ROOT/.tools/gocache"
export GOTMPDIR="$NANOBOT_ROOT/.tools/tmp"
export TMPDIR="$GOTMPDIR"
export GOTOOLCHAIN=local
export PATH="$GOROOT/bin:$PATH"
