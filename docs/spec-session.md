# Session Persistence — Compatibility Specification (Python → Go)

**Status:** specification, derived by reading the frozen reference source. No Go code here.
**Reference source (read-only, frozen):** `nanobot` @ commit `1bb712d3488915ca4ed9ccc1a93067ff722f5ab9`
**Source root used for all citations:** `/run/media/adriano/e681b5ac-a4fb-44d4-aebf-9d6584065787/dsh-projetos/nanobot/upstream/nanobot/`
**All paths in citations are relative to that root.**

**Goal:** a Go implementation that writes session files Python can read, and reads session files Python wrote.

**How to read this document**

* Every normative claim carries a `file:line` citation. Line numbers are from the frozen commit.
* `OBSERVED` = read directly in the source at the cited line. `INFERENCE` = derived from cited code.
  `UNVERIFIED` = not determinable from the sources I read; never assumed.
* Code snippets are verbatim excerpts, not paraphrases.
* Nothing in this document was executed against a live nanobot instance; the Python environment for
  the project was **not** installed in the analysis sandbox (`filelock`, `loguru`, `tiktoken` absent).
  Claims about runtime behaviour are therefore source-derived. The checks I *did* execute are marked
  `OBSERVED` inline, with the interpreter/toolchain version:
  * CPython 3.14.7 — the verbatim `storage_key` / `decode_storage_key` / `safe_filename` algorithms,
    `json.dumps` separator and float formatting, and `datetime.fromisoformat(...).isoformat()`
    normalisation;
  * Go 1.23.5 — `encoding/json` escaping (`SetEscapeHTML` on/off), map key ordering, float formatting,
    and invalid-UTF-8 handling.

---

## 0. Summary of the persistence model

`OBSERVED` — Session persistence is a per-session **append-on-save, rewrite-whole-file** JSONL store:

* One file per session key, named by a **base64url encoding of the key** with padding stripped.
* Line 1 is a **metadata record**; optionally line 2 is a **provider_state record**; every remaining
  line is an **ordinary message record** (`nanobot/session/manager.py:1314-1339`).
* "Append" is logical, not physical: every `save()` writes a **brand-new temp file** with all records
  and `os.replace()`s it over the target (`nanobot/session/manager.py:1316`, `:1344`). The old file's
  inode is destroyed. There is no physical append path in this class.
* A sidecar `<storage_key>.checkpoint.json` holds volatile in-flight turn state
  (`nanobot/session/manager.py:1029-1030`, `:1215-1260`).
* Files live **outside the agent workspace**, under the instance data directory, namespaced by a
  workspace identity id (`nanobot/session/manager.py:551-585`).

---

## 1. DIRECTORY LAYOUT

### 1.1 Resolution chain

`JsonlSessionStore.__init__` (`nanobot/session/manager.py:551-585`) — verbatim:

```python
def __init__(self, workspace: Path, *, sessions_root: Path | None = None):
    canonical_workspace = Path(workspace).expanduser().resolve(strict=False)
    ensure_dir(canonical_workspace)
    root = (
        Path(sessions_root).expanduser().resolve(strict=False)
        if sessions_root is not None
        else get_runtime_subdir("sessions").resolve(strict=False)
    )
    if root == canonical_workspace or root.is_relative_to(canonical_workspace):
        raise RuntimeError(
            "session storage must be outside the agent workspace; "
            "move --config outside --workspace or choose a nested workspace directory"
        )
    ensure_dir(root)
    with suppress(OSError):
        os.chmod(root, 0o700)
    self.workspace = canonical_workspace
    self._migration_lock = FileLock(
        str(root / ".workspace-migration.lock"),
        timeout=_SESSION_MIGRATION_LOCK_TIMEOUT_SECONDS,
    )
    with self._migration_lock:
        workspace_id = self._load_or_create_workspace_id(canonical_workspace, root)
        workspace_id = self._claim_workspace_namespace(root, canonical_workspace, workspace_id)
        self.sessions_dir = ensure_dir(root / workspace_id)
        self.legacy_sessions_dir = get_legacy_sessions_dir()
        self._session_files_lock = FileLock(str(self.sessions_dir / _SESSION_FILES_LOCK_FILENAME))
        with self._session_files_lock:
            self._migrate_from_workspace(canonical_workspace)
```

Derived layout:

```
<root>                                   # "sessions root"
├── .workspace-migration.lock            # FileLock, timeout 30 s   (manager.py:568-571, :74)
└── <workspace_id>/                      # 32 lowercase hex chars    (manager.py:73, :579)
    ├── .workspace                       # text: absolute workspace path + "\n"  (manager.py:709-710)
    ├── .session-files.lock              # FileLock, default timeout  (manager.py:75, :581-583)
    ├── <storage_key>.jsonl              # canonical session file    (manager.py:1027)
    ├── <storage_key>.checkpoint.json    # runtime checkpoint sidecar (manager.py:1030)
    ├── <lossy>.jsonl                    # retired lossy path, never read (manager.py:1032-1033)
    └── .migration-conflicts/            # archived migration conflicts (manager.py:874-885)
```

`OBSERVED` — `ensure_dir` is `path.mkdir(parents=True, exist_ok=True)` (`nanobot/utils/helpers.py:355-358`).

`OBSERVED` — the sessions root defaults to `get_runtime_subdir("sessions")`:

* `get_runtime_subdir(name) = ensure_dir(get_data_dir() / name)` (`nanobot/config/paths.py:20-22`)
* `get_data_dir() = ensure_dir(get_config_path().parent)` (`nanobot/config/paths.py:15-17`)
* `get_config_path()` returns the active config path or `Path.home() / ".nanobot" / "config.json"`
  (`nanobot/config/loader.py:35-39`)

**Default root:** `~/.nanobot/sessions`. Confirmed by docs table
`docs/architecture.md:152` (`<config-dir>/sessions/<workspace-id>/*.jsonl`, default `~/.nanobot/sessions/...`)
and `docs/architecture.md:183`.

`OBSERVED` — production call sites pass an explicit root derived from the loaded config's directory:
`nanobot/agent/loop.py:481-484` and `nanobot/cli/commands.py:493-496` both use
`data_dir / "sessions"` where `data_dir = config.runtime_data_dir`
(`nanobot/config/schema.py:450-452`: `self._source_path.parent`).

### 1.2 What `sessions_root` does

* `sessions_root` replaces the whole root. The workspace-namespace subdirectory is still appended.
* `OBSERVED` — `test_sessions_follow_active_custom_config_data_root` asserts
  `manager.sessions_dir.parent == custom_instance / "sessions"` (`tests/session/test_session_location.py:128`).
* `OBSERVED` — `test_session_root_inside_workspace_fails_closed` asserts a `RuntimeError` matching
  `"must be outside the agent workspace"` (`tests/session/test_session_location.py:133-137`).

### 1.3 What `workspace` does

`workspace` does **not** select the storage directory directly. It selects the **namespace id**:

1. Canonicalised with `expanduser().resolve(strict=False)` (`manager.py:552`) — so symlinked and
   `..`-containing paths that resolve to the same directory share a namespace
   (`test_equivalent_workspace_paths_share_one_store`, `tests/session/test_session_location.py:194-220`).
2. `workspace/.nanobot/workspace-id` holds the id (`manager.py:71-72`, `:631-637`).
3. On first use the id is generated as `secrets.token_hex(16)` → 32 lowercase hex chars
   (`manager.py:681`), validated by `^[0-9a-f]{32}$` (`manager.py:73`, `:624`).
4. If the marker is missing, the id is **recovered** by scanning `<root>/*/` for a namespace whose
   `.workspace` marker records this workspace path (`manager.py:639-668`, `:676-679`).
5. If two namespaces claim the same workspace, startup fails (`manager.py:663-667`).
6. If the id is claimed by a *different, still-existing* workspace path, a fresh id is minted and the
   workspace marker rewritten — this is the "copied workspace gets isolated identity" path
   (`manager.py:757-759`, `test_copied_workspace_gets_isolated_session_identity`,
   `tests/session/test_session_location.py:175-191`).
7. If the recorded path no longer exists, the namespace is re-adopted — the "workspace moved" path
   (`manager.py:752-755`, `test_workspace_move_preserves_session_identity`,
   `tests/session/test_session_location.py:140-155`).

`OBSERVED` — the `.workspace` marker contains the **resolved absolute workspace path** plus a newline
(`manager.py:709-710`), written atomically with mode `0o600` (`manager.py:606-617`).
`test_sessions_are_stored_outside_workspace` asserts
`marker.read_text().strip() == str(workspace.resolve())` (`tests/session/test_session_location.py:62-63`).

`OBSERVED` — symlink safety: the workspace-id marker, the `.nanobot` state dir, the namespace dir, and
the namespace `.workspace` marker all raise `RuntimeError` if they are symlinks
(`manager.py:621-622`, `:634-635`, `:723-724`, `:729-730`).

### 1.4 `legacy_sessions_dir`

`OBSERVED` — `self.legacy_sessions_dir = get_legacy_sessions_dir()` (`manager.py:580`), and

```python
def get_legacy_sessions_dir() -> Path:
    """Return the legacy global session directory used for migration fallback."""
    return Path.home() / ".nanobot" / "sessions"
```
(`nanobot/config/paths.py:69-71`; asserted in `tests/config/test_config_paths.py:36`.)

**When it is used — and only then:**

| Use | Citation |
|---|---|
| Building `get_legacy_session_path(key)` = `legacy_sessions_dir / f"{safe_key(key)}.jsonl"` | `manager.py:1035-1036` |
| **Deletion only** — `_delete_unlocked` unlinks it as one of four candidate paths | `manager.py:1410-1416` |

`OBSERVED` — it is **never read**. `test_load_ignores_legacy_global_path` asserts
`sm._load(key) is None` while the legacy file still exists (`tests/agent/test_session_collision.py:81-92`).
`test_delete_session_cleans_legacy_file` asserts deletion removes it
(`tests/agent/test_session_delete.py:96-114`).

`INFERENCE` — `legacy_sessions_dir` is a **delete-only tombstone path**: it exists so that deleting a
session cannot be undone by a pre-relocation file being resurrected.

### 1.5 The retired in-workspace location

`OBSERVED` — `_migrate_from_workspace` treats `<workspace>/sessions/*.jsonl` as the legacy source
(`manager.py:908-962`) and physically **removes** sources after a verified copy.
`restore_to_workspace` copies canonical files back into `<workspace>/sessions/` for rollback
(`manager.py:964-999`). See §11.

---

## 2. FILE NAMING

### 2.1 `storage_key()` — the canonical algorithm

```python
@staticmethod
def storage_key(key: str) -> str:
    return base64.urlsafe_b64encode(key.encode()).decode().rstrip("=")
```
(`nanobot/session/manager.py:1005-1007`)

Precise semantics:

