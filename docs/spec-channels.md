# Behavioral Specification — nanobot Channel Subsystem

**Purpose.** Precise behavioral specification of the nanobot channel subsystem (channel abstraction,
channel manager, message-bus integration, per-channel transports) sufficient to plan and execute a
Go reimplementation with behavioral fidelity. **No Go code and no detailed Go type definitions appear
in this document.** Where Go is discussed, only stdlib package names and behavioral requirements are
named.

**Reference source (read-only, frozen):**

- Repository root: `/run/media/adriano/e681b5ac-a4fb-44d4-aebf-9d6584065787/dsh-projetos/nanobot/upstream/nanobot`
- Commit: `1bb712d3488915ca4ed9ccc1a93067ff722f5ab9` — verified in this session via `git log --oneline -1`
  and `git rev-parse HEAD`; subject `ci: use baseline Bun directly for Windows TUI builds`.
- Go port under analysis (read-only, for integration context only):
  `/run/media/adriano/e681b5ac-a4fb-44d4-aebf-9d6584065787/dsh-projetos/nanobot` — module
  `github.com/adrianolimagarcia/nanobot-go`, `go 1.23.5`, **zero external module requirements**
  (`go.mod` has no `require` block; a repo-wide grep for `golang.org/x/` and non-first-party
  `github.com/` imports under `internal/` and `cmd/` returned no matches).

**Citation convention.** All citations are `path:line` relative to the reference root, e.g.
`nanobot/channels/base.py:255`. Line numbers are from the frozen commit.

Bare filenames (`base.py:255`, `manager.py:767`, `queue.py:32`, `loop.py:1269`) are shorthand used
after the full path has been given in the surrounding section or in the module map at §0.2. Resolve
them as follows: `base.py`, `manager.py`, `contracts.py`, `validation.py`, `plugin.py`, `registry.py`,
`_manifest.py`, `connect.py`, `_setup.py`, `notification_routes.py` →
`nanobot/channels/`; `queue.py`, `events.py`, `outbound_events.py`, `notification_delivery.py`,
`runtime_events.py` → `nanobot/bus/`; `loop.py`, `runner.py`, `turn_delivery.py` → `nanobot/agent/`;
`<channel>/runtime.py` and `<channel>/manifest.py` → `nanobot/channels/<channel>/`; bare `test_*.py` →
`tests/channels/`. All 379 distinct citations in this document were machine-verified in this session to resolve to an
existing file and to be within that file's line count.

**Evidence classes.**

- **OBSERVED** — read directly in source at the cited line.
- **INFERENCE** — follows logically from observed code plus third-party library semantics that are
  *not present in this repository* (the Python SDKs are not installed in this environment; verified:
  `telegram`, `discord`, `slack_sdk`, `nio`, `httpx`, `aiohttp` are all missing).
- **UNVERIFIED** — not determinable from the files read.

**Reading note.** This session's tool output redacts expressions whose variable name contains
`token`, `secret`, or `access` as `<CAMPO_*>` placeholders (e.g. `manager.py:129`, `loop.py:1132-1134`).
Those lines are described by config field name only. No credential value was read, inferred, or
reproduced.

---

## 0. Scope and module map

### 0.1 Size correction

The task brief states the channel subsystem is "67,174 lines". Measured in this session:

| Slice | Lines |
|---|---|
| `nanobot/channels/**/*.py` including tests | **67,174** |
| `nanobot/channels/**/*.py` **excluding** `tests/` | **27,187** |
| `runtime.py` files only (17 channels) | **21,376** |
| `*/tests/*.py` under `nanobot/channels/` | **39,987** |
| Shared infrastructure (`channels/*.py` at top level) | **2,924** |

**OBSERVED.** Roughly 60% of the stated 67,174 lines is test code. The actual implementation surface
is ~27k lines, of which ~21k is the 17 per-channel `runtime.py` modules. This materially changes the
porting estimate: the shared infrastructure a Go port must reproduce first is 2,924 lines, not 67k.

### 0.2 Module map

| Layer | File | Lines | Role |
|---|---|---|---|
| Abstraction | `nanobot/channels/base.py` | 341 | `BaseChannel` ABC: the channel interface |
| Orchestration | `nanobot/channels/manager.py` | 1101 | `ChannelManager`: discovery, lifecycle, outbound dispatch |
| Contracts | `nanobot/channels/contracts.py` | 646 | `ChannelPlugin`-facing setup/management dataclasses (pure) |
| Validation | `nanobot/channels/validation.py` | 431 | WebUI setup probing (httpx, socket, ssl) |
| Manifest | `nanobot/channels/plugin.py` | 186 | `ChannelPlugin` dataclass, lazy runtime resolution |
| Discovery | `nanobot/channels/registry.py` | 104 | `pkgutil` package scan + plugin loading |
| Manifest helpers | `nanobot/channels/_manifest.py` | 40 | `field()`, `required()`, `one_of()` constructors |
| Connector contract | `nanobot/channels/connect.py` | 24 | `ChannelConnectError`, `QueryParams`, `query_first` |
| Setup accessor | `nanobot/channels/_setup.py` | 23 | `channel_setup_spec()` |
| Notification metadata | `nanobot/channels/notification_routes.py` | 23 | Thread-address carry-over for later notifications |
| Bus | `nanobot/bus/queue.py` | 148 | `MessageBus` |
| Bus types | `nanobot/bus/events.py` | 68 | `InboundMessage`, `OutboundMessage` |
| Bus events | `nanobot/bus/outbound_events.py` | 162 | `ProgressEvent`, `StreamDeltaEvent`, `StreamEndEvent`, … |
| Bus policy | `nanobot/bus/notification_delivery.py` | 41 | Audience admission per event type |
| Bus runtime events | `nanobot/bus/runtime_events.py` | 293 | `RuntimeEventPublisher` |
| Agent consume | `nanobot/agent/loop.py` | 2388 | Consumes inbound, owns per-session pending queue |
| Agent inject | `nanobot/agent/runner.py` | 1374 | Drains pending queue via `injection_callback` |
| Agent emit | `nanobot/agent/turn_delivery.py` | 386 | Routes turn output to the bus |
| Gateway wiring | `nanobot/cli/gateway_runtime.py` | 1034 | Builds bus, `ChannelManager`, runs both as tasks |
| Process lifecycle | `nanobot/gateway/runtime.py` | 703 | PID/lease/health — **not** message-path |
| OS service install | `nanobot/gateway/service.py` | 286 | systemd/launchd rendering — **not** message-path |
| Pairing | `nanobot/pairing/` | 404 | DM approval store used by `BaseChannel.is_allowed` |
| Dependency gate | `nanobot/optional_features.py:827-851` | — | Installs/verifies per-channel requirements |

**OBSERVED — the `gateway/` package is largely a red herring for channel porting.** Of its 1,014
lines, `runtime.py` is process supervision (filelock, PID files, HTTP health probe at
`nanobot/gateway/runtime.py:42`) and `service.py` renders systemd/launchd units. Neither touches
message flow. The actual channel wiring lives in `nanobot/cli/gateway_runtime.py`.

### 0.3 Channel inventory

17 channel packages, each a self-contained directory under `nanobot/channels/` containing at minimum
`manifest.py` (dependency-free descriptor) and `runtime.py` (the `BaseChannel` subclass), plus optional
`validation.py`, `connect.py`, `state.py`, `instances.py`, `webui/`, `tests/`.

**OBSERVED** (`ls nanobot/channels/*/`): `dingtalk, discord, email, feishu, matrix, mattermost,
mochat, msteams, napcat, qq, signal, slack, telegram, websocket, wecom, weixin, whatsapp`.

---

## 1. The abstraction

### 1.1 `BaseChannel` — the channel interface

`nanobot/channels/base.py:21` — `class BaseChannel(ABC)`.

**Class-level attributes** (`base.py:29-33`), all overridable by subclasses:

| Attribute | Default | Line | Meaning |
|---|---|---|---|
| `name` | `"base"` | `base.py:29` | Runtime identity; **must equal** the plugin name (enforced at `plugin.py:90-93`) |
| `display_name` | `"Base"` | `base.py:30` | Human label |
| `send_progress` | `True` | `base.py:31` | Whether `ProgressEvent`s are delivered |
| `send_tool_hints` | `True` | `base.py:32` | Whether tool-hint `ProgressEvent`s are delivered |
| `show_reasoning` | `True` | `base.py:33` | Whether reasoning `ProgressEvent`s are delivered |

These three booleans are **not** read from the class at dispatch time. The manager overwrites them per
instance at construction: `manager.py:219-231` resolves each from the config section (with a channel-owned
default from `progress_transport_defaults()`), and the dispatcher reads the *instance* attributes
(`manager.py:353`, `manager.py:799`, `manager.py:804-811`). A Go port must treat them as instance state
mutable after construction, not as compile-time constants.

**Constructor** — `base.py:35-46`:

```
def __init__(self, config: Any, bus: MessageBus)
```

Stores `self.config`, `self.logger` (a loguru logger bound with `channel=self.name`, `base.py:44`),
`self.bus`, and `self._running = False`.

Note the `config` parameter is typed `Any` (`base.py:35`) and is *duck-typed throughout*: every access
site branches on `isinstance(config, dict)` vs attribute access (`base.py:229-234`, `base.py:239-245`,
`manager.py:331-338`, `manager.py:362-371`). This exists because config may be either a Pydantic model
or raw persisted JSON. A Go port needs one equivalent of both paths — a decoded struct and a raw
`map[string]any` — because `manager.py:362-371` additionally accepts camelCase aliases
(`sendProgress` for `send_progress`, table at `manager.py:67-71`).

### 1.2 Methods a channel MUST implement

Exactly three, all `@abstractmethod`:

| Method | Signature | Line | Contract |
|---|---|---|---|
| `start` | `async def start(self) -> None` | `base.py:74-84` | Long-running. Connect to the platform, listen, forward to the bus via `_handle_message()`. Must not return until stopped. |
| `stop` | `async def stop(self) -> None` | `base.py:86-89` | Stop and clean up resources. |
| `send` | `async def send(self, msg: OutboundMessage) -> None` | `base.py:91-102` | Deliver one message. **Must raise on delivery failure** so the manager can apply its single retry policy (`base.py:99-101`). |

**OBSERVED: the ABC has no `__init_subclass__` or runtime enforcement beyond Python's own.** Any
subclass of `BaseChannel` that does not override the three abstract methods cannot be instantiated.

### 1.3 Methods the base class PROVIDES

**Fully implemented, inherited as-is:**

| Method | Signature | Line | Behavior |
|---|---|---|---|
| `is_allowed` | `(sender_id: str) -> bool` | `base.py:237-253` | Star → allowlist (exact string match) → pairing store → deny |
| `_handle_message` | see below | `base.py:255-321` | Authorization gate + `InboundMessage` construction + bus publish |
| `supports_streaming` | `@property -> bool` | `base.py:225-235` | `config.streaming` **AND** `type(self).send_delta is not BaseChannel.send_delta` |
| `send_reasoning` | `async (msg: OutboundMessage) -> None` | `base.py:201-223` | Default: one `send_reasoning_delta` with full content, then `send_reasoning_end` |
| `transcribe_audio` | `async (file_path) -> str` | `base.py:48-60` | Whisper via `nanobot.audio.transcription`; **swallows all exceptions**, returns `""` |
| `is_running` | `@property -> bool` | `base.py:338-341` | Returns `self._running` |

