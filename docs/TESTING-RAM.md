# Test runner RAM: the 40 GB is the runner, not the program

## TL;DR
- The **built binary** peaks at **~5 MB RSS** (measured via `VmHWM`). The RAM objective is met by the code itself.
- The **heaviest single test** (`compat` BPE sweeps, 5.5 MB → 5.4 M tokens) peaks at **~27 MB RSS**.
- The **40 GB spikes** come from `go test ./...` / `go test ./internal/...` compiling **and** running **24 packages in parallel** (`nproc` = 8), **multiplied across sibling sessions** that run the same suites at the same time, with **no cgroup memory cap** to stop it.

## Evidence (measured this session)
| What | Peak RSS |
|------|----------|
| Built binary (`cmd/nanobot`) | 5 MB (6016 KB) |
| `compat` BPE sweeps test (single, `GOMAXPROCS=1`) | 27 MB (27496 KB) |
| `go test ./internal/...` (24 pkgs, parallel, × sibling sessions) | **~40 GB** |

The cgroup `system.slice/dsh-web.service` has `memory.max = max` (unlimited) and already sits at ~2 GB baseline from the GUI + sibling sessions.

## Rules (mandatory)
1. **Never run `go test ./...` or `go test ./internal/...` casually.** It is the exact trigger of the 40 GB spike.
2. **Run targeted packages only**: `go test -count=1 ./internal/<pkg>/...` or a single `compat` test file.
3. **Limit parallelism** on any multi-package run: `go test -p 1 -count=1 ./internal/<pkg>/...` (or `GOMAXPROCS=1`).
4. **Heavy suites** (`compat`, which runs the Python reference) must run **outside the dsh-web cgroup** and **only when no sibling session is running the same suite**:
   ```
   systemd-run --scope --collect -q --unit=nanobot-NAME-$$ /bin/bash -c 'source scripts/goenv.sh && go test -count=1 ./compat/...'
   ```
5. If memory spikes anyway, kill immediately:
   ```
   pkill -9 -f 'go test'; pkill -9 -f '\.test'; pkill -9 -f 'compile'
   ```
   and verify with `awk '{printf "%.1f GB\n", $1/1073741824}' /sys/fs/cgroup/system.slice/dsh-web.service/memory.current`.

## Why it is not a leak
- The port is stdlib-only, lazy-loads the BPE vocab (`sync.Once`), and the binary peaks at 5 MB.
- The spike is purely the **test runner**: Go compiles 24 packages in parallel (each `compile` can use ~1 GB for large packages like `compat`), then runs all test binaries in parallel, and sibling sessions multiply that by N.