1. `key.encode()` — UTF-8 (Python's default codec). **Non-ASCII keys encode as UTF-8 bytes before base64.**
2. `base64.urlsafe_b64encode` — standard base64 with `+`→`-` and `/`→`_`.
3. `.rstrip("=")` — **all** trailing `=` removed (base64 padding is at most 2, but `rstrip` removes any number).

### 2.2 `decode_storage_key()`

```python
@staticmethod
def decode_storage_key(stem: str) -> str | None:
    try:
        padding = 4 - len(stem) % 4
        if padding != 4:
            stem += "=" * padding
        return base64.urlsafe_b64decode(stem).decode("utf-8")
    except _SESSION_DATA_ERRORS:
        return None
```
(`nanobot/session/manager.py:1009-1017`)

* `_SESSION_DATA_ERRORS = (ValueError, TypeError, AttributeError, KeyError)` (`manager.py:51`).
  `binascii.Error` and `UnicodeDecodeError` are both `ValueError` subclasses, so they are caught.
* Padding is restored from the **length only** (`4 - len % 4`), not from the original byte count.
  `INFERENCE`: this is correct because padding is a pure function of encoded length mod 4.

`OBSERVED` (verified by executing the exact algorithm in CPython 3.14 with stdlib only):

| stem | result |
|---|---|
| `dGVsZWdyYW06MQ` | `telegram:1` |
| `dGVsZWdyYW06MQ==` | `telegram:1` (decodes, but **not canonical**) |
| `telegram_1` | `None` |
| `` (empty) | `''` (decodes, canonical) |
| `YQxx` | `'a\x0cq'` (decodes, canonical) |
| `not-base64!!` | `None` |

### 2.3 `session_key_from_path()` — the canonical-only gate

```python
@classmethod
def session_key_from_path(cls, path: Path) -> str | None:
    key = cls.decode_storage_key(path.stem)
    if key is None or cls.storage_key(key) != path.stem:
        return None
    return key
```
(`nanobot/session/manager.py:1019-1024`)

**This is the acceptance rule for Go-written filenames.** A file is only a session file if
`base64url_nopad(utf8(key)) == path.stem` **byte-for-byte**. Consequences:

* A stem with padding (`dGVsZWdyYW06MQ==`) is rejected even though it decodes.
* A stem produced with standard (non-URL-safe) base64 containing `+` or `/` is rejected.
* Used by `_list_sessions_unlocked` (`manager.py:1547`) and `restore_to_workspace` (`manager.py:976`).

### 2.4 `safe_filename()` fallback

```python
_UNSAFE_CHARS = re.compile(r'[<>:"/\\|?*]')

def safe_filename(name: str) -> str:
    """Replace unsafe path characters with underscores."""
    return _UNSAFE_CHARS.sub("_", name).strip()
```
(`nanobot/utils/helpers.py:366`, `:374-376`)

```python
@staticmethod
def safe_key(key: str) -> str:
    return safe_filename(key.replace(":", "_"))
```
(`nanobot/session/manager.py:1001-1003`)

`safe_filename` is **not** the canonical filename algorithm. It is retained for two retired paths only:

* `get_legacy_lossy_path(key)` = `sessions_dir / f"{safe_filename(key.replace(':', '_'))}.jsonl"`
  (`manager.py:1032-1033`) — note this is textually the same formula as `safe_key`.
* `get_legacy_session_path(key)` = `legacy_sessions_dir / f"{safe_key(key)}.jsonl"` (`manager.py:1035-1036`).

`OBSERVED` — the lossy path is **never read** (`test_load_ignores_legacy_lossy_path`,
`tests/agent/test_session_collision.py:68-78`) and **never written** by `save`
(`test_save_uses_new_path_not_lossy`, `tests/agent/test_session_collision.py:47-65`).
It is only unlinked during delete (`manager.py:1414`).

`OBSERVED` — why the lossy path was retired: `safe_key("telegram:a_b") == safe_key("telegram:a:b")`
(`tests/agent/test_session_collision.py:43`, `:95-96`), while the base64url stems differ
(`:44`, `:99-107`).

`OBSERVED` — `list_sessions` **ignores** files whose stem is not canonical, so a stale lossy file with
otherwise-recoverable records does not appear (`test_list_sessions_ignores_legacy_stem`,
`tests/session/test_session_list_repair_legacy.py:10-33`).

### 2.5 Extension and derived paths

| Path | Formula | Citation |
|---|---|---|
| session | `sessions_dir / (storage_key(key) + ".jsonl")` | `manager.py:1026-1027` |
| checkpoint | `sessions_dir / (storage_key(key) + ".checkpoint.json")` | `manager.py:1029-1030`, `:59` |
| lossy legacy | `sessions_dir / (safe_filename(key.replace(':', '_')) + ".jsonl")` | `manager.py:1032-1033` |
| global legacy | `legacy_sessions_dir / (safe_key(key) + ".jsonl")` | `manager.py:1035-1036` |

### 2.6 Worked examples (computed with the exact algorithms)

`OBSERVED` — computed by running the verbatim `storage_key` / `safe_filename` implementations in
CPython 3.14 (stdlib only) and confirming `decode_storage_key(stem) == key` for each:

| # | session key | canonical filename | legacy lossy stem | checkpoint filename |
|---|---|---|---|---|
| 1 | `telegram:1` | `dGVsZWdyYW06MQ.jsonl` | `telegram_1` | `dGVsZWdyYW06MQ.checkpoint.json` |
| 2 | `websocket:123e4567-e89b-12d3-a456-426614174000` | `d2Vic29ja2V0OjEyM2U0NTY3LWU4OWItMTJkMy1hNDU2LTQyNjYxNDE3NDAwMA.jsonl` | `websocket_123e4567-e89b-12d3-a456-426614174000` | (same stem + `.checkpoint.json`) |
| 3 | `unified:default` | `dW5pZmllZDpkZWZhdWx0.jsonl` | `unified_default` | (same stem + `.checkpoint.json`) |

Additional verified values for reference:

| session key | canonical filename |
|---|---|
| `cli:default` | `Y2xpOmRlZmF1bHQ.jsonl` |
| `telegram:ç` | `dGVsZWdyYW06w6c.jsonl` |
| `telegram:-1001234567890` | `dGVsZWdyYW06LTEwMDEyMzQ1Njc4OTA.jsonl` |
| `telegram:a_b` | `dGVsZWdyYW06YV9i.jsonl` |
| `telegram:a:b` | `dGVsZWdyYW06YTpi.jsonl` |
| `dream:20260528-100000` | `ZHJlYW06MjAyNjA1MjgtMTAwMDAw.jsonl` |

`OBSERVED` — a round-trip check (`decode_storage_key(storage_key(k)) == k`) was executed in CPython 3.14
and returned `True` for every key in both tables above.

---

## 3. JSONL RECORD FORMAT

### 3.1 File-level rules

* Encoding: **UTF-8**, no BOM, `\n` line terminators. `open(path, encoding="utf-8")` on read
  (`manager.py:1055`, `:1135`, `:1442`, `:1498`, `:1551`, `:1377`); `open(tmp, "x", encoding="utf-8")` on write
  (`manager.py:1319`).
* One JSON value per line. Blank / whitespace-only lines are skipped on every read path
  (`manager.py:1057-1059`, `:1137-1139`, `:1444-1446`, `:1500-1502`, `:1570-1571`).
* Every non-blank line **must decode to a JSON object**; `_json_object` raises
  `ValueError("session records must be JSON objects")` otherwise (`manager.py:79-83`).
* Serialization is `json.dumps(value, ensure_ascii=False)` — **default separators** `", "` and `": "`,
  **no** `sort_keys`, **no** indentation (`manager.py:1331`, `:1337`, `:1339`).
* A trailing newline is written after the last record (`manager.py:1339`).

`OBSERVED` — verified in CPython 3.14: `json.dumps({"a":1,"b":2})` → `{"a": 1, "b": 2}` (spaces present).

### 3.2 Record type dispatch

```python
record_type = data.get("_type")
if record_type == "metadata":
    ...
elif record_type == _PROVIDER_STATE_RECORD_TYPE:   # "provider_state"
    ...
else:
    messages.append(data)
```
(`nanobot/session/manager.py:1064-1090`)

`_PROVIDER_STATE_RECORD_TYPE = "provider_state"` (`manager.py:53`).

**Critical:** the `else` branch is the *message* branch. Any record whose `_type` is neither
`"metadata"` nor `"provider_state"` — **including a record with an unknown future `_type`, and including
a message record that happens to carry a `_type` field with another value** — is appended to the message
list verbatim. `INFERENCE`: a Go port must reproduce this exactly; treating unknown `_type` values as
skippable would diverge.

### 3.3 The metadata record

Written at `nanobot/session/manager.py:1320-1331`:

```python
metadata_line = {
    "_type": "metadata",
    "key": session.key,
    "created_at": session.created_at.isoformat(),
    "updated_at": session.updated_at.isoformat(),
    "metadata": session.metadata,
    "last_archived": session.last_archived,
    # Keep old nanobot releases able to read sessions written
    # during the field-name migration.
    "last_consolidated": session.last_consolidated,
}
f.write(json.dumps(metadata_line, ensure_ascii=False) + "\n")
```

| Field | Type | Semantics | Citation |
|---|---|---|---|
| `_type` | string, exactly `"metadata"` | record discriminator | `manager.py:1321`, `:1065` |
| `key` | string | the raw session key (may contain `:`, unicode, any char). Written but **not authoritative** — the filename is the source of truth for `load`; `read`/`read_metadata` prefer the stored value | `manager.py:1322`, `:1466-1468`, `:1512` |
| `created_at` | ISO-8601 string | `datetime.now().isoformat()` at `Session` construction. Naive local time, microseconds when non-zero | `manager.py:1323`, `:281` |
| `updated_at` | ISO-8601 string | last mutation time; naive local. **Used as the checkpoint base fingerprint** | `manager.py:1324`, `:1282` |
| `metadata` | JSON object | the full `session.metadata` dict; free-form, see §3.6 | `manager.py:1325` |
| `last_archived` | integer | replay start offset into `messages`; canonical name | `manager.py:1326` |
| `last_consolidated` | integer | duplicate of `last_archived` under the legacy name; always written equal | `manager.py:1329`, `:285`, `:303-310` |

Read-side rule for the offset:

```python
def _archive_offset(data: dict[str, Any]) -> int:
    for key in ("last_archived", "last_consolidated"):
        offset = data.get(key)
        if isinstance(offset, int) and not isinstance(offset, bool):
            return offset
    return 0
```
(`manager.py:86-92`) — **`last_archived` wins; `last_consolidated` is the fallback; `bool` is explicitly
excluded; anything else yields `0`.**

`OBSERVED` — Go must write **both** keys to be forward/backward compatible with releases on either side
of the rename (`manager.py:1327-1329`).

### 3.4 The provider_state record

Written at `nanobot/session/manager.py:1332-1337`:

```python
if session.provider_state is not None:
    provider_state_line = {
        "_type": _PROVIDER_STATE_RECORD_TYPE,
        "state": session.provider_state.to_private_record(),
    }
    f.write(json.dumps(provider_state_line, ensure_ascii=False) + "\n")
```

`state` is `ProviderConversationState.to_private_record()` (`nanobot/providers/base.py:198-207`):

```python
{
    "kind": str,               # non-empty
    "provider": str,           # non-empty
    "model": str,              # non-empty
    "version": int,            # bool rejected
    "payload": dict,           # deep copy of provider-private data
    "pending_messages": list,  # list of dicts
}
```

Validation on read, `ProviderConversationState.from_private_record` (`nanobot/providers/base.py:209-247`):
returns `None` unless `value` is a dict, `kind`/`provider`/`model` are non-empty strings, `version` is an
`int` and **not** a `bool`, `payload` is a `dict`, `pending_messages` is a `list` and every element is a
`dict`. A `None` result is treated as an invalid record on the repair path (`manager.py:1168-1173`), but
on the **load** path `provider_state` simply becomes `None` (`manager.py:1085-1088` — the `None` return is
assigned without complaint).

`OBSERVED` — the writer emits at most one provider_state record, immediately after the metadata line.

`OBSERVED` — the line-prefix fast path used by `list_sessions`:

```python
_PROVIDER_STATE_RECORD_PREFIX_RE = re.compile(
    r'^\s*\{\s*"_type"\s*:\s*"provider_state"\s*(?:,|\})'
)
```
(`manager.py:54-56`, used by `_is_provider_state_record_line`, `manager.py:200-202`)

`INFERENCE` — this regex requires `_type` to be the **first key** of the object. **Verified with Go
1.23.5**: `json.Marshal(map[string]any{...})` sorts keys, and `"_type"` sorts before `"state"` because
`_` (0x5F) < `s` (0x73) — so a Go `map` writer does satisfy the fast path. A Go **struct** whose fields
are declared with `state` before `_type`, or any writer emitting unsorted keys, would miss the fast path.
That is not a correctness problem: the parsed check
`if item.get("_type") in {"metadata", _PROVIDER_STATE_RECORD_TYPE}: continue` (`manager.py:1583-1587`)
still skips the record. The only cost is that the record then counts against the
`_SESSION_LIST_PREVIEW_MAX_RECORDS` / `_SESSION_LIST_PREVIEW_MAX_CHARS` scan budget (`manager.py:1574-1580`).
For `load`, key order is irrelevant (parsed `dict.get`).

### 3.5 Ordinary message records

Message dicts are written verbatim: `for msg in session.messages: f.write(json.dumps(msg, ensure_ascii=False) + "\n")`
(`manager.py:1338-1339`). **The store does not schema-validate them.**

Fields produced by the codebase, with the producing site:

| Field | Type | Producer / meaning | Citation |
|---|---|---|---|
| `role` | string | `"user"` \| `"assistant"` \| `"tool"` \| `"system"` are the roles the SDK accepts; the store itself does not validate | `nanobot/sdk/clients.py:27`; `manager.py:312-320` |
| `content` | string \| list \| null | plain text, or a list of content blocks (`{"type":"text","text":…}`, `{"type":"image_url",…}`). Persisted image blocks are **replaced by a text placeholder** before writing | `manager.py:314`; `nanobot/agent/loop.py:2090-2115` |
| `timestamp` | ISO-8601 string | set by `Session.add_message` (`datetime.now().isoformat()`) and by `entry.setdefault("timestamp", datetime.now().isoformat())` for turn messages | `manager.py:317`; `nanobot/agent/loop.py:2252` |
| `tool_calls` | list of objects | OpenAI shape: `{"id": str, "type": "function", "function": {"name": str, "arguments": str \| object}}` | `nanobot/agent/runner.py:502`; `tests/session/test_session_store.py:169-188` |
| `tool_call_id` | string | on `role:"tool"` rows; must match a declared assistant tool-call id or the row is dropped at persistence | `nanobot/agent/loop.py:2212-2233` |
| `name` | string | tool name on `role:"tool"` rows | `nanobot/agent/runner.py:548` |
| `reasoning_content` | string | assistant reasoning; present only when non-`None` or when `thinking_blocks` exist | `nanobot/utils/helpers.py:698-715` |
| `thinking_blocks` | list of objects | provider thinking blocks | `nanobot/utils/helpers.py:714-715` |
| `latency_ms` | integer | turn latency stamped onto the last assistant row | `nanobot/agent/loop.py:2267-2268` |
| `media` | list of strings | local media paths for a user turn; replayed as `[image: path]` breadcrumbs | `nanobot/agent/loop.py:698`; `manager.py:403-407` |
| `_channel_delivery` | `true` | marks a proactive assistant delivery mirrored into the channel session | `nanobot/cli/gateway_runtime.py:544`; consumed at `manager.py:374`, `nanobot/utils/helpers.py:473` |
| `_command` | `true` | slash-command turn; **filtered out of LLM replay** and out of WebUI visible turns | `nanobot/agent/loop.py:1850`; `manager.py:386-387`; `nanobot/webui/session_access.py:74` |
| `_hidden_history` | `true` \| object | hidden-history marker (see §8) | `manager.py:335`; `nanobot/agent/loop.py:1054` |
| `_automation_turn` | object | automation-turn marker `{"kind": …}`; also hidden (see §8) | `nanobot/session/automation_turns.py:10`, `:42-43`, `:75-86` |
| `injected_event` | string | e.g. `"subagent_result"` | `nanobot/agent/loop.py:1054`, `:2298` |
| `subagent_task_id` | string \| null | dedup key for injected subagent results | `nanobot/agent/loop.py:1050-1052`, `:2290-2292` |
| `sender_id` | string | e.g. `"subagent"` | `nanobot/agent/loop.py:2297` |
| `_runtime_context` | object | runtime-context marker `{"version":1,"sources":[…],"suffix":str}` or `{…,"blocks":[…]}` (see §8) | `nanobot/runtime_context.py:120-145`; `nanobot/agent/loop.py:710` |
| `cli_apps` | list | CLI-app attachments; replayed as `[CLI App Attachment: …]` lines | `nanobot/apps/cli/utils.py:9-12`; `manager.py:408-434` |
| `mcp_presets` | list | MCP preset attachments | `nanobot/agent/tools/mcp.py:1325-1327` |
| `session_mentions` | list | structured session mentions | `nanobot/agent/tools/sessions.py:31-33` |
| `_recovery_interrupted` | `true` | synthesized tool row for an interrupted turn | `nanobot/session/recovery.py:325-334` |
| `_webui_recovery_id` / `_recovery_followup_id` | string | recovery correlation ids | `nanobot/session/recovery.py:36`, `:38` |
| `source` | string | SDK ingest source tag | `nanobot/sdk/clients.py:59-60` |
| any other key | any | the SDK copies **every** field of an ingested message except `role`, `content`, `_runtime_context` | `nanobot/sdk/clients.py:26`, `:54-58` |

`OBSERVED` — reserved keys that the SDK will **not** copy into extras: `{"role", "content", "_runtime_context"}`
(`nanobot/sdk/clients.py:26`).

### 3.6 Session metadata (`metadata` field) keys

`metadata` is a free-form object. Keys the codebase reads or writes:

| Key | Meaning | Citation |
|---|---|---|
| `last_channel` | `"channel:chat_id"` route for unified sessions | `nanobot/session/keys.py:9`, `:19-27`, `:30-42` |
| `_last_summary` | `{"text": str, "last_active": ISO str}` compaction summary | `nanobot/session/manager.py:338-341`; `nanobot/session/summary.py:42-57` |
| `_nanobot_model_preset` | session-scoped model preset name (non-empty string, else `ValueError`) | `nanobot/session/model_selection.py:9`, `:12-22` |
| `runtime_checkpoint` | volatile in-flight turn state (see §7) | `manager.py:57`, `:1230`, `:1296` |
| `pending_user_turn` | `true` while a user turn is unanswered | `nanobot/session/recovery.py:34`; `nanobot/agent/loop.py:2309` |
| `pending_user_followups` | list of follow-up records | `nanobot/session/recovery.py:37`, `:104` |
| `webui_recovery` | recovery status blob | `nanobot/session/recovery.py:35`, `:876` |
| `goal_state` | sustained-goal blob (dict or JSON string); legacy alias `thread_goal` | `nanobot/session/goal_state.py:12-13`, `:28-33`, `:53-56` |
| `session_handle` | short pronounceable handle `^[a-z]{4,16}$` | `nanobot/session/session_handles.py:14`, `:73-78` |
| `_last_usage` | last LLM usage dict | `nanobot/agent/loop.py:2036` |
| `title`, `title_user_edited` | WebUI title and its user-edit flag | `nanobot/session/manager.py:258-263`, `:68-69`; `nanobot/session/webui_turns.py:289` |
| `webui` | `true` marker for WebUI-owned sessions | `nanobot/session/webui_turns.py:128`; `nanobot/webui/workspaces.py:367` |
| `workspace_scope` | workspace scope metadata (dict) | `nanobot/security/workspace_access.py:165` |

`OBSERVED` — `_FORK_VOLATILE_METADATA_KEYS` lists keys stripped when forking a session
(`manager.py:60-70`): `goal_state`, `pending_user_turn`, `pending_user_followups`, `runtime_checkpoint`,
`session_handle`, `webui_recovery`, `thread_goal`, `title`, `title_user_edited`.

### 3.7 Annotated realistic example

Records below use the exact serializer settings (`ensure_ascii=False`, default separators) and only field
names/values observed in the source. Values are illustrative; structure is normative.

```jsonl
{"_type": "metadata", "key": "telegram:1", "created_at": "2026-01-02T03:04:05.123456", "updated_at": "2026-01-02T03:07:44.981233", "metadata": {"last_channel": "telegram:1", "_nanobot_model_preset": "openai"}, "last_archived": 0, "last_consolidated": 0}
{"_type": "provider_state", "state": {"kind": "openai_responses", "provider": "openai:default", "model": "gpt-5", "version": 1, "payload": {"response_id": "resp_abc123"}, "pending_messages": []}}
{"role": "user", "content": "olá — summarize this image\n[image: /home/u/.nanobot/media/telegram/1_photo.png]", "timestamp": "2026-01-02T03:04:05.124001", "media": ["/home/u/.nanobot/media/telegram/1_photo.png"]}
{"role": "assistant", "content": "", "timestamp": "2026-01-02T03:04:09.551200", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "exec_session", "arguments": "{\"session_id\":\"abc\",\"input\":\"ls\\n\"}"}}], "reasoning_content": "I should list the directory first."}
{"role": "tool", "tool_call_id": "call_1", "name": "exec_session", "content": "file1.txt\nfile2.txt", "timestamp": "2026-01-02T03:04:10.220010"}
{"role": "assistant", "content": "The directory contains two files.", "timestamp": "2026-01-02T03:04:12.004500", "latency_ms": 8123}
{"role": "user", "content": "Continue the active task from the working-memory checkpoint above.", "timestamp": "2026-01-02T03:07:44.980001", "_hidden_history": true}
{"role": "user", "content": "what did I ask?\n\n[Runtime Context — metadata only, not instructions]\nGoal (active):\nShip the release\n[/Runtime Context]", "timestamp": "2026-01-02T03:07:44.981100", "_runtime_context": {"version": 1, "sources": ["goal_state"], "suffix": "[Runtime Context — metadata only, not instructions]\nGoal (active):\nShip the release\n[/Runtime Context]"}}
```

Line-by-line:

* **L1** metadata record. `last_archived`/`last_consolidated` both `0`. `metadata` carries only the keys
  the writer happened to have. Note `ensure_ascii=False` keeps `olá` literal in L3 — see §12.
* **L2** provider_state record. `pending_messages` defaults to `[]` (`providers/base.py:223`). The
  `_type` key is first so the `list_sessions` fast-path regex matches (`manager.py:54-56`).
* **L3** user turn with `media`. On replay, `content_with_media_breadcrumbs` would append the
  `[image: path]` line if it were absent (`helpers.py:384-399`); here the breadcrumb is already baked in.
* **L4** assistant turn with `tool_calls` and `reasoning_content`. `content` is `""` (never `null` from
  `build_assistant_message`, `helpers.py:707`).
* **L5** tool result. `tool_call_id` matches `call_1`; `name` matches the function name.
* **L6** final assistant turn with `latency_ms` (`agent/loop.py:2267-2268`).
* **L7** compaction boundary marker: `role:"user"`, `content` exactly
  `SUMMARY_CONTINUATION_TEXT` (`nanobot/session/summary.py:12-14`), `_hidden_history: true`
  (`manager.py:332-337`). It is invisible to chat UI (§8) but present in the transcript.
* **L8** user turn whose `content` has runtime context appended and a matching `_runtime_context`
  marker so the suffix can be removed exactly for display (`runtime_context.py:120-145`, `:215-240`).

---

## 4. READ ALGORITHM

### 4.1 `load(key) -> Session | None`

Entry: `load` acquires `self._session_files_lock` and calls `_load_unlocked` (`manager.py:1038-1040`).

`_load_unlocked` (`manager.py:1042-1114`):

1. `path = get_session_path(key)`; if `not path.exists()` → return `None` (`:1043-1045`).
2. Initialise `messages=[]`, `metadata={}`, `created_at=None`, `updated_at=None`,
   `last_consolidated=0`, `provider_state=None` (`:1048-1053`).
3. Open with `encoding="utf-8"` and iterate lines (`:1055-1056`).
4. `line = line.strip()`; skip empty (`:1057-1059`).
5. `json.loads(line)` then `_json_object(...)` — **any** non-object or malformed JSON raises and aborts
   the whole load (`:1061-1062`, `:79-83`).
6. Dispatch on `data.get("_type")` (`:1064-1090`):
   * `"metadata"`: replace `metadata` with `data["metadata"]` **if it is a dict, else `{}`**; set
     `created_at`/`updated_at` via `datetime.fromisoformat` when the value is a non-empty `str`
     (**no** `suppress` here — a bad format raises `ValueError`); `last_consolidated = _archive_offset(data)`
     (`:1065-1084`).
   * `"provider_state"`: `provider_state = ProviderConversationState.from_private_record(data.get("state"))`
     (`:1085-1088`).
   * else: `messages.append(data)` (`:1089-1090`).
7. Construct `Session(key=key, messages=…, created_at=created_at or datetime.now(),
   updated_at=updated_at or datetime.now(), metadata=…, last_consolidated=…, provider_state=…)`
   (`:1092-1100`). `Session.__post_init__` (`:289-301`) then:
   * forces `metadata = {}` if it is not a dict;
   * forces `provider_state = None` if it is not a `ProviderConversationState`;
   * **resets `last_consolidated` to `0`** unless it is a non-bool `int` with `0 <= v <= len(messages)`.
8. Overlay the checkpoint sidecar (`:1101`, see §7).
9. Run the legacy `write_stdin` → `exec_session` migration; if it changed anything, **drop the
   provider state** (`:1102-1103`, §11.4).
10. On `_SESSION_DATA_ERRORS = (ValueError, TypeError, AttributeError, KeyError)` (`:51`): log a warning
    and delegate to `_repair_unlocked(key)` (`:1105-1114`).

`OBSERVED` — `OSError` (including `PermissionError`) is **not** in `_SESSION_DATA_ERRORS` and therefore
propagates. `TestLoadErrors::test_permission_error_is_not_treated_as_corrupt_data`
(`tests/session/test_session_fsync.py:229-243`) asserts this for `get_or_create`, `read_session_file`,
`read_session_metadata`, and `list_sessions`.

### 4.2 Ordering and duplicate assumptions

`OBSERVED` — the file is processed **strictly in line order**; there is no sorting, no timestamp
ordering, no dedup. `INFERENCE`:

* **Last metadata record wins** — every `"metadata"` record overwrites `metadata`, `created_at`,
  `updated_at`, `last_consolidated` (`:1065-1084`). A duplicate metadata line is not an error.
* **Last provider_state record wins** (`:1085-1088`).
* **Message order is file order**, exactly as written. There is no re-sorting by `timestamp`.
* **No deduplication of messages.** Duplicate rows are preserved. `INFERENCE`: dedup-like behaviour in
  the codebase lives elsewhere (e.g. the subagent-followup check at `nanobot/agent/loop.py:2290-2292`),
  not in the store.
* **`load` does not require the metadata record to be first.** `update_metadata` and `list_sessions`
  do (§4.4, §5.3).

### 4.3 Corruption recovery — `repair`

`repair(key, path=None)` takes the lock and calls `_repair_unlocked` (`manager.py:1116-1118`).

`_repair_unlocked` (`manager.py:1120-1199`) is the **tolerant** reader:

* Per line, `json.loads` failures increment `skipped` and `continue` (`:1140-1144`).
* Non-dict JSON increments `skipped` and `continue` (`:1145-1147`).
* `created_at` / `updated_at` parsing is wrapped in `with suppress(ValueError)` (`:1159-1165`) — unlike
  the strict `_load_unlocked` path.
* A `provider_state` record whose `state` fails validation increments `skipped` (`:1167-1173`).
* If `skipped`, logs `"Skipped {n} corrupt lines in session {key}"` (`:1178-1179`).
* **Returns `None`** when `not messages and not metadata and provider_state is None` (`:1181-1182`).
* Otherwise builds the same `Session`, overlays the checkpoint, runs the legacy migration, returns it
  (`:1184-1196`).
* A `_SESSION_DATA_ERRORS` failure here returns `None` (`:1197-1199`).

**`repair` never writes.** `OBSERVED` — there is no write call in `_repair_unlocked`; the corrupt file
stays on disk unchanged. It is a read-only salvage path. `test_list_sessions_ignores_legacy_stem` asserts
`corrupt_path.exists()` after listing (`tests/session/test_session_list_repair_legacy.py:33`).

`_load_unlocked` uses the repair result but **does not persist it** (`:1107-1114`).

### 4.4 `read`, `read_metadata`, `list_sessions`

**`_read_unlocked`** (`manager.py:1432-1487`) — the payload view:

* Same line loop; `metadata` records populate `metadata`/`created_at`/`updated_at`/`stored_key`
  (`:1450-1468`).
* `provider_state` records are **skipped entirely** (`:1469-1470`) — the private record never leaks into
  this view.
* Other records append to `messages` (`:1471-1472`).
* Runs `_migrate_legacy_exec_session_records(messages, metadata)` **on the returned copies** (`:1473`) —
  note this mutates the in-memory payload but not the file.
* Returns `{"key": stored_key or key, "created_at": …, "updated_at": …, "metadata": …, "messages": …}`
  (`:1474-1480`). `created_at`/`updated_at` are `None` when absent or non-string (`:1460-1465`).
* On `_SESSION_DATA_ERRORS`: repair, and on success return `session_payload(repaired)` (`:1481-1487`).
  `session_payload` (`:1201-1209`) **re-derives `created_at`/`updated_at` as `datetime.now()`** when the
  file lacked them, because the repaired `Session` was built with those fallbacks.

**`_read_metadata_unlocked`** (`manager.py:1493-1537`) — first-record-only:

* Iterates lines, skipping blanks, and on the **first non-blank line** parses it. If
  `data.get("_type") != "metadata"` → **return `None`** (`:1505-1506`). The rest of the file is never read.
* Returns `{"key": <stored key if non-empty str else key>, "created_at": str|None, "updated_at": str|None,
  "metadata": dict}` (`:1507-1524`). Note: **no `last_archived`/`last_consolidated`** in this payload.
* Empty file → the loop ends → `return None` (`:1525`).
* On data errors: repair, and on success return the repaired values with `isoformat()` timestamps
  (`:1526-1537`).

**`_list_sessions_unlocked`** (`manager.py:1543-1641`):

1. `for path in self.sessions_dir.glob("*.jsonl")` — **non-recursive** (`:1546`). Temp files
   (`.<name>.<hex>.tmp`) and `.checkpoint.json` do not match; `.migration-conflicts/*.jsonl` is not
   descended into.
2. `storage_key = self.session_key_from_path(path)`; `None` → **skip the file** (`:1547-1549`).
3. `first_line = f.readline().strip()`; **if the first line is blank the entire file is skipped**
   (`:1552-1553`) — `INFERENCE`: a leading blank line makes a session invisible to listing while
   `load()` still works.
4. Parse the first line; require `_type == "metadata"` (`:1554-1556`). Otherwise the file is skipped.
5. `key = stored key if non-empty str else storage_key` (`:1557-1562`).
6. `title = _metadata_title(metadata)` (`:1564`): `metadata["title"]` if it is a `str`; returned raw when
   `metadata["title_user_edited"] is True`, otherwise passed through `strip_think` (`:254-263`).
7. Preview scan over the remaining lines (`:1569-1596`):
   * skip blank lines;
   * skip lines matching `_is_provider_state_record_line`;
   * count `scanned_records` and `scanned_chars`; break when `scanned_records > 200` or
     `scanned_chars > 1_000_000` (`:49-50`, `:1576-1580`);
   * parse and skip records whose `_type` is `metadata` or `provider_state` (`:1583-1587`);
   * `text = _message_preview_text(item)`; empty → continue;
   * first `role == "user"` text becomes `preview` and **breaks the loop**; otherwise the first
     `role == "assistant"` text is kept as `fallback_preview` (`:1588-1595`);
   * `preview = preview or fallback_preview` (`:1596`).
8. `fallback_time = datetime.fromtimestamp(path.stat().st_mtime).isoformat()` (`:1597`) — used when the
   record lacks `created_at`/`updated_at`.
9. Emits `{"key","created_at","updated_at","title","preview","path"}` where `path` is `str(path)`
   (`:1600-1617`).
10. `except FileNotFoundError: continue` (`:1618-1619`); `except _SESSION_DATA_ERRORS:` → repair and, on
    success, emit a repaired row whose preview is the first non-empty `_message_preview_text`
    (`:1620-1640`).
11. Final sort: `sorted(sessions, key=lambda item: item["updated_at"], reverse=True)` (`:1641`) —
    **lexicographic string sort of the ISO timestamps, descending**. `INFERENCE`: rows with mixed
    timezone offsets do not sort chronologically.

`_message_preview_text` (`manager.py:245-251`) applies `public_history_message` (§8), scrubs subagent
announce bodies when `injected_event == "subagent_result"`, then `_text_preview` (`:221-242`):
concatenate string content or `type=="text"` blocks, apply `_sanitize_assistant_replay_text`
(`:205-218`), collapse whitespace, truncate to 120 chars with a `…` suffix (`:48`, `:240-242`).

---

## 5. WRITE ALGORITHM

### 5.1 `save()` — full rewrite

`save(session, *, fsync=False)` takes `_session_files_lock` and calls `_save_unlocked`
(`manager.py:1211-1213`). `SessionManager.save` short-circuits when `not session.policy.persist`
(`manager.py:1786-1792`).

`_save_unlocked` (`manager.py:1314-1361`) — verbatim body:

```python
path = self.get_session_path(session.key)
tmp_path = path.with_name(f".{path.name}.{secrets.token_hex(8)}.tmp")
try:
    with open(tmp_path, "x", encoding="utf-8") as f:
        metadata_line = { ... }                                   # §3.3
        f.write(json.dumps(metadata_line, ensure_ascii=False) + "\n")
        if session.provider_state is not None:
            provider_state_line = { ... }                          # §3.4
            f.write(json.dumps(provider_state_line, ensure_ascii=False) + "\n")
        for msg in session.messages:
            f.write(json.dumps(msg, ensure_ascii=False) + "\n")
        if fsync:
            f.flush()
            os.fsync(f.fileno())
    os.replace(tmp_path, path)
    self.get_runtime_checkpoint_path(session.key).unlink(missing_ok=True)
    if fsync:
        with suppress(PermissionError):
            fd = os.open(str(path.parent), os.O_RDONLY)
            try:
                os.fsync(fd)
            except OSError as exc:
                if exc.errno != errno.EINVAL:
                    raise
            finally:
                os.close(fd)
finally:
    tmp_path.unlink(missing_ok=True)
```

Normative consequences:

| Property | Behaviour | Citation |
|---|---|---|
| Append vs rewrite | **Always a full rewrite.** The whole file is regenerated from memory on every save | `:1318-1339` |
| Temp file | same directory, name `.<final-name>.<16 hex chars>.tmp`, created with `"x"` (exclusive) | `:1316`, `:1319` |
| Metadata line | written first, always, and **updated on every save** (created_at/updated_at/metadata/last_archived/last_consolidated) | `:1320-1331` |
| Provider state line | written **only if** `session.provider_state is not None`; positioned immediately after the metadata line | `:1332-1337` |
| Atomicity | `os.replace(tmp, path)` — same-directory rename, atomic on POSIX | `:1344` |
| Checkpoint sidecar | **unconditionally unlinked** after a successful replace | `:1348` |
| fsync | `fsync=False` (default): **no fsync at all**, not even the file. `fsync=True`: file fsync **before** close, then directory fsync after replace | `:1340-1342`, `:1350-1359` |
| Directory fsync errors | `PermissionError` suppressed; `OSError` with `errno == EINVAL` swallowed (unsupported on some filesystems); **all other `OSError` re-raised** | `:1350-1359`; `tests/session/test_session_fsync.py:78-106` |
| File mode | **Not set explicitly.** `open(tmp,"x")` uses the process umask → typically `0o644`. Contrast with `_write_text_atomic` (`0o600`) and the checkpoint writer (`0o600`) | `:1319` vs `:606-617`, `:1251` |
| Cleanup | `tmp_path.unlink(missing_ok=True)` in `finally`, even on failure | `:1360-1361` |

`OBSERVED` — `test_save_with_fsync_calls_fsync` expects **2** `os.fsync` calls on non-Windows (file +
directory) and **1** on Windows (`tests/session/test_session_fsync.py:48-56`). `INFERENCE`: the directory
fsync is skipped on Windows because `os.open(dir, O_RDONLY)` fails there and `PermissionError` is
suppressed.

### 5.2 `update_metadata()` — surgical first-line rewrite

`update_metadata(key, updates, *, fsync=False)` (`manager.py:1363-1404`):

1. Under the lock, `path = get_session_path(key)`; if it does not exist → return `False` (`:1372-1374`).
2. Open the file and `readline()` **only the first line** (`:1377-1378`).
3. `json.loads(first_line)`; if `data.get("_type") != "metadata"` → return `False` (`:1379-1381`).
4. Copy `data["metadata"]` (or `{}`), `metadata.update(deepcopy(updates))`, `data["metadata"] = metadata`
   (`:1382-1389`).
5. Write the new first line, then `shutil.copyfileobj(source, target)` — **the rest of the file is copied
   byte-for-byte** (`:1390-1392`).
6. `os.replace(tmp, path)`; directory fsync when `fsync` (`:1396-1398`).
7. Returns `True`; on `_SESSION_DATA_ERRORS` logs and returns `False` (`:1400-1402`).

`OBSERVED` — this is a **merge**, not a replace: keys present in `updates` overwrite, keys absent are
preserved. `updates` is deep-copied so the caller's dict is not aliased (`:1388`).

`OBSERVED` — `SessionManager.update_session_metadata` additionally mirrors the updates into the cached
`Session` object when one is cached (`manager.py:1950-1961`).

`OBSERVED` — used by `SessionHandleResolver` to allocate `session_handle` names
(`nanobot/session/session_handles.py:186-195`).

### 5.3 Concurrency, locking, and concurrent writers

`OBSERVED` — `filelock>=3.25.2` (`pyproject.toml:53`), imported as
`from filelock import FileLock` (`manager.py:21`).

Two locks:

| Lock | Path | Timeout | Purpose | Citation |
|---|---|---|---|---|
| Migration lock | `<root>/.workspace-migration.lock` | `_SESSION_MIGRATION_LOCK_TIMEOUT_SECONDS = 30` | guards workspace-id allocation and namespace claim | `manager.py:74`, `:568-571` |
| Session-files lock | `<sessions_dir>/.session-files.lock` | **default** (filelock default is `-1` = wait forever) | guards every read/write/delete/list of canonical files | `manager.py:75`, `:581-583` |

Methods that take `self._session_files_lock`: `locked_session_files` (`:587-591`), `load` (`:1039`),
`repair` (`:1117`), `save` (`:1212`), `save_runtime_checkpoint` (`:1222`), `update_metadata` (`:1371`),
`delete` (`:1407`), `read` (`:1429`), `read_metadata` (`:1490`), `list_sessions` (`:1540`).
`restore_to_workspace` takes **both** locks (`:974`).

`INFERENCE` — **the locks are advisory and per-host.** They only serialise writers that go through a
`JsonlSessionStore`. A Go implementation must acquire the *same* `<sessions_dir>/.session-files.lock`
with the same protocol to be safe alongside a running Python process; otherwise the last `os.replace`
wins and the other writer's changes are silently lost (this is the classic lost-update on whole-file
rewrite).