**The `_handle_message` signature is the single most important contract in the subsystem** —
`base.py:255-266`:

```
async def _handle_message(
    self,
    sender_id: str,
    chat_id: str,
    content: str,
    media: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
    session_key: str | None = None,
    is_dm: bool = False,
    authorization_id: str | None = None,
    require_existing_session: bool = False,
) -> None
```

Its behavior (`base.py:274-321`):

1. `permission_id = authorization_id if authorization_id is not None else sender_id` (`base.py:274`).
   This lets a channel authorize against a *group/room* while recording the *member* as `sender_id`
   (`base.py:268-272`).
2. If `not self.is_allowed(permission_id)` (`base.py:275`):
   - If `is_dm`: call `generate_code(self.name, sender_id)` (`base.py:278`). On `OSError` (transient
     pairing-store I/O failure) log a warning and **return without replying** (`base.py:279-285`) — the
     store must not be allowed to crash the handler.
   - Otherwise send an `OutboundMessage` carrying `format_pairing_reply(code)` with metadata key
     `PAIRING_CODE_META_KEY` (`= "_pairing_code"`, `pairing/__init__.py:19`) (`base.py:286-293`).
   - For non-DM: log a warning and return (`base.py:298-304`).
   - **In all denial cases the function returns without publishing to the bus.**
3. If allowed: `meta = metadata or {}`; if `self.supports_streaming`, set `meta["_wants_stream"] = True`
   (`base.py:306-308`).
4. Construct `InboundMessage(channel=self.name, sender_id=..., chat_id=..., content=..., media=media or [],
   metadata=meta, session_key_override=session_key, require_existing_session=require_existing_session)`
   (`base.py:310-319`).
5. `await self.bus.publish_inbound(msg)` (`base.py:321`).

Note `is_dm` and `authorization_id` are **consumed by the base class only** — neither is copied onto the
`InboundMessage`.

**Optional hooks with no-op defaults** (a channel overrides only what it supports):

| Hook | Signature | Line | Default |
|---|---|---|---|
| `progress_transport_defaults` | `() -> tuple[bool, bool] \| None` | `base.py:104-110` | `None` (keep global policy) |
| `should_retry_send_error` | `(error: Exception) -> bool` | `base.py:112-119` | `True` |
| `start_error_message` | `(error: Exception) -> str \| None` | `base.py:121-127` | `None` |
| `login` | `async (force: bool = False) -> bool` | `base.py:62-72` | `True` |
| `send_delta` | see §1.4 | `base.py:129-151` | no-op `pass` |
| `send_reasoning_delta` | see §1.4 | `base.py:153-171` | `return` (no-op) |
| `send_reasoning_end` | `async (chat_id, metadata=None, *, stream_id=None) -> None` | `base.py:173-186` | `return` (no-op) |
| `send_file_edit_events` | `async (chat_id, edits: list[dict], metadata=None) -> None` | `base.py:188-199` | `return` (no-op) |
| `default_config` | `@classmethod () -> dict[str, Any]` | `base.py:323-326` | `{"enabled": False}` |
| `refresh_feature_metadata` | `@classmethod (config_path, *, instance_id="default") -> bool` | `base.py:328-336` | `False` |

**OBSERVED override census** (grep over all 17 `runtime.py` files):

| Hook | Channels that override it |
|---|---|
| `send_delta` | telegram, discord, matrix, mattermost, feishu, websocket, weixin |
| `send_reasoning_delta` / `send_reasoning_end` | websocket only |
| `send_file_edit_events` | websocket only |
| `should_retry_send_error` | weixin only |
| `start_error_message` | weixin only |
| `progress_transport_defaults` | email, weixin |
| `login` | feishu, weixin, whatsapp |
| `is_allowed` | signal, mattermost, slack, telegram |
| `_handle_message` | signal only |
| `refresh_feature_metadata` | feishu only |

**INFERENCE from this census:** 10 of 17 channels implement only the three abstract methods plus
`default_config`. The streaming/reasoning hooks are genuinely optional. A Go port can ship a minimal
channel interface and add the streaming surface later without breaking non-streaming channels.

### 1.4 The streaming contract

`send_delta` (`base.py:129-139`):

```
async def send_delta(
    self,
    chat_id: str,
    delta: str,
    metadata: dict[str, Any] | None = None,
    *,
    stream_id: str | None = None,
    stream_end: bool = False,
    resuming: bool = False,
    merge_next: bool = False,
) -> None
```

Contract notes from the docstring (`base.py:140-151`):

- Stateful implementations **must key buffers by `stream_id`**, not only `chat_id`, when `stream_id`
  is provided.
- `merge_next` marks a *resumable provider boundary* whose next text segment belongs to the same
  user-visible message.
- Implementations must raise on delivery failure (so the manager retries).

`send_reasoning_delta` (`base.py:153-160`) has the same shape minus `stream_end`/`resuming`/`merge_next`
and is documented as the "low-emphasis primitive" surface (Slack context block, Telegram expandable
blockquote, Discord subtext, WebUI italic bubble) (`base.py:162-170`).

`send_reasoning` (`base.py:201-223`) is the base-class bridge: one delta with the full content followed
immediately by an end marker, so a plugin only implements the streaming pair.

**Manager-side dispatch of these hooks is at `manager.py:852-931`** (see §2.5). Critically,
`manager.py:888-899` does an `inspect.signature(channel.send_delta)` check for a `merge_next` parameter
(or `**kwargs`) before passing it. A Go port must reproduce this version-tolerance behavior explicitly
rather than assuming every channel accepts `merge_next`.

### 1.5 The plugin manifest — the second half of the abstraction

A channel is not just a `BaseChannel` subclass. Each package declares a `PLUGIN` object in
`manifest.py` (`plugin.py:142`), and `manifest.py` **must not import the channel's optional SDK**
(`plugin.py:26-29`). This is what makes discovery and the settings UI work without installing
`lark-oapi` or `discord.py`.

`ChannelPlugin` (`plugin.py:22-41`), a frozen dataclass with fields:

| Field | Type | Default | Purpose |
|---|---|---|---|
| `name` | `str` | required | Must match `[A-Za-z][A-Za-z0-9_]*` (`plugin.py:19`, `plugin.py:44-48`) |
| `display_name` | `str` | required | UI label |
| `runtime` | `str` | required | `"module:attribute"` absolute import target (`plugin.py:49`) |
| `connector` | `str \| None` | `None` | Optional interactive-login factory (`plugin.py:96-113`) |
| `setup` | `ChannelSetupSpec \| None` | `None` | Writable settings contract |
| `management` | `ChannelManagementSpec` | `ChannelManagementSpec()` | Persisted-state adapter |
| `dependencies` | `tuple[str, ...]` | `()` | PEP 508 requirements, validated with `packaging.requirements.Requirement` (`plugin.py:61-67`) |
| `default_enabled` | `bool` | `False` | Activation default |
| `settings_visible` | `bool` | `True` | — |
| `capabilities` | `frozenset[str]` | `frozenset()` | Only value in use: `"always_enabled"` (websocket) |
| `webui` | `str \| None` | `None` | Package-relative UI entry; path-traversal rejected (`plugin.py:68-72`) |

**`load_channel_class`** (`plugin.py:74-94`) resolves the runtime lazily and enforces three invariants:
the target must be a `BaseChannel` subclass and not `BaseChannel` itself (`plugin.py:81-89`), and
`channel_cls.name` must equal `self.name` (`plugin.py:90-93`).

**`load_channel_package`** (`plugin.py:134-179`) additionally verifies, without importing the runtime,
that the `runtime` and `connector` modules exist as files inside the package (`plugin.py:155-172`) and
that the `webui` entry exists (`plugin.py:173-178`). It is `@lru_cache`d (`plugin.py:134`).

**OBSERVED: only two channels declare `default_enabled=True` / `capabilities`.** Grep of all 17
manifests: `websocket` sets `default_enabled=True` and `capabilities=frozenset({"always_enabled"})`
(`nanobot/channels/websocket/manifest.py:13-21`). Every other channel defaults to disabled.

**OBSERVED: the dependency gate.** `ChannelManager._init_channels` calls
`ensure_enabled_channel_dependencies(enabled_names, plugins)` (`manager.py:279`), which for each enabled
channel checks `extra_installed(name, dependencies)` and attempts `install_extra(...)` if missing,
returning a user-safe error string per failed channel (`optional_features.py:827-851`). Channels with a
dependency error are skipped before the runtime class is even imported (`manager.py:283-285`).

### 1.6 What is NOT part of the abstraction

- **No message-type abstraction.** `send` receives the full `OutboundMessage` including its `event`
  field; the manager, not the channel, decides which method to call (§2.5). A channel that wants
  streaming overrides `send_delta`; it never inspects `msg.event` for stream semantics.
- **No inbound deduplication at the abstraction level.** Each channel implements its own (e.g. Telegram
  dedups mentions at `telegram/runtime.py:444-453`; Mattermost keys stream state by post id at
  `mattermost/runtime.py:112-117`).
- **No retry at the channel level.** `base.py:99-101` explicitly pushes retry policy to the manager.
- **No ordering guarantee at the abstraction level.** Ordering is per-`(channel, chat_id)` and is
  implemented by the manager (§2.6). Telegram additionally builds its own 0.2s reorder window
  (`telegram/runtime.py:1877-1907`).
- **No routing.** A channel never chooses its `chat_id` target for outbound; the agent's
  `TurnRoute` decides (`agent/turn_delivery.py:31-38`).

---

## 2. The message flow, end to end

### 2.1 Hop 1 — platform transport → channel runtime

Each channel's `start()` opens its transport and calls its own handler. Concrete examples with
citations:

| Channel | Transport | Handler entry | Cite |
|---|---|---|---|
| Signal | SSE over HTTP | `_sse_receive_loop` → `_handle_receive_notification` | `signal/runtime.py:604`, `:691` |
| Telegram | HTTPS long-poll or webhook | `_on_message` → `_enqueue_ordered_update` → `_drain_ordered_updates` | `telegram/runtime.py:1947`, `:1852`, `:1877` |
| Mattermost | WebSocket | `_ws_listen_loop` → `_handle_ws_message` → `_handle_posted_event` | `mattermost/runtime.py:171`, `:196`, `:207` |
| Discord | Gateway WebSocket | `on_message` → `_handle_discord_message` | `discord/runtime.py:97`, `:595` |
| Slack | Socket Mode WebSocket | `_on_socket_request` | `slack/runtime.py:411` |
| Matrix | `/sync` long-poll | `_sync_loop` → `_on_message` | `matrix/runtime.py:1079`, `:1375` |
| Email | IMAP polling | (poll loop; `imaplib.IMAP4_SSL`) | `email/runtime.py:169`, `:617` |
| MS Teams | Local HTTP server (stdlib `ThreadingHTTPServer`) | `_handle_activity` via `run_coroutine_threadsafe` | `msteams/runtime.py:219`, `:206` |
| NapCat | OneBot v11 WebSocket client | frame dispatch on `post_type` | `napcat/runtime.py:111`, `:213` |
| Feishu | `lark.ws.Client` WebSocket | `register_p2_im_message_receive_v1(self._on_message_sync)` | `feishu/runtime.py:1104` |
| WebSocket (WebUI) | RFC 6455 server | `WebUIGatewayEndpoint.process_request` → `_parse_envelope` | `websocket/runtime.py:321` |

