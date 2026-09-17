# Reference environment

Everything the differential suites need in order to run the Python reference.
Recorded because two of these are non-obvious and one of them was discovered
late, after an agent had already worked around its absence.

## Interpreter

`.tools/venv/bin/python` — CPython **3.14.7**.

The frozen upstream checkout lives at `upstream/nanobot/` and is imported by
inserting it on `sys.path`, never by installing it:

```python
sys.path.insert(0, "upstream/nanobot")
```

## Packages in the venv

| Package | Version | Why |
| --- | --- | --- |
| `rapidfuzz` | 3.14.6 | `FileDiff.from_text` uses `rapidfuzz.distance.Indel.opcodes` — NOT `difflib` |
| `tiktoken` | 0.14.0 | `cl100k_base` token counts |
| `regex` | 2026.9.10 | tiktoken's `_pat_str` needs `\p{...}` and possessive quantifiers; stdlib `re` raises `bad escape \p` |
| `httpx` | 0.28.1 | reference HTTP client |
| `pydantic` | 2.13.5 | config schema |
| `loguru` | 0.7.3 | reference logging |

## python-telegram-bot — installed OUT of the venv, on purpose

`upstream/nanobot/nanobot/channels/telegram/manifest.py:42` declares the channel
dependency as:

```
python-telegram-bot[socks,webhooks]>=22.6,<23.0
```

The venv does **not** have it. Without it,
`import nanobot.channels.telegram.runtime` raises `ModuleNotFoundError: No
module named 'telegram'`, and the Telegram differential dumper had to lift pure
functions out of the module with `ast.get_source_segment` + `compile`/`exec`.
That workaround is why parts of the Telegram port were verified only
function-by-function, with the transport class itself unreachable.

It is now installed into `.tools/ptb-libs` with `--no-deps --target`, so the
shared venv is **not modified** — no existing suite can change behaviour because
of it, and nothing picks it up unless it opts in:

```bash
.tools/venv/bin/pip install --no-deps --target .tools/ptb-libs \
  "python-telegram-bot[socks,webhooks]>=22.6,<23.0"
```

To use it, insert the target **before** the upstream checkout:

```python
import sys
sys.path.insert(0, ".tools/ptb-libs")
sys.path.insert(0, "upstream/nanobot")
import nanobot.channels.telegram.runtime   # now importable
```

`httpx` and the rest of PTB's runtime dependencies resolve from the venv, which
is why `--no-deps` is safe: PTB 22.8 needs `httpx>=0.27` and the venv already
has 0.28.1.

Verified: `telegram.__version__ == "22.8"` and
`TelegramChannel = nanobot.channels.telegram.runtime.TelegramChannel` imports
cleanly.

### Consequence

The Telegram transport is now **differentially testable**, and a test that
needs PTB must add `.tools/ptb-libs` itself. A test that does not need PTB must
NOT add it: making `nanobot.channels.telegram.runtime` importable changes what
channel discovery sees, and the channel-discovery suites were pinned against a
reference where that import fails.

## Toolchain for the Go side

`scripts/goenv.sh` pins `GOROOT`/`GOPATH`/`GOCACHE` and redirects
`GOTMPDIR`/`TMPDIR` into `$NANOBOT_ROOT/.tools/tmp`. It must be sourced in every
fresh shell. `GOTOOLCHAIN=local`, Go 1.23.5.
