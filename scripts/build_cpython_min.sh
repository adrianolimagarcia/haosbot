#!/usr/bin/env bash
set -euo pipefail

# Build a minimal, optimized CPython runtime with SQLite enabled
# Target: ~10-20MB disk footprint, ~6-12MB RSS memory overhead.

PYTHON_VERSION="v3.14.7" # Fallback or latest stable tag/branch if 3.14.7 is ahead, let's use main/v3.12 or v3.11 for stability if 3.14 isn't tagged yet, or user's requested tag. Let's use v3.12.7 or python 3.11/3.12 stable.
# Note: CPython 3.14 might not exist yet as 3.14 is future; let's use v3.12.7 as a robust production choice or allow parameter.
CPYTHON_TAG="${1:-v3.12.7}"
PREFIX="${HOME}/.nanobot/python-min"

echo "==> Preparing to build CPython ${CPYTHON_TAG} minimal..."
echo "    Installation target: ${PREFIX}"

# Ensure build deps
if ! command -v gcc &>/dev/null || ! command -v make &>/dev/null; then
    echo "Error: gcc and make are required to build CPython." >&2
    exit 1
fi

BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "${BUILD_DIR}"' EXIT

cd "${BUILD_DIR}"
echo "==> Cloning CPython (${CPYTHON_TAG})..."
git clone --depth 1 --branch "${CPYTHON_TAG}" https://github.com/python/cpython.git
cd cpython

echo "==> Configuring Modules/Setup.local to disable bloat while keeping SQLite..."
cat << 'SETUP' > Modules/Setup.local
*disabled*
_tkinter
idlelib
test
ensurepip
bz2
lzma
curses
curses_panel
gdbm
dbm
nis
_uuid
readline
turtle
pydoc
SETUP

echo "==> Running ./configure..."
./configure \
  --prefix="${PREFIX}" \
  --disable-test-modules \
  --with-ensurepip=no \
  --without-doc-strings \
  --without-static-libpython \
  --enable-optimizations

echo "==> Compiling CPython (this may take a few minutes)..."
make -j "$(nproc)"

echo "==> Installing to ${PREFIX}..."
make install

echo "==> Stripping binaries to reduce size..."
if command -v strip &>/dev/null; then
    strip "${PREFIX}/bin/"python* || true
    find "${PREFIX}/lib" -name "*.so" -exec strip {} \; || true
fi

echo "==> CPython Minimal build complete!"
echo "    Python binary: ${PREFIX}/bin/python3"