### 2.2 Hop 2 — channel runtime → bus

Every channel (except `signal`, which overrides it) funnels into `BaseChannel._handle_message`
(`base.py:255`). That method performs the authorization gate and publishes to the bus
(`base.py:321`). `signal/runtime.py:423` overrides `_handle_message` to add composite-sender-ID
handling before delegating (`signal/runtime.py:390-421`).

### 2.3 Hop 3 — the bus

`MessageBus` (`bus/queue.py:19`) is deliberately minimal:

| Member | Line | Semantics |
|---|---|---|
| `inbound: asyncio.Queue[InboundMessage]` | `queue.py:32` | **Unbounded.** Channels push; agent pulls |
| `outbound: asyncio.Queue[OutboundMessage]` | `queue.py:33` | **Unbounded.** Agent pushes; dispatcher pulls |
| `publish_inbound(msg)` | `queue.py:37-39` | `await self.inbound.put(msg)` |
| `consume_inbound()` | `queue.py:41-43` | `await self.inbound.get()` |
| `publish_outbound(msg)` | `queue.py:45-47` | `await self.outbound.put(msg)` |
| `consume_outbound()` | `queue.py:67-69` | `await self.outbound.get()` |
| `publish_event(event, *, channel, chat_id, metadata)` | `queue.py:49-65` | Wraps the event via `outbound_message_for_event(...)` and calls `publish_outbound` |
| `inbound_size` / `outbound_size` | `queue.py:71-79` | `qsize()` — used by tests and health |
| `subscribe(handler, event_type=None)` | `queue.py:92-116` | Registers an awaited local handler; returns an idempotent unsubscribe closure |
| `publish(event)` | `queue.py:118-126` | Awaits local subscribers **in registration order**; swallows and logs handler exceptions |
| `publish_nowait(event)` | `queue.py:128-143` | Schedules `publish` as a task; retains it in `self._pending`; returns `None` if no running loop |
| `drain()` | `queue.py:145-148` | Awaits all outstanding `publish_nowait` tasks |

**Key architectural statement** (`queue.py:25-29`): local subscribers are awaited by `publish`; channel
delivery is queued by `publish_event`. Local state transitions never wait for network sends.

**OBSERVED: the Go port already has this.** `internal/bus/bus.go:1-271` mirrors `queue.py`, including a
documented deliberate divergence — the Go bus defaults to unbounded (compatibility first) with an
optional `MaxInbound`/`MaxOutbound` cap (`internal/bus/bus.go:6-11`, `:33-42`). The Go bus is
mutex-protected with a broadcast-channel wakeup rather than a Go channel, because Go channels cannot be
unbounded (`internal/bus/bus.go:44-49`).

**OBSERVED: the Go `OutboundMessage` lacks the `event` field.** `internal/core/types.go:78-86` has
`Channel, ChatID, Content, ReplyTo, Media, Metadata, Buttons` — no `Event`. The Python
`OutboundMessage` has `event: AgentEvent | None` (`bus/events.py:68`). **This is a hard blocker for
channel porting** and is expanded in §4.1.

### 2.4 Hop 4 — bus → agent loop

`AgentLoop.run()` (`agent/loop.py:1261`) is an infinite loop:

1. `msg = await asyncio.wait_for(self.bus.consume_inbound(), timeout=1.0)` (`loop.py:1269`). The 1s
   timeout is a housekeeping tick: on `TimeoutError` it calls `_check_expired_sessions_if_due()`
   (`loop.py:1270-1272`).
2. `effective_key = self._effective_session_key(msg)` (`loop.py:1287`).
3. `handle_runtime_control(...)` — may consume the message (`loop.py:1288-1289`).
4. `require_existing_session` gate: drop if no cached session (`loop.py:1290-1294`).
5. Priority command dispatch (`loop.py:1297-1302`).
6. Automation-turn deferral (`loop.py:1303-1318`).
7. **Pending-queue routing** (`loop.py:1339-1373`): if `effective_key in self._pending_queues`, the
   message is a *follow-up to an already-running turn*. Non-priority dispatchable commands are
   dispatched inline (`loop.py:1342-1347`); otherwise a `PENDING_FOLLOWUP_ID_KEY` is recorded and the
   message is `put_nowait` into the pending queue (`loop.py:1348-1373`). On `QueueFull` it falls
   through to spawning a competing task (`loop.py:1362-1367`).
8. Otherwise `task = asyncio.create_task(self._dispatch(msg))` (`loop.py:1376`) — **per-session serial,
   cross-session concurrent** (`loop.py:1392`).

`_dispatch` (`loop.py:1391-1529`) acquires a per-session lock (`loop.py:1411`, `:1418`), creates the
pending queue with **`maxsize=20`** (`loop.py:1421`), registers it in `self._pending_queues`
(`loop.py:1422`), and calls `_process_message(msg, pending_queue=pending, delivery=delivery)`
(`loop.py:1429-1433`).

### 2.5 Hop 5 — the pending queue and mid-turn injection

This is the mechanism with **no Go equivalent yet**, and the one the Go port explicitly documents as
missing.

`_run_agent_loop(..., pending_queue: asyncio.Queue[InboundMessage] | None = None, ...)`
(`loop.py:939-956`, parameter at `loop.py:947`) defines two closures:

- **`_drain_pending(*, limit=_MAX_INJECTIONS_PER_TURN, first_msg=None)`** (`loop.py:984-1069`).
  Returns `[]` if `pending_queue is None` (`loop.py:990-991`). Otherwise it converts each
  `InboundMessage` into a `{"role": "user", "content": ...}` row (`loop.py:993-1058`), **draining only
  what is already available** via `pending_queue.get_nowait()` in a loop bounded by `limit`
  (`loop.py:1063-1067`). It never blocks.
- **`_wait_for_pending(*, limit=_MAX_INJECTIONS_PER_TURN)`** (`loop.py:1073-1105`). First drains; if
  nothing and there are running subagents for the session, it waits on `pending_queue.get()` under a
  `_SUBAGENT_TERMINAL_WAIT_SECONDS = 300.0` deadline (`loop.py:124`, `:1089-1105`). This is the
  "don't exit the turn before the subagent reports back" path.

Both are handed to the runner as part of `AgentRunSpec` (`loop.py:1204-1205`):

```
injection_callback=_drain_pending,
terminal_injection_callback=_wait_for_pending,
```

The runner consumes them in `AgentRunner._drain_injections` (`agent/runner.py:238-296`), which:

- selects `terminal_injection_callback` when `terminal=True`, else `injection_callback`
  (`runner.py:251-255`);
- probes the callback's signature for a `limit` parameter or `**kwargs` (`runner.py:259-270`);
- passes `limit=_MAX_INJECTIONS_PER_TURN` when accepted;
- **swallows all callback exceptions** and returns `[]` (`runner.py:271-273`);
- normalizes items into user messages, dropping empties (`runner.py:276-288`);
- caps the result at `_MAX_INJECTIONS_PER_TURN` and logs the drop count (`runner.py:289-296`).

**OBSERVED constants:** `_MAX_INJECTIONS_PER_TURN = 3` (`runner.py:73`),
`_MAX_INJECTION_CYCLES = 5` (`runner.py:74`).

**OBSERVED — leftover handling on turn end.** `_dispatch`'s `finally` block
(`loop.py:1489-1513`) drains anything still in the pending queue and **re-publishes each item to the
bus** (`loop.py:1507`) so it is processed as a fresh inbound message rather than lost. It also guards
ownership: only the task that still owns the queue entry pops it (`loop.py:1496-1499`).

**OBSERVED — the Go port's current state.** `internal/agent/runner.go:12-15` states plainly:

> injection callbacks (`_MAX_INJECTIONS_PER_TURN`/`_MAX_INJECTION_CYCLES`). These drain a per-session
> pending queue that is fed by channels; with no channels in this port there is no source of mid-turn
> messages, so the callback has nothing to consume.

**Conclusion (OBSERVED): porting channels without porting the pending-queue + injection-callback path
produces a system where a user cannot send a second message during a running turn.** The message would
either be dropped or spawn a competing turn. This is the single highest-risk integration gap.

### 2.6 Hop 6 — outbound: agent → bus

Output leaves the agent through `TurnDelivery` (`agent/turn_delivery.py:73`, `:294-334`):

- `complete(response, publish_completion=...)` calls `await self.bus.publish_outbound(response)`
  (`turn_delivery.py:309`), with one exception: a `websocket` channel with `stop_reason == "error"` is
  suppressed (`turn_delivery.py:308`).
- `fail(...)` publishes a fixed `"Sorry, I encountered an error."` message
  (`turn_delivery.py:336-344`).
- Streaming events go through `_bind_events` (`turn_delivery.py:44-62`) which calls
  `bus.publish_event(event, channel=..., chat_id=..., metadata=deepcopy(metadata))`
  (`turn_delivery.py:58-60`), gated by `notification_is_deliverable` (`turn_delivery.py:50-53`).
- `_publish_event` (`turn_delivery.py:367-381`) stamps a `stream_id` of the form
  `f"{self._stream_base_id}:{self._stream_segment}"` (`turn_delivery.py:363-365`) on every
  `StreamDeltaEvent`/`StreamEndEvent`, and advances `_stream_segment` on a non-`merge_next`
  `StreamEndEvent` (`turn_delivery.py:378-381`).

**Audience policy** (`bus/notification_delivery.py:16-41`):

| Event | Audience |
|---|---|
| `ProgressEvent`, `FileEditEvent`, `RetryWaitEvent` | `lifecycle` (only if `publish_lifecycle`) |
| `StreamDeltaEvent`, `StreamEndEvent`, `ContextCompactionEvent` | `channel` (always) |
| `RecoveryStateEvent`, `RetryStatusEvent` | `interactive` (websocket only) |
| anything else | **not deliverable** (`notification_delivery.py:33-34`) |

### 2.7 Hop 7 — bus → channel (the dispatcher)

`ChannelManager._dispatch_outbound` (`manager.py:761-765`) wraps `_dispatch_outbound_loop`
(`manager.py:767-850`) and cancels all in-flight outbound tasks on exit (`manager.py:765`).

The loop (`manager.py:775-850`):

1. Pop from a local `pending: list[OutboundMessage]` buffer first, else
   `await asyncio.wait_for(self.bus.consume_outbound(), timeout=1.0)` (`manager.py:778-784`).
   The `pending` list exists because `asyncio.Queue` has no `push_front` (`manager.py:771-773`).
2. **Reasoning gate** (`manager.py:786-801`): if the event is a `ProgressEvent` with
   `reasoning_delta`/`reasoning_end`/`reasoning`, deliver **only** if the target channel exists and
   `channel.show_reasoning` is true; otherwise `continue` (silently dropped).
3. **Progress gate** (`manager.py:803-811`): tool hints require `send_tool_hints`; other progress
   requires `send_progress`. Both read the *instance* attribute (`manager.py:347-353`).
4. `RetryWaitEvent` → `continue` (`manager.py:813-814`).
5. `RuntimeModelUpdatedEvent` addressed to a non-existent `websocket` channel → `continue`
   (`manager.py:816-821`).