`OBSERVED` — lock re-entrancy: `_load_unlocked` calls `_repair_unlocked` while the lock is already held
(`:1107`), and `_repair_unlocked` does not re-acquire it. `FileLock` instances in `filelock` are
re-entrant per object, so this is safe. A Go port must not deadlock on the equivalent nesting.

`INFERENCE` — cross-process behaviour is **last-writer-wins at file granularity**: each `save` replaces
the whole file, so a concurrent writer that read an older snapshot will clobber the newer content once it
acquires the lock. There is no merge, no version check, no compare-and-swap. `INFERENCE`: correctness
across concurrent writers relies entirely on holding the lock across the read-modify-write cycle, which
`SessionManager` does not do — `get_or_create` loads under the lock, then the caller mutates in memory,
then `save` re-acquires the lock (`:1740-1759`, `:1786-1792`).

### 5.4 Delete

`_delete_unlocked` (`manager.py:1410-1426`) unlinks, in order, and ignoring missing files:

1. `get_session_path(key)` — canonical
2. `get_runtime_checkpoint_path(key)` — sidecar
3. `get_legacy_lossy_path(key)` — retired lossy path in `sessions_dir`
4. `get_legacy_session_path(key)` — retired global path in `legacy_sessions_dir`

Returns `True` if **any** unlink succeeded. `OSError` per path is logged, not raised (`:1424-1425`).
`SessionManager.delete_session` also invalidates the cache and fires the delete observer
(`manager.py:1870-1876`).