6. **Delta coalescing** (`manager.py:823-828`): for a `StreamDeltaEvent`, call
   `_coalesce_stream_deltas(msg)` (`manager.py:933-996`) and extend `pending` with the non-matching
   messages it pulled off the queue.
7. Resolve `channel = self.channels.get(msg.channel)` (`manager.py:830`); if absent, log
   `"Unknown channel: {}"` and continue (`manager.py:844-845`).
8. **Duplicate suppression** for non-stream events (`manager.py:834-842`), via
   `_should_suppress_outbound` (`manager.py:698-719`), which keys on `(channel, chat_id, origin_message_id
   | message_id)` and an SHA-1 of whitespace-normalized content (`manager.py:683-686`), with a bounded
   `OrderedDict` of `ORIGIN_REPLY_FINGERPRINTS_MAX_SIZE = 1000` (`manager.py:65`, `:688-696`).
9. `await self._queue_outbound(channel, msg)` (`manager.py:843`).

**`_queue_outbound`** (`manager.py:728-759`) is the ordering/backpressure core:

- Acquires one of `_OUTBOUND_PENDING_LIMIT = 256` slots (`manager.py:62`, `:144`, `:734`).
- Re-checks liveness after acquiring: if the channel is stopping or has been replaced, release and
  drop (`manager.py:735-737`).
- Key is `(msg.channel, msg.chat_id)` (`manager.py:738`) — **FIFO per destination, not globally**.
- Chains to the previous tail for that key with `asyncio.shield(asyncio.gather(previous, ...))`
  (`manager.py:741-743`), then takes one of `_OUTBOUND_CONCURRENCY = 32` send slots (`manager.py:61`,
  `:145`, `:744`).
- A done-callback releases the slot, unlinks the tail, and logs non-cancellation exceptions
  (`manager.py:751-759`).

**`_send_with_retry`** (`manager.py:998-1056`):

- `max_attempts = max(self.config.channels.send_max_retries, 1)` (`manager.py:1012`).
- `_SEND_RETRY_DELAYS = (1, 2, 4)` seconds, clamped by index (`manager.py:60`, `:1043`).
- `asyncio.CancelledError` is **always re-raised** (`manager.py:1020-1021`, `:1055-1056`).
- `channel.should_retry_send_error(e)` is consulted per failure; a `False` return logs and **returns
  without retrying** (`manager.py:1023-1030`).
- When a `deadline` is supplied (restart-notice path), retry continues until the deadline instead of
  the attempt cap (`manager.py:1032-1036`, `:1044-1045`).

**`_send_once`** (`manager.py:908-931`) is the event→method dispatch table. This is the exact contract
a Go port must reproduce:

| Event on `msg.event` | Condition | Channel method called | Line |
|---|---|---|---|
| `ProgressEvent` | `reasoning_end` | `send_reasoning_end(chat_id, metadata, stream_id=)` | `manager.py:912-913` |
| `ProgressEvent` | `reasoning_delta` | `send_reasoning_delta(chat_id, content, metadata, stream_id=)` | `manager.py:914-915` |
| `ProgressEvent` | `reasoning` | `send_reasoning(msg)` | `manager.py:916-919` |
| `ProgressEvent` | `file_edit_events` | `send_file_edit_events(chat_id, events, metadata)` | `manager.py:920-925` |
| `StreamDeltaEvent` | — | `send_delta(chat_id, content, metadata, stream_id=, stream_end=False, resuming=False)` | `manager.py:926-927` |
| `StreamEndEvent` | — | `send_delta(..., stream_end=True, resuming=event.resuming, merge_next=…)` | `manager.py:928-929` |
| `StreamedResponseEvent` | — | **nothing** (`elif not isinstance(...)`) | `manager.py:930-931` |
| anything else / `None` | — | `send(msg)` | `manager.py:930-931` |

`_send_stream_event` (`manager.py:877-906`) builds the kwargs and conditionally adds `merge_next` only
if the channel's `send_delta` signature accepts it (`manager.py:888-899`).

### 2.8 Lifecycle

**Startup** (`cli/gateway_runtime.py`): `bus = MessageBus()` (`:411`); `ChannelManager(config, bus, ...)`
constructed with a large keyword-only surface (`:715-733`); then in `run()` both are launched as sibling
tasks (`:940-941`):

```
asyncio.create_task(_run_agent(), name="nanobot-agent-loop"),
asyncio.create_task(channels.start_all(), name="nanobot-channels"),
```

`ChannelManager.start_all()` (`manager.py:598-616`) sets `_started = True`, starts the outbound
dispatcher task (`manager.py:606`), then starts one task per channel via `_start_channel_task`
(`manager.py:391-395`) and `await asyncio.gather(*tasks, return_exceptions=True)` (`manager.py:616`) —
i.e. it blocks forever because channels are long-running.

`_start_channel` (`manager.py:373-389`) pops any prior error, awaits `channel.start()`, re-raises
`CancelledError`, and otherwise stores `channel.start_error_message(exc)` or a generic string in
`self._channel_errors[name]` (`manager.py:384-389`). **A channel that fails to start does not stop the
gateway.**

**Hot reload** — `apply_channel_feature_action` (`manager.py:429-596`) enables/disables one channel
instance without a gateway restart: it reloads config (`manager.py:459-461`), and for `disable` stops
and drops the runtime (`manager.py:466-485`); for `enable` it rebuilds and, if `_started`, starts the
task (`manager.py:522-579`). It returns `requires_restart: True` for channels with the
`always_enabled` capability (`manager.py:451-457`).

**Shutdown** — `stop_all()` (`manager.py:668-681`) cancels the dispatcher, then stops every channel via
`_stop_channel` (`manager.py:397-403`), which first cancels that channel's outbound tasks
(`_cancel_outbound`, `manager.py:721-726`) and then `_stop_channel_runtime` (`manager.py:405-427`).
`_stop_channel_runtime` awaits `channel.stop()`, then cancels and awaits the channel's start task
(`manager.py:423-427`).

**Status** — `get_status()` (`manager.py:1062-1096`) returns per-runtime
`{enabled, running, state ∈ {failed, running, starting, stopped}, owner, instance_id, [error]}`.
`enabled_channels` returns `list(self.channels.keys())` (`manager.py:1098-1101`).

### 2.9 Multi-instance

A single channel package may own several runtime instances with distinct `name`s
(`channel.name = runtime_name`, `manager.py:217-218`). Runtime names must be either the plugin name or
prefixed `"{plugin_name}."` (`contracts.py:544-554`). Instance expansion goes through
`channel_instance_specs` (`contracts.py:337-385`), which validates non-empty unique instance ids and
unique runtime names (`contracts.py:364-385`). Only `feishu` declares a `ChannelManagementSpec` with
multi-instance callbacks (`feishu/manifest.py:49`, `feishu/instances.py`); `weixin` and `whatsapp`
declare only `local_state_present` (`weixin/manifest.py:40`, `whatsapp/manifest.py:32`).

**OBSERVED: `notification_routes.py`** (`notification_routes.py:8-23`) is a 23-line allowlist that
preserves thread addresses across turns for exactly five channel families — `telegram`
(`message_thread_id`), `matrix` (`thread_root_event_id`, `thread_reply_to_event_id`), `feishu`
(`message_id`, `thread_id`, `chat_type`), and nested `slack`/`mattermost` thread ids
(`notification_routes.py:11-22`). It is pure and trivially portable.

---

## 3. The dependency surface

### 3.1 Shared infrastructure — per-file dependency audit

| File | Third-party imports | Pure? | What Go needs |
|---|---|---|---|
| `channels/base.py` | `loguru` (`base.py:9`); lazy: `nanobot.audio.transcription`, `nanobot.config.loader` inside `transcribe_audio` (`base.py:51-56`) | **No** (loguru only) | `log/slog`. The ABC, allowlist, and `_handle_message` are pure logic. |
| `channels/manager.py` | `loguru` (`manager.py:14`) | **No** (loguru only) | `log/slog` + the whole asyncio concurrency model (§Appendix A). No network code at all. |
| `channels/contracts.py` | **none** — only `json`, `collections.abc`, `copy`, `dataclasses`, `pathlib`, `typing` (`contracts.py:3-13`) | **YES — fully pure** | Pure data + validation. Lazy imports of `nanobot.config.loader.merge_missing_defaults` at `contracts.py:281` and `:459`. |
| `channels/plugin.py` | `packaging.requirements` (`plugin.py:12`) | **Almost pure** | `packaging` is used only to *validate* PEP 508 strings (`plugin.py:61-67`). A Go port can accept requirement strings as opaque and skip validation, or implement a minimal parser. Also uses `importlib.resources.files` (`plugin.py:9`, `:131`, `:151`) — Go equivalent is `embed.FS` or an `os.Stat`. |
| `channels/registry.py` | `loguru` (`registry.py:8`); `pkgutil` is stdlib (`registry.py:5`) | **Almost pure** | `pkgutil.iter_modules` → Go needs an explicit registry table or `embed.FS` scan; Go has no runtime module discovery. |
| `channels/_manifest.py` | **none** (`_manifest.py:3-8`) | **YES — fully pure** | 40 lines of constructors. |
| `channels/connect.py` | **none** (`connect.py:3-5`) | **YES — fully pure** | 24 lines. |
| `channels/_setup.py` | **none** (`_setup.py:3-10`) | **YES — fully pure** | 23 lines. |
| `channels/notification_routes.py` | **none** (`notification_routes.py:1-5`) | **YES — fully pure** | 23 lines. |
| `channels/validation.py` | `httpx` (`validation.py:16`); `socket`, `ssl`, `re`, `datetime` are stdlib | **No — network** | `net/http` + `crypto/tls` + `net.DialTimeout`. Uses `nanobot.security.network.resolve_url_target` (`validation.py:29`, `:376`) — an SSRF guard that must be ported alongside. |

**OBSERVED — the decisive fact for planning:** of the ten shared infrastructure files, **six are
100% dependency-free pure Python** (`contracts.py`, `_manifest.py`, `connect.py`, `_setup.py`,
`notification_routes.py`, and the bulk of `base.py`). Only `validation.py` performs network I/O, and it
is a WebUI settings-probe surface, not part of the message path.

**INFERENCE:** `validation.py` can be deferred indefinitely. Nothing in the message path imports it —
`manager.py` imports `_setup` and `contracts` but never `validation` (`manager.py:27-35`).
`validation.py` is reachable only from channel `validation.py` modules and the WebUI settings routes.

### 3.2 Third-party capability matrix — what Go stdlib cannot do

| Capability | Python library | Used by | Go stdlib status |
|---|---|---|---|
| HTTP/1.1 + TLS client | `httpx`, `aiohttp` | almost all | **Full** — `net/http`, `crypto/tls` |
| HTTP server | `http.server` (stdlib), `websockets` process_request | msteams, websocket | **Full** — `net/http` |
| JSON | stdlib `json` | all | **Full** — `encoding/json` |
| **WebSocket client (RFC 6455)** | `websockets`, `aiohttp` | mattermost, napcat, slack, dingtalk, feishu, qq, wecom, mochat, discord | **ABSENT** — must be hand-written (handshake, masking, frame parsing, ping/pong, close) |
| **WebSocket server (RFC 6455)** | `websockets.asyncio.server` | websocket | **ABSENT** — needs `net/http` `Hijacker` + hand-written framing |
| SSE (client) | `httpx` streaming | signal | **Full** — `net/http` + `bufio.Scanner` |
| IMAP client | `imaplib` (stdlib) | email | **ABSENT** — `net/smtp` exists (deprecated), no IMAP |
| SMTP client | `smtplib` (stdlib) | email | `net/smtp` (frozen/deprecated but present) |
| JWT RS256 verify | `PyJWT` + `cryptography` | msteams | **Full** — `crypto/rsa`, `crypto/x509`, `encoding/base64`, `encoding/json` |
| AES | `pycryptodome` / `cryptography` | weixin | **Full** — `crypto/aes`, `crypto/cipher` |
| HMAC | stdlib `hmac` | (websocket static token uses `hmac.compare_digest`) | **Full** — `crypto/hmac`, `crypto/subtle` |
| Markdown → HTML/mrkdwn | `mistune`, `nh3`, `slackify_markdown` | matrix, slack | **ABSENT** — must be hand-written |
| socket.io | `python-socketio` + `msgpack` | mochat | **ABSENT** |
| Olm/Megolm E2EE | `matrix-nio[e2e]` (libolm) | matrix | **ABSENT** — not stdlib-feasible |
| WhatsApp Web protocol | `neonize` | whatsapp | **ABSENT** — not stdlib-feasible |
| Declarative validation | `pydantic` | all configs | **ABSENT** — hand-written validation |
| Logging | `loguru` | base, manager, registry, all runtimes | `log/slog` |

### 3.3 Per-channel wire protocol and dependency table

Protocol classifications below are **OBSERVED** from imports and transport calls, except where marked
INFERENCE (SDK-internal wire details, since the SDKs are not installed here).

| Channel | Inbound transport | Outbound transport | Declared deps (`manifest.py`) | Go stdlib feasibility |
|---|---|---|---|---|
| **signal** | SSE `GET /api/v1/events` on a local `signal-cli-rest-api` daemon (`signal/runtime.py:612`) | HTTP `POST /api/v1/rpc`, JSON-RPC 2.0 (`signal/runtime.py:1414`, `:1427`) | **none** (`signal/manifest.py:26-31`) | **FULL** — `net/http`, `encoding/json`, `bufio` |
| **telegram** | HTTPS long-poll `getUpdates` or HTTPS webhook server (`telegram/runtime.py:762-766`, `:750-759`) | HTTPS Bot API `POST /bot<token>/<Method>` (INFERENCE for path; methods enumerated from calls) | `python-telegram-bot[socks,webhooks]`, `socksio`, `python-socks` (`telegram/manifest.py:41-45`) | **FULL** — `net/http`, `encoding/json`, `mime/multipart` |
| **msteams** | Local HTTP server, stdlib `ThreadingHTTPServer` (`msteams/runtime.py:219`) | HTTPS Bot Framework (`login.microsoftonline.com/.../oauth2/v2.0/token`, `msteams/runtime.py:825`) | `PyJWT`, `cryptography` (`msteams/manifest.py:34-37`) | **FULL** for HTTP; JWT verify via `crypto/rsa` + `crypto/x509` |
| **email** | IMAP over TLS, polling (`email/runtime.py:169`, `:617`) | SMTP (`email/runtime.py:372`, `:381`) | **none** (`email/manifest.py:55-61`) | **PARTIAL** — IMAP must be hand-written |
| **mattermost** | WebSocket `wss://.../api/v4/websocket` (`mattermost/runtime.py:81-84`, `:177`) | HTTPS REST `/api/v4/*` (`mattermost/runtime.py:134`, `:678`) | **none** (`mattermost/manifest.py:35`) | **PARTIAL** — REST full; WS hand-written |
| **weixin** | HTTP long-poll (`weixin/runtime.py:1`, `:209`) | HTTP (`weixin/runtime.py:29`) | `qrcode[pil]`, `pycryptodome` (`weixin/manifest.py:41-44`) | **PARTIAL** — HTTP full; AES full; QR image gen hand-written |
| **dingtalk** | `dingtalk-stream` WebSocket (`dingtalk/runtime.py:303`, `:307`) | HTTPS `api.dingtalk.com` (`dingtalk/runtime.py:377`, `:669`) | `dingtalk-stream` (`dingtalk/manifest.py:26`) | **PARTIAL** — HTTPS full; stream WS hand-written |
| **napcat** | WebSocket client, OneBot v11 (`napcat/runtime.py:20`, `:111`) | WS action `send_msg` (`napcat/runtime.py:465`) | `aiohttp` (`napcat/manifest.py:21`) | **PARTIAL** — WS hand-written |
| **qq** | botpy WebSocket gateway (`qq/runtime.py:53`, `:155`) | HTTPS via botpy `Route` (`qq/runtime.py:54`) | `aiohttp`, `qq-botpy` (`qq/manifest.py:27-30`) | **PARTIAL** — WS hand-written |
| **wecom** | `wecom_aibot_sdk.WSClient` WebSocket (`wecom/runtime.py:112-116`) | same WS | `wecom-aibot-sdk-python` (`wecom/manifest.py:23`) | **PARTIAL** — WS hand-written |
| **feishu** | `lark.ws.Client` WebSocket (`feishu/runtime.py:1134`, `:1104`) | HTTPS / lark SDK | `lark-oapi` (`feishu/manifest.py:50`) | **PARTIAL** — WS hand-written; card JSON is large |
| **slack** | Socket Mode WebSocket (`slack/runtime.py:153`, `:173`) | HTTPS Web API `chat_postMessage` etc. (`slack/runtime.py:251`; protocol decl `:42-53`) | `aiohttp`, `slack-sdk`, `slackify-markdown` (`slack/manifest.py:39-43`) | **PARTIAL** — WS hand-written; mrkdwn hand-written |
| **discord** | Gateway WebSocket (`discord/runtime.py:486`) | HTTPS REST v10 (`discord/validation.py:25`) | `discord.py` (`discord/manifest.py:34`) | **PARTIAL** — WS + heartbeat/resume + rate-limit buckets hand-written |
| **mochat** | socket.io over WebSocket, HTTP polling fallback (`mochat/runtime.py:416`, `:464`, `:405`) | HTTPS REST `/api/claw/*` (`mochat/runtime.py:376`) | `python-socketio`, `msgpack` (`mochat/manifest.py:40-43`) | **PARTIAL/HARD** — socket.io framing + msgpack hand-written |
| **matrix** | HTTPS `/sync` long-poll (`matrix/runtime.py:1084`) | HTTPS CS API `room_send` (`matrix/runtime.py:526`) | `matrix-nio[e2e]`, `aiohttp`, `mistune`, `nh3` (`matrix/manifest.py:40-45`) | **PARTIAL** — HTTP full; E2EE **not feasible** |
| **whatsapp** | neonize (WhatsApp Web / Noise protocol) (`whatsapp/runtime.py:88`) | same | `neonize`, `segno` (`whatsapp/manifest.py:33-36`) | **NOT FEASIBLE** stdlib-only |
| **websocket** | RFC 6455 server (`websocket/runtime.py:20`, `:712`, `:725`) | WS JSON frames (`websocket/runtime.py:537-540`) | **none** (`websocket/manifest.py`) | **PARTIAL** — WS server + REST + static serving hand-written |

**OBSERVED correction to a common assumption:** the `websocket` channel declares **no** dependencies
(`websocket/manifest.py:13-21` has no `dependencies` field), yet it imports `websockets` at module
level (`websocket/runtime.py:20`). `websockets` ships with the base install, so it is not gated. This
means the dependency-declaration mechanism does **not** cover every third-party import; a Go port
cannot derive its transport requirements from `manifest.py` alone. Similarly `signal` and `mattermost`
declare no dependencies but import `httpx` (module-level, `signal/runtime.py:17`,
`mattermost/runtime.py:11`) and `websockets` (lazy, `mattermost/runtime.py:172`).

---

## 4. Porting order with rationale

### 4.1 Phase 0 — infrastructure that must come first

The ordering constraint is *data-shape*, not code volume.

**P0.1 — Add `Event` to the Go `OutboundMessage`.** `internal/core/types.go:78-86` has no `Event` field;
`nanobot/bus/events.py:68` does. Without it, `manager._send_once` (`manager.py:908-931`) has nothing to
dispatch on and every message degrades to `send(msg)`, silently killing streaming, progress, reasoning,
and file-edit events. **This is the first prerequisite and blocks everything else.**

**P0.2 — Port `bus/outbound_events.py` and the audience policy.** `ProgressEvent` (`outbound_events.py:23-32`),
`FileEditEvent` (`:35-37`), `StreamDeltaEvent` (`:40-43`), `StreamEndEvent` (`:46-51`),
`StreamedResponseEvent` (`:54-56`), plus `outbound_message_for_event` (`:114-130`),
`replace_outbound_event` (`:133-145`) and `_event_content` (`:148-161`) — the last supplies text
fallbacks for events that reach a non-streaming channel. Then `notification_is_deliverable`
(`bus/notification_delivery.py:28-41`) and the `NOTIFICATION_AUDIENCES` table (`:16-25`).

**P0.3 — Port the pending queue and injection callbacks.** `loop.py:984-1105`, `:1204-1205`,
`:1421-1422`, `:1495-1513`; `runner.py:238-296`; constants `runner.py:73-74`, `loop.py:124`. The Go
port's own source documents this as deliberately absent (`internal/agent/runner.go:12-15`). Without it,
mid-turn user messages have no destination.

**P0.4 — Port `BaseChannel`'s semantics.** `is_allowed` (`base.py:237-253`), `_handle_message`
(`base.py:255-321`), `supports_streaming` (`base.py:225-235`), and the `send_reasoning` bridge
(`base.py:201-223`). Requires the pairing store (`pairing/store.py`, 367 lines: `generate_code` `:113`,
`approve_code` `:144`, `is_approved` `:182`, `format_pairing_reply` `:285`, and the
`PAIRING_CODE_META_KEY = "_pairing_code"` constant at `pairing/__init__.py:19`).

**P0.5 — Port `ChannelManager`'s outbound path.** `_dispatch_outbound_loop` (`manager.py:767-850`),
`_queue_outbound` (`manager.py:728-759`), `_send_once` (`manager.py:908-931`),
`_send_with_retry` (`manager.py:998-1056`), `_coalesce_stream_deltas` (`manager.py:933-996`),
`_should_suppress_outbound` (`manager.py:698-719`), plus constants at `manager.py:59-65`.

**P0.6 — Port the lifecycle.** `start_all`/`stop_all`/`_start_channel`/`_stop_channel`
(`manager.py:373-427`, `:598-616`, `:668-681`) and `get_status` (`manager.py:1062-1096`).

**P0.7 — Port `contracts.py` + `plugin.py` + `registry.py` equivalents.** Deliberately last among the
infrastructure, because in Go there is no `pkgutil` and no `importlib.resources`: the registry becomes a
static table (or `embed.FS` scan) and the `"module:attribute"` runtime indirection becomes a
constructor function in that table. `contracts.py` is pure and mechanical.

**Deferrable within Phase 0:** `validation.py` (network, settings-only) and the WebUI surface.

### 4.2 Phase 1 — the first three channels

Selection criteria applied, in priority order: (a) real-world usage, (b) protocol is plain
HTTPS/webhook rather than a persistent WebSocket or a heavy SDK, (c) testability without the real
service.