`OBSERVED` — `test_delete_session_removes_runtime_checkpoint` (`tests/session/test_session_store.py:288-299`)
and `test_delete_session_cleans_legacy_file` / `..._both_locations`
(`tests/agent/test_session_delete.py:96-144`).

### 5.5 Cache (relevant to write semantics)

`SessionManager` keeps an LRU `OrderedDict` of at most `SESSION_CACHE_MAX_SIZE = 128` sessions plus a
`WeakValueDictionary` overflow (`manager.py:44`, `:1659-1683`). `INFERENCE`: two `SessionManager`
instances in one process do **not** share cache, so a write through one is invisible to the other until
it reloads. `flush_all()` re-saves every cached session with `fsync=True` for graceful shutdown
(`manager.py:1847-1863`).

---

## 6. SESSION KEY FORMAT

### 6.1 Shape

`OBSERVED` — the canonical shape is `"{channel}:{chat_id}"`:

```python
def session_key_for_channel(channel: str, chat_id: str, *, unified_session: bool = False) -> str:
    if unified_session:
        return UNIFIED_SESSION_KEY
    return f"{channel}:{chat_id}"
```
(`nanobot/session/keys.py:12-16`)

`UNIFIED_SESSION_KEY = "unified:default"` (`nanobot/session/keys.py:8`).

`OBSERVED` — the default derivation on an inbound message:

```python
def session_key(self) -> str:
    return self.session_key_override or f"{self.channel}:{self.chat_id}"
```
(`nanobot/bus/events.py:40-42`)

### 6.2 Observed key namespaces

| Namespace | Example | Source |
|---|---|---|
| channel DM/group | `telegram:1`, `discord:guild:123`, `cli:default` | `session/keys.py:16`; `bus/events.py:42` |
| unified session | `unified:default` | `session/keys.py:8` |
| WebUI | `websocket:<chat_id>` where `chat_id` matches `^[A-Za-z0-9_:-]{1,64}$` | `nanobot/webui/session_identity.py:9-20` |
| Dream runs | `dream:YYYYMMDD-HHMMSS` | `nanobot/agent/memory.py:700-702` |
| Subagent | `system:subagent:…` shape is used in routing (e.g. `nanobot/agent/subagent.py:516-527` sets `chat_id=f"{origin['channel']}:{origin['chat_id']}"`) | `nanobot/agent/subagent.py:516-527` |

`OBSERVED` — a chat_id may itself contain `:`, so **a key is not safely splittable on the first `:`
alone for routing purposes**; the codebase splits at most once where needed
(`last_channel_from_metadata`: `route.split(":", 1)`, `nanobot/session/keys.py:39`).

`OBSERVED` — keys are bounded to **512 characters** at the session-handle and session-message
boundaries (`nanobot/session/session_handles.py:16`, `nanobot/session/session_messages.py:13`).
`INFERENCE`: this bound is enforced by those subsystems, not by the store; `storage_key` accepts any
length. **UNVERIFIED:** whether any code path rejects a >512-char key before it reaches the store.

`OBSERVED` — `InboundMessage.session_key_override` exists specifically for thread-scoped sessions
(`nanobot/bus/events.py:35`), and the agent loop returns `UNIFIED_SESSION_KEY` when unified mode is on and
no override is set (`nanobot/agent/loop.py:903-904`).

### 6.3 Metadata route