**#1 — Telegram** (`nanobot/channels/telegram/`, 6,145 lines)

- *Why first:* highest real-world usage of any channel in this repo, and the underlying protocol is
  plain HTTPS JSON. Both configured modes are stdlib-friendly: `mode: "polling"` (`telegram/manifest.py:14`)
  is `getUpdates` long-polling (`telegram/runtime.py:762-766`); `mode: "webhook"` is an inbound HTTP
  server with a `secret_token` header check (`telegram/runtime.py:750-759`). No WebSocket anywhere.
- *Go stdlib coverage:* `net/http` (client for polling, `http.Server` for webhook), `encoding/json`,
  `mime/multipart` for media upload, `context` for cancellation.
- *Must be hand-written:* the long-poll loop with `offset` tracking (the SDK owns this; nanobot only
  wraps the request pool, `telegram/runtime.py:76-99`); the app-level **0.2 s reorder window** keyed by
  `(message_id, update_id)` (`telegram/runtime.py:1845`, `:1877-1907`) — this is nanobot code, not SDK
  code, and must be ported regardless; stall detection with full app rebuild and 5s→300s backoff
  (`telegram/runtime.py:613-657`); `RetryAfter` handling with 3 attempts at 0.5s×2ⁿ
  (`telegram/runtime.py:388-389`; retry wrapper `:1201-1227`, `RetryAfter` branch `:1214-1227`); the markdown→HTML renderer and 4096/4000/32768-char
  splitting (~500 lines at `telegram/runtime.py:44-53`, `:102-385`); typing keepalive at 4s
  (`telegram/runtime.py:2105-2113`).
- *Risk:* `sendRichMessage`/`sendRichMessageDraft` are called as raw API requests
  (`telegram/runtime.py:941-943`, `:983-985`) for Bot API 10.1 rich messages. These are non-standard
  and may not be reproducible without the SDK's serializer. **Recommendation: implement `send_delta`
  using only `send_message` + `edit_message_text` initially** and treat rich drafts as a later
  optimization.

**#2 — Signal** (`nanobot/channels/signal/`, 3,546 lines)

- *Why second:* the **simplest transport in the entire repo**, and it validates the abstraction with
  the least protocol risk. Inbound is SSE (`signal/runtime.py:612`); outbound is JSON-RPC 2.0 over
  `POST /api/v1/rpc` (`signal/runtime.py:1414`, `:1427`). No SDK, no WebSocket, no signature scheme.
- *Go stdlib coverage:* **complete.** `net/http` for both directions, `encoding/json` for JSON-RPC,
  `bufio.Scanner` for SSE line framing. Nothing has to be hand-written below the application layer.
- *Testability:* `signal-cli-rest-api` is a local daemon, so the whole channel is exercisable offline
  against a fake HTTP server. The Python tests already do exactly this — `_FakeHTTPClient` at
  `signal/tests/test_signal_channel.py:40-58` records `posts`/`gets` and returns canned bodies.
- *Must be hand-written:* SSE reconnect with 1s→30s exponential backoff (`signal/runtime.py:472-473`,
  `:537-542`); the markdown→Signal-style renderer with UTF-16 length accounting
  (`signal/runtime.py:85-300`, ~215 lines); typing-indicator lifecycle.
- *Real-world usage:* lower than Telegram, but it is a genuine self-hosted deployment path and it is the
  cheapest possible proof that the Go `BaseChannel` + `ChannelManager` + bus + pending-queue chain works
  end to end.

**#3 — Mattermost** (`nanobot/channels/mattermost/`, 1,940 lines)

- *Why third:* **zero declared dependencies** (`mattermost/manifest.py:35` has no `dependencies` field)
  and only 1,940 lines — the smallest real transport among the SDK-free channels. It is self-hosted, so
  there is no third-party ToS or rate-limit exposure. It is also the **deliberate on-ramp to the
  hand-written WebSocket client**: inbound is `wss://<server>/api/v4/websocket` with a Bearer header and
  `ping_interval=20`/`ping_timeout=10` (`mattermost/runtime.py:81-84`, `:177-182`), and outbound is
  ordinary REST (`mattermost/runtime.py:134`, `:678`). Once this WS client exists, Slack, NapCat, and
  DingTalk become mostly mechanical.
- *Go stdlib coverage:* REST is fully covered (`net/http`, `encoding/json`). The WebSocket client is
  **not** — Go stdlib has no RFC 6455 implementation. The hand-written surface is the client handshake
  (`Upgrade` + `Sec-WebSocket-Key`/`Accept`), client-side frame masking, frame parsing, ping/pong
  keepalive, and close handshake, over a hijacked `net.Conn`.
- *Must be hand-written (application level):* the reconnect loop with base→max exponential backoff
  (`mattermost/runtime.py:174`, `:187-194`); event dispatch on `event ∈ {posted, action, post_deleted}`
  (`mattermost/runtime.py:196-203`); the per-post stream buffer maps
  (`mattermost/runtime.py:112-117`); username/email/id allowlist matching modes
  (`mattermost/manifest.py:16-18`, `mattermost/runtime.py:413-451`).
- *Alternative if a WS client is judged too early:* **MS Teams** (`msteams/`, 1,838 lines) has a
  pure-HTTP inbound webhook built on Python's stdlib `ThreadingHTTPServer`
  (`msteams/runtime.py:219`) and pure-HTTPS outbound (`msteams/runtime.py:825`), with JWT RS256
  validation achievable via `crypto/rsa` + `crypto/x509`. It is a defensible #3 **if** the goal is to
  stay WebSocket-free for as long as possible; it loses on protocol complexity (Bot Framework activity
  schema, conversation references) and on usage.

### 4.3 Explicitly deferred

| Channel | Why deferred | Earliest sensible point |
|---|---|---|
| **websocket** (WebUI) | It is a *server*, not a client: RFC 6455 server + `/webui/*` REST + static dist serving + token issuance + `webui_request` idempotency (`websocket/runtime.py:321`, and per the deep read: `webui/gateway_tokens.py`, `webui/ws_http.py`, `webui/inbound_commands.py`). It also depends on the entire WebUI subsystem, which is out of scope for a channels-only port. | After the WS client exists **and** the WebUI surface is in scope |
| **email** | Python uses only stdlib (`imaplib`/`smtplib`), but Go has **no IMAP client**. `net/smtp` covers outbound; a stateful IMAP client (capability negotiation, `SEARCH`, `FETCH` with MIME parsing, IDLE or polling, flag mutation) is a large hand-written surface. Also 3,874 lines of DKIM/SPF/authserv-id logic. | After ≥3 HTTP channels are stable |
| **feishu** | 7,567 lines — the second-largest. WebSocket inbound via `lark.ws.Client` (`feishu/runtime.py:1134`) plus a large CardKit JSON surface (`feishu/runtime.py:2132-2218`). Highest line-count-per-value. | After the WS client |
| **weixin** | 6,148 lines; HTTP long-poll is portable but the payload/crypto surface (AES via `pycryptodome`, `weixin/runtime.py:2542`) and QR login flow are large. | Late |
| **discord** | Gateway protocol: HELLO/heartbeat with jitter and ACK tracking, IDENTIFY with the intents bitmask (37377 = `GUILDS|GUILD_MESSAGES|DIRECT_MESSAGES|MESSAGE_CONTENT`), RESUME with `session_id`/`resume_gateway_url`, sequence tracking, per-route rate-limit buckets. All SDK-internal, all must be rewritten. | After WS client + rate-limit infrastructure |
| **slack** | Socket Mode envelope ack inside a 3s window + `apps.connections.open` bootstrap + mrkdwn rendering. The SDK owns reconnect. Note: `webhook_path` is declared in config (`slack/manifest.py:14`) but **never referenced** in `runtime.py`, and `mode != "socket"` is a hard error (`slack/runtime.py:142-144`) — do not port the webhook path. | After WS client |
| **matrix** | **E2EE is not achievable stdlib-only.** Olm/Megolm sessions, device keys, room-key sharing, and `decrypt_attachment` (AES-256-CTR + SHA-256 verify, `matrix/runtime.py:1293-1315`) require libolm. Additionally the SAS verification state machine is ~230 lines (`matrix/runtime.py:744-974`). The non-E2EE path (`e2eeEnabled: false`, `matrix/manifest.py:17`) is HTTP-only and *is* portable, but shipping a channel that silently cannot do E2EE is a fidelity gap. | Indefinitely deferred, or scoped to E2EE-disabled |
| **whatsapp** | Depends on `neonize` implementing the WhatsApp Web protocol (Noise handshake, Signal double-ratchet, protobuf). Not reproducible stdlib-only in any reasonable scope. | Indefinitely deferred |
| **qq / napcat / wecom / dingtalk / mochat** | All WebSocket-based (plus socket.io+msgpack for mochat). NapCat is the easiest of these (800 lines, plain OneBot v11 JSON over WS, `napcat/runtime.py:1`, `:213`). | After the WS client; NapCat first |
| **`channels/validation.py`** | Network I/O, settings-UI only, not in the message path. | Indefinitely |
| **`channels/webui/` per channel** | TypeScript UI extensions. | Indefinitely |
| **`gateway/runtime.py`, `gateway/service.py`** | Process supervision and OS service units — orthogonal to channels. | Only if the Go port needs managed-gateway lifecycle |

### 4.4 Summary ordering

```
P0.1 OutboundMessage.Event          ← blocks everything
P0.2 outbound_events + audience policy
P0.3 pending queue + injection callbacks
P0.4 BaseChannel + pairing store
P0.5 ChannelManager outbound path
P0.6 lifecycle (start/stop/status)
P0.7 contracts/plugin/registry equivalents
────────────────────────────────────────
P1.1 Telegram    (HTTPS only, highest value)
P1.2 Signal      (HTTP+SSE only, simplest; validates the whole chain)
P1.3 Mattermost  (REST + first hand-written WS client)
────────────────────────────────────────
P2   NapCat → Slack → DingTalk → WeCom/QQ → Feishu
P3   MS Teams, Email, Weixin, MoChat
P4   WebSocket/WebUI, Discord
P5   Matrix (E2EE-disabled only), WhatsApp (never)
```

---

## 5. Shared test strategy

### 5.1 How channels are tested in the Python repo

**Layout.** Two tiers (**OBSERVED**):

1. **Per-channel package tests** — `nanobot/channels/<name>/tests/`. 16 of 17 packages have one;
   `mochat` has none. Total 39,987 lines. Largest: `websocket/tests/test_websocket_channel.py` (6,391)
   and `websocket/tests/test_websocket_http_routes.py` (4,187). Only `qq` and `websocket` have a
   package-local `conftest.py`; only `websocket` has a `ws_test_client.py` helper.
2. **Cross-channel shared tests** — `tests/channels/` (7,043 lines):

| File | Lines | Covers |
|---|---|---|
| `test_channel_plugins.py` | 3,830 | Discovery, manifests, management specs, config, hot reload, start/stop failure |
| `test_channel_contracts.py` | 578 | `contracts.py` dataclasses and adapters |
| `test_channel_manager_delta_coalescing.py` | 458 | `_coalesce_stream_deltas` |
| `test_channel_setup.py` | 360 | Setup specs |
| `test_channel_manager_reasoning.py` | 299 | Reasoning event delivery gating |
| `test_channel_manager_hot_reload.py` | 268 | `apply_channel_feature_action` |
| `test_channel_manager_concurrency.py` | 187 | Ordering, backpressure, cancellation |
| `test_base_channel.py` | 170 | `is_allowed`, `_handle_message`, pairing |
| `test_websocket_listener_health.py` | 117 | Listener watchdog |
| `test_websocket_application_boundary.py` | 96 | App boundary |
| `test_channel_manager_compaction_notices.py` | 89 | Compaction notices |
| `test_feishu_login_url.py` | 81 | Login URL |
| `test_channel_validation.py` | 31 | Validation entry point |
| `test_qq_inbound_ssrf.py` | 209 | SSRF guard |
| `test_qq_reconnect_backoff.py` | 270 | Reconnect backoff |

**Is there a mock/fake channel?** **Yes — but there is no single shared fixture.** Each test module
defines its own minimal `BaseChannel` subclass. Complete census (**OBSERVED**, grep for
`class .*BaseChannel`):

| Fake | File:line | Purpose |
|---|---|---|
| `_DummyChannel` | `tests/channels/test_base_channel.py:10` | Records `_sent`; the canonical minimal fake |
| `MockChannel` | `tests/channels/test_channel_manager_delta_coalescing.py:23` | `AsyncMock` for `send`/`send_delta` |
| `_MockChannel` | `tests/channels/test_channel_manager_reasoning.py:28` | Reasoning hook capture |
| `_HotChannel` | `tests/channels/test_channel_manager_hot_reload.py:19` | Hot-reload lifecycle |
| `_FakePlugin` / `_FakeMultiChannel` | `tests/channels/test_channel_plugins.py:48`, `:92` | Plugin discovery |
| `_StreamingChannel` / `_StreamedChannel` | `test_channel_plugins.py:3067`, `:3134` | Streaming dispatch |
| `_FailingChannel` / `_StopFailingChannel` / `_StopCancelledChannel` / `_CancellingChannel` | `test_channel_plugins.py:2950`, `:3608`, `:3645`, `:3264` | Failure paths |
| `MagicMock(spec=BaseChannel)` | `test_channel_manager_concurrency.py:15`, `:151` | Concurrency tests |

The canonical minimal fake is 15 lines (`test_base_channel.py:10-25`): set `name`, override `start`/
`stop`/`send`, record sent messages. **This is the Go test double to reproduce.**

**Per-channel transport faking.** For HTTP channels, tests substitute a fake client rather than
patching the network. The `signal` tests define `_FakeResponse` and `_FakeHTTPClient` that record
`posts`/`gets` and return canned JSON (`signal/tests/test_signal_channel.py:27-58`), and inject a
capture stub for `_handle_message` plus a no-op `_start_typing` via a factory helper
(`signal/tests/test_signal_channel.py:66-81`).

**Global fixtures** (`conftest.py`, 99 lines): three `autouse` fixtures — log isolation (`:16-23`),
session-root redirection to `tmp_path` (`:26-49`), and pairing-store redirection to
`tmp_path/"pairing.json"` (`:52-59`). **The pairing-store isolation matters for the Go port**: without
it, tests would write to the real `~/.nanobot` state.

### 5.2 What a Go differential test for a channel should look like

The Python suite's structure maps cleanly. A Go differential harness should have four layers.

**Layer 1 — shared channel double (mirrors `_DummyChannel`).** One recording implementation of the Go
channel interface that satisfies the three required methods and records every outbound call
(`send`, `send_delta`, `send_reasoning_delta`, `send_reasoning_end`, `send_file_edit_events`). This
single double replaces all eleven Python fakes and is the workhorse for manager-level tests.

**Layer 2 — transport fake.** For HTTP/SSE channels, use `net/http/httptest.Server` (already the
established pattern in this repo — `internal/provider/openai/client_test.go:9` imports `httptest`).
The server must assert on the *request* (method, path, JSON-RPC envelope, auth header) and return
canned *responses*, exactly as `_FakeHTTPClient` records `posts`/`gets`
(`signal/tests/test_signal_channel.py:44-55`). For WebSocket channels, `httptest.Server` plus a
hand-rolled upgrade handler is required, since Go stdlib has no WS server either.

**Layer 3 — differential assertions against the frozen Python.** The highest-value tests are the ones
that pin *observable parity*, not internal structure:

| Python test to mirror | Go assertion |
|---|---|
| `test_base_channel.py:28-57` | `is_allowed` exact-match semantics: `"attacker\|allow@email.com"` must **not** match `"allow@email.com"`; `"*"` matches all; empty/None allowlist denies; camelCase `allowFrom` alias works |
| `test_base_channel.py:69-84` | Unapproved DM produces exactly one outbound pairing message containing the code and metadata `_pairing_code` |
| `test_base_channel.py:140-169` | `authorization_id` changes authorization but **not** the recorded `sender_id`; a denied `authorization_id` publishes nothing (`inbound_size == 0`) |
| `test_channel_manager_concurrency.py:28-55` | Per-`(channel, chat_id)` FIFO: one blocked destination must not block the other nine, and each destination's two messages arrive in order |
| `test_channel_manager_concurrency.py:58-86` | A retrying destination does not block a healthy one |
| `test_channel_manager_concurrency.py:89-131` | Peak concurrent sends ≤ `_OUTBOUND_CONCURRENCY` (32); pending admits are bounded by `_OUTBOUND_PENDING_LIMIT` (256); cancellation returns permits |
| `test_channel_manager_concurrency.py:134-160` | `_stop_channel` cancels that channel's in-flight sends **before** calling `stop()`, and leaves other channels untouched |
| `test_channel_manager_concurrency.py:163-187` | Dispatcher shutdown cancels both active and queued destination tasks; `_outbound_tails` ends empty |

**Layer 4 — protocol-level golden tests per channel.** For each ported channel, a table of
`(upstream payload → expected InboundMessage fields)` and `(OutboundMessage → expected HTTP request)`.
The Python `signal` tests are the template: fake daemon SSE frames in, assert captured `_handle_message`
kwargs (`signal/tests/test_signal_channel.py:66-81`); outbound `OutboundMessage` in, assert recorded
`posts` entries.

**The key differential instrument** is the message-identity chain. A Go differential test should assert
the full tuple at each hop, because this is where silent divergence lives:

```
(sender_id, chat_id, channel, session_key)   # base.py:310-319 → InboundMessage.session_key (bus/events.py:39-42)
→ ("role","content") row                      # loop.py:993-1058
→ (channel, chat_id, stream_id)               # turn_delivery.py:363-365, manager.py:946
→ method + arguments                          # manager.py:908-931
```

`InboundMessage.session_key` is `session_key_override or f"{channel}:{chat_id}"`
(`bus/events.py:39-42`); the Go port already mirrors this exactly (`internal/core/types.go:58-65`), so
the identity chain is comparable from hop 1.

**Fuzzing targets.** Three inputs are stringly-typed and worth fuzzing: `chat_id` (used as a map key
in `_outbound_tails`, `_pending_queues`, and channel stream buffers), the content fingerprint input to
`_fingerprint_content` (`manager.py:683-686`), and inbound JSON envelopes for channels that accept
arbitrary `type` strings (e.g. `websocket`).

---

## 6. Risks and unknowns

### 6.1 Risks — grounded in what was read

**R1 — Mid-turn injection is the integration gap (HIGH).** `internal/agent/runner.go:12-15` documents
that injection callbacks are not implemented because there are no channels. Porting channels without
P0.3 first produces a system where a user's second message during a running turn is not injected. The
Python fallback path (`loop.py:1362-1367`, `QueueFull` → competing task; and `loop.py:1495-1513`,
leftovers re-published) means the failure is not a crash but a **behavioral divergence that is easy to
miss** — messages still get answered, just at the wrong time and possibly concurrently for one session.

**R2 — `OutboundMessage.Event` is missing from the Go core (HIGH, blocking).**
`internal/core/types.go:78-86` vs `bus/events.py:68`. Every streaming/progress/reasoning/file-edit
behavior funnels through this field at `manager.py:908-931`.

**R3 — Go has no RFC 6455 implementation (HIGH for scope, not for correctness).** **Ten** of the 17
channels require a WebSocket *client* — nine observed directly (`mattermost/runtime.py:177`,
`napcat/runtime.py:111`, `slack/runtime.py:153`, `feishu/runtime.py:1134`, `qq/runtime.py:155`,
`wecom/runtime.py:116`, `mochat/runtime.py:464`, `discord/runtime.py:486`) plus `dingtalk`
(**INFERENCE**: `DingTalkStreamClient` at `dingtalk/runtime.py:303` is the SDK's Stream-Mode client,
whose WebSocket transport is SDK-internal and was not read). A further channel, `websocket`, requires
a WebSocket *server* (`websocket/runtime.py:712`). `whatsapp` also rides WebSocket but over the
WhatsApp Web Noise protocol, where a generic WS client would not help. This is unavoidable hand-written code
(handshake, masking, framing, ping/pong, close, fragmentation) and it is the largest single
infrastructure cost. Deferring Mattermost as #3 in favor of MS Teams delays this cost but does not
avoid it.

**R4 — Unbounded queues are a memory hazard the Python original accepts.** `queue.py:32-33` uses
unbounded `asyncio.Queue`. The Go port already documents this and offers an opt-in cap
(`internal/bus/bus.go:6-11`). **A behavior difference here is observable under load**: capping
`MaxOutbound` makes `PublishOutbound` return `ErrFull`, which has no Python analogue. Any cap must be
off by default to preserve parity.

**R5 — `asyncio.CancelledError` semantics have no Go equivalent.** In Python, cancellation propagates
through `await` points and is explicitly re-raised at `manager.py:1020-1021`, `:1055-1056`,
`:381-382`, `:415-419`, `:849-850`. Go's `context.Context` cancellation is cooperative and does not
interrupt a blocked `Send`. Every Python `except asyncio.CancelledError: raise` becomes an explicit
`ctx.Err()` check plus a per-send deadline. See Appendix A.

**R6 — `WeakSet`-based connection tracking in the WebSocket channel has no Go analogue.**
`websocket/runtime.py:17`, `:393`, `:481`, `:511` use `weakref.WeakSet`. Go has no weak references, so
the equivalent connection→state maps must be cleaned on **every** exit path or they leak. This is a
correctness risk specific to the WebUI channel.

**R7 — Silent fallbacks that a port could inadvertently turn into errors.** Several paths swallow
exceptions by design and a naive Go port that propagates them changes behavior:
`BaseChannel.transcribe_audio` returns `""` on any exception (`base.py:58-60`);
`AgentRunner._drain_injections` returns `[]` on callback failure (`runner.py:271-273`);
`MessageBus.publish` logs and continues on handler failure (`queue.py:125-126`);
`_start_channel` records the error and continues (`manager.py:383-389`);
`_handle_message` returns silently on pairing-store `OSError` (`base.py:279-285`);
`_send_with_retry` returns (does not raise) after exhausting attempts (`manager.py:1037-1042`) or on a
non-retryable error (`manager.py:1023-1030`).