`OBSERVED` — `remember_last_channel` stores `metadata["last_channel"] = f"{channel}:{chat_id}"`
(`nanobot/session/keys.py:19-27`); `last_channel_from_metadata` reads it back, requires a `":"` and
non-empty halves, and splits on the first `:` (`nanobot/session/keys.py:30-42`).
Used to resolve the delivery route for unified sessions (`nanobot/agent/context.py:58-65`).

### 6.4 Workspace id derivation

`OBSERVED` — 32 lowercase hex characters (`^[0-9a-f]{32}$`, `manager.py:73`), generated by
`secrets.token_hex(16)` (`manager.py:681`, `:758`), stored at `<workspace>/.nanobot/workspace-id`
(`manager.py:71-72`, `:631-637`), written atomically with mode `0o600`
(`_write_text_atomic` default, `manager.py:606`), content `f"{workspace_id}\n"` (`manager.py:678`,
`:689`, `:706`).

Full algorithm: §1.3.

---

## 7. RUNTIME CHECKPOINT SIDECAR

### 7.1 Constants

```python
_RUNTIME_CHECKPOINT_KEY = "runtime_checkpoint"
_RUNTIME_CHECKPOINT_VERSION = 1
_RUNTIME_CHECKPOINT_SUFFIX = ".checkpoint.json"
```
(`nanobot/session/manager.py:57-59`)

### 7.2 Path

`get_runtime_checkpoint_path(key) = sessions_dir / f"{storage_key(key)}.checkpoint.json"`
(`manager.py:1029-1030`).

### 7.3 Format

Written by `save_runtime_checkpoint` (`manager.py:1215-1260`):

```python
payload: dict[str, Any] = {
    "version": _RUNTIME_CHECKPOINT_VERSION,          # 1
    "session_key": session.key,
    "base_updated_at": session.updated_at.isoformat(),
    "base_message_count": len(session.messages),
    "checkpoint": checkpoint,                         # session.metadata["runtime_checkpoint"], a dict
    "provider_state": (
        session.provider_state.to_private_record()
        if session.provider_state is not None
        else None
    ),
}
target = self.get_runtime_checkpoint_path(session.key)
tmp = target.with_name(f".{target.name}.{secrets.token_hex(8)}.tmp")
try:
    with open(tmp, "x", encoding="utf-8") as handle:
        os.chmod(tmp, 0o600)
        json.dump(payload, handle, ensure_ascii=False, separators=(",", ":"))
    os.replace(tmp, target)
finally:
    tmp.unlink(missing_ok=True)
```

Differences from the main JSONL writer — **these matter**:

| Property | Checkpoint sidecar | Main JSONL |
|---|---|---|
| Serializer | `json.dump(..., ensure_ascii=False, separators=(",", ":"))` — **compact**, no spaces | `json.dumps(..., ensure_ascii=False)` — default `", "` / `": "` separators |
| Format | single JSON object, **not** JSONL | one JSON object per line |
| File mode | `os.chmod(tmp, 0o600)` | not set (umask default) |
| fsync | **none** | only when `fsync=True` |
| Citation | `:1248-1258` | `:1316-1344` |

### 7.4 When written

* `SessionManager.save_runtime_checkpoint(session)` (`manager.py:1794-1804`):
  * returns immediately when `not session.policy.persist`;
  * delegates to the JSONL store only when `self._store is self._jsonl_store`; a third-party store falls
    back to a full `save(session)`.
* `JsonlSessionStore.save_runtime_checkpoint` (`manager.py:1215-1260`):
  * takes the session-files lock;
  * **if the main JSONL does not exist yet, it performs a full `_save_unlocked(session)` and returns**
    (`:1223-1228`) — the sidecar is never written without a base file;
  * if `session.metadata["runtime_checkpoint"]` is **not** a dict, it **unlinks** the sidecar and returns
    (`:1230-1233`);
  * otherwise writes the payload atomically.
* Producer: `AgentLoop._set_runtime_checkpoint` (`nanobot/agent/loop.py:2303-2306`) sets
  `session.metadata["runtime_checkpoint"] = payload` then calls `save_runtime_checkpoint`.
* The payload is prepared in `AgentLoop.run`'s `_checkpoint` closure
  (`nanobot/agent/loop.py:976-985`): the `provider_state` entry is **popped** out of the public payload
  and moved onto `session.provider_state`; a `provider_state_checkpoint_version` marker is added instead.
* Checkpoint payload shapes emitted by `AgentRunner` (`nanobot/agent/runner.py:196-205`, `:496-506`,
  `:551-564`, `:765-775`):
  * `phase: "final_response"` — `iteration`, `model`, `assistant_message`, `completed_tool_results: []`,
    `pending_tool_calls: []`
  * `phase: "awaiting_tools"` — plus `pending_tool_calls: [<openai tool call>, …]`
  * `phase: "tools_completed"` — plus `completed_tool_results: [<tool message>, …]`,
    `pending_tool_calls: []`
  * `phase: "error"` (`nanobot/agent/runner.py:1025`) — **no producer contract**; treated as
    review-only (`nanobot/session/recovery.py:270-273`)

### 7.5 When read — `_overlay_runtime_checkpoint_unlocked`

`manager.py:1262-1312`, called from both `_load_unlocked` (`:1101`) and `_repair_unlocked` (`:1193`):

1. `lstat()` the sidecar. `FileNotFoundError` → return silently (`:1300-1301`).
2. If the entry is **not a regular file** → log a warning and return, **without deleting** (`:1265-1271`).
3. **Staleness by mtime**: `if main_path.stat().st_mtime_ns > checkpoint_stat.st_mtime_ns:` → unlink the
   sidecar and return (`:1275-1277`). Rationale in-source: closes the crash window between replacing the
   JSONL and unlinking its previous sidecar.
4. Parse the JSON and require a dict (`:1278`).
5. Validate all of (`:1279-1285`):
   * `version == 1`
   * `session_key == session.key`
   * `base_updated_at == session.updated_at.isoformat()`
   * `base_message_count == len(session.messages)`
   * `isinstance(checkpoint, dict)`
   Any failure → **unlink the sidecar** and return.
6. `provider_state`: `None` stays `None`; otherwise `ProviderConversationState.from_private_record(...)`;
   an invalid non-`None` record raises `ValueError` (`:1288-1295`).
7. On success: `session.metadata["runtime_checkpoint"] = raw["checkpoint"]` and
   `session.provider_state = provider_state` (`:1296-1299`).
8. On `_RUNTIME_CHECKPOINT_DATA_ERRORS = (OSError, ValueError, TypeError, AttributeError, KeyError)`
   (`manager.py:52`): log a warning and unlink the sidecar if it is a regular non-symlink file
   (`:1302-1312`).

`OBSERVED` — the fingerprint is **exact string equality** against
`session.updated_at.isoformat()`, where `session.updated_at` was itself parsed from the JSONL metadata
record's `updated_at` via `datetime.fromisoformat`. This is the single most fragile interop point; see §12.

`OBSERVED` — `test_completed_session_supersedes_stale_checkpoint` (`tests/session/test_session_store.py:264-285`)
covers the stale-sidecar case including a simulated crash after the main replace;
`test_invalid_runtime_checkpoint_is_discarded` (`:302-313`) covers the malformed case.

### 7.6 When cleared

| Trigger | Citation |
|---|---|
| Any successful `_save_unlocked` — unconditional `unlink(missing_ok=True)` after the replace | `manager.py:1348` |
| Sidecar staleness / validation failure during overlay | `:1276`, `:1286`, `:1312` |
| `metadata["runtime_checkpoint"]` not a dict during `save_runtime_checkpoint` | `:1232` |
| `delete_session` / `_delete_unlocked` | `:1413` |
| `Session.clear()` clears `provider_state` but **not** `metadata["runtime_checkpoint"]` | `:477-483` — **UNVERIFIED** whether a caller relies on this |

`OBSERVED` — `restore_runtime_checkpoint(session)` (`nanobot/session/recovery.py:276-371`) is the consumer
that materializes a checkpoint into history: it appends the assistant row and completed tool results,
synthesizes interrupted tool rows for pending calls (`content: "Error: Task interrupted before this tool
finished."`, `_recovery_interrupted: True`, `role: "tool"`), dedups by an overlap scan over
`_checkpoint_message_key`, then pops `runtime_checkpoint` and `pending_user_turn` and sets
`session.updated_at = datetime.now()`. It clears `session.provider_state` unless the checkpoint is
`provider_state_checkpoint_version == "v1"` **and** the phase is exactly `final_response` or
`tools_completed` with matching rows (`:346-366`). It **does not write** — the caller saves.

---

## 8. HISTORY VISIBILITY

### 8.1 Constants

```python
HIDDEN_HISTORY_META = "_hidden_history"                        # nanobot/session/history_visibility.py:10
AUTOMATION_HISTORY_META = "_automation_turn"                   # nanobot/session/automation_turns.py:10
RUNTIME_CONTEXT_HISTORY_META = "_runtime_context"              # nanobot/runtime_context.py:14
RUNTIME_CONTEXT_MESSAGE_META = "runtime_context"               # nanobot/runtime_context.py:15
SUMMARY_CONTINUATION_TEXT = (
    "Continue the active task from the working-memory checkpoint above."
)                                                              # nanobot/session/summary.py:12-14
```

### 8.2 Marking a message hidden

```python
def _has_hidden_history_marker(message: Mapping[str, Any] | None) -> bool:
    if not message:
        return False
    marker = message.get(HIDDEN_HISTORY_META)
    return marker is True or isinstance(marker, Mapping)


def is_hidden_history_message(message: Mapping[str, Any] | None) -> bool:
    """True for persisted messages that should not be shown as chat turns."""
    return _has_hidden_history_marker(message) or is_automation_history_message(message)
```
(`nanobot/session/history_visibility.py:13-22`)

So a message is hidden when **any** of:

* `message["_hidden_history"] is True` — e.g. the summary checkpoint
  (`manager.py:332-337`), the subagent injected result (`nanobot/agent/loop.py:1054`)
* `message["_hidden_history"]` is a mapping (e.g. `{"kind": "subagent_result", "subagent_task_id": …}`,
  `nanobot/agent/loop.py:1044-1054`)
* `is_automation_history_message(message)` is true (`nanobot/session/automation_turns.py:75-86`):
  * `message["_automation_turn"] is True` or is a mapping, **or**
  * `message[spec.legacy_history_meta_key] is True` for any registered automation spec.

`OBSERVED` — the registered specs are `CRON_AUTOMATION_SPEC` and `LOCAL_TRIGGER_AUTOMATION_SPEC`, loaded
lazily (`nanobot/session/automation_turns.py:55-61`). **UNVERIFIED** — I did not read
`nanobot/cron/session_turns.py` or `nanobot/triggers/local_session_turns.py`, so the concrete values of
`legacy_history_meta_key` and `kind` are not established here.

`OBSERVED` — `automation_history_overrides_for_spec` builds
`{"_automation_turn": {"kind": …}, <legacy_key>: True, <history fields>}` (`automation_turns.py:42-49`).

### 8.3 `public_history_message` — exact removal of runtime context

`nanobot/runtime_context.py:215-240`:

```python
def public_history_message(message: Mapping[str, Any]) -> dict[str, Any]:
    cleaned = deepcopy(dict(message))
    marker = cleaned.pop(RUNTIME_CONTEXT_HISTORY_META, None)
    if not isinstance(marker, Mapping):
        return cleaned
    marker_data = cast(Mapping[str, Any], marker)
    if marker_data.get("version") != 1:
        return cleaned

    content = cleaned.get("content")
    suffix = marker_data.get("suffix")
    if isinstance(content, str) and isinstance(suffix, str) and suffix:
        if content == suffix:
            cleaned["content"] = ""
        elif content.endswith("\n\n" + suffix):
            cleaned["content"] = content[: -(len(suffix) + 2)]
        return cleaned

    expected = marker_data.get("blocks")
    if isinstance(content, list) and isinstance(expected, list) and expected:
        expected_blocks = cast(list[Any], expected)
        count = len(expected_blocks)
        if content[-count:] == expected_blocks:
            cleaned["content"] = content[:-count]
    return cleaned
```

Precise semantics for a Go port:

1. Always `deepcopy` the message; always `pop` the `_runtime_context` key. The returned object never
   carries the marker.
2. If the marker is absent or not a mapping, return the copy **with the marker removed** and content
   untouched.
3. If `marker["version"] != 1`, return the copy **with the marker removed** and content untouched.
4. String case: remove exactly `"\n\n" + suffix` from the end, or the whole content when it equals the
   suffix. **If neither matches, content is left unchanged** (the marker is still removed).
5. List case: remove the trailing `len(marker["blocks"])` elements only if they are **deep-equal** to
   `marker["blocks"]`. Otherwise content is left unchanged.

`OBSERVED` — the producer that guarantees the exact match is `append_runtime_context`
(`nanobot/runtime_context.py:120-145`): for string content it returns
`f"{text}\n\n{suffix}"` (or just `suffix` when text is empty) and the marker
`{"version": 1, "sources": [...], "suffix": suffix}`; for list content it returns
`[*content, *context_blocks]` and `{"version": 1, "sources": [...], "blocks": context_blocks}` where each
context block is `{"type": "text", "text": text}`.

`OBSERVED` — `public_history_messages` maps the helper over an iterable
(`nanobot/runtime_context.py:243-245`).

### 8.4 Where hidden-ness and public projection are applied

| Consumer | Behaviour | Citation |
|---|---|---|
| `Session.get_history` | `include_runtime_context=False` → `public_history_message(message)`; drops `_command` messages; drops empty assistant rows without `tool_calls`/`reasoning_content`/`thinking_blocks` | `manager.py:386-393`, `:435-437` |
| `Session.get_history` output projection | only `role`, `content`, `tool_calls`, `tool_call_id`, `name`, `reasoning_content`, `thinking_blocks` are forwarded | `manager.py:438-442` |
| `list_sessions` preview | `public_history_message` via `_message_preview_text` | `manager.py:245-251` |
| WebUI visible turns | `role in {"user","assistant"}` and not `_command` and not `is_hidden_history_message` | `nanobot/webui/session_access.py:70-74` |
| Session fork | user-message index counts only **non-hidden** user messages; every copied message goes through `public_history_message` | `manager.py:1905-1911` |
| Title generation | skips `_command` and hidden messages, then applies `public_history_message` | `nanobot/session/webui_turns.py:148-152`, `:176-179` |
| Memory archive | skips `_command` and `is_summary_checkpoint` messages | `nanobot/agent/memory.py:1005-1008`, `:1239-1243` |
| Auto-compact | skips `_command` and `is_summary_checkpoint` messages | `nanobot/agent/autocompact.py:57-61` |