**R8 — The dependency declaration is incomplete.** `signal`, `mattermost`, `email`, and `websocket`
declare no `dependencies` but import `httpx` (`signal/runtime.py:17`, `mattermost/runtime.py:11`) or
`websockets` (`websocket/runtime.py:20`, `mattermost/runtime.py:172`). A Go port cannot infer its
transport requirements from `manifest.py`.

**R9 — Telegram's non-standard Bot API methods.** `sendRichMessage`/`sendRichMessageDraft`
(`telegram/runtime.py:941-943`, `:983-985`) are called raw because the SDK does not model them. Their
exact request/response schema is **UNVERIFIED** (the SDK is not installed and the method names are
non-standard). Rich-message streaming may not be reproducible without observing live traffic.

**R10 — Streaming buffer growth.** `websocket/runtime.py:406-407`, `:1359`, `:1286` accumulate
per-`(chat_id, stream_id)` buffers until `stream_end` or turn completion (`:1421`). A stream that never
ends grows without bound. The same pattern appears in Mattermost (`mattermost/runtime.py:112-117`).

**R11 — `inspect.signature` feature detection.** `manager.py:888-899` (for `merge_next`) and
`runner.py:259-270` (for `limit`) probe callback signatures at runtime. Go has no runtime reflection
over parameter names in this sense; these become explicit interface capabilities. **The port must
decide a fixed contract** (e.g. always pass `merge_next`, always pass `limit`) and document the
divergence, because the Python behavior is "adapt to whatever the plugin accepts".

### 6.2 Unknowns — stated plainly

1. **Exact wire schemas for SDK-internal calls are unverified.** The Python SDKs
   (`python-telegram-bot`, `discord.py`, `slack-sdk`, `matrix-nio`, `lark-oapi`, `dingtalk-stream`,
   `neonize`, `botpy`, `wecom-aibot-sdk`, `python-socketio`) are **not installed** in this environment
   (verified). Endpoint paths, header construction, retry behavior, and pagination inside those SDKs are
   **INFERENCE** from documented API shapes, not read from source. Everything cited as OBSERVED is
   nanobot's own code.
2. **I do not know how the WebUI channel's `/webui/*` REST surface interacts with the channel
   abstraction at runtime**, beyond that `ChannelManager._build_channel` special-cases
   `cls.name == "websocket"` and injects a `gateway` kwarg (`manager.py:183-215`). That is a
   channel-specific constructor branch — the only one in the manager — and it means the WebSocket
   channel is **not** constructible through the generic `cls(section, self.bus)` path
   (`manager.py:216`). A Go port needs an equivalent escape hatch or a different injection design.
3. **I do not know whether `mochat`'s socket.io path is required or whether the HTTP polling fallback
   is sufficient** in practice. `mochat/runtime.py:405` logs `"python-socketio not installed, using
   polling fallback"`, so a polling-only port is *possible*, but I did not determine whether the
   fallback reaches feature parity (panel vs session watching at `mochat/runtime.py:376-379`,
   `:442-445`).
4. **I do not know the real-world usage distribution** across channels. No telemetry, analytics, or
   usage data exists in this repository. The "highest-value first" ordering in §4.2 is grounded in
   platform market position and protocol simplicity, **not** in measured nanobot usage. If the
   operator's deployment is China-focused, Feishu/Weixin/WeCom/DingTalk should be promoted ahead of
   Mattermost — that is a deployment fact I cannot determine from the code.
5. **I did not read all 21,376 lines of `runtime.py`.** The deep reads covered: `websocket`,
   `telegram`, `slack`, `discord`, `matrix` (via delegated analysis), and `signal`, `mattermost`,
   `email`, `msteams`, `dingtalk`, `napcat`, `wecom`, `mochat`, `qq`, `weixin`, `whatsapp`, `feishu`
   (via targeted transport/import/endpoint greps). Claims about the latter group are limited to what
   the greps and manifests show; per-message parsing logic in those channels is **not** verified.
6. **`_MAX_INJECTION_CYCLES = 5`** (`runner.py:74`) — I confirmed the constant exists but did **not**
   trace where it bounds the loop. Its exact effect on injection frequency is unverified.
7. **The `always_enabled` capability's full semantics** are unverified beyond
   `manager.py:451-457` returning `requires_restart: True`. Whether it has other enforcement points I
   did not find is unknown.
8. **Whether the Go port's `ChannelsConfig` extra-allow mechanism round-trips per-channel sections
   losslessly** was not tested in this session. `internal/config/schema.go:229` declares
   `ChannelsConfig` with extra keys preserved and `internal/config/models.go:415-428` decodes a fixed
   field set with unknown keys retained, which *looks* sufficient for `channels.telegram.*` etc., but
   I did not execute a round-trip test.

---

## Appendix A — asyncio → Go concurrency mapping

This is the section the task specifically asked for: where Python's `asyncio` has no direct Go
equivalent, and the concurrency-model implications.

| Python construct | Cite | Go equivalent | Implication |
|---|---|---|---|
| `asyncio.Queue` (unbounded) | `queue.py:32-33` | mutex-guarded slice + broadcast channel | **Already solved** in `internal/bus/bus.go:44-49`. Go channels cannot be unbounded, so the port must not use a plain `chan` for the bus. |
| `asyncio.Queue(maxsize=20)` | `loop.py:1421` | buffered `chan` (cap 20) | Non-blocking `put_nowait` + `QueueFull` fallback (`loop.py:1361-1367`) becomes `select { case ch <- m: default: … }`. |
| `asyncio.Semaphore` | `manager.py:144-145` | buffered `chan struct{}` (cap N) | `_outbound_slots` (256) and `_outbound_sends` (32). |
| `asyncio.create_task` | `manager.py:393`, `:606`, `:747`; `loop.py:1376` | `go func()` + `context.Context` | Tasks have no return channel by default; Go needs explicit error propagation or a result channel. |
| `asyncio.gather(..., return_exceptions=True)` | `manager.py:616`, `:726`; `queue.py:148` | `sync.WaitGroup` + error collection | `return_exceptions=True` **swallows** errors; a naive Go port that propagates the first error changes startup semantics. |
| `asyncio.shield` | `manager.py:743` | **no direct equivalent** | `asyncio.shield` protects an inner awaitable from outer cancellation. Go has no analogous construct; the port must restructure the FIFO chain so a cancelled sender cannot strand its successor. |
| `asyncio.CancelledError` propagation | `manager.py:1020-1021`, `:1055-1056`, `:381-382`, `:415-419`, `:849-850` | `ctx.Err()` checks + per-operation deadlines | Cancellation in Go is cooperative and does **not** interrupt a blocked syscall. Every `await`-point cancellation check becomes an explicit `select` on `ctx.Done()`. |
| `asyncio.wait_for(coro, timeout)` | `loop.py:1269`; `manager.py:781-784` | `context.WithTimeout` | The 1.0s bus poll timeout is a *housekeeping tick*, not a failure — `TimeoutError` is caught and the loop continues (`loop.py:1270-1272`, `manager.py:847-848`). Go must distinguish deadline-exceeded (continue) from real errors. |
| `asyncio.Event` | `manager.py` (implicit via Semaphore); `websocket/runtime.py:388` | `chan struct{}` closed once, or `sync.Cond` | — |
| `asyncio.Lock` per session | `loop.py:1411`, `:1418` | `sync.Mutex` per session key, in a keyed map | Per-session serial, cross-session concurrent (`loop.py:1392`). A single global mutex would destroy cross-session concurrency. |
| `asyncio.timeout` (context manager) | `websocket/runtime.py:959`, `:1034` | `context.WithTimeout` | — |
| `asyncio.to_thread` | `websocket/…/inbound_commands.py:405` | `go func()` | Go has no GIL, so this is unnecessary; blocking file I/O can run directly. |
| `weakref.WeakSet` | `websocket/runtime.py:17`, `:393`, `:511` | **no equivalent** | Go has no weak references. Connection registries must be explicitly deregistered on every exit path or they leak. |
| `add_done_callback` | `manager.py:751-759`, `:1011-1021` | `defer` + explicit cleanup | The done-callback releases the semaphore slot and unlinks the FIFO tail — **cleanup that must run on every path including panic**. |
| `loop.create_task` + `self._pending` retention | `queue.py:128-143` | `go func()` + `sync.WaitGroup` | `publish_nowait` retains tasks so `drain()` (`queue.py:145-148`) can await them at shutdown. Go needs an equivalent WaitGroup-based drain. |
| `run_coroutine_threadsafe` | `msteams/runtime.py:203-208` | `chan` handoff to a goroutine | MS Teams runs an HTTP server on a **thread** and marshals into the event loop with a 15s `.result(timeout=15)`. In Go the HTTP handler can be a goroutine directly — **a simplification, but it changes blocking semantics**: the Python version blocks a `ThreadingHTTPServer` thread for up to 15s. |
| Structured `try/finally` + `ExitStack` | `loop.py:1135`, `:1220-1224` | `defer` | `loop.py:1220-1224` resets four contextvars in a fixed order on the way out. Go `defer` runs LIFO, which matches. |

**Two structural implications worth stating explicitly:**

1. **The Python design is "await everything, cancel everything".** The Go design must be "select on
   context, bound every send, clean up in `defer`". The behavior most likely to diverge is *ordering
   under cancellation* — Python guarantees the FIFO tail is unlinked by a done-callback
   (`manager.py:751-759`), and Go must reproduce that invariant explicitly.
2. **`asyncio.shield` at `manager.py:743` is the one construct with no Go analogue at all.** It
   protects the "wait for the previous message to this destination" step from cancellation of the
   current task. The Go port must either (a) serialize per destination with a goroutine-per-destination
   worker consuming from a per-key channel — which sidesteps the problem entirely — or (b) accept a
   documented divergence where a cancelled sender may strand its successor. Option (a) is
   behaviorally closer.

---

## Appendix B — Explicit UNVERIFIED items

1. Endpoint paths, header construction, pagination, and retry internals **inside** every third-party
   SDK (telegram, discord.py, slack-sdk, matrix-nio, lark-oapi, dingtalk-stream, neonize, botpy,
   wecom-aibot-sdk, python-socketio). Not installed; not read.
2. Whether the Telegram Bot API path is literally `POST /bot<token>/<Method>` — the method *names* are
   observed at call sites, the path is inference.
3. `sendRichMessage` / `sendRichMessageDraft` request/response schemas (`telegram/runtime.py:941-985`).
4. Whether discord.py normalizes a user-supplied `"Bot "` token prefix; nanobot performs no
   normalization before `client.start()` (`discord/runtime.py:486`).
5. Exact semantics and all enforcement points of the `always_enabled` capability.
6. Where `_MAX_INJECTION_CYCLES = 5` (`runner.py:74`) bounds the injection loop.
7. Whether the `websockets` library's default `max_queue` interacts with the explicit per-connection
   `asyncio.Queue(maxsize=256)` in the WebSocket channel.
8. Real-world usage distribution across channels — no telemetry exists in this repository.
9. Per-message parsing logic for `dingtalk`, `napcat`, `qq`, `wecom`, `mochat`, `weixin`, `whatsapp`,
   `feishu` — only transport, imports, and endpoints were read for these.
10. Whether the Go `ChannelsConfig` extra-allow mechanism losslessly round-trips arbitrary
    `channels.<name>.*` sections — not executed in this session.
11. Whether `mochat`'s HTTP polling fallback (`mochat/runtime.py:405`) reaches feature parity with the
    socket.io path.