`OBSERVED` — `is_summary_checkpoint(message)` = hidden **and** `content == SUMMARY_CONTINUATION_TEXT`
(`nanobot/session/summary.py:16-21`). This is how the durable compaction boundary is recognised.

`OBSERVED` — `Session.get_history` explicitly **does not** filter hidden messages out of the LLM replay;
hidden messages (except `_command`) still reach the model. Only `_command` is dropped
(`manager.py:386-387`). `INFERENCE`: hiding is a **presentation** concept, not an LLM-context concept.

---

## 9. COMPACTION / LEGAL MESSAGE START

### 9.1 `find_legal_message_start`

```python
def find_legal_message_start(messages: list[dict[str, Any]]) -> int:
    """Find the first index whose tool results have matching assistant calls."""
    declared: set[str] = set()
    start = 0
    for i, msg in enumerate(messages):
        role = msg.get("role")
        if role == "assistant":
            for raw_call in cast(list[object], msg.get("tool_calls") or []):
                tool_call = cast(dict[str, Any], raw_call) if isinstance(raw_call, dict) else None
                if tool_call is not None and tool_call.get("id"):
                    declared.add(str(tool_call["id"]))
        elif role == "tool":
            tid = msg.get("tool_call_id")
            if tid and str(tid) not in declared:
                start = i + 1
                declared.clear()
    return start
```
(`nanobot/utils/helpers.py:478-494`)

Exact semantics:

* Single forward pass. `declared` accumulates tool-call ids from **every** assistant message seen so far.
* On a `role == "tool"` row whose `tool_call_id` is truthy and **not** in `declared`: set `start = i + 1`
  and **clear** `declared`.
* Return `start` (0 when no orphan was found).
* `tool_call_id` and `tool_call["id"]` are compared as **strings** (`str(...)`), so an integer id in the
  file is coerced. `None`/`""`/`0`/`false` ids are ignored (`if tid`).
* Non-dict elements in `tool_calls` are ignored (`isinstance(raw_call, dict)`).
* An assistant `tool_calls` value that is not a list iterates as `[]` because of `or []`
  (`INFERENCE`: `msg.get("tool_calls")` returning a non-list truthy value, e.g. a string, would iterate
  its characters and find no dicts — harmless but worth replicating).
* `start` is the index **after** the last orphan tool row encountered, not necessarily the first orphan's
  index. `INFERENCE`: with multiple orphan clusters, the returned start skips past the **last** orphan.

Call sites: `Session.get_history` before projection (`manager.py:380-382`) and again after the token
budget trim (`manager.py:471-473`).

### 9.2 `recent_message_start_index`

```python
def recent_message_start_index(
    messages: list[dict[str, Any]],
    max_messages: int,
    *,
    extend_to_user: bool = False,
) -> int:
    """Return the start index for a recent replay window."""
    if max_messages <= 0:
        return len(messages)
    start_idx = max(0, len(messages) - max_messages)
    if not extend_to_user or len(messages) <= max_messages:
        return start_idx
    if any(messages[i].get("role") == "user" for i in range(start_idx, len(messages))):
        return start_idx

    recovered_user = next(
        (i for i in range(start_idx - 1, -1, -1) if messages[i].get("role") == "user"),
        None,
    )
    if recovered_user is None:
        return start_idx
    if recovered_user > 0 and messages[recovered_user - 1].get("_channel_delivery"):
        return recovered_user - 1
    return recovered_user
```
(`nanobot/utils/helpers.py:452-475`)

Exact semantics:

* `max_messages <= 0` → return `len(messages)` — **an empty window**, not the whole list. `INFERENCE`:
  this is a "no window requested" sentinel; the caller `Session.get_history` guards with
  `if max_messages <= 0: start_idx = 0` and never passes a non-positive value here
  (`manager.py:359-366`).
* Otherwise `start_idx = max(0, len(messages) - max_messages)`.
* If `extend_to_user` is false, or the window already contains a `user` row, return `start_idx`.
* Otherwise walk **backwards** from `start_idx - 1` for the nearest `user` row.
* If that row is at index `> 0` and the row immediately before it has truthy `_channel_delivery`, return
  `recovered_user - 1` — i.e. keep the proactive assistant delivery the user is replying to.
* If no user row exists before the window, return the original `start_idx`.

### 9.3 How compaction consumes these

`Session.commit_summary_checkpoint(summary, *, insert_at=None, last_active=None)`
(`manager.py:323-342`):

1. `boundary = len(self.messages) if insert_at is None else insert_at`.
2. Insert at `boundary` a message
   `{"role": "user", "content": SUMMARY_CONTINUATION_TEXT, "_hidden_history": True, "timestamp": now}`.
3. `self.metadata["_last_summary"] = {"text": summary, "last_active": (last_active or self.updated_at).isoformat()}`.
4. `self.last_archived = boundary`.

`Session.get_history` then replays `self.messages[self.last_archived:]` (`manager.py:358`), so the
pre-boundary transcript is replaced by the summary plus the hidden continuation marker.

`OBSERVED` — the checkpoint is always inserted **before** the boundary index, so after insertion the
marker sits at index `boundary` and `last_archived == boundary` points at it.

`OBSERVED` — `Session.clear()` resets `messages`, `last_archived`, `provider_state`, and pops
`_last_summary` (`manager.py:477-483`).

`OBSERVED` — `_last_summary` read/validation: `session_summary_from_metadata` requires
`metadata["_last_summary"]` to be a mapping with a non-empty string `text`; `last_active` is used only
when it parses with `datetime.fromisoformat`, otherwise `fallback_last_active.isoformat()`
(`nanobot/session/summary.py:37-58`).

`OBSERVED` — `Session.get_history` pipeline order (`manager.py:344-475`), which a Go port must reproduce:

1. `replayable = messages[last_archived:]`
2. `start_idx = recent_message_start_index(replayable, max_messages, extend_to_user=…)` when
   `max_messages > 0`, else `0`; `sliced = replayable[start_idx:]`
3. advance to the first `user` row, keeping one preceding `_channel_delivery` row (`:371-377`)
4. `find_legal_message_start(sliced)` and drop the prefix (`:379-382`)
5. per-message projection with `_command` skip, runtime-context handling, assistant sanitization, media
   breadcrumbs, CLI-app breadcrumbs, empty-assistant drop (`:384-442`)
6. token budget trim from the tail, then realign to the first `user` row, then `find_legal_message_start`
   again (`:444-474`)

---

## 10. TOKEN ESTIMATION

### 10.1 `estimate_message_tokens`

```python
def estimate_message_tokens(message: dict[str, Any]) -> int:
    """Estimate prompt tokens contributed by one persisted message."""
    content = message.get("content")
    parts: list[str] = []
    if isinstance(content, str):
        parts.append(content)
    elif isinstance(content, list):
        for raw_part in cast(list[object], content):
            part = cast(dict[str, Any], raw_part) if isinstance(raw_part, dict) else None
            if part is not None and part.get("type") == "text":
                text = part.get("text", "")
                if isinstance(text, str) and text:
                    parts.append(text)
            else:
                parts.append(json.dumps(raw_part, ensure_ascii=False))
    elif content is not None:
        parts.append(json.dumps(content, ensure_ascii=False))

    for key in ("name", "tool_call_id"):
        value = message.get(key)
        if isinstance(value, str) and value:
            parts.append(value)
    if message.get("tool_calls"):
        parts.append(json.dumps(message["tool_calls"], ensure_ascii=False))

    rc = message.get("reasoning_content")
    if isinstance(rc, str) and rc:
        parts.append(rc)

    payload = "\n".join(parts)
    if not payload:
        return 4
    try:
        enc = _get_token_encoding()
        return max(4, len(enc.encode(payload)) + 4)
    except Exception:
        return max(4, len(payload.encode("utf-8")) + 4)
```
(`nanobot/utils/helpers.py:783-819`)

Precise semantics:

1. Content handling:
   * `str` → the string is appended verbatim. An empty string adds an empty element, which contributes
     only a `\n` separator when other parts exist; if it is the only part, `payload` is `""` and the
     function returns `4`.
   * `list` → for each element: if it is a dict with `type == "text"` **and** a non-empty string `text`,
     the text is appended; if it is a dict with `type == "text"` but the text is missing/empty/non-string,
     **nothing is appended** (the inner guard has no `else`); any other element — non-dict, or a dict
     whose `type` is not `"text"` — is appended as `json.dumps(raw_part, ensure_ascii=False)`.
   * `None` → nothing.
   * any other type → `json.dumps(content, ensure_ascii=False)`.
2. `name` and `tool_call_id` are appended when they are **non-empty strings**.
3. `tool_calls` is appended as `json.dumps(..., ensure_ascii=False)` when **truthy** (empty list → skipped).
4. `reasoning_content` appended when a non-empty string. `thinking_blocks` is **not** counted.
5. `payload = "\n".join(parts)`.
6. Empty payload → **exactly 4**.
7. Otherwise `max(4, len(enc.encode(payload)) + 4)` where `enc = tiktoken.get_encoding("cl100k_base")`
   (`nanobot/utils/helpers.py:105-107`), with a UTF-8 **byte-length** fallback of
   `max(4, len(payload.encode("utf-8")) + 4)` if tiktoken raises.

`OBSERVED` — `tiktoken` is a hard dependency (`nanobot/utils/helpers.py:19`); the fallback is for
runtime failures, not for absence.

`INFERENCE` — **exact token parity with a Go port is only achievable by embedding the same
`cl100k_base` BPE vocabulary.** The +4 per message and the `max(4, …)` floor are a rough chat-format
overhead allowance, not the real provider overhead.

`OBSERVED` — the sibling `_estimate_prompt_tokens_with_source` uses the same encoding and the same
`per_message_overhead` concept (`nanobot/utils/helpers.py:719-771`). **UNVERIFIED** — I did not
determine the exact value of `per_message_overhead` used there; it is outside the listed scope.

### 10.2 Where it is used for persistence

`OBSERVED` — `estimate_message_tokens` is used by `Session.get_history` for the `max_tokens` trim
(`manager.py:444-453`). It does **not** influence the on-disk format.

---

## 11. LEGACY MIGRATION

### 11.1 In-workspace `*.jsonl` → out-of-workspace namespace

`_migrate_from_workspace(workspace)` (`manager.py:908-962`), run once at store construction under both
locks (`:584-585`):

1. `old_dir = workspace / "sessions"` (`:910`). If it is a symlink → log a warning and **return**
   (`:911-914`); if it is not a directory → return.
2. For each `src in old_dir.glob("*.jsonl")` (`:915`):
   * symlink or non-file → log `"Skipping unsafe legacy session file"` and continue (`:916-918`);
   * `source_snapshot = _session_file_snapshot(src)`; `None` → log `"Skipping invalid or changing legacy
     session file"` and continue (`:920-923`).
3. `dst = self.sessions_dir / src.name` — **same filename**, i.e. the canonical base64url stem
   (`:919`).
4. Conflict resolution (`:924-943`):
   * `dst` does not exist → install the snapshot.
   * `dst` exists but is unreadable → keep the legacy file and skip.
   * digests equal → no-op.
   * `source.updated_at > destination.updated_at` → archive the **destination** into
     `.migration-conflicts/<stem>.destination.<digest12>.<hex8>.jsonl`, then install the source.
   * otherwise → archive the **source** into
     `.migration-conflicts/<stem>.workspace.<digest12>.<hex8>.jsonl`.
5. Verification: re-snapshot `dst`; `None` → raise; the installed digest must equal the selected digest
   (`:945-955`).
6. `_remove_migrated_source(src, source_snapshot)` — re-`stat`s and unlinks **only if** dev/ino/size/
   mtime_ns are unchanged, then directory-fsyncs (`:887-906`). Failure to remove is logged, not raised
   (`:956-960`).
7. Any `OSError` in the loop is logged and the iteration abandoned — **the source is kept**
   (`:961-962`).

Snapshot identity (`_SessionFileSnapshot`, `manager.py:509-517`, built by `_session_file_snapshot`
`:763-809`): SHA-256 of all raw lines, size, `mtime_ns`, `device`, `inode`, plus a decoded
`updated_at` float taken from the **first metadata record found** (`:785-788`) — falling back to
`mtime_ns / 1e9` when absent (`:802`). A file with **no** parseable records (`saw_record` false) or that
changes during the read returns `None` (`:790-797`).

`OBSERVED` — `updated_at` is derived as `datetime.fromisoformat(raw).timestamp()` (`:788`). `INFERENCE`:
for naive timestamps this interprets them in the **local timezone**, so conflict ordering is
timezone-of-host dependent.

`OBSERVED` — tests: `test_legacy_in_workspace_sessions_are_migrated`
(`tests/session/test_session_location.py:223-240`), `test_migration_keeps_source_when_install_fails`
(`:243-256`), `test_migration_preserves_newest_valid_conflict` (`:259-283`),
`test_legacy_migration_rejects_symlinked_session_file` (`:311-327`),
`test_legacy_migration_rejects_symlinked_sessions_directory` (`:330-345`).

### 11.2 `restore_to_workspace()` — explicit rollback

`manager.py:964-999`:

* Refuses a symlinked `<workspace>/sessions` (`:970-971`), then `ensure_dir`s it (`:972`).
* Under both locks, for each `*.jsonl` in `sessions_dir` whose `session_key_from_path` is non-`None`
  (`:975-977`):
  * unreadable source → append to `conflicts`, continue (`:978-981`);
  * destination missing → install the snapshot, `restored += 1` (`:993-994`);
  * destination exists with the same digest → `unchanged += 1` (`:985-989`);
  * destination exists with a different digest → append the **destination** to `conflicts` and skip
    (`:990-991`).
* Returns `SessionRestoreResult(restored, unchanged, conflicts)` (`:995-999`, dataclass at `:519-523`).
* **Never deletes** the canonical files (`test_explicit_rollback_restore_copies_sessions_back_without_deleting_new_store`,
  `tests/session/test_session_location.py:286-308`).

### 11.3 The legacy lossy path

`get_legacy_lossy_path(key)` (`manager.py:1032-1033`) and `get_legacy_session_path(key)` (`:1035-1036`)
exist only so `delete` can remove files left by pre-base64 releases (§1.4, §5.4). They are **never read**
and **never written**. `list_sessions` explicitly ignores non-canonical stems
(`tests/session/test_session_list_repair_legacy.py:10-33`).

### 11.4 Legacy `write_stdin` → `exec_session` record migration

A **read-time, in-memory** migration applied by `_load_unlocked` (`:1102`), `_repair_unlocked` (`:1194`),
and `_read_unlocked` (`:1473`):

* `_migrate_legacy_exec_message` (`manager.py:158-167`): if `message["name"] == "write_stdin"` → set to
  `"exec_session"`; recurse into `message["tool_calls"]`.
* `_migrate_legacy_exec_tool_call` (`:138-155`): for tool calls whose function name is `write_stdin` or
  `exec_session`, rename `write_stdin` → `exec_session` and rewrite arguments.
* `_migrate_legacy_exec_arguments` (`:96-135`): accepts `arguments` as a JSON **string** or an object;
  renames `chars` → `input`; maps `wait_timeout_ms` (when `wait_for`/`until_exit` is set) or
  `yield_time_ms` → `timeout_ms`; drops `yield_time_ms`, `wait_timeout_ms`, `max_output_chars`,
  `max_output_tokens`. When the original was a string, it re-serializes with
  `json.dumps(arguments, ensure_ascii=False, separators=(",", ":"))` (`:130-134`) — **compact separators**,
  unlike the session writer.
* `_migrate_legacy_exec_session_records` (`:170-197`) additionally rewrites the runtime checkpoint's
  `assistant_message`, `pending_tool_calls`, and `completed_tool_results[].name`.
* **If anything changed, `session.provider_state` is set to `None`** (`:1102-1103`, `:1194-1195`) because
  the private provider continuation no longer matches the rewritten history.

`OBSERVED` — a TODO marks this for removal after 0.3.1 (`manager.py:95`).

`OBSERVED` — tests: `test_load_migrates_legacy_write_stdin_history`
(`tests/session/test_session_store.py:162-217`) and
`test_load_migrates_legacy_write_stdin_runtime_checkpoint` (`:220-261`).

`OBSERVED` — the migration is **not persisted** on the `load`/`read` path; it happens again on every read
until the session is next saved.

### 11.5 Offset field-name migration

`last_consolidated` → `last_archived` (§3.3): read prefers `last_archived`, falls back to
`last_consolidated` (`manager.py:86-92`); write emits **both** (`:1326`, `:1329`).

### 11.6 Goal-state key migration

`thread_goal` → `goal_state` (`nanobot/session/goal_state.py:13`, `:28-33`); the legacy key is dropped on
migration via `discard_legacy_goal_state_key` (`:36-38`). `thread_goal` is also in
`_FORK_VOLATILE_METADATA_KEYS` (`manager.py:68`).

---

## 12. GOTCHAS — what breaks a naive Go port

### 12.1 Unicode and escaping

1. **`ensure_ascii=False`.** Every session record is written with `ensure_ascii=False`
   (`manager.py:1331`, `:1337`, `:1339`). Non-ASCII text is stored as raw UTF-8. Go's `encoding/json`
   escapes `<`, `>`, and `&` to `\u003c`, `\u003e`, `\u0026` **by default**. `OBSERVED` (Go 1.23.5):

   ```
   json.Marshal        -> {"_type":"provider_state","state":{"a":"a\u003cb\u003e\u0026c\u2028d\u2029e"}}
   SetEscapeHTML(false)-> {"_type":"provider_state","state":{"a":"a<b>&c\u2028d\u2029e"}}
   ```

   Python's `json.loads` accepts both forms, so the file stays readable — but the bytes differ, which
   breaks the SHA-256 digest comparisons used by migration (`manager.py:774-805`, `:841-849`) and any
   byte-level test fixture. Use `json.Encoder` with `SetEscapeHTML(false)`.
2. **Go still escapes U+2028/U+2029** as `\u2028`/`\u2029` even with `SetEscapeHTML(false)`
   (`OBSERVED`, Go 1.23.5, output above). Python does not escape them. Readable either way;
   byte-different. `INFERENCE`: this makes exact byte-parity with Python unattainable for text
   containing those two code points; treat byte-identity as best-effort and rely on semantic
   round-tripping.
3. **Lone surrogates.** Python `str` can hold unpaired UTF-16 surrogates. `json.dumps(..., ensure_ascii=False)`
   does **not** raise on them (`OBSERVED`, CPython 3.14); the failure happens when the resulting string is
   encoded to UTF-8 for the file write, raising `UnicodeEncodeError`. `UnicodeEncodeError` **is** a
   `ValueError` subclass (`OBSERVED`), so it is inside `_SESSION_DATA_ERRORS` (`manager.py:51`) — but
   `_save_unlocked` has no `except` clause, only a `finally` (`manager.py:1318-1361`), so the error
   propagates out of `save` and the temp file is cleaned up. nanobot defends at the input edges with
   `sanitize_surrogates` / `sanitize_surrogates_deep`
   (`nanobot/utils/helpers.py:38-102`, used at `nanobot/providers/base.py:872`, `nanobot/cli/agent.py:375`,
   `nanobot/cli/terminal.py:95`) but **not inside the store**. Go strings are byte sequences and cannot
   represent lone surrogates, so this asymmetry cannot be reproduced exactly.
   `OBSERVED` (Go 1.23.5): `json.Marshal` of invalid UTF-8 bytes emits `\ufffd` per invalid byte
   (`string([]byte{0xff,0xfe})` → `"\ufffd\ufffd"`). `INFERENCE`: Go should follow that behaviour and
   never emit raw surrogate escapes.
4. **`ensure_ascii=True` is a trap for keys.** A Go port that writes `\u00e9` instead of `é` produces a
   file Python reads identically — but a Go port that writes the *filename* from an ASCII-escaped key
   would compute the wrong base64. Encode the **UTF-8 bytes of the key**, not a JSON-escaped form.

### 12.2 Key ordering and object identity

5. `json.dumps` with `sort_keys=False` (the default) preserves **insertion order**
   (`manager.py:1331`, `:1337`, `:1339`). Python `json.loads` returns a plain `dict`, so key order never
   affects semantics — but it does affect:
   * the `_PROVIDER_STATE_RECORD_PREFIX_RE` fast path (`manager.py:54-56`), which requires `_type` to be
     the first key of the record;
   * the `.migration-conflicts` digests (`manager.py:882`);
   * any test fixture that compares bytes.
   `INFERENCE`: emit `_type` first for provider_state records to keep the fast path working.
6. Go's `encoding/json` marshals `map[string]any` in **sorted key order**. If Go stores a message as a
   `map`, the field order of the whole record changes. `INFERENCE`: use a struct with explicit field
   order, or accept byte-level divergence.

### 12.3 `null` vs missing

7. **A missing key and a `null` value are different.** `data.get("_type")` returns `None` for both
   `"absent"` and `"present but null"`, so record dispatch is unaffected. But:
   * `_archive_offset` requires a non-bool `int`; `null` → `0` (`manager.py:86-92`).
   * `Session.__post_init__` resets `last_consolidated` to `0` when it is not an int in range
     (`manager.py:295-301`).
   * `_read_unlocked` returns `created_at: None` for a non-string value, including explicit `null`
     (`manager.py:1460-1465`).
   * `provider_state` record's `state: null` → `from_private_record(None)` → `None` → `provider_state`
     becomes `None` on the load path (`manager.py:1085-1088`).
8. **Do not emit `"content": null`.** `Session.add_message` always sets a string
   (`manager.py:312-320`), and `build_assistant_message` uses `content or ""`
   (`nanobot/utils/helpers.py:707`). `estimate_message_tokens` treats `None` content as zero parts
   (`helpers.py:798-799`), and `get_history` handles `None` via `message.get("content", "")`
   (`manager.py:394`). `INFERENCE`: `null` content is tolerated on read but is not produced by Python.
9. **Do not omit `timestamp`.** `get_history` does not require it, but `list_sessions` and the WebUI
   readers use it (`nanobot/webui/session_access.py:80`), and the SDK snapshot surface exposes it.

### 12.4 Numbers

10. **Python distinguishes `int` and `float`; Go's `encoding/json` does not.** `OBSERVED` side by side:

    | value | Python `json.dumps` (CPython 3.14) | Go `json.Marshal` (1.23.5) |
    |---|---|---|
    | `1.0` | `1.0` | `1` |
    | `10` | `10` | `10` |
    | `1e20` | `1e+20` | `100000000000000000000` |
    | `0.1` | `0.1` | `0.1` |

    Consequences:

    * Values round-trip **numerically** through both parsers, but bytes differ.
    * **A float can silently become an int.** `json.loads("1")` in Python yields `int(1)`, whereas
      `json.loads("1.0")` yields `float(1.0)`. A Go writer that re-serializes a Python-written `1.0`
      as `1` changes the Python-side type of that value in `metadata` and in message fields.
    * **`_archive_offset` requires a real integer.** A Go writer emitting `0.0` for `last_archived`
      would be rejected by `isinstance(offset, int)` and silently read as `0` (`manager.py:90`), causing
      the entire transcript to be replayed instead of the post-summary suffix. Emit JSON integers for
      `last_archived`, `last_consolidated`, `base_message_count`, `version`, `latency_ms`, and
      `created_at_ms`.
11. `isinstance(x, bool)` guards exist at `manager.py:90` and `nanobot/providers/base.py:231`. In Go,
    `true` marshals as `true`, never as `1`, so this is safe — but a Go **reader** must reject JSON
    `true`/`false` where an int is required.

### 12.5 Timestamps and timezones

12. **`datetime.now()` is naive local time.** `created_at`/`updated_at`/`timestamp` are written as
    naive ISO-8601 with **no offset** (`manager.py:281-282`, `:317`, `:1323-1324`). Go's `time.Now()`
    carries a location; `time.Format(time.RFC3339)` on a local time emits an offset
    (`2026-01-02T03:04:05-03:00`) and on UTC emits `Z`. Both are **accepted** by
    `datetime.fromisoformat` on Python ≥ 3.11, but they are **not** byte-identical to Python's output,
    and they change the checkpoint fingerprint (item 14).
13. **Fractional-second precision is normalised, not preserved.** `datetime.fromisoformat(x).isoformat()`
    drops `.000000` entirely and pads to exactly 6 digits otherwise
    (`OBSERVED`, verified in CPython 3.14):
    `'…05.1'` → `'…05.100000'`; `'…05.000000'` → `'…05'`; `'…05.123456789'` → `'…05.123456'`.
    Also `'2026-01-02 03:04:05'` → `'2026-01-02T03:04:05'` (space → `T`).
14. **The checkpoint fingerprint is exact string equality.**
    `_overlay_runtime_checkpoint_unlocked` requires
    `raw["base_updated_at"] == session.updated_at.isoformat()` (`manager.py:1282`), where
    `session.updated_at = datetime.fromisoformat(metadata_record["updated_at"])`. Therefore a Go writer
    that stores `"updated_at": "2026-01-02T03:04:05Z"` and a checkpoint with
    `"base_updated_at": "2026-01-02T03:04:05Z"` will have its checkpoint **silently discarded** by
    Python, because Python normalises `Z` to `+00:00` before comparing. Mitigation: write naive local
    timestamps with 6-digit-or-absent fractional seconds, exactly as `datetime.now().isoformat()` does.
15. `base_message_count` must equal `len(session.messages)` after load — i.e. the count of **non-metadata,
    non-provider_state** lines (`manager.py:1283`). A Go writer must count records the same way.
16. **Sidecar staleness is decided by `st_mtime_ns`**, not by content (`manager.py:1275`). A Go writer
    that writes the checkpoint and the JSONL in the same filesystem-timestamp tick, in the wrong order,
    can have a valid checkpoint discarded (or a stale one honoured) on coarse-granularity filesystems.
    `INFERENCE`: always write the JSONL **before** the checkpoint, and delete the checkpoint after a full
    save, matching `manager.py:1348`.
17. `list_sessions` sorts by **string** `updated_at` (`manager.py:1641`). Naive and offset-aware
    timestamps sort inconsistently. `INFERENCE`: write one consistent form.
18. `datetime.fromtimestamp(path.stat().st_mtime).isoformat()` (`manager.py:1597`) and
    `datetime.fromisoformat(raw).timestamp()` (`manager.py:788`) both use the host timezone. Two hosts in
    different zones can disagree about migration conflict ordering.

### 12.6 Structural gotchas

19. **The first line must be the metadata record.**
    * `_read_metadata_unlocked` returns `None` if the first non-blank line is not metadata
      (`manager.py:1505-1506`).
    * `update_metadata` returns `False` in the same situation (`manager.py:1380-1381`).
    * `_list_sessions_unlocked` skips the file entirely (`manager.py:1556`).
    A **leading blank line** also makes the session invisible to `list_sessions` (`manager.py:1552-1553`).
    `load()` still works — an asymmetry to preserve or at least be aware of.
20. **`_load_unlocked` is strict, `_repair_unlocked` is tolerant.** Any single malformed line makes the
    strict read fail and fall through to repair. If the file has **no** messages, **no** metadata, and
    **no** provider state, repair returns `None` (`manager.py:1181-1182`) → `load` returns `None` → the
    session is treated as absent. `INFERENCE`: a Go writer that emits only a metadata record with
    `"metadata": {}` and no messages produces a file Python will read successfully on the strict path
    (metadata dict is `{}`, which is falsy — but the strict path does not apply the emptiness check), yet
    would return `None` if the strict path ever fails. Emit a consistent shape.
21. **Unknown `_type` values become messages** (`manager.py:1089-1090`). Do not invent record types
    without expecting them to surface as chat turns.
22. **`repair` never rewrites the file** (`manager.py:1120-1199`). A Go port that "repairs and saves"
    would diverge — it would destroy lines Python deliberately preserved.
23. **The metadata record is updated on every full save**, including `updated_at`
    (`manager.py:1324`). There is no partial metadata write except `update_metadata`
    (`manager.py:1363-1404`), which rewrites only the first line and byte-copies the remainder.
24. **File permissions differ per file.** Session `.jsonl`: umask default. Checkpoint `.json`: `0o600`.
    `workspace-id` and `.workspace`: `0o600`. Sessions root directory: `0o700` (best-effort,
    `OSError` suppressed). Go should match `0o600`/`0o700` where Python does, but must **not** assume
    session files are `0o600` when reading.
25. **`_json_object` rejects JSON arrays/scalars.** A line containing `42`, `"x"`, `[1,2]`, or `null`
    raises `ValueError("session records must be JSON objects")` (`manager.py:79-83`) on the strict path
    and is counted as `skipped` on the repair path (`manager.py:1145-1147`).
26. **The `.checkpoint.json` suffix does not match `*.jsonl`**, so sidecars never appear in
    `list_sessions`. Temp files (`.<name>.<hex>.tmp`) likewise. A Go port must not rename its temp files
    to something ending in `.jsonl`.
27. **`glob("*.jsonl")` is non-recursive but matches directories too.** A directory named `x.jsonl`
    inside `sessions_dir` would make `list_sessions` raise `IsADirectoryError` (an `OSError`, not in
    `_SESSION_DATA_ERRORS`, so it propagates). `INFERENCE`: a latent robustness bug in the reference,
    not something to replicate deliberately.
28. **`sessions_root` must not be inside the workspace** — `RuntimeError` otherwise
    (`manager.py:559-563`). A Go port should fail the same way rather than silently writing into the
    workspace.
29. **Cross-process locking.** Python uses `filelock` on `<sessions_dir>/.session-files.lock`
    (`manager.py:581-583`). `INFERENCE`: to be safe alongside a live Python process, a Go writer must take
    the same lock (an OS advisory lock on that path). Otherwise the whole-file `os.replace` semantics
    cause silent lost updates.
30. **Session keys are not safely splittable on `:`.** `discord:guild:123` is a legitimate key
    (`nanobot/webui/session_identity.py:10` allows `:` in chat ids). Split at most once where the
    codebase does (`nanobot/session/keys.py:39`).

---

## 13. Highest-risk compatibility gaps

Ranked by likelihood × blast radius for a Go reimplementation.

| # | Risk | Why it bites | Evidence |
|---|---|---|---|
| 1 | **Checkpoint fingerprint string mismatch** | `base_updated_at` is compared to `datetime.fromisoformat(updated_at).isoformat()` by exact equality. Go's `RFC3339` emits `Z`/offsets and different fractional precision; Python normalises both. A mismatch silently **deletes** the sidecar, losing in-flight turn state. | `manager.py:1282`; `manager.py:1055-1083`; verified normalisation in CPython |
| 2 | **Whole-file rewrite + lost updates** | Every save replaces the file. Without taking `.session-files.lock` with the same protocol, a Go writer and a Python writer silently clobber each other. | `manager.py:1314-1344`, `:581-583` |
| 3 | **Filename encoding** | `session_key_from_path` requires `base64url_nopad(utf8(key)) == stem` byte-for-byte. Padding, standard base64, ASCII-escaping the key before encoding, or URL-encoding the key all produce files Python will list as non-sessions. | `manager.py:1005-1024` |
| 4 | **Integer-typed metadata fields** | `_archive_offset` and `Session.__post_init__` reject floats and bools. A Go writer emitting `0.0` for `last_archived` silently resets the replay offset to 0, replaying the whole transcript. | `manager.py:86-92`, `:295-301` |
| 5 | **First-line ordering** | Metadata must be the first record, with no leading blank line, for `read_metadata`, `update_metadata`, and `list_sessions`. | `manager.py:1505-1506`, `:1380-1381`, `:1552-1556` |
| 6 | **Unknown `_type` becomes a chat message** | A Go writer adding a new record type silently injects junk turns into the LLM context and the UI. | `manager.py:1089-1090` |
| 7 | **`_runtime_context` removal is exact-match** | `public_history_message` only strips the suffix/blocks on a byte-exact match. If Go re-serializes content with different whitespace, the hidden runtime context leaks into user-visible history. | `runtime_context.py:215-240` |
| 8 | **JSON byte divergence (`SetEscapeHTML`)** | Affects SHA-256 digests used for migration conflict detection and dedup; can cause a legitimate migration to be treated as a conflict, or a conflict to be missed. | `manager.py:774-805`, `:830-849`, `:874-885` |
| 9 | **Token estimate parity** | `estimate_message_tokens` uses `cl100k_base` with `+4` per message and a `max(4, …)` floor. Without the same vocabulary, `get_history`'s `max_tokens` window differs → different prompts, different behaviour, different cost. | `helpers.py:783-819`, `:105-107` |
| 10 | **`find_legal_message_start` semantics** | Returns the index after the **last** orphan tool row and clears the declared-id set on each orphan. A "first orphan" implementation diverges and can drop or keep different tool pairs, producing provider 400s. | `helpers.py:478-494` |
| 11 | **Checkpoint staleness by mtime** | Ordering of JSONL write vs checkpoint write vs checkpoint unlink decides whether a valid checkpoint is honoured. Coarse mtime granularity makes this racy. | `manager.py:1275-1277`, `:1348` |
| 12 | **Hidden-history semantics are presentation-only** | Hidden messages still reach the LLM (`get_history` only drops `_command`). A Go port that filters hidden messages out of replay changes model behaviour. | `manager.py:386-393` |
| 13 | **Naive local timestamps** | Python writes naive local time with no offset. A Go port writing UTC (`Z`) is readable but sorts and normalises differently, breaking `list_sessions` ordering and item 1. | `manager.py:281-282`, `:1641` |
| 14 | **`last_archived`/`last_consolidated` dual write** | Reading prefers `last_archived`; older releases read only `last_consolidated`. Writing just one breaks one of the two directions. | `manager.py:86-92`, `:1326-1329` |
| 15 | **`repair` must not write** | A "helpful" Go repair that persists the salvaged content destroys the corrupt-but-preserved original that Python intentionally leaves on disk. | `manager.py:1120-1199` |
| 16 | **`.migration-conflicts/` and digest-based conflict resolution** | Requires the exact snapshot fields (sha256 of raw lines, dev, ino, size, mtime_ns, decoded `updated_at`). Partial implementations can lose the newer transcript. | `manager.py:509-517`, `:763-809`, `:924-962` |

---

## 14. UNVERIFIED items

Explicitly not determined from the sources I read. Do not treat these as settled.

1. **`per_message_overhead`** in `_estimate_prompt_tokens_with_source` — I read the function's tail
   (`nanobot/utils/helpers.py:719-771`) but not its full body, so the exact constant is unknown.
   It does not affect `estimate_message_tokens`, which hardcodes `+4`.
2. **Concrete automation-spec values** — `CRON_AUTOMATION_SPEC` and `LOCAL_TRIGGER_AUTOMATION_SPEC`
   (`nanobot/cron/session_turns.py`, `nanobot/triggers/local_session_turns.py`) were not read. The
   `kind` strings and `legacy_history_meta_key` values used by `is_automation_history_message`
   (`nanobot/session/automation_turns.py:75-86`) are therefore unknown.
3. **Session-key length enforcement** — the 512-char bound appears in
   `nanobot/session/session_handles.py:16` and `nanobot/session/session_messages.py:13`, but I did not
   find (or rule out) a store-level or gateway-level bound on arbitrary session keys.
4. **Behaviour of `Session.clear()` with respect to `metadata["runtime_checkpoint"]`** — `clear()` pops
   `_last_summary` but leaves `runtime_checkpoint` in metadata (`manager.py:477-483`). Whether any caller
   then saves and thus re-creates a sidecar was not traced.
5. **Windows path/behaviour specifics** — `os.replace` over an existing file, `os.open(dir, O_RDONLY)`
   failing with `PermissionError`, and `FileLock` semantics on Windows were inferred from the code's
   defensive `suppress(PermissionError)` and the `_IS_WINDOWS` branch in the tests
   (`tests/session/test_session_fsync.py:11`, `:52-56`), not verified on Windows.
6. **`filelock` default timeout** — I read that `_session_files_lock` is constructed without a `timeout`
   argument (`manager.py:581-583`) and that the migration lock uses `timeout=30` (`:568-571`). The
   concrete default (`-1` = block indefinitely) is from my knowledge of `filelock`, not from this
   repository. **Verify against `filelock>=3.25.2` before relying on it.**
7. **No live round-trip test was performed.** The Python project's dependencies (`filelock`, `loguru`,
   `tiktoken`) are not installed in this analysis environment, so I could not construct a real session
   file with nanobot and re-read it. All file-format claims are source-derived. The executed checks were
   the stdlib-only reproductions of `storage_key`, `decode_storage_key`, `safe_filename`, `json.dumps`
   separators and float formatting, `datetime.fromisoformat(...).isoformat()` normalisation, and the
   Python exception hierarchy check (`UnicodeEncodeError` ⊂ `ValueError`) — all in CPython 3.14.7 — plus
   the Go 1.23.5 `encoding/json` checks. Each is marked `OBSERVED` inline.
8. **Exact `_meta` payload shape for persisted content blocks** — `nanobot/agent/loop.py:2090-2115`
   reads `block["_meta"]["path"]` when redacting an inline image, and
   `nanobot/utils/helpers.py:340-352` shows `build_image_content_blocks` producing
   `{"type":"image_url","image_url":{"url":…},"_meta":{"path":…}}`. I did not enumerate every producer of
   `_meta`, so the full set of `_meta` keys that can appear in a persisted record is not established.

---

## 15. Quick reference — constants

| Constant | Value | Citation |
|---|---|---|
| `_PROVIDER_STATE_RECORD_TYPE` | `"provider_state"` | `manager.py:53` |
| `_PROVIDER_STATE_RECORD_PREFIX_RE` | `r'^\s*\{\s*"_type"\s*:\s*"provider_state"\s*(?:,|\})'` | `manager.py:54-56` |
| `_RUNTIME_CHECKPOINT_KEY` | `"runtime_checkpoint"` | `manager.py:57` |
| `_RUNTIME_CHECKPOINT_VERSION` | `1` | `manager.py:58` |
| `_RUNTIME_CHECKPOINT_SUFFIX` | `".checkpoint.json"` | `manager.py:59` |
| `_SESSION_FILES_LOCK_FILENAME` | `".session-files.lock"` | `manager.py:75` |
| `_SESSION_MIGRATION_LOCK_TIMEOUT_SECONDS` | `30` | `manager.py:74` |
| `_WORKSPACE_STATE_DIR` | `".nanobot"` | `manager.py:71` |
| `_WORKSPACE_ID_FILE` | `"workspace-id"` | `manager.py:72` |
| `_WORKSPACE_ID_RE` | `^[0-9a-f]{32}$` | `manager.py:73` |
| `_COPY_CHUNK_SIZE` | `1048576` | `manager.py:76` |
| `_SESSION_PREVIEW_MAX_CHARS` | `120` | `manager.py:48` |
| `_SESSION_LIST_PREVIEW_MAX_RECORDS` | `200` | `manager.py:49` |
| `_SESSION_LIST_PREVIEW_MAX_CHARS` | `1_000_000` | `manager.py:50` |
| `SESSION_CACHE_MAX_SIZE` | `128` | `manager.py:44` |
| `HIDDEN_HISTORY_META` | `"_hidden_history"` | `history_visibility.py:10` |
| `AUTOMATION_HISTORY_META` | `"_automation_turn"` | `automation_turns.py:10` |
| `RUNTIME_CONTEXT_HISTORY_META` | `"_runtime_context"` | `runtime_context.py:14` |
| `SUMMARY_CONTINUATION_TEXT` | `"Continue the active task from the working-memory checkpoint above."` | `summary.py:12-14` |
| `UNIFIED_SESSION_KEY` | `"unified:default"` | `keys.py:8` |
| `LAST_CHANNEL_METADATA_KEY` | `"last_channel"` | `keys.py:9` |
| `SESSION_MODEL_PRESET_METADATA_KEY` | `"_nanobot_model_preset"` | `model_selection.py:9` |
| `SESSION_MESSAGE_METADATA_KEY` | `"_session_message"` | `session_messages.py:11` |
| `SESSION_HANDLE_METADATA_KEY` | `"session_handle"` | `session_handles.py:14` |
| `GOAL_STATE_KEY` | `"goal_state"` (legacy `"thread_goal"`) | `goal_state.py:12-13` |
| `_get_token_encoding()` | `tiktoken.get_encoding("cl100k_base")` | `helpers.py:105-107` |
| `_UNSAFE_CHARS` | `r'[<>:"/\\|?*]'` | `helpers.py:366` |
| Session file mode | not set (umask) | `manager.py:1319` |
| Checkpoint file mode | `0o600` | `manager.py:1251` |
| `workspace-id` / `.workspace` mode | `0o600` | `manager.py:606-617`, `:678`, `:710` |
| Sessions root dir mode | `0o700`, best-effort | `manager.py:565-566` |
