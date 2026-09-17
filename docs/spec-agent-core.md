# Behavioral Specification — nanobot Agent Core (turn loop + provider layer)

**Purpose.** Precise behavioral specification of the nanobot agent turn loop and provider layer,
sufficient to reimplement it in Go with behavioral fidelity. **No Go code is specified here.**

**Reference source (read-only, frozen):**

- Repository root: `/run/media/adriano/e681b5ac-a4fb-44d4-aebf-9d6584065787/dsh-projetos/nanobot/upstream/nanobot`
- Commit: `1bb712d3488915ca4ed9ccc1a93067ff722f5ab9` (verified via `git log -1`, clean working tree)
- Commit subject: `ci: use baseline Bun directly for Windows TUI builds`

**Citation convention.** All citations are `path:line` relative to that reference root, e.g.
`nanobot/agent/runner.py:435`. Line numbers are from the frozen commit. Every behavioral claim below
carries a citation. Anything I could not verify from source is marked **UNVERIFIED** explicitly.

**Evidence classes used in this document.**

- **OBSERVED** — read directly in source at the cited line.
- **INFERENCE** — follows logically from observed code plus third-party library semantics not
  present in this repository (e.g. OpenAI SDK header behavior).
- **UNVERIFIED** — not determinable from the files read.

**Reading note.** In this session's tool output, lines that assign to a variable whose name contains
`token` are replaced by an opaque placeholder by an output filter. Those lines were re-read with
`od -c` and the true source is quoted in this document where relevant. This affects
`nanobot/agent/loop.py:824,836,1132,1133,1134`, `nanobot/agent/runner.py:312`,
`nanobot/providers/base.py:1041,1042,1057,1089,1090`, and
`nanobot/providers/openai_compat_provider.py:605`.

---

## 0. Scope and module map

| Layer | File | Role |
|---|---|---|
| Bus | `nanobot/bus/queue.py`, `nanobot/bus/events.py`, `nanobot/bus/outbound_events.py` | inbound/outbound queues, message dataclasses, typed events |
| Loop | `nanobot/agent/loop.py` | consume bus, session resolution, commands, stage pipeline, persistence, cancellation |
| Runner | `nanobot/agent/runner.py` | LLM/tool iteration loop, usage, stop reasons |
| Tools | `nanobot/agent/tools/base.py`, `registry.py`, `execution.py`, `schema.py` | tool ABC, schema, dispatch, parallel batching |
| Provider ABC | `nanobot/providers/base.py` | dataclasses, retry policy, streaming fallback |
| Provider impl | `nanobot/providers/openai_compat_provider.py` | OpenAI-compatible wire protocol |
| Provider factory | `nanobot/providers/factory.py` | provider construction + fallback wrapping |
| Fallback | `nanobot/providers/fallback_provider.py` | failover + circuit breaker |
| Delivery | `nanobot/agent/turn_delivery.py` | routing, outbound publication, stream segmenting |

Key numeric defaults: `max_tool_iterations=200`, `max_tool_result_chars=16000`,
`max_concurrent_subagents=4`, `provider_retry_mode="standard"` (`nanobot/config/schema.py:129-132`).

---

## 1. MESSAGE BUS

### 1.1 Message dataclasses

**`InboundMessage`** — `nanobot/bus/events.py:24-49`. Dataclass fields, in declaration order:

| # | Field | Type | Default | Line |
|---|---|---|---|---|
| 1 | `channel` | `str` | required | `events.py:28` |
| 2 | `sender_id` | `str` | required | `events.py:29` |
| 3 | `chat_id` | `str` | required | `events.py:30` |
| 4 | `content` | `str` | required | `events.py:31` |
| 5 | `timestamp` | `datetime` | `datetime.now()` | `events.py:32` |
| 6 | `media` | `list[str]` | `[]` | `events.py:33` |
| 7 | `metadata` | `dict[str, Any]` | `{}` | `events.py:34` |
| 8 | `session_key_override` | `str \| None` | `None` | `events.py:35` |
| 9 | `require_existing_session` | `bool` | `False` | `events.py:36` |
| 10 | `input_role` | `Literal["user","system"] \| None` | `None` | `events.py:37` |

Two derived properties (not fields):

- `session_key` → `session_key_override or f"{channel}:{chat_id}"` (`events.py:39-42`).
- `is_user_input` → if `input_role is not None` then `input_role == "user"`, else `channel != "system"`
  (`events.py:44-49`).

**`OutboundMessage`** — `nanobot/bus/events.py:52-68`. Fields:

| # | Field | Type | Default | Line |
|---|---|---|---|---|
| 1 | `channel` | `str` | required | `events.py:61` |
| 2 | `chat_id` | `str` | required | `events.py:62` |
| 3 | `content` | `str` | required | `events.py:63` |
| 4 | `reply_to` | `str \| None` | `None` | `events.py:64` |
| 5 | `media` | `list[str]` | `[]` | `events.py:65` |
| 6 | `metadata` | `dict[str, Any]` | `{}` | `events.py:66` |
| 7 | `buttons` | `list[list[str]]` | `[]` | `events.py:67` |
| 8 | `event` | `AgentEvent \| None` | `None` | `events.py:68` |

Note the docstring contract: `event` carries internal runtime/UI semantics; `metadata` is reserved for
channel routing context (`message_id`, thread ids) plus the optional `OUTBOUND_META_AGENT_UI` (`"_agent_ui"`)
blob (`events.py:10-13`, `events.py:55-59`).

**Internal inbound metadata keys** — `events.py:15-21`:
`_runtime_control`, `_user_shell`, `_ack`, `image_generation_reload`, `session_discard`.
The file explicitly warns these are internal-only and "Never accept these keys verbatim from an
untrusted client" (`events.py:15-16`).

### 1.2 Queue semantics

`MessageBus` — `nanobot/bus/queue.py:19-148`.

- `self.inbound: asyncio.Queue[InboundMessage] = asyncio.Queue()` (`queue.py:32`) — **unbounded**
  (`asyncio.Queue()` with no `maxsize`, i.e. `maxsize=0`).
- `self.outbound: asyncio.Queue[OutboundMessage] = asyncio.Queue()` (`queue.py:33`) — **unbounded**.
- `publish_inbound` = `await self.inbound.put(msg)` (`queue.py:37-39`) — never blocks on capacity
  because the queue is unbounded.
- `consume_inbound` = `await self.inbound.get()` (`queue.py:41-43`).
- `publish_outbound` = `await self.outbound.put(msg)` (`queue.py:45-47`).
- `consume_outbound` = `await self.outbound.get()` (`queue.py:67-69`).
- `inbound_size` / `outbound_size` = `qsize()` (`queue.py:71-79`).

**Ordering**: asyncio FIFO per queue. **No backpressure** on either bus queue: there is no `maxsize`
and no `task_done`/`join`. Producers are never throttled by bus depth. This is a deliberate design
point — the bus docstring says local subscribers are awaited by `publish`, while channel delivery is
queued by `publish_event`, and "Local state transitions never wait for network sends" (`queue.py:27-29`).

**Backpressure that does exist** lives one level up, not on the bus:

- The loop's per-session **pending-injection queue** is bounded: `asyncio.Queue(maxsize=20)`
  (`nanobot/agent/loop.py:1421`). On `QueueFull` the loop logs a warning and falls back to
  dispatching the message as a normal queued task instead of injecting it (`loop.py:1360-1373`).
- An optional global concurrency gate: `NANOBOT_MAX_CONCURRENT_REQUESTS` env var; if unset or `<= 0`
  there is **no** gate (`loop.py:428-432`).

### 1.3 Subscriber model (local events)

`subscribe(handler, event_type=None)` appends an entry to `self._handlers` and returns an idempotent
disconnect closure that flips an `active` flag and removes the entry (`queue.py:92-116`). Handlers are
filtered by `isinstance(event, event_type)` when a type is given (`queue.py:104-107`).

- `publish(event)` awaits each handler **in registration order**; handler exceptions are caught and
  logged (`logger.exception("event handler failed for {}", ...)`) and do **not** propagate
  (`queue.py:118-126`).
- `publish_nowait(event)` schedules `publish` as a task on the running loop, keeps it in
  `self._pending`, and discards it on completion; if there is no running loop it logs debug and
  returns `None` — **the event is dropped** (`queue.py:128-143`). The docstring explicitly says this
  is not a global event FIFO (`queue.py:133`).
- `drain()` gathers all pending dispatch tasks (`queue.py:145-148`).
- `publish_event(event, channel, chat_id, metadata)` wraps the event in an `OutboundMessage` via
  `outbound_message_for_event` and puts it on the outbound queue (`queue.py:49-65`).

### 1.4 How channels connect to the loop

**Inbound**: channels call `MessageBus.publish_inbound` from `Channel.publish_inbound`
(`nanobot/channels/base.py:305-321`). That helper sets `metadata["_wants_stream"] = True` when the
channel declares `supports_streaming` (`base.py:307-309`), and passes through
`session_key_override` and `require_existing_session` (`base.py:317-318`).

**Outbound**: the channel manager runs a consumer loop over `self.bus.consume_outbound()` with a
1-second `asyncio.wait_for` timeout (`nanobot/channels/manager.py:775-782`), then routes by
`msg.channel` and dispatches typed events (e.g. reasoning deltas are only delivered to channels whose
`show_reasoning` is set — `manager.py:784-800`).

**Loop side**: `AgentLoop.run()` polls `self.bus.consume_inbound()` with a 1.0 s timeout so it can
periodically run idle-session compaction checks (`nanobot/agent/loop.py:1267-1272`).

**Streaming opt-in path**: `_wants_stream` in the delivery message metadata is what enables stream
segmentation — `TurnDelivery.__post_init__` sets `_stream_base_id` only when
`enable_stream and delivery_message.metadata.get("_wants_stream")` (`nanobot/agent/turn_delivery.py:210-211`).

---

## 2. AGENT LOOP — TURN LIFECYCLE

`AgentLoop` is declared at `nanobot/agent/loop.py:196`. `TurnContext` (the per-turn mutable state bag)
is at `loop.py:132-193`.

### 2.1 Inbound consumption and admission (`AgentLoop.run`, `loop.py:1261-1379`)

1. `self._running = True` (`loop.py:1263`).
2. Loop: `await asyncio.wait_for(self.bus.consume_inbound(), timeout=1.0)` (`loop.py:1269`).
   - `asyncio.TimeoutError` → `self._check_expired_sessions_if_due()` and continue (`loop.py:1270-1272`).
   - `asyncio.CancelledError` → re-raise if `not self._running or task_is_cancelling()`; otherwise log
     "Ignoring leaked CancelledError" and continue (`loop.py:1273-1281`).
   - Any other exception → log warning and continue (`loop.py:1282-1284`).
3. `raw = msg.content.strip()`; `effective_key = self._effective_session_key(msg)` (`loop.py:1286-1287`).
4. **Runtime-control interception**: `await agent_context.handle_runtime_control(self, msg, self.tools)`;
   if true, `continue` (`loop.py:1288-1289`). That handler performs session discard for
   `_runtime_control == session_discard` and image-generation reload
   (`nanobot/agent/context.py:44-48`).
5. **Session-existence filter**: if `msg.require_existing_session` and
   `self.sessions.get_cached(effective_key) is None` → drop the message silently (`loop.py:1290-1294`).
6. If `msg.is_user_input` → `await self.runtime_event_publisher.user_input_accepted(msg, effective_key)`
   (`loop.py:1295-1296`).
7. **Priority slash commands** (channel != "system" and `self.commands.is_priority(raw)`) are
   dispatched inline **before** the session lock: `_dispatch_command_inline(msg, effective_key, raw,
   self.commands.dispatch_priority)` then `continue` (`loop.py:1297-1302`).
8. **Automation deferral**: cron / local-trigger coordinators may defer the turn if the session is
   already active (`loop.py:1303-1318`).
9. **Unified-session rewrite**: if `effective_key != msg.session_key`, the message is replaced with
   `session_key_override=effective_key` (`loop.py:1319-1324`).
10. **Recovery admission** for websocket messages when the session already has a pending queue
    (`loop.py:1329-1335`).
11. **Mid-turn injection routing**: if `effective_key in self._pending_queues`:
    - A non-priority *dispatchable* command is dispatched inline instead of queued (`loop.py:1340-1347`).
    - Otherwise `record_pending_followup(session, pending_msg)` mints a follow-up id, the message is
      re-published with `PENDING_FOLLOWUP_ID_KEY` in metadata, saved, and `put_nowait` into the bounded
      pending queue; on `QueueFull` it falls through to normal task dispatch (`loop.py:1348-1373`).
12. Otherwise: `task = asyncio.create_task(self._dispatch(msg))` and
    `self._track_active_task(effective_key, task)` (`loop.py:1376-1377`).
13. `finally: await self.aclose()` (`loop.py:1378-1379`).

**Concurrency model**: per-session serial, cross-session concurrent. `_dispatch` acquires a per-session
`asyncio.Lock` from a `WeakValueDictionary` (`loop.py:403-405`, `loop.py:2382-2388`) and an optional
global semaphore, via `async with lock, gate:` (`loop.py:1411-1418`). The comment at `loop.py:1392`
states the intent verbatim: "Process a message: per-session serial, cross-session concurrent."

### 2.2 `_dispatch` — task ownership and cancellation (`loop.py:1391-1530`)

1. Re-derive `session_key`; re-apply `session_key_override` if it differs (`loop.py:1393-1395`).
2. Recovery admission / task registration (`loop.py:1396-1410`).
3. Create an **unrouted** fallback delivery, then inside the lock create the real delivery with
   `enable_stream=True` (`loop.py:1414`, `loop.py:1424-1428`).
4. Publish the session's pending-injection queue: `pending = asyncio.Queue(maxsize=20)` and
   `self._pending_queues[session_key] = pending` (`loop.py:1421-1422`).
5. `response = await self._process_message(msg, pending_queue=pending, delivery=delivery)`
   (`loop.py:1429-1433`).
6. `continuing = turn_continuation.internal_continuation_pending(msg.metadata)`; then
   `await delivery.complete(response, publish_completion=not continuing)` (`loop.py:1434-1439`).
7. Notify automation coordinators of completion (`loop.py:1440-1441`).
8. **`asyncio.CancelledError` path** (`loop.py:1442-1479`):
   - notify coordinators with the error; log "Task cancelled for session {}";
   - `await delivery.abort_stream()` inside a try/except that only logs at debug (`loop.py:1446-1453`);
   - if the session is being discarded **or** `_preserve_inflight_turns_on_shutdown` is set → re-raise
     **without** materializing partial context (`loop.py:1458-1462`);
   - otherwise `restore_runtime_checkpoint(session)`, clear the pending-user-turn marker, save, log
     "Restored partial context for cancelled session {}", then re-raise (`loop.py:1463-1479`).
9. **Generic exception path** (`loop.py:1480-1488`): log exception, `await delivery.fail(...)`,
   notify coordinators. `delivery.fail` publishes
   `OutboundMessage(content="Sorry, I encountered an error.")` (`turn_delivery.py:336-344`).
10. **`finally`** (`loop.py:1489-1516`): pop the pending queue only if it is still ours; drain every
    leftover item back onto the bus with `publish_inbound` (so they are re-processed as fresh inbound
    messages, never silently lost); log the count; `await delivery.idle()` unless an internal
    continuation is pending; publish the next deferred automation turn.
11. Outer `except asyncio.CancelledError` (`loop.py:1517-1520`): if completion was not published and
    the raw text normalizes to `/compact`, publish a completion anyway.
12. Outer `finally` (`loop.py:1521-1530`): unregister the recovery task; if `pending is None`
    (i.e. we never entered the locked body) run `delivery.idle()` and publish the next deferred turn.

### 2.3 `_process_message` — the stage pipeline (`loop.py:1594-1687`)

Turn kind: `TurnKind.USER if msg.is_user_input else TurnKind.SYSTEM` (`loop.py:1613`).

- SYSTEM sessions derive their key from `chat_id.split(":", 1)` falling back to `("cli", chat_id)`
  (`loop.py:1614-1618`); USER sessions use `session_key or msg.session_key` (`loop.py:1619-1620`).
- `turn_id = f"{key}:{time.time_ns()}"` (`loop.py:1630`).
- `original_user_text` is `None` for SYSTEM turns and for internal continuations, else `msg.content`
  (`loop.py:1634-1639`).
- `streaming = on_stream is not None or delivery.streaming` (`loop.py:1650`).
- When streaming, the event sink is wrapped by `track_output`, which sets
  `ctx.streamed_content = True` if any non-empty `StreamDeltaEvent` arrived before the last
  `StreamEndEvent` (`loop.py:1663-1677`).

Then, in order, with per-stage timing logs (`loop.py:1689-1714`):

```
restore → compact → command → build → run → save → respond
```

(`loop.py:1679-1687`). If the `command` stage returns truthy, `_process_message` returns
`ctx.outbound` immediately and **BUILD/SAVE are skipped** (`loop.py:1681-1682`).

#### Stage RESTORE — `_restore_turn` (`loop.py:1748-1803`)

- For USER turns with media: `reference_non_image_attachments(content, media)` splits attachments;
  non-image attachments are turned into text references and only image paths remain in `media`
  (`loop.py:1752-1758`).
- Session resolution: `get_cached(key)` when `require_existing_session` (raising
  `RuntimeError("required session is not active")` if absent), else `get_or_create(key)`
  (`loop.py:1760-1766`).
- `ctx.ephemeral = ctx.ephemeral or not session.policy.persist` (`loop.py:1768`).
- **Tool restriction**: if `session.policy.disabled_tools` is non-empty, a fresh `ToolRegistry` is
  built containing only allowed tools (`loop.py:1770-1777`). `SessionPolicy` fields are
  `persist=True`, `log_content=True`, `disabled_tools=frozenset()`
  (`nanobot/session/manager.py:267-272`).
- Content logging is suppressed when `session.policy.log_content` is false (`loop.py:1781-1785`).
- `_remember_session_route` records the user-facing destination into session metadata unless the turn
  is not a user turn, the channel is `cli`/`system`, the sender is `subagent`, or the message carries
  automation metadata (`loop.py:917-937`, `loop.py:1787-1792`).
- `await ctx.delivery.started()` publishes the turn-started lifecycle event when the route publishes
  lifecycle (`turn_delivery.py:226-231`).
- `restore_runtime_checkpoint(session)` → save (`loop.py:1797-1798`);
  `restore_pending_interruption(session)` → save, but only when the message is not itself a recovery
  inbound (`loop.py:1799-1803`).

#### Stage COMPACT — `_compact_session` (`loop.py:1805-1811`)

`self.auto_compact.prepare_session(session, session_key)` returns `(session, pending_summary)`; the
pending summary is stored on `ctx.pending_summary` (`loop.py:1807-1811`).

#### Stage COMMAND — `_dispatch_command` (`loop.py:1813-1863`)

- Skipped for SYSTEM turns or `channel == "system"` (`loop.py:1814-1815`).
- Builds a `CommandContext(msg, session, key, raw, loop, runtime, is_user_turn, turn_scopes)` and calls
  `self.commands.dispatch(cmd_ctx)` (`loop.py:1825-1835`).
- `/compact` returns `True` with **no** outbound (it reports through events) (`loop.py:1836-1838`).
- If a result is returned: `ctx.outbound = result`; unless the command is `/new`, the user message is
  persisted early (`_persist_user_message_early(..., _command=True)`), an assistant message with
  `_command=True` is appended, the pending-user-turn marker is cleared, the session is saved, and a
  `session_turn_persisted` event is published when not ephemeral (`loop.py:1839-1862`).
- Returns `False` when no handler matched, so the turn continues to BUILD.

**Command router semantics** (`nanobot/command/router.py`):

- `normalize_command_text` strips a Telegram/Discord `@bot` suffix from the command word while
  preserving arguments (`router.py:23-39`).
- Three tiers, checked in order: priority (exact), exact, then longest-prefix-first (`router.py:57-65`).
- `is_dispatchable_command` returns `False` for priority commands, `True` for exact/prefix matches, and
  `cmd.startswith("/")` for anything else — so malformed slash commands are rejected by the router
  instead of reaching the LLM (`router.py:85-100`).
- Unmatched slash commands produce an `OutboundMessage` with a "Did you mean" suggestion via
  `difflib.get_close_matches(..., n=1, cutoff=0.6)` and `metadata["render_as"] = "text"`
  (`router.py:125-165`).

**Registered built-in commands** (`nanobot/command/builtin.py:1074-1102`):

- priority: `/stop`, `/restart`, `/status`
- exact: `/new`, `/compact`, `/status`, `/model`, `/history`, `/goal`, `/trigger`, `/dream`,
  `/dream-log`, `/dream-restore`, `/dream-prompt`, `/evaluator-prompt`, `/skill`, `/help`, `/pairing`,
  and `USER_SHELL_COMMAND`
- prefix (accept arguments): `/model `, `/history `, `/goal `, `/trigger `, `/dream-log `,
  `/dream-restore `, `/dream-prompt `, `/evaluator-prompt `, `/pairing `, `<USER_SHELL> `

`/stop` cancels active tasks, subagents and exec sessions, and drains the pending queue to avoid
mid-turn injection deadlock (`builtin.py:213-231`). `/compact` is special-cased in
`_dispatch_command_inline` to create a *new task* rather than dispatch inline, because compaction must
wait for the active turn to commit its session (`loop.py:788-794`). A user-shell command
(`INBOUND_META_USER_SHELL`) is run as a background task so the inbound consumer stays responsive
(`loop.py:806-808`).

#### Stage BUILD — `_build_turn` (`loop.py:1865-1971`)

1. Runtime resolution: `runtime = ctx.runtime or self.runtime_for_session(session)`
   (`loop.py:1866-1870`). `runtime_for_session` reads the session's model-preset metadata; a removed
   preset falls back to the default, logs a warning, drops the metadata key and saves
   (`loop.py:539-561`).
2. `ctx.on_runtime_admitted(runtime)` hook if set (`loop.py:1877-1878`).
3. Non-ephemeral turns re-run `auto_compact.prepare_session` (`loop.py:1879-1884`).
4. `ctx.history = session.get_history(extend_to_user=is_subagent)` (`loop.py:1887`).
   `Session.get_history` slices from `self.last_archived`, applies an optional count limit via
   `recent_message_start_index(..., extend_to_user=...)`, and shifts the start forward to avoid
   beginning mid-turn (`nanobot/session/manager.py:344-374`).
5. Provider conversation state staging (`loop.py:1888-1958`): if a stored state exists **and** the
   provider can resume it (`provider.can_resume_conversation_state(state, model)`), the fresh current
   message is built and appended as a *pending* message; subagent follow-ups de-duplicate by
   `subagent_task_id` via `_meta` (`loop.py:1923-1950`). If the provider cannot resume the state, the
   state is dropped (`loop.py:1957-1958`).
6. **Early user-message persistence**: `_persist_user_message_early(...)` (`loop.py:1959-1964`),
   which appends `{"role":"user", content, **extra}` via `session.add_message`, sets the
   `pending_user_turn` marker, acknowledges any pending follow-up id, and saves
   (`loop.py:677-718`). Media paths are persisted under `extra["media"]`; runtime-context blocks are
   appended into the text with `append_runtime_context` and recorded under `RUNTIME_CONTEXT_HISTORY_META`
   (`loop.py:690-711`).
7. `ctx.transcript_input = self._build_transcript_input(ctx)` (`loop.py:1971`), which captures
   `history`, `current_message`, `media` (USER turns only), `session_summary` and
   `runtime_context_blocks` into a `TranscriptInput`
   (`loop.py:720-729`; dataclass at `nanobot/agent/context.py:72-86`).

**Context assembly** happens inside the runner via `ContextBuilder.build_transcript`
(`nanobot/agent/context.py:276-308`): a `system` message built by `build_system_prompt`
(`context.py:101-152`) followed by `transcript.history`, followed by the fresh current message from
`build_current_message` (`context.py:310-332`). `build_user_content` converts image paths into
`{"type":"image_url","image_url":{"url":"data:<mime>;base64,<b64>"},"_meta":{"path": ...}}` blocks
prepended before the text block; non-image or unreadable paths are skipped
(`context.py:334-363`).

#### Stage RUN — `_run_turn` (`loop.py:1974-2012`)

- `ctx.visible_run_started_at` defaults to `time.time()`; `await ctx.delivery.running(started_at=...)`
  (`loop.py:1976-1978`).
- Runs inside `capture_message_deliveries()` so a tool that already sent a message to the same
  destination can suppress the duplicate final response (`loop.py:1980`, `loop.py:2003-2008`).
- Calls `_run_agent_loop(...)` with `runtime`, `streaming`, `session`, `pending_queue`, `ephemeral`,
  `hooks`, `hook_factories`, `turn_scopes`, `tools`, `request_context`, `provider_state`, `events`
  (`loop.py:1981-1996`).
- Copies result fields into the context: `final_content`, `all_messages`, `summary_checkpoint`,
  `provider_compaction_applied`, `stop_reason`, `failure_error_kind`; sets
  `ctx.suppress_response = True` when the same route already received a message **and**
  (`not result.had_injections or stop_reason == "empty_final_response"`) (`loop.py:1997-2008`).
- `ctx.usage = result.usage`; `ctx.delivery.record_usage(result.round_usages)`; then
  `turn_continuation.maybe_continue_turn(ctx)` for USER turns (`loop.py:2009-2012`).

`_run_agent_loop` (`loop.py:939-1247`) is the bridge to `AgentRunner`. It builds the `AgentRunSpec`
with: `max_iterations=self.max_iterations`, `max_tool_result_chars`, `transcript_input`,
`transcript_builder`, `hook`, **`concurrent_tools=True`** (hard-coded, `loop.py:1179`),
`workspace`, `session_key`, `provider_retry_mode`, `checkpoint_callback=_checkpoint`,
`consolidate_history`/`consolidate_provider_compaction` (only when a session exists and the turn is not
ephemeral), `injection_callback=_drain_pending`, `terminal_injection_callback=_wait_for_pending`,
`continuation_callback=_goal_continue`, `finalize_on_max_iterations`, `provider_state`,
`llm_usage_source`, `events` (`loop.py:1170-1219`).

Context vars bound around the run and reset in `finally`: file states, request context, workspace scope
(`loop.py:1132-1134` true source `bind_file_states(self._file_state_store.for_session(active_session_key))`,
`bind_request_context(request_ctx)`, `bind_workspace_scope(effective_scope)`; reset at
`loop.py:1220-1224`).

After the run: `session.provider_state = result.provider_state` unless ephemeral
(`loop.py:1225-1226`). On `stop_reason == "max_iterations"` a warning is logged and, when streaming
and allowed, the final content (or `pending_stream_content`) is pushed as `StreamDeltaEvent` +
`StreamEndEvent` so streaming channels update their card instead of leaving it empty
(`loop.py:1227-1244`). On `stop_reason == "error"` the error is logged (`loop.py:1245-1246`).

**Injection callbacks**: `_drain_pending` drains only already-available items up to
`_MAX_INJECTIONS_PER_TURN`, converting each `InboundMessage` into a `{"role":"user","content":...}`
row, resolving runtime context for user inputs, and tagging subagent results with
`HIDDEN_HISTORY_META` and `injected_event="subagent_result"` (`loop.py:984-1069`). `_wait_for_pending`
additionally blocks for a subagent result when the runner is ready to exit, bounded by
`_SUBAGENT_TERMINAL_WAIT_SECONDS = 300.0` (`loop.py:1073-1105`, constant at `loop.py:124`).

#### Stage SAVE — `_persist_turn` (`loop.py:2014-2061`)

- `turn_continuation.prepare_save_boundary(ctx)` (`loop.py:2016`).
- For USER turns with blank final content and no suppression:
  `ctx.final_content = EMPTY_FINAL_RESPONSE_MESSAGE` (`loop.py:2018-2023`).
- `ctx.turn_latency_ms` computed from `visible_run_started_at` for SYSTEM turns / internal
  continuations, else from `turn_wall_started_at` (`loop.py:2025-2034`).
- `session.metadata["_last_usage"] = ctx.usage.to_dict()` when usage exists and not ephemeral
  (`loop.py:2035-2036`).
- `self._save_turn(session, ctx.all_messages, ctx.save_skip, ...)` (`loop.py:2037-2042`).
- If provider compaction was applied and a summary checkpoint exists, `session.provider_state = None`
  so the next request rebuilds from the portable checkpoint (`loop.py:2043-2050`).
- `delivery.record_latency`, clear pending-user-turn marker, clear runtime checkpoint,
  `self.sessions.save(session)`, and publish `session_turn_persisted` unless ephemeral
  (`loop.py:2051-2061`).

**`_save_turn`** (`loop.py:2140-2272`) is the exact persistence filter and is the highest-fidelity
part of the loop:

- Computes `declared_tool_call_ids` from existing assistant `tool_calls` and `fulfilled_tool_call_ids`
  from existing `tool` messages (`loop.py:2151-2164`).
- Validates the summary checkpoint boundary against `[skip-1, len(messages)]`; an out-of-range boundary
  is ignored with a warning (`loop.py:2117-2138`).
- Iterates `messages[skip:]`:
  - inserts the summary checkpoint at the recorded boundary (`loop.py:2186-2187`), and handles the
    special case where the trigger input is already the session tail (`loop.py:2176-2181`);
  - strips `PENDING_FOLLOWUP_ID_KEY` and `_meta` from the persisted entry (`loop.py:2190-2209`);
  - **skips** assistant messages with no content and no tool calls — comment: "they poison session
    context" (`loop.py:2211-2212`);
  - for `role == "tool"`: drops the message if the `tool_call_id` is missing, not declared, or already
    fulfilled, logging "Dropping invalid tool result {} from session {}" (`loop.py:2213-2227`);
    list content is sanitized via `_sanitize_persisted_blocks`, and an empty result is replaced by
    `{"type":"text","text":"[tool result omitted during persistence]"}` to preserve the pair
    (`loop.py:2229-2239`);
  - for `role == "user"`: list content is sanitized; a fully-filtered user message is skipped; the
    runtime-context metadata is re-attached under `RUNTIME_CONTEXT_HISTORY_META` (`loop.py:2240-2249`);
  - `entry.setdefault("timestamp", datetime.now().isoformat())` (`loop.py:2250`);
  - updates declared tool-call ids and tracks the last assistant index (`loop.py:2254-2265`).
- `_sanitize_persisted_blocks` replaces `image_url` blocks whose URL starts with `data:image/` by a
  text placeholder derived from the block's `_meta.path` (`loop.py:2090-2115`).
- The last assistant message gets `latency_ms` (`loop.py:2268-2269`); saved follow-up ids are
  acknowledged; `session.updated_at = datetime.now()` (`loop.py:2270-2272`).

#### Stage RESPOND — `_prepare_outbound` (`loop.py:2063-2088`)

- `delivery.record_stop_reason(stop_reason, failure_error_kind=...)` (`loop.py:2064-2067`).
- If `ctx.suppress_response` → `ctx.outbound = None` (`loop.py:2068-2070`).
- SYSTEM turns use `delivery.background_response(...)`, whose default content is
  `"Background task completed."` when content is empty (`loop.py:2071-2078`;
  `turn_delivery.py:268-292`).
- USER turns use `_assemble_outbound(...)` (`loop.py:2079-2086`), which:
  - logs `"Response to {channel}:{sender_id}: {preview}"` (first 120 chars) or `[content hidden]`
    (`loop.py:1727-1731`);
  - sets `event = StreamedResponseEvent()` when content was streamed **and** the stop reason is not
    `error`/`tool_error` (`loop.py:1733-1736`);
  - adds `metadata["latency_ms"]` when known (`loop.py:1737-1738`);
  - returns `OutboundMessage(channel, chat_id, content=final_content, event=event, metadata=meta)`
    (`loop.py:1740-1746`).
- Ephemeral turns additionally set `metadata["_stop_reason"]` (`loop.py:2087-2088`).

### 2.4 Turn delivery, publication and stream segmenting

`TurnDelivery` (`nanobot/agent/turn_delivery.py:177-386`):

- Route defaults: non-system messages route to their own channel/chat with
  `publish_lifecycle=True`; system messages split `chat_id` on the first `:` into channel/chat and add
  a Slack `thread_ts` when the session key has a third segment (`turn_delivery.py:152-174`).
- `_bind_events` builds an `EventSink` that filters by `notification_is_deliverable` and publishes via
  `bus.publish_event` with a deep copy of the route metadata (`turn_delivery.py:44-62`).
- `complete(response, publish_completion)`:
  - publishes `response` to the outbound queue **unless** `response.channel == "websocket" and
    stop_reason == "error"` (`turn_delivery.py:305-309`);
  - when `response is None` and the lifecycle channel is `cli`, publishes an empty-content
    `OutboundMessage` as a sentinel (`turn_delivery.py:310-318`);
  - when `publish_completion`, emits a **`TurnCompleted`** runtime event (not `TurnEndEvent`)
    through `RuntimeEventPublisher.turn_completed`, which calls `bus.publish(...)` — i.e. **local
    subscribers only, never the outbound queue** — with `outcome`/`failure_kind` from
    `_turn_outcome(stop_reason)` (`turn_delivery.py:65-70`, `turn_delivery.py:319-333`,
    `nanobot/bus/runtime_events.py:61-75`, `nanobot/bus/runtime_events.py:259-288`). `latency_ms`,
    `runtime`, `usage` and `round_usages` are popped from per-session caches at emission time
    (`runtime_events.py:279-282`).
- `_turn_outcome`: `"error"` → `("failed","model")`, `"tool_error"` → `("failed","tool")`, else
  `("completed", None)` (`turn_delivery.py:65-70`).
- Stream segmenting (`turn_delivery.py:363-381`): each `StreamDeltaEvent`/`StreamEndEvent` is rewritten
  with `stream_id = f"{_stream_base_id}:{_stream_segment}"`; a `StreamEndEvent` with
  `merge_next=True` keeps the segment open, otherwise the segment counter increments. `abort_stream`
  emits a `StreamEndEvent` only when a stream is currently open (`turn_delivery.py:383-386`).

### 2.5 Cancellation summary

| Trigger | Mechanism | Observed behavior |
|---|---|---|
| `/stop` (priority) | `cmd_stop` → `loop._cancel_active_tasks(key)` | cancels + awaits all tracked tasks, cancels subagents, terminates exec sessions, drains the pending queue (`builtin.py:213-231`, `loop.py:873-885`) |
| `/restart` | `os.execv` (posix) or spawn | `builtin.py:234-253` |
| task cancellation | `_dispatch` `except asyncio.CancelledError` | restore runtime checkpoint and save, unless discarding or gateway shutdown (`loop.py:1442-1479`) |
| shutdown | `aclose()` under `_close_lock` | cancels all active tasks, gathers them, then closes subagents and exec sessions; aggregates errors into `BaseExceptionGroup` (`loop.py:1532-1581`) |
| gateway shutdown | `preserve_inflight_turns_on_shutdown()` | keeps the durable checkpoint intact so recovery can offer "Continue" (`loop.py:1381-1389`) |

---

## 3. AGENT RUNNER

`AgentRunner` at `nanobot/agent/runner.py:141`; `AgentRunSpec` at `runner.py:88-115`;
`AgentRunResult` at `runner.py:118-138`.

### 3.1 Constants

| Name | Value | Line |
|---|---|---|
| `_DEFAULT_ERROR_MESSAGE` | `"Sorry, I encountered an error calling the AI model."` | `runner.py:65` |
| `_ARREARAGE_ERROR_MESSAGE` | billing/quota text (see §9) | `runner.py:66-69` |
| `_PERSISTED_MODEL_ERROR_PLACEHOLDER` | `"[Assistant reply unavailable due to model error.]"` | `runner.py:70` |
| `_MAX_EMPTY_RETRIES` | `2` | `runner.py:71` |
| `_MAX_LENGTH_RECOVERIES` | `3` | `runner.py:72` |
| `_MAX_INJECTIONS_PER_TURN` | `3` | `runner.py:73` |
| `_MAX_INJECTION_CYCLES` | `5` | `runner.py:74` |

### 3.2 Entry point `run()` (`runner.py:308-360`)

1. `hook = spec.hook or AgentHook()` (`runner.py:309`).
2. `messages, compaction = self._initial_transcript_and_compaction(spec)` (`runner.py:310`).
   That helper raises `ValueError` if both `transcript_input` and `initial_messages` are provided, if
   `transcript_builder` is missing with `transcript_input`, if neither is provided, or if
   `consolidate_history` is set without `transcript_input` (`runner.py:362-384`).
3. `context = AgentRunHookContext(messages=deepcopy(messages))` (`runner.py:311`).
4. `llm_usage_source_token = bind_llm_usage_source(spec.llm_usage_source or
   source_from_session_key(spec.session_key))` (`runner.py:312-314`; true source verified by `od`).
5. `await hook.before_run(context)` then `result = await self._run_core(...)` (`runner.py:316-318`).
6. `asyncio.CancelledError` → set `stop_reason="cancelled"`, `error=None`, then **re-raise**
   (`runner.py:319-324`).
7. Any other exception → `stop_reason="error"`, `error=f"Error: {type(exc).__name__}: {exc}"`,
   `await hook.on_error(context)`, re-raise (`runner.py:325-331`).
8. Success → copy result fields into the hook context; if `context.error is not None` call
   `hook.on_error`; then `hook.after_run` (`runner.py:332-345`).
9. `finally` → `hook.on_finally(context)` always attempted (with an inner try/except that logs
   "AgentHook.on_finally error after {}"), then `reset_llm_usage_source(token)`
   (`runner.py:346-360`).

### 3.3 `_run_core` — the iteration loop (`runner.py:386-844`)

Local state initialized at `runner.py:393-433`: `final_content`, `tools_used`, `usage`,
`round_usages`, `error`, `failure_error_kind`, `stop_reason="completed"`, `tool_events`,
`external_lookup_counts`, `workspace_violation_counts`, `empty_content_retries=0`,
`length_recovery_parts`, `had_injections`, `injection_cycles=0`, `pending_stream_content=None`.
A `ProviderConversationStateController` and a `ModelRequestState` are constructed here
(`runner.py:411-433`).

**Loop bound**: `for iteration in range(spec.max_iterations):` (`runner.py:435`). The default
`max_iterations` is `200`, sourced from `AgentDefaults.max_tool_iterations`
(`nanobot/config/schema.py:129`) via `AgentLoop.__init__` (`nanobot/agent/loop.py:321-323`) and passed
through `AgentRunSpec` (`loop.py:1174`).

Per iteration, in exact order:

1. `context = AgentHookContext(iteration=iteration, messages=messages, session_key=...)`;
   `await hook.before_iteration(context)` (`runner.py:436-441`).
2. `request_message_count = len(messages)`; `request_messages` = compaction-projected messages if a
   compaction state exists, else `messages` (`runner.py:442-447`).
3. `response, raw_usage = await self._request_model(...)` (`runner.py:448-455`).
4. `messages_for_model = request_state.messages`; `conversation_state.observe_response(response,
   messages)`; compaction accepts the request (`runner.py:456-463`).
5. `context.response`, `context.tool_calls` set (`runner.py:464-465`).
6. **Reasoning extraction**: `original_content = response.content`;
   `reasoning_text, cleaned_content = extract_reasoning(response.reasoning_content,
   response.thinking_blocks, response.content)`; `response.content = cleaned_content`
   (`runner.py:467-473`). `extract_reasoning` priority order is dedicated `reasoning_content` →
   Anthropic `thinking_blocks` → inline `<think>`/`<thought>` blocks, with inline tags always stripped
   from content (`nanobot/utils/helpers.py:292-324`).
7. Usage accounting: `round_usages.append(raw_usage)`; `usage = self._merge_usage(usage, raw_usage)`
   (`runner.py:474-476`). If reasoning text exists and was not already streamed:
   `hook.emit_reasoning(reasoning_text)` then `hook.emit_reasoning_end()` and
   `context.streamed_reasoning = True` (`runner.py:477-480`).

#### Branch A — tools execute (`response.should_execute_tools`) (`runner.py:482-579`)

`should_execute_tools` is true only when there is at least one tool call **and** `finish_reason` is in
`("tool_calls", "function_call", "stop")` (`nanobot/providers/base.py:602-608`).

1. If the hook wants streaming: `await hook.on_stream_end(context, resuming=True)`
   (`runner.py:484-485`).
2. Build the assistant message:
   `build_assistant_message(response.content or "", tool_calls=[tc.to_openai_tool_call() for tc in
   response.tool_calls], reasoning_content=..., thinking_blocks=...)`, then
   `conversation_state.project_response_message(...)`, then `messages.append(...)`
   (`runner.py:487-497`). `build_assistant_message` returns
   `{"role":"assistant","content":content or "", "tool_calls": [...], "reasoning_content": ...,
   "thinking_blocks": ...}` — `reasoning_content` is only present when it was not `None` or
   thinking blocks exist, and is passed through `strip_reasoning_tags`
   (`nanobot/utils/helpers.py:698-716`).
3. Emit checkpoint `phase="awaiting_tools"` with `pending_tool_calls` (`runner.py:498-508`).
4. `await hook.before_execute_tools(context)` (`runner.py:510`).
5. `results, new_events = await execute_tool_calls(spec.tools, response.tool_calls,
   concurrent=spec.concurrent_tools, external_lookup_counts=..., workspace_violation_counts=...,
   hook=..., context=..., model_messages=messages_for_model,
   compacted_tool_results=request_state.compacted_tool_results)` (`runner.py:512-522`).
6. `tool_events.extend(new_events)`; `tools_used` extends with the name of each tool whose event status
   is `"ok"`, zipped positionally against `response.tool_calls` (`runner.py:523-528`).
7. **Tool results appended back** (`runner.py:531-545`) — one message per tool call, positionally
   zipped with `results`:

   ```python
   {
     "role": "tool",
     "tool_call_id": tool_call.id,
     "name": tool_call.name,
     "content": self.context_governor.normalize_tool_result(
         governance_config, tool_call.id, tool_call.name, result),
   }
   ```

8. Optional `provider_state` checkpoint using governed model messages
   (`runner.py:546-568`).
9. `empty_content_retries = 0`; `length_recovery_parts.clear()` (`runner.py:569-570`).
10. Injection checkpoint 1: drain after tool execution (`runner.py:571-577`).
11. `await hook.after_iteration(context)`; `continue` (`runner.py:578-579`).

#### Branch B — no tool execution

If `response.has_tool_calls` but `should_execute_tools` was false, a warning is logged:
`"Ignoring tool calls under finish_reason='{}' for {}"` (`runner.py:581-586`). The calls are dropped.

Then `clean = hook.finalize_content(context, response.content)` (`runner.py:588`).

**B1. Empty-content retry** (`runner.py:589-629`): when `finish_reason` is **not** in
`{"error","length","refusal","content_filter"}` and `is_blank_text(clean)`:
- increment `empty_content_retries`; if `< _MAX_EMPTY_RETRIES` (i.e. the first retry), log a warning,
  close the stream if streaming (`on_stream_end(resuming=False)`), `after_iteration`, and `continue`
  (`runner.py:594-606`).
- otherwise log "attempting finalization", close the stream, and issue a **no-tools** request with the
  finalization retry prompt appended (`runner.py:607-620`); the retry's usage is recorded and merged
  into both `round_usages` and `usage`; `original_content` and `clean` are recomputed
  (`runner.py:621-629`).

**B2. Length recovery** (`runner.py:631-656`): if `response.finish_reason == "length"` and fewer than
`_MAX_LENGTH_RECOVERIES` segments have been collected:
- append `_restore_outer_whitespace(clean or "", original_content)` to `length_recovery_parts`;
- log "Output truncated on turn {} for {} ({}/{}); continuing";
- if streaming, set `context.stream_continues_current_message = True` and
  `on_stream_end(resuming=True)`;
- append the assistant segment, then `build_length_recovery_message(clean or "")`, then
  `after_iteration` and `continue` (`runner.py:631-656`).
`build_length_recovery_message` embeds the last 64 characters of the delivered text inside
`<already_delivered_tail>` and instructs the model to continue from that exact endpoint
(`nanobot/utils/runtime.py:36-41`, `nanobot/utils/runtime.py:78-90`, `_LENGTH_RECOVERY_TAIL_CHARS`
at `runtime.py:17`).

**B3. Late streaming recovery** (`runner.py:658-673`): when `length_recovery_parts` is non-empty, the
hook wants streaming, `not context.streamed_content`, `finish_reason != "error"` and the content is
non-blank, the terminal segment is emitted through `hook.on_stream` so the visible prefix is not
duplicated, and `context.streamed_content = True`.

**B4. Injection check before stream end** (`runner.py:687-708`): drains injections with
`allow_continuation = finish_reason not in {"refusal","content_filter"}` and
`wait_at_terminal = assistant_message is not None and finish_reason not in
{"error","length","refusal","content_filter"}`. Comment at `runner.py:687-689` states the ordering
rationale: the stream is kept alive (`resuming=True`) so streaming channels do not prematurely
finalize the card. Then `on_stream_end(resuming=should_continue)`; if continuing, clear
`length_recovery_parts`, `after_iteration`, `continue` (`runner.py:707-713`).

**B5. Error terminal** (`runner.py:715-736`): `finish_reason == "error"` →
`final_content = _ARREARAGE_ERROR_MESSAGE if LLMProvider.is_arrearage_response(response) else
clean or spec.error_message or _DEFAULT_ERROR_MESSAGE`; `stop_reason = "error"`; `error =
final_content`; `_append_model_error_placeholder(messages)`; then one last injection drain
("after LLM error"); if it continues, `continue`; otherwise
`failure_error_kind = LLMProvider.public_error_kind(response)` and `break` (`runner.py:715-736`).

**B6. Blank terminal** (`runner.py:737-754`): `final_content = EMPTY_FINAL_RESPONSE_MESSAGE`
(`"I completed the tool steps but couldn't produce a final answer. Please try again or narrow the
task."` — `nanobot/utils/runtime.py:19-22`), `stop_reason = "empty_final_response"`, error set,
`_append_final_message`, injection drain ("after empty response"), `break` unless continuing.

**B7. Success terminal** (`runner.py:756-789`): append the assistant message (projected through
provider state), emit checkpoint `phase="final_response"` with `provider_state`, compute
`final_content` as either the joined length-recovery chain plus the terminal tail (stripped) or `clean`,
set `context.final_content` / `context.stop_reason`, `after_iteration`, `break`.

#### Loop exhaustion — `for ... else` (`runner.py:790-823`)

`stop_reason = "max_iterations"`. Then:
1. Drain remaining injections ("after max_iterations") so they land in history rather than being
   re-published by `_dispatch`'s finally block (`runner.py:797-802`; rationale in comment
   `runner.py:792-796`).
2. If `spec.finalize_on_max_iterations`: `_try_finalize_after_max_iterations(...)`
   (`runner.py:804-812`), which appends `build_budget_exhausted_finalization_message()` and issues a
   **no-tools** request; on exception it logs and returns `None`; a response with
   `finish_reason == "error"` or any tool calls is rejected; a blank result is rejected
   (`runner.py:1163-1217`).
3. If that yields nothing: `_max_iterations_fallback(spec)`, which formats
   `spec.max_iterations_message` if provided, else renders the template
   `agent/max_iterations_message.md` with `max_iterations` (`runner.py:1258-1268`).
4. If `length_recovery_parts` exists, the terminal content is appended as a `\n\n`-separated tail and
   `pending_stream_content = terminal_tail` (`runner.py:815-820`).
5. `self._append_final_message(messages, terminal_content)` (`runner.py:823`).

`_append_final_message` (`runner.py:1355-1368`) replaces a trailing assistant message that has no tool
calls and different content, returns early if the content is identical, and otherwise appends a new
assistant message. `_append_model_error_placeholder` (`runner.py:1370-1374`) appends the placeholder
only if the last message is not an assistant message without tool calls.

### 3.4 Result (`runner.py:825-844`)

`AgentRunResult(final_content, messages, tools_used, usage, round_usages, stop_reason, error,
failure_error_kind, tool_events, had_injections, pending_stream_content, provider_state,
summary_checkpoint, provider_compaction_applied)`.

**`stop_reason` vocabulary observed as assigned by the runner**: `"completed"` (initial,
`runner.py:399`), `"error"` (`runner.py:720`), `"empty_final_response"` (`runner.py:739`),
`"max_iterations"` (`runner.py:791`). `"cancelled"` is assigned only on the hook context
(`runner.py:321`), not on the result. `"tool_error"` is *consumed* by delivery code
(`nanobot/agent/turn_delivery.py:68`, `nanobot/agent/loop.py:1735`) but **no code path in
`runner.py` assigns it** — see Gotchas §10.1.

### 3.5 Tool call collection, concurrency and batching

`execute_tool_calls` — `nanobot/agent/tools/execution.py:56-111`.

- Batching: `_partition_tool_batches(tools, tool_calls, concurrent=...)`
  (`execution.py:292-316`). When `concurrent=False`, every call becomes its own single-element batch
  (strictly sequential). When `concurrent=True`, a call joins the current batch only if its tool
  object exists and `tool.concurrency_safe` is true; otherwise the current batch is flushed and the
  call is emitted alone.
- `Tool.concurrency_safe` = `read_only and not exclusive` (`nanobot/agent/tools/base.py:194-197`);
  `read_only` defaults to `False` and `exclusive` to `False` (`base.py:189-202`).
- Batches with more than one call run through `asyncio.gather(*...)` (`execution.py:82-95`);
  single-element batches run sequentially (`execution.py:96-107`). **There is no numeric concurrency
  bound on tool execution** — the bound is structural: only `concurrency_safe` tools are ever batched
  together, and everything else is serialized one at a time. Result order is stable: results are
  appended in batch order, and within a gathered batch in argument order (`execution.py:80-111`).
- Read-result de-duplication: a `functools.cache`d `read_results()` indexes prior `tool` messages by
  `tool_call_id` from `model_messages`, excluding ids in `compacted_tool_results`
  (`execution.py:69-79`). It is only installed for `read_file` via
  `file_read_context(tool_call.id, read_results)` (`execution.py:167-170`).
- Per-turn throttles carried across iterations of the same run: `external_lookup_counts` and
  `workspace_violation_counts` (created at `runner.py:401-403`, passed at `runner.py:516-517`).

**Per-call execution** — `_execute_tool_call` (`execution.py:114-223`):

1. Repeated external lookup guard: `repeated_external_lookup_error(name, arguments, counts)`. It
   returns an error only on the **3rd** attempt at the same target
   (`_MAX_REPEAT_EXTERNAL_LOOKUPS = 2`, `nanobot/utils/runtime.py:13`, `runtime.py:109-130`);
   signatures are `web_fetch:<url lower>` and `web_search:<query lower>` (`runtime.py:93-106`).
2. `tools.prepare_call(name, arguments)` is called (guarded by `callable(...)` and a 3-tuple check)
   (`execution.py:136-146`).
3. On prep error → payload is `prep_error + retry hint`; the event detail is
   `prep_error.split(": ", 1)[-1][:120]` (`execution.py:147-163`).
4. `await hook.before_execute_tool(context, tool_call, tool, params)` (`execution.py:165`).
5. Execution: `await tool.execute(**params)` when a tool object was resolved, else
   `await tools.execute(name, params)` (`execution.py:171-174`).
   **Note**: the resolved-tool path bypasses `ToolRegistry.execute`'s hint wrapping
   (`nanobot/agent/tools/registry.py:187-201`); that wrapper is only reached when `prepare_call` is
   unavailable.
6. `asyncio.CancelledError` is re-raised untouched (`execution.py:175-176`).
7. Any other exception → `hook.on_execute_tool_error(...)`, event
   `{"name":..., "status":"error", "detail": str(exc)}`, payload
   `_with_retry_hint(f"Error: {type(exc).__name__}: {exc}")`, then violation classification
   (`execution.py:177-194`).
8. `ToolResult` with `is_error` → `hook.on_execute_tool_error(...)`, payload
   `_with_retry_hint(result)`, event detail `result.replace("\n"," ").strip()[:120]`
   (`execution.py:196-213`).
9. Success → `hook.after_execute_tool(...)`, event
   `{"name":..., "status":"ok", "detail": <str(result), newlines collapsed, 120 chars max or "(empty)">}`
   and the **raw result** is returned (`execution.py:215-223`).

**Error payload shape**: errors are represented as *strings*, not exceptions — they are ordinary tool
result content that gets appended as a `tool` message. Every error string gets the suffix
`"\n\n[Analyze the error above and try a different approach.]"`
(`_RETRY_HINT`, `execution.py:22`; `_with_retry_hint` appends exactly once, `execution.py:49-53`).

**Boundary classification** (`execution.py:226-289`):
- SSRF markers: `"internal/private url detected"`, `"private/internal address"`, `"private address"`
  (`execution.py:25-29`). On match the payload becomes the raw text plus `_SSRF_BOUNDARY_NOTE`
  (`execution.py:30-37`, `execution.py:283-285`) and the event detail is prefixed
  `"ssrf_violation: "`.
- Workspace markers: `"outside the configured workspace"`, `"outside allowed directory"`,
  `"working_dir is outside"`, `"working_dir could not be resolved"`, `"path outside working dir"`,
  `"path traversal detected"` (`execution.py:39-46`). The 3rd repeat of the same target escalates to a
  "refusing repeated workspace-bypass attempts" error
  (`_MAX_REPEAT_WORKSPACE_VIOLATIONS = 2`, `runtime.py:16`, `runtime.py:173-201`); event detail becomes
  `"workspace_violation_escalated: "`.

### 3.6 Request construction and model calls

`_build_request_kwargs` (`runner.py:846-863`) produces exactly:
`{"messages": ..., "tools": ..., "model": spec.runtime.model, "retry_mode": spec.provider_retry_mode,
"temperature": generation.temperature, "max_tokens": generation.max_tokens,
"reasoning_effort": generation.reasoning_effort}`.

`_request_model` (`runner.py:865-1074`):

- `tool_definitions = spec.tools.get_definitions()` (`runner.py:876`).
- `messages, provider_context = await self.context_governor.prepare_request(request_state, messages,
  tool_definitions=..., transcript=...)` (`runner.py:877-882`).
- **Streaming choice**: `wants_streaming = hook.wants_streaming()` (`runner.py:889`). Both branches
  call `chat_stream_with_retry`; the streaming branch additionally passes
  `on_content_delta=_stream`, `on_thinking_delta=_thinking`, `on_tool_call_delta=_provider_tool_event`
  and `on_stream_recover=_stream_recover` (`runner.py:955-996`). **The non-streaming branch still calls
  `chat_stream_with_retry` with no delta callbacks** (`runner.py:992-996`).
- Reasoning delta handling (`runner.py:965-977`): the accumulated buffer is passed through
  `strip_reasoning_tags` on every chunk, and only the *incremental clean* suffix is emitted, so
  partial tags never leak. `context.streamed_reasoning = True` is set only when the incremental part
  is non-empty.
- Timing: `request_started_at = time.perf_counter()` immediately before awaiting; on success
  `response.ttft_ms = max(0, round((first_output_at - request_started_at) * 1000))` and
  `response.generation_ms = max(1, round(generation_elapsed_s * 1000))` where generation time excludes
  the first-token wait (`runner.py:899-914`, `runner.py:999-1011`).
- `asyncio.CancelledError` → pause generation timing, close native reasoning, re-raise
  (`runner.py:1002-1005`).
- Provider compaction summarization and usage recording (`runner.py:1012-1018`).
- If the final response is an error, every still-active provider-hosted tool is closed with
  `phase="error"` (`runner.py:1019-1029`).
- **Malformed tool call handling** (`runner.py:1030-1073`, `_drop_malformed_tool_calls` at
  `runner.py:1076-1112`): calls with a missing/non-string name are dropped; if **all** calls were
  dropped and the original finish reason was `tool_calls`/`function_call`, the request is retried once
  with a user-role note appended; if the retry also produces only malformed calls, a **no-tools**
  request is issued as the final fallback (`runner.py:1042-1073`). Dropping also sets
  `response.provider_state = None` and, when nothing valid remains,
  `response.finish_reason = "stop"` (`runner.py:1105-1112`).

`_request_no_tools` (`runner.py:1219-1248`) repeats the governance preparation with
`tool_definitions=None` and `_build_request_kwargs(..., tools=None)`.

### 3.7 Usage accounting

- `_usage_or_estimate` (`runner.py:1270-1292`): an error response with no/zero usage gets
  `LLMUsage.empty_request()`; otherwise a missing/zero usage is estimated locally from the prompt
  chain plus the assistant message; timing is attached with `with_timing`.
- `_estimate_response_usage` (`runner.py:1309-1333`).
- `_merge_usage` (`runner.py:1335-1344`) is `left + right`, i.e. `LLMUsage.__add__`
  (`nanobot/providers/base.py:418-444`).
- `round_usages` holds one entry per runner-visible model round, with recovery dispatches folded into
  the same value (comment at `runner.py:126-127`).

---

## 4. PROVIDER INTERFACE

### 4.1 `LLMProvider` abstract surface (`nanobot/providers/base.py:623`)

Abstract methods (exactly two):

```python
@abstractmethod
async def chat(
    self,
    messages: list[dict[str, Any]],
    tools: list[dict[str, Any]] | None = None,
    model: str | None = None,
    max_tokens: int = 4096,
    temperature: float = 0.7,
    reasoning_effort: str | None = None,
    tool_choice: str | dict[str, Any] | None = None,
) -> LLMResponse: ...
```
(`base.py:922-947`)

```python
@abstractmethod
def get_default_model(self) -> str: ...
```
(`base.py:1919-1922`)

Constructor: `__init__(self, api_key: str | None = None, api_base: str | None = None, *,
provider_name: str)`; `provider_name` must be a non-empty string or `ValueError` is raised; it sets
`self.generation = GenerationSettings()` (`base.py:697-711`).

Concrete/overridable methods (all observed):

| Method | Line | Contract |
|---|---|---|
| `set_llm_call_observer(observer)` | `base.py:713-715` | fail-open per-physical-call observer |
| `can_resume_conversation_state(state, model=None) -> bool` | `base.py:801-807` | base returns `False` |
| `supports_native_compaction(model=None) -> bool` | `base.py:809-811` | base returns `False` |
| `chat_stream(...) -> LLMResponse` | `base.py:1298-1338` | default = `asyncio.wait_for(self.chat(...), timeout=resolve_stream_idle_timeout_s())`, then one `on_content_delta(response.content)` |
| `chat_with_context(*, provider_context, **kwargs)` | `base.py:1340-1348` | default delegates to `chat` |
| `chat_stream_with_context(*, provider_context, **kwargs)` | `base.py:1350-1358` | default delegates to `chat_stream` |
| `chat_stream_with_retry(...)` | `base.py:1443-1510` | retry wrapper; `stream=True` |
| `chat_with_retry(...)` | `base.py:1512-1561` | retry wrapper; `stream=False` |
| `is_transient_response(response) -> bool` (classmethod) | `base.py:1011-1028` | see §9 |
| `is_arrearage_response(response) -> bool` (classmethod) | `base.py:1030-1051` | see §9 |
| `public_error_kind(response) -> str` (classmethod) | `base.py:1723-1735` | `billing`/`connection`/`timeout`/`rate_limit`/`server`/`unknown` |

**What `chat()` returns**: an `LLMResponse`. `chat()` never raises for provider/HTTP failures on the
`_safe_chat` path — `_safe_chat` catches every non-cancellation exception and converts it with
`_error_response_from_exception` into `LLMResponse(content=f"Error calling LLM: {detail}",
finish_reason="error", ...)` (`base.py:1262-1296`, `base.py:949-1004`). `asyncio.CancelledError` is
observed and re-raised (`base.py:1275-1287`). Note that `OpenAICompatProvider.chat` also has its own
broad `except Exception` returning `self._handle_error(...)` (`openai_compat_provider.py:2012-2013`),
so errors are converted at two layers.

**Streaming exposure**: `chat()` itself does **not** stream. Streaming is exposed through the
separate `chat_stream(...)` entry point with three optional callbacks
(`base.py:1298-1310`):

```python
on_content_delta: Callable[[str], Awaitable[None]] | None
on_thinking_delta: Callable[[str], Awaitable[None]] | None
on_tool_call_delta: Callable[[dict[str, Any]], Awaitable[None]] | None
```

`chat_stream` returns the *same* `LLMResponse` type as `chat`, fully aggregated.

### 4.2 `LLMResponse` (`base.py:553-608`)

| Field | Type | Default | Line |
|---|---|---|---|
| `content` | `str \| None` | required | `base.py:556` |
| `tool_calls` | `list[ToolCallRequest]` | `[]` | `base.py:557` |
| `finish_reason` | `str` | `"stop"` | `base.py:558` |
| `usage` | `LLMUsage \| None` | `None` | `base.py:559` |
| `generation_ms` | `int \| None` | `None` | `base.py:564` |
| `ttft_ms` | `int \| None` | `None` | `base.py:565` |
| `retry_after` | `float \| None` | `None` | `base.py:566` |
| `reasoning_content` | `str \| None` | `None` | `base.py:567` |
| `thinking_blocks` | `list[dict[str, Any]] \| None` | `None` | `base.py:568` |
| `provider_state` | `ProviderConversationState \| None` | `None` (repr off) | `base.py:569` |
| `provider_compaction_applied` | `bool` | `False` (repr off) | `base.py:572` |
| `provider_compaction_state` | `ProviderConversationState \| None` | `None` (repr off) | `base.py:575-578` |
| `provider_compaction_scope` | `"prior_context" \| "current_request" \| None` | `None` (repr off) | `base.py:582-585` |
| `preserve_provider_state_on_error` | `bool \| None` | `None` (repr off) | `base.py:588` |
| `error_status_code` | `int \| None` | `None` | `base.py:590` |
| `error_kind` | `str \| None` | `None` | `base.py:591` |
| `error_type` | `str \| None` | `None` | `base.py:592` |
| `error_code` | `str \| None` | `None` | `base.py:593` |
| `error_retry_after_s` | `float \| None` | `None` | `base.py:594` |
| `error_should_retry` | `bool \| None` | `None` | `base.py:595` |

Derived properties:
- `has_tool_calls` = `len(tool_calls) > 0` (`base.py:597-600`).
- `should_execute_tools` = `has_tool_calls and finish_reason in ("tool_calls","function_call","stop")`
  (`base.py:602-608`). Docstring notes this "Blocks gateway-injected calls under `refusal` /
  `content_filter` / `error`".

### 4.3 `ToolCallRequest` (`base.py:63-107`)

| Field | Type | Default | Line |
|---|---|---|---|
| `id` | `str` | required | `base.py:66` |
| `name` | `str` | required | `base.py:67` |
| `arguments` | `Any` | required | `base.py:68` |
| `extra_content` | `dict \| None` | `None` | `base.py:69` |
| `provider_specific_fields` | `dict \| None` | `None` | `base.py:70` |
| `function_provider_specific_fields` | `dict \| None` | `None` | `base.py:71` |

- `has_valid_name()` checks `isinstance(name, str) and bool(name)` at runtime; the docstring explains
  that a degenerate call persisted and replayed makes upstream APIs reject the whole request and
  "permanently wedges the session" (`base.py:73-84`).
- `to_openai_tool_call()` returns
  `{"id", "type": "function", "function": {"name", "arguments"}}` where `arguments` is passed through
  unchanged if it is a `str`, else `json.dumps(arguments, ensure_ascii=False)`; the three optional
  fields are added as `extra_content`, `provider_specific_fields`, and
  `function.provider_specific_fields` when truthy (`base.py:86-107`).
- `parse_tool_arguments(arguments)` (`base.py:110-130`): `None` → `{}`; non-str passthrough; blank
  string → `{}`; valid JSON → parsed value **unless** it is `None` (then the original string is kept);
  malformed JSON → the original string is returned unchanged so the registry can reject it.
- `tool_arguments_object_for_replay` / `tool_arguments_json_for_replay` (`base.py:133-163`) may repair
  malformed JSON via `json_repair` **for history replay only**.

### 4.4 `LLMUsage` (`base.py:268-550`)

Fields (frozen, slots): `input_tokens:int`, `output_tokens:int`, `total_tokens:int`,
`cache_read_tokens:int|None=None`, `cache_write_tokens:int|None=None`, `reported_tokens:int=0`,
`estimated_tokens:int=0`, `generation_ms:int=0`, `measured_output_tokens:int=0`, `ttft_ms:int=0`,
`timed_requests:int=0`, `context_tokens:int|None=None`, `request_count:int=0`
(`base.py:282-294`).

`__post_init__` invariants (`base.py:296-337`) — all must hold or `ValueError` is raised:
- every integer field is a non-negative `int` and **not** a `bool`;
- `cache_read_tokens`/`cache_write_tokens`/`context_tokens` are `None` or non-negative int (not bool);
- `total_tokens >= input_tokens + output_tokens`;
- `reported_tokens + estimated_tokens == total_tokens`;
- `cache_read_tokens + cache_write_tokens <= input_tokens`.

Factories: `reported(*, input_tokens, output_tokens, total_tokens=None, cache_read_tokens=None,
cache_write_tokens=None)` normalizes `total_tokens` to `max(visible_total, total_tokens)` and sets
`reported_tokens = total_tokens`, `context_tokens = input_tokens`, `request_count = 1`
(`base.py:339-363`). `estimated(*, input_tokens, output_tokens)` sets `estimated_tokens = total_tokens`
(`base.py:365-375`). `empty_request()` is all zeros with `request_count = 1` (`base.py:377-385`).

`source` → `"reported"` when `estimated_tokens == 0`, `"estimated"` when `reported_tokens == 0`, else
`"mixed"` (`base.py:387-393`).

`with_timing(*, generation_ms, ttft_ms)` sets `measured_output_tokens = output_tokens if
generation_ms is not None else 0` and `timed_requests = 1 if ttft_ms is not None else 0`
(`base.py:395-416`).

`__add__` sums everything; cache counts sum only when **both** sides are non-`None` (otherwise the
result is `None`); `context_tokens` takes the right operand's value when present, else the left's
(`base.py:418-444`).

Serialization: `to_dict()` emits 15 keys (`base.py:446-463`) and `from_dict` requires the key set to
match exactly and re-derives `source` (`base.py:488-550`). `to_turn_dict()` projects to the compact
per-turn shape with `prompt_tokens`/`completion_tokens`/`total_tokens`/`request_count`/
`estimated_tokens` plus optional `context_tokens`, `cached_tokens`, `cache_write_tokens`,
`generation_ms`+`measured_completion_tokens`, `ttft_ms`+`timed_requests` (`base.py:465-486`).

### 4.5 `GenerationSettings` (`base.py:611-617`)

`@dataclass(frozen=True)`: `temperature: float = 0.7`, `max_tokens: int = 4096`,
`reasoning_effort: str | None = None`. In practice the value installed on a provider comes from the
active preset: `AgentDefaults`/`ModelPresetConfig` default to `temperature=0.1`,
`max_tokens=8192`, `context_window_tokens=200000`
(`nanobot/config/schema.py:102-105`, `schema.py:126-128`), converted by
`ModelPresetConfig.to_generation_settings()` (`schema.py:107-113`) and assigned at
`nanobot/providers/factory.py:239`.

### 4.6 `ProviderConversationState` (`base.py:166-248`)

`@dataclass`: `kind: str`, `provider: str`, `model: str`, `version: int`,
`payload: dict[str, Any] = {}` (repr off), `pending_messages: list[dict[str, Any]] = []` (repr off)
(`base.py:177-182`).

- Docstring contract: `payload` may contain encrypted reasoning or provider-private protocol items and
  must be kept out of logs and public chat history (`base.py:168-175`).
- `with_pending_messages(messages)` returns a copy with a **deep-copied** pending list
  (`base.py:184-196`).
- `to_private_record()` / `from_private_record(value)` are the private-session-sidecar serialization
  boundary; `from_private_record` returns `None` for any structural violation, explicitly rejecting
  `bool` for `version` (`base.py:198-248`).

`ProviderCallContext` (`@dataclass(frozen=True)`) carries `conversation_state`,
`context_window_tokens`, `session_id`, `events` (`base.py:251-265`).

`ProviderConversationStateController` (`nanobot/providers/conversation_state.py:35`) governs the
lifecycle: `prepare_request` (`:100-168`), `observe_response` (`:170-211`), `project_response_message`
(`:213-224`), `checkpoint` (`:226-249`), `finish` (`:251-257`). `observe_response` adopts a new state
only when the candidate exists, `finish_reason` is in `{"stop","tool_calls","function_call"}`, and the
provider can resume it; otherwise, on an error it preserves state when
`preserve_provider_state_on_error is True` or (`None` and the response is transient); otherwise it
clears state (`conversation_state.py:178-211`).

---

## 5. STREAMING

### 5.1 Callback contract

Three delta callbacks plus one recovery callback
(`nanobot/providers/base.py:1307-1309`, `base.py:1455`):

| Callback | Payload | Meaning |
|---|---|---|
| `on_content_delta(str)` | text chunk | answer text |
| `on_thinking_delta(str)` | text chunk | reasoning/thinking text |
| `on_tool_call_delta(dict)` | `{"index", "call_id", "name", "arguments_delta"}` | live tool-call progress |
| `on_stream_recover()` | — | provider asks the consumer to close the current stream segment and start a new one |

The `on_tool_call_delta` payload shape is built in `OpenAICompatProvider.chat_stream`
(`openai_compat_provider.py:2166-2187`): `index` falls back to the enumerate index when the wire
`index` is absent; `call_id`, `name`, `arguments_delta` default to `""`.

### 5.2 Consumption in the runner (`runner.py:955-996`)

- `_stream(delta)`: records generation timing; when the delta is non-empty sets
  `context.streamed_content = True`, closes native reasoning, then `await hook.on_stream(context, delta)`
  (`runner.py:958-963`).
- `_thinking(delta)`: accumulates into `thinking_buf`, computes the incremental difference of
  `strip_reasoning_tags(thinking_buf)`, and on non-empty increment sets
  `context.streamed_reasoning = True`, marks native reasoning open, and calls
  `hook.emit_reasoning(incremental)` (`runner.py:965-977`).
- `_stream_recover()`: pauses generation timing, closes native reasoning, and calls
  `hook.on_stream_end(context, resuming=True)` (`runner.py:979-982`).
- `_provider_tool_event(event)`: ignores non-`hosted_tool` kinds; closes native reasoning; forwards to
  `hook.on_provider_tool_event`; tracks active hosted calls by `call_id` with
  `phase` in `{"start"}` / `{"end","error"}` (`runner.py:941-953`).

### 5.3 Aggregation into the final `LLMResponse`

`OpenAICompatProvider.chat_stream` (`openai_compat_provider.py:2015-2201`):

1. Sets `kwargs["stream"] = True`, `kwargs["timeout"] = idle_timeout_s`, and
   `kwargs["stream_options"] = {"include_usage": True}` (`:2124-2126`).
2. Zhipu/GLM additionally gets `extra_body["tool_stream"] = True` when tools and
   `on_tool_call_delta` are present (`:2118-2123`).
3. Iterates the SDK stream with a per-chunk `asyncio.wait_for(..., timeout=idle_timeout_s)`
   (`:2134-2139`).
4. `completed |= bool(chunk.choices[0].finish_reason)` (`:2143-2144`) — a stream that ends without any
   finish reason raises `ConnectionError("Model stream ended before a finish reason was received")`
   (`:2188-2189`).
5. Callbacks are invoked per chunk: content (with Mistral thinking blocks filtered out via
   `_extract_text_content`, `:2147-2154`), reasoning (`delta.reasoning_content or delta.reasoning`,
   falling back to `_extract_thinking_content(raw_delta_content)`, `:2155-2165`), and tool-call deltas
   (`:2166-2187`).
6. All raw chunks are collected and passed to `_parse_chunks(chunks)` (`:2190`).
7. `asyncio.TimeoutError` is converted to
   `LLMResponse(content=f"Error calling LLM: stream stalled for more than {idle_timeout_s:g} seconds",
   finish_reason="error", error_kind="timeout")` (`:2191-2199`).

`_parse_chunks` (`:1688-1846`) accumulates:
- `content_parts` from `delta.content` via `_extract_text_content`;
- `reasoning_parts` from `delta.reasoning_content`, then `delta.reasoning`, then
  `_extract_thinking_content(raw_delta_content)` (Mistral);
- `tc_bufs: dict[int, {...}]` keyed by `tc.index` (falling back to the enumerate index) with
  `id` (last non-empty wins), `name` (last non-empty wins), and `arguments` (**string concatenation**),
  plus `extra_content`/`prov`/`fn_prov`;
- legacy `delta.function_call` is accumulated into index 0 (`:1722-1735`, `:1780`);
- `finish_reason` = the last non-empty `choices[0].finish_reason` seen (`:1757-1758`, `:1788-1789`);
- `usage` = last non-null `_extract_usage(chunk)` (`:1749`, `:1781`, `:1785`).
- Duplicate/empty tool-call ids are replaced by fresh 9-char ids (`:1816-1823`).
- `content = "".join(content_parts) or None`; `reasoning_content = "".join(reasoning_parts) or None`
  (`:1825`, `:1845`).
- `arguments` is finalized with `parse_tool_arguments(...)` (`:1830`).
- If no structured tool calls were found, `_extract_text_tool_calls(content)` scans for
  `<tool_call>...</tool_call>` blocks and lifts them into `ToolCallRequest`s, removing the matched
  spans from the visible content (`:1837-1838`; helper at `:245-295`).

### 5.4 Stream idle timeout

`resolve_stream_idle_timeout_s(*, env_value=None, default=90.0, maximum=3600.0)`
(`base.py:39-60`), env var `NANOBOT_STREAM_IDLE_TIMEOUT_S` (`base.py:28`), defaults
`DEFAULT_STREAM_IDLE_TIMEOUT_S = 90.0`, `MAX_STREAM_IDLE_TIMEOUT_S = 3600.0` (`base.py:29-30`).
Invalid or non-positive values fall back to the default with a warning; values above the maximum are
clamped (`base.py:47-60`).

### 5.5 Outbound stream events (channels)

`AgentProgressHook.on_stream` (`nanobot/agent/progress_hook.py:54-68`) buffers deltas, strips inline
thinking, and publishes `StreamDeltaEvent(content=incremental)` only for non-empty answer increments.
`on_stream_end` (`:70-77`) publishes `StreamEndEvent(resuming=resuming,
merge_next=context.stream_continues_current_message)` and resets the buffer.
`emit_reasoning` / `emit_reasoning_end` publish `ProgressEvent(content=..., reasoning_delta=True)` and
`ProgressEvent(reasoning_end=True)` (`:147-159`).

`StreamDeltaEvent` / `StreamEndEvent` / `StreamedResponseEvent` field definitions:
`nanobot/bus/outbound_events.py:40-56`.

---

## 6. OPENAI-COMPATIBLE PROVIDER

`OpenAICompatProvider` at `nanobot/providers/openai_compat_provider.py:509`.

### 6.1 Construction and client (`:518-637`)

Constructor parameters (`:518-530`): `api_key`, `api_base`, `default_model="gpt-4o"`,
`extra_headers`, `spec: ProviderSpec|None`, `extra_body`, `api_type="auto"`, `extra_query`, `proxy`,
`provider_name="openai"`.

- `_api_type = api_type if spec and spec.name == "openai" else "auto"` (`:536`).
- `_effective_base = api_base or (spec.default_api_base if spec else None) or None` (`:541`).
  **No `/v1` suffix is appended anywhere**; the configured/default base URL is used verbatim
  (`:541`, `:606`), and registry defaults already include `/v1`
  (e.g. `nanobot/providers/registry.py:201`).
- Default headers: `{"x-session-affinity": uuid4().hex}` (`:543`); OpenRouter attribution headers
  `HTTP-Referer: https://github.com/HKUDS/nanobot`, `X-OpenRouter-Title: nanobot`,
  `X-OpenRouter-Categories: cli-agent,personal-agent` when the spec is `openrouter` or the base URL
  contains `openrouter` (`:97-101`, `:354-358`, `:544-545`); user `extra_headers` override last
  (`:546-547`).
- OpenCode affinity: `x-opencode-session` header, either a per-instance uuid4 (`:548-553`) or a
  per-conversation `sha256(session_id)` hex digest when a `ProviderCallContext.session_id` exists
  (`:914-926`).
- `_api_key_for_client = api_key or "no-key"` (`:554`).
- Local endpoint detection `_is_local_endpoint` (`:372-400`): true for `spec.is_local` or hosts
  `localhost`, `host.docker.internal`, or a loopback/private IP literal.
- Lazy client creation guarded by an `asyncio.Lock` (`:557-560`, `:614-637`).
- Timeout: `_openai_compat_timeout_s()` = `NANOBOT_OPENAI_COMPAT_TIMEOUT_S` env or
  `_OPENAI_COMPAT_REQUEST_TIMEOUT_S = 120.0` (`:132`, `:210-227`).
- HTTP client selection (`:567-612`):
  - proxy set → `httpx.AsyncClient(timeout=..., proxy=..., trust_env=False, follow_redirects=True)`
    (`:573-579`);
  - local endpoint → `httpx.Limits(keepalive_expiry=0)` and
    `httpx.AsyncHTTPTransport(proxy=None, limits=...)` so local traffic bypasses `HTTP_PROXY` and dead
    keepalive connections (`:580-600`);
  - otherwise `http_client=None`, so the SDK builds its default client with `trust_env=True`
    (`:601-603`).
- `AsyncOpenAI(api_key=self._api_key_for_client, base_url=self._effective_base,
  default_headers=self._default_headers, default_query=self._extra_query or None, max_retries=0,
  timeout=timeout_s, http_client=http_client)` (`:604-612`). **`max_retries=0`** — retry policy is
  owned entirely by nanobot.

**Auth header — INFERENCE, not OBSERVED in this repository.** The API key is handed to the OpenAI SDK
(`:604-612`); the resulting `Authorization: Bearer <key>` request header is SDK behavior, not present
in this codebase. There is no explicit `Authorization` construction anywhere in
`openai_compat_provider.py`. Treat the exact header name/format as an SDK contract to re-verify when
porting. **UNVERIFIED in-repo.**

### 6.2 Request JSON built (Chat Completions)

`_build_kwargs` (`:928-1104`) assembles the kwargs dict handed to
`client.chat.completions.create(**kwargs)`. Resulting top-level fields:

| Field | Condition | Line |
|---|---|---|
| `model` | always; `model or self.default_model`, then `_request_model_name` | `:939`, `:947`, `:949-955` |
| `messages` | always; `_sanitize_messages(_sanitize_empty_content(messages), model_name)` | `:951-954` |
| `temperature` | only when `_supports_temperature(model_name, reasoning_effort)` | `:959-960` |
| `max_tokens` **or** `max_completion_tokens` | `max_completion_tokens` when `spec.supports_max_completion_tokens` or `_requires_max_completion_tokens(model)`; value is `max(1, max_tokens)` | `:962-967` |
| `reasoning_effort` | when a wire effort survives remapping and is not `"none"` | `:1038-1039` |
| `tools` + `tool_choice` | only when `tools` is truthy; `tool_choice` defaults to `"auto"` | `:1069-1071` |
| `extra_body` | thinking controls, provider-specific knobs, then merged configured `extra_body` | `:1043-1059`, `:1096-1100` |
| `extra_headers` | when the caller passed affinity headers | `:1101-1102` |
| `stream` | `True` (streaming path only) | `:2124` |
| `timeout` | `idle_timeout_s` (streaming path only) | `:2125` |
| `stream_options` | `{"include_usage": True}` (streaming path only) | `:2126` |

`_requires_max_completion_tokens` (`:176-181`): true for slug `kimi-k3`, any slug containing `gpt-5`,
or `o1`/`o3`/`o4` (exact, or prefixed with `-`/`.`).

`_supports_temperature` (`:896-912`): false for slug `kimi-k3`; false whenever `reasoning_effort` is
set to anything other than `"none"`; otherwise false when the lowercased model name contains
`gpt-5`, `o1`, `o3`, or `o4`.

**Message roles and keys.** `_ALLOWED_MSG_KEYS = {"role","content","tool_calls","tool_call_id","name",
"reasoning_content","extra_content"}` (`:89-92`), enforced by
`_sanitize_request_messages` (`nanobot/providers/base.py:908-920`), which also forces
`content=None` on assistant messages lacking a `content` key. `_sanitize_messages`
(`:700-795`) then:

- optionally strips `reasoning_content` for specs with `strip_history_reasoning_content` (`:715-721`);
- for Gemini, injects thought signatures (`:722-723`, `:818-870`);
- normalizes tool-call ids only for Mistral (`_should_normalize_tool_call_ids`, `:682-684`): an id that
  is already 9 alphanumeric chars is kept, otherwise `sha1(id)[:9]` (`:673-680`); a `deque`-based map
  keeps tool results matched to their (possibly duplicated) declared ids (`:749-758`);
- forces `function.arguments` to a JSON **string** via `tool_arguments_json_for_replay`, defaulting to
  `"{}"` (`:776-785`);
- for DeepSeek non-multimodal models, coerces content to a plain string
  (`force_string_content`, `:709-713`, `:790-794`; multimodal allow-list `_DEEPSEEK_MULTIMODAL_MODELS`
  at `:119-121`);
- ends with `_enforce_role_alternation(sanitized)` (`:795`).

**Tool schema shape.** Tools are passed through unchanged from `ToolRegistry.get_definitions()`, i.e.
the OpenAI function shape produced by `Tool.to_schema()`:
`{"type":"function","function":{"name","description","parameters"}}`
(`nanobot/agent/tools/base.py:306-315`).

**Thinking/reasoning controls** (`:986-1067`): `reasoning_effort` is normalized to a semantic form
(`"minimum"` → `"minimal"`, `:990-993`); Kimi K3 maps non-`none`/`minimal` to `"max"` and drops
`none`/`minimal` from the wire (`:997-1006`); DashScope maps `minimal` → `minimum` (`:1007-1009`);
provider `reasoning_effort_remap` may rewrite or omit the value (`:1024-1034`); implicit-reasoning
models strip it entirely (`:1014-1019`, `:1036-1037`). Thinking styles are applied to `extra_body`
only when `reasoning_effort is not None` (`:1043-1059`), using `_THINKING_STYLE_MAP`
(`:137-144`: `thinking_type` → `{"thinking":{"type":"enabled"|"disabled"}}`, `enable_thinking`,
`reasoning_split`) plus the gateway map `{"reasoning":{"effort":effort}}` (`:145-150`). For
`_KIMI_THINKING_MODELS`, `reasoning_effort` is removed again because Moonshot rejects both
(`:1061-1067`).

**DeepSeek reasoning backfill** (`:1073-1094`): when explicit thinking is enabled or the spec is
DeepSeek with a `deepseek-v4*`/`deepseek-reasoner` model, every assistant message lacking
`reasoning_content` gets `reasoning_content = ""`.

**Configured `extra_body` merge** (`:1096-1100`, `_merge_chat_extra_body` at `:464-483`): non-`tools`
keys deep-merge into `extra_body`; configured `tools` are **appended** to the top-level tool list, not
merged into `extra_body`.

### 6.3 Response parsing (Chat Completions, non-streaming)

`_parse(response)` (`:1542-1686`):

- A raw `str` response becomes `LLMResponse(content=response, finish_reason="stop")` (`:1543-1544`).
- Empty `choices`: if the payload carries `content`/`output_text`, that is returned with
  `finish_reason` from the payload (or `"stop"`); otherwise
  `LLMResponse(content="Error: API returned empty choices.", finish_reason="error",
  error_kind="empty")` (`:1552-1570`).
- Otherwise `choices[0].message` supplies `content` (via `_extract_text_content`) and the base
  `finish_reason` (defaulting to `"stop"`) (`:1572-1575`).
- StepFun fallback: when content is empty and `spec.reasoning_as_content`, `message.reasoning` becomes
  the content (`:1578-1580`).
- `reasoning_content` = `message.reasoning_content`, else `_extract_text_content(message.reasoning)`,
  else (when `spec.extract_thinking_blocks`) `_extract_thinking_content(message.content)`
  (`:1581-1589`).
- **All choices** are scanned for tool calls; `finish_reason` is overridden to
  `ch.finish_reason` when it is `"tool_calls"` or `"stop"` (`:1590-1603`).
- Each tool call becomes `ToolCallRequest(id, name, arguments=parse_tool_arguments(fn.arguments),
  extra_content, provider_specific_fields, function_provider_specific_fields)`; ids that are empty or
  duplicated are replaced with a fresh 9-char id (`:1605-1625`).
- If no structured tool calls exist, `_extract_text_tool_calls(content)` is attempted
  (`:1626-1627`).
- `usage` comes from `_extract_usage(response_map)` (`:1633`).
- An SDK-object (non-dict) response takes the parallel branch at `:1637-1686` with equivalent
  semantics.

`_extract_usage` (`:1463-1518`): `prompt_tokens`/`completion_tokens` from the `usage` mapping or
object; `wire_total = usage.total_tokens`; `cache_read_tokens` is resolved from the first present of
`prompt_tokens_details.cached_tokens`, `cached_tokens`, `prompt_cache_hit_tokens` (`:1495-1505`);
`cache_write_tokens` from `prompt_tokens_details.cache_write_tokens` (`:1507-1510`); the result is
`LLMUsage.reported(...)` (`:1512-1518`). `_get_nested_int` preserves an explicit zero and treats
booleans as absent (`:1520-1540`).

### 6.4 Error handling and metadata

`_extract_error_metadata(e)` (`:1848-1894`) produces `error_status_code` (from `e.status_code` or
`e.response.status_code`), `error_kind` (`"timeout"` if the exception class name contains `timeout`,
`"connection"` if it contains `connection`), `error_type`/`error_code` via
`LLMProvider._extract_error_type_code(payload)`, `error_retry_after_s` from response headers, and
`error_should_retry` from an `x-should-retry` header valued exactly `"true"`/`"false"`
(case-insensitive) (`:1870-1878`).

`_handle_error(e, *, spec, api_base)` (`:1896-1928`): message is
`f"Error: {body_text.strip()[:500]}"` when a body exists, else `f"Error calling LLM: {e}"`. For local
specs whose text contains `502`, `connection`, or `refused`, a troubleshooting hint is appended
(`:1912-1917`). `retry_after` is taken from headers first, then parsed out of the message text
(`:1919-1922`). The response is `finish_reason="error"` with the extracted metadata merged
(`:1923-1928`).

### 6.5 Responses API path (secondary)

`_should_use_responses_api` (`:1106-1145`) selects the Responses API only for `spec.name` in
`("openai","github_copilot")` or models listed in `spec.responses_models`, requires a direct OpenAI
base for the provider-level case (`_is_direct_openai_base`, `:403-408`), and consults a per-model
circuit breaker (3 failures → open, 300 s probe interval; `:368-369`, `:1197-1226`). Body
construction is `_build_responses_body` (`:1257-1366`); errors with status in `{400,404,422}` whose
body contains a compatibility marker fall back to Chat Completions (`:1228-1255`, `:1990-2000`).
GitHub Copilot never falls back (`:1991-1995`, `:2102-2106`).

**UNVERIFIED**: the internal behavior of `nanobot/providers/openai_responses/*` (stream capture, state
building, compaction thresholds) was not read for this spec.

---

## 7. TOOL INTERFACE

### 7.1 Base class

`Tool(ABC)` — `nanobot/agent/tools/base.py:159`. Abstract members:

| Member | Kind | Signature | Line |
|---|---|---|---|
| `name` | abstract property | `-> str` | `base.py:171-175` |
| `description` | abstract property | `-> str` | `base.py:177-181` |
| `parameters` | abstract property | `-> dict[str, Any]` | `base.py:183-187` |
| `execute` | abstract coroutine | `async def execute(self, **kwargs: Any) -> Any` | `base.py:226-229` |

Non-abstract surface:

- `read_only` property → `False` (`base.py:189-192`).
- `concurrency_safe` property → `read_only and not exclusive` (`base.py:194-197`).
- `exclusive` property → `False` (`base.py:199-202`).
- Plugin metadata class attributes: `config_key = ""`, `_plugin_discoverable = True`,
  `_scopes = {"core"}` (`base.py:206-208`); `config_cls() -> type[BaseModel] | None` (`:210-212`);
  `enabled(ctx) -> bool` (`:214-216`); `create(ctx) -> Tool` (`:218-220`);
  `runtime_context_provider() -> RuntimeContextProvider | None` (`:222-224`).
- `error(content)` static helper returning `ToolResult.error(content)` (`base.py:231-233`).
- `cast_params(params)` → schema-driven coercion (`base.py:251-256`, `_cast_object` `:235-249`,
  `_cast_value` `:258-295`).
- `validate_params(params) -> list[str]` (`base.py:297-304`); raises
  `ValueError(f"Schema must be object type, got {schema.get('type')!r}")` when the schema root is not
  `object`; returns `[f"parameters must be an object, got {type(params).__name__}"]` for non-dict
  input.
- `to_schema()` (`base.py:306-315`).

`ToolResult(str)` (`base.py:144-156`) is a **`str` subclass** with an `is_error: bool` attribute;
`ToolResult.error(content)` sets `is_error=True`. Because it subclasses `str`, `isinstance(result,
str)` is true and it flows through string handling paths.

### 7.2 JSON schema production

- `Tool.parameters` returns the JSON Schema for the argument object.
- `tool_parameters(schema)` is a class decorator that deep-copies the schema at class creation, injects
  a `parameters` property returning a **fresh deep copy** on each access, and removes `parameters`
  from `__abstractmethods__` (`base.py:318-350`).
- `tool_parameters_schema(*, required=None, description="", additional_properties=False, **properties)`
  builds the root `{"type":"object","properties":...}` via `ObjectSchema.to_json_schema()`
  (`nanobot/agent/tools/schema.py:217-235`). Built-ins default to `additionalProperties=False` so
  misspelled arguments are rejected before execution (`schema.py:225-229`).
- Schema fragment classes and their emitted keywords (`schema.py:20-214`): `StringSchema`
  (`type`, `description`, `minLength`, `maxLength`, `enum`), `IntegerSchema` (`minimum`, `maximum`,
  `enum`), `NumberSchema`, `BooleanSchema` (`default`), `ArraySchema` (`items` — defaulting to
  `StringSchema("")`, `minItems`, `maxItems`), `ObjectSchema` (`properties`, `required`,
  `description`, `additionalProperties`). A nullable schema emits `type: ["x","null"]`.
- `Schema.validate_json_schema_value(val, schema, path="")` (`base.py:51-121`) is the single validator:
  type checks (`integer` rejects `bool`; `number` rejects `bool` and non-finite), `enum`, numeric
  bounds, string length bounds, object `required` / `additionalProperties` (`False` → "unexpected
  parameter"), array `minItems`/`maxItems`/`items`. Error strings are human-readable, e.g.
  `f"{label} should be integer"`, `f"missing required {path}"`, `f"unexpected parameter {path}"`.
- `Schema.fragment(value)` normalizes a `Schema` instance or an existing dict to a fragment dict
  (`base.py:123-132`).

### 7.3 Registration and dispatch

`ToolRegistry` — `nanobot/agent/tools/registry.py:19`.

- `register(tool)` / `unregister(name)` invalidate the definition cache (`:30-38`).
- `get(name)` → `Tool | None` (`:40-42`); `has(name)` (`:71-73`); `tool_names` preserves insertion
  order (`:203-206`).
- `get_definitions()` (`:86-108`): cached list of `tool.to_schema()`; **built-ins and MCP tools are
  split and each group sorted by name**, then concatenated as `builtins + mcp_tools`. Ordering is
  therefore deterministic and independent of registration order — relevant for prompt caching and for
  any index-based cache markers.
- `prepare_call(name, params) -> (tool, params, error_or_None)` (`:110-147`):
  1. unknown tool → error string
     `f"Error: Tool '{name}' not found.{hint} Available: {', '.join(self.tool_names)}"` where `hint`
     is `f" Did you mean '{suggestion}'? Tool names must match exactly."` when a case/format-insensitive
     unique match exists (`:117-124`, `:53-69`);
  2. `ContextAware` legacy setter bridge (`:128-129`);
  3. `_coerce_params` → `_coerce_argument_value` (JSON-decode strings starting with `{`/`[`; `None` or
     blank → `{}`; otherwise unchanged) then `_unwrap_arguments_payload` (a sole `{"arguments": ...}`
     key is unwrapped unless the tool schema itself declares an `arguments` property)
     (`:131`, `:149-185`);
  4. non-dict params → error
     `f"Error: Tool '{name}' parameters must be a JSON object, got {type(params).__name__}. Use named
     parameters like tool_name(param1=\"value1\", param2=\"value2\") matching the tool schema."`
     (`:132-139`);
  5. `tool.cast_params(params)` then `tool.validate_params(cast_params)`; on errors →
     `f"Error: Invalid parameters for tool '{name}': " + "; ".join(errors)` (`:141-146`).
- `execute(name, params)` (`:187-201`) is the *legacy* wrapper: it calls `prepare_call`, and on error
  or on an `is_error` result appends `hint = "\n\n[Analyze the error above and try a different
  approach.]"`; exceptions become `f"Error executing {name}: {str(e)}" + hint`. **The runner path
  bypasses this wrapper** — see §3.5 and §10.2.

### 7.4 Exact tool-result content injected back into the conversation

The message appended by the runner is exactly (`runner.py:533-543`):

```json
{"role": "tool", "tool_call_id": "<call id>", "name": "<tool name>", "content": <normalized result>}
```

`content` is the value returned by `ContextGovernor.normalize_tool_result(config, tool_call_id,
tool_name, result)` (`nanobot/agent/context_governance.py:709-759`):

1. `ensure_nonempty_tool_result(tool_name, result)` first (`:715`). Semantically empty results (`None`,
   blank string, empty list, or a list whose text blocks stringify to blank) are replaced by
   `f"({tool_name} completed with no output)"` (`nanobot/utils/runtime.py:43-60`).
2. Tools in `TOOL_RESULT_OFFLOAD_EXEMPT_TOOLS = frozenset({"read_file"})` are returned as-is
   (`context_governance.py:69`, `:716-717`).
3. **String results**: `maybe_persist_tool_result(workspace, session_key, tool_call_id, text,
   max_chars=config.max_tool_result_chars)` (`:719-726`, `:732-733`).
   - `maybe_persist_tool_result` returns the content unchanged when `workspace is None`, `max_chars <=
     0`, or `len(content) <= max_chars` (`nanobot/utils/helpers.py:580-593`).
   - Otherwise it writes the full text atomically to
     `<workspace>/.nanobot/tool-results/<safe(session_key)>/<safe(tool_call_id)>.txt`
     (`helpers.py:367-368`, `:595-603`; `_TOOL_RESULTS_DIR = ".nanobot/tool-results"` at `:368`) and
     returns a rendered reference (`_render_tool_result_reference`, `helpers.py:512-531`):
     ```
     [tool output persisted]
     Full output saved to workspace path: <abs path>
     Original size: <n> chars
     Preview:
     <first 1200 chars>
     ...                       # only when the preview itself was truncated
     Preview is also truncated.
     Result truncated. Read the saved file if you need the complete output.
     ```
     (`_TOOL_RESULT_PREVIEW_CHARS = 1200`, `helpers.py:367`.) When the rendered reference itself
     exceeds `max_chars` it collapses to `f"[truncated: {reference_path}]"` (`helpers.py:529-530`).
     When `workspace is None` the persisted reference is instead truncated with
     `truncate_text(content, max_chars)` (`context_governance.py:727-728`), i.e.
     `text[:max_chars] + "\n... (truncated)"` (`helpers.py:371`, `:402-406`).
   - Persist failures log `"Tool result persist failed for {} in {}; using raw result"` and fall back
     to `truncate_text(...)` for strings, or the original object otherwise
     (`context_governance.py:752-758`).
4. **List results** (multimodal blocks): every element must be a dict with a string `type`, and any
   `text` block must carry a string `text`; otherwise the raw list is returned untouched. Valid lists
   are returned with each text block's `text` passed through `persist_text` using the id
   `f"{tool_call_id}_text_{index}"`; non-text blocks (e.g. images) pass through unchanged
   (`context_governance.py:734-751`).
5. Anything else is returned unchanged (`:759`).

---

## 8. MESSAGE / TYPE VOCABULARY

### 8.1 Conversation roles

Observed roles in the transcript vocabulary:

| Role | Produced by | Notes |
|---|---|---|
| `system` | `ContextBuilder.build_transcript` | exactly one, first (`nanobot/agent/context.py:286-297`) |
| `user` | user input, injections, recovery prompts, summary continuation markers | content may be `str` or a block list |
| `assistant` | model responses, command echoes, persisted placeholders | may carry `tool_calls`, `reasoning_content`, `thinking_blocks` |
| `tool` | tool results | requires `tool_call_id`; `name` also set by the runner |

`Session.add_message(role, content, **kwargs)` stores `{"role", "content", "timestamp", **kwargs}`
(`nanobot/session/manager.py:312-321`). Additional persisted keys observed: `media`, `_command`,
`latency_ms`, `injected_event`, `subagent_task_id`, `_channel_delivery`, `_meta` (stripped on
persistence), `PENDING_FOLLOWUP_ID_KEY`, `RUNTIME_CONTEXT_HISTORY_META`, `HIDDEN_HISTORY_META`
(`loop.py:690-711`, `loop.py:2250`, `loop.py:2268-2269`, `loop.py:2294-2300`, `manager.py:332-337`).

### 8.2 Content blocks

| Block | Shape | Source |
|---|---|---|
| text | `{"type":"text","text": <str>}` | `base.py:1110-1121`, `context.py:334-363` |
| image | `{"type":"image_url","image_url":{"url":"data:<mime>;base64,<b64>"},"_meta":{"path": <str>}}` | `context.py:343-359` |
| cache control | any block + `"cache_control": {"type":"ephemeral"}` | `openai_compat_provider.py:640-671` |
| thinking (inbound, Mistral) | `{"type":"thinking","thinking":[{"type":"text","text":...}]}` | `openai_compat_provider.py:1438-1461` |

Image MIME detection uses magic bytes (`detect_image_mime`, `nanobot/utils/helpers.py:327-336`: PNG,
JPEG, GIF87a/89a, RIFF+WEBP) with a `mimetypes.guess_type` fallback, and the block is skipped entirely
when the file is missing or the MIME is not `image/*` (`context.py:344-353`).

### 8.3 Tool-call / tool-result types

- Assistant tool call (wire/OpenAI shape): `{"id", "type":"function", "function": {"name",
  "arguments": <JSON string>}}` (`base.py:86-107`).
- Tool result: `{"role":"tool","tool_call_id","name","content"}` (`runner.py:533-543`).
- `_ALLOWED_MSG_KEYS` restricts outbound Chat messages to
  `role, content, tool_calls, tool_call_id, name, reasoning_content, extra_content`
  (`openai_compat_provider.py:89-92`).

### 8.4 Reasoning

- `reasoning_content: str` on assistant messages (`nanobot/utils/helpers.py:708-713`) — DeepSeek/Kimi/
  MiMo style.
- `thinking_blocks: list[dict]` — Anthropic extended thinking; entries with `type == "thinking"` and a
  `thinking` string are concatenated with `"\n\n"` (`nanobot/utils/helpers.py:314-321`).
- Inline tags: `extract_think`/`strip_think`/`strip_reasoning_tags`
  (`nanobot/utils/helpers.py:226-260`).

### 8.5 Typed events (`AgentEvent` hierarchy)

Base marker class `AgentEvent` (`nanobot/events.py:11-12`). Dataclasses observed:

| Event | Fields | Line |
|---|---|---|
| `ContextCompactionEvent` | `compaction_id`, `phase` ∈ {started, succeeded, failed, cancelled} | `events.py:15-18` |
| `RetryWaitEvent` | `content=""` | `events.py:21-23` |
| `RetryStatusEvent` | `state` ∈ {waiting, recovered, cleared, exhausted}, `attempt`, `max_attempts`, `error_kind`, `next_retry_at` | `events.py:26-33` |
| `RecoveryStateEvent` | `status`, `recovery_id`, `reason`, `attempts`, `can_continue` | `events.py:36-41` |
| `ProgressEvent` | `content`, `tool_hint`, `reasoning`, `reasoning_delta`, `reasoning_end`, `stream_id`, `tool_events`, `file_edit_events` | `outbound_events.py:23-32` |
| `FileEditEvent` | subclass of `ProgressEvent` | `outbound_events.py:35-37` |
| `StreamDeltaEvent` | `content`, `stream_id` | `outbound_events.py:40-43` |
| `StreamEndEvent` | `content`, `stream_id`, `resuming`, `merge_next` | `outbound_events.py:46-51` |
| `StreamedResponseEvent` | — | `outbound_events.py:54-56` |
| `TurnEndEvent` | `latency_ms`, `goal_state`, `usage`, `round_usages`, `context_window_tokens`, `outcome`, `failure_kind`, `failure_error_kind`, `failure_attempts`, `failure_message` | `outbound_events.py:59-70` |
| `GoalStatusEvent`, `GoalStateSyncEvent`, `SessionUpdatedEvent`, `UserInputEvent`, `RuntimeModelUpdatedEvent`, `TurnModelUpdatedEvent` | see lines | `outbound_events.py:73-111` |

**Two distinct "turn end" event classes — do not conflate them.** `TurnEndEvent`
(`outbound_events.py:59-70`) is an *outbound* event carried on `OutboundMessage.event`; it is
constructed by the WebUI projection layer (`nanobot/session/webui_turns.py:772`) and consumed by
`nanobot/webui/outbound_projection.py:227`. `TurnCompleted`
(`nanobot/bus/runtime_events.py:61-75`) is the *local-subscriber* event emitted by
`TurnDelivery.complete` (`nanobot/agent/turn_delivery.py:321`). The runtime-events module also
defines `TurnRuntimeAdmitted` (`runtime_events.py:44-49`), `TurnRunStatusChanged`
(`runtime_events.py:52-58`), `SessionTurnPersisted` (`runtime_events.py:78-84`), and
`GoalStateChanged` (`runtime_events.py:87-91`).

`EventSink` is a frozen dataclass holding `publish: Callable[[AgentEvent], Awaitable[None]] | None`
and `accepts_type: Callable[[type[AgentEvent]], bool] | None`; `accepts()` is true only when `publish`
is set and the type filter allows it; `emit()` swallows and logs exceptions; `NO_EVENTS` is the empty
sink (`nanobot/events.py:43-75`).

Text fallbacks for events without content (`_event_content`, `outbound_events.py:148-161`):
`"Compressing context…"`, `"Unable to compact context."`, `"Context compaction cancelled."`,
`"Context compacted."`.

---

## 9. RETRY / FALLBACK / ERRORS

### 9.1 Transient error classification

Class constants on `LLMProvider` (`nanobot/providers/base.py:626-693`):

- `_CHAT_RETRY_DELAYS = (1, 2, 4)` (`:626`)
- `_PERSISTENT_MAX_DELAY = 60` (`:627`)
- `_PERSISTENT_IDENTICAL_ERROR_LIMIT = 10` (`:628`)
- `_RETRY_HEARTBEAT_CHUNK = 30` (`:629`)
- `_TRANSIENT_ERROR_MARKERS = ("429","rate limit","500","502","503","504","overloaded","timeout",
  "timed out","connection","server error","server_error","temporarily unavailable","速率限制",
  "访问量过大")` (`:630-646`)
- `_RETRYABLE_STATUS_CODES = {408, 409, 429}` (`:647`)
- `_TRANSIENT_ERROR_KINDS = {"timeout","connection"}` (`:648`)
- `_NON_RETRYABLE_429_ERROR_TOKENS` (`:649-658`) and `_RETRYABLE_429_ERROR_TOKENS` (`:659-666`)
- `_NON_RETRYABLE_429_TEXT_MARKERS` (`:667-682`) and `_RETRYABLE_429_TEXT_MARKERS` (`:683-693`)

`is_transient_response(response)` (`:1011-1028`), in priority order:
1. `error_should_retry is not None` → return it verbatim (structured metadata wins).
2. `error_status_code` present: `429` → `_is_retryable_429_response(response)`; `408/409/429` or
   `>= 500` → `True`.
3. `error_kind` in `{"timeout","connection"}` → `True`.
4. Otherwise substring-match `_TRANSIENT_ERROR_MARKERS` against `content.lower()`
   (`_is_transient_error`, `:1006-1009`).

`_is_retryable_429_response` (`:1087-1107`): non-retryable semantic tokens or non-retryable text
markers → `False`; retryable tokens or text markers → `True`; **"Unknown 429 defaults to WAIT+retry"**
→ `True` (`:1106-1107`). The semantic tokens are `cls._normalize_error_token(response.error_type)` and
`cls._normalize_error_token(response.error_code)` (`:1089-1090`; true source verified by `od`), where
`_normalize_error_token` is `str(value).strip().lower()` or `None` (`:1053-1058`).

### 9.2 Retry loop

All citations in this subsection are `nanobot/providers/base.py` unless prefixed otherwise.

`_run_with_retry` (`base.py:1737-1917`):

- `attempt` starts at 0 and increments before each call; `persistent = retry_mode == "persistent"`
  (`:1750-1752`).
- Non-error response → emit `"recovered"` status if `attempt > 1` and return (`:1774-1776`).
- **Streaming guard**: when `should_retry_guard()` is false (content already streamed):
  - if `error_kind == "timeout"`: if an `on_stream_recover` callback exists, log
    "LLM stream stalled after content was emitted; starting a new stream segment and retrying" and
    call it; otherwise log the "suppressing delta callbacks" variant and **null out
    `on_content_delta`/`on_thinking_delta`/`on_tool_call_delta`** and clear the guard
    (`:1778-1796`);
  - otherwise log "LLM stream failed after content was emitted; skipping retry", emit `"cleared"`,
    and return the response (`:1797-1802`).
- Identical-error accounting keys on `content.strip().lower()` (`:1803-1808`).
- **Non-transient errors** (`:1810-1850`): before giving up, if the request contains image content
  (either in `messages` or inside a `ProviderCallContext` state), the request is retried **once
  without images**; on success the images are stripped from the original messages in place so later
  iterations do not repeat the cycle (`:1811-1848`).
- **Persistent mode** stops after `_PERSISTENT_IDENTICAL_ERROR_LIMIT` (10) identical transient errors,
  emitting `"exhausted"` (`:1852-1871`).
- **Standard mode** gives up when `attempt > len(delays)` (i.e. after 4 attempts total), emitting
  `"exhausted"` with `max_attempts = len(delays) + 1 = 4` (`:1873-1892`).
- Delay: `base_delay = delays[min(attempt - 1, len(delays) - 1)]` → 1, 2, 4, 4, …;
  `delay = retry_after + RETRY_AFTER_BUFFER if retry_after else base_delay` with
  `RETRY_AFTER_BUFFER = 1` (`:31`, `:1894-1896`); persistent mode clamps to `_PERSISTENT_MAX_DELAY`
  (`:1897-1898`).
- Sleep is chunked by `_RETRY_HEARTBEAT_CHUNK = 30` s, re-emitting `on_retry_wait` and a `"waiting"`
  `RetryStatusEvent` before each chunk (`:1689-1721`).
- Fallthrough return: `last_response` if any, else a fresh call (`:1917`).

`retry_after` extraction (`:1614-1687`):
- `_extract_retry_after(text)` matches four regexes for "retry after N [unit]", "try again in N unit",
  "wait N unit before retry", and `retry[-_]?after` with `N` (`:1617-1630`); units `ms|milliseconds` →
  `max(0.1, v/1000)`, `m|min|minutes` → `max(0.1, v*60)`, default seconds → `max(0.1, v)`
  (`:1632-1639`).
- `_extract_retry_after_from_headers` prefers `retry-after-ms` (ms → s, must be `> 0`), then
  `retry-after` as a number of seconds, then as an HTTP date parsed by `parsedate_to_datetime` with
  remaining seconds `max(0.1, ...)` (`:1641-1679`).
- `_extract_retry_after_from_response` prefers `error_retry_after_s > 0`, then `retry_after > 0`, then
  parses the content text (`:1681-1687`).

### 9.3 Arrearage / quota

Citations here are `nanobot/providers/base.py` unless prefixed.

`is_arrearage_response(response)` (`base.py:1030-1051`): true when `error_status_code == 402`, or when
`error_type`/`error_code` (normalized lowercase) is in `_NON_RETRYABLE_429_ERROR_TOKENS`, or when the
lowercased content contains any `_NON_RETRYABLE_429_TEXT_MARKERS`. The true source of the two token
lines was verified by `od` as `cls._normalize_error_token(response.error_type)` /
`cls._normalize_error_token(response.error_code)` (`:1041-1042`).

User-visible text (`runner.py:66-69`):

> The AI provider rejected the request because the API key is out of quota or the account is in
> arrears. Please top up / check the billing status of your API key and try again.

This is selected in the runner's error terminal (`runner.py:715-719`) and is also what gets persisted
into `final_content`/`error`.

`public_error_kind` (`:1723-1735`) returns, in order: `"billing"` (arrearage), `error_kind` when it is
`"connection"`/`"timeout"`, `"rate_limit"` for status 429, `"server"` for status `>= 500`, else
`"unknown"`.

### 9.4 Fallback provider

`FallbackProvider` — `nanobot/providers/fallback_provider.py:102`; constructed by the factory only
when `config.agents.defaults.fallback_models` is non-empty
(`nanobot/providers/factory.py:287-295`). The factory passes
`provider_factory=lambda fb: _make_provider_core(config, preset=fb)` so fallback providers are plain
(non-recursive) providers (`factory.py:293`; recursion note at `fallback_provider.py:117`).

- Circuit breaker: `_PRIMARY_FAILURE_THRESHOLD = 3`, `_PRIMARY_COOLDOWN_S = 60` (`:28-29`);
  `_primary_available()` is half-open after the cooldown (`:193-200`).
- Failover is request-scoped; each candidate runs its **own** retry policy first
  (`_retry_with_fallback` calls each candidate with `retry_mode="standard"`, `:368-381`).
- Failover is skipped when content was already streamed, **except** for stream timeouts, which trigger
  `on_stream_recover` and continue in a new segment (`:476-493`, `:517-531`).
- Fallback request kwargs override `model`, `max_tokens`, `temperature` from the fallback preset
  (`:558-563`); `reasoning_effort` is removed when the preset does not define one (`:583-586`);
  conversation state is dropped when the fallback provider cannot resume it (`:564-582`).
- The fallback-model observer is only notified **after** a usable (non-error) fallback response
  (`:596-606`), with the rationale in the comment at `:597-600`.
- On total failure the last error response is returned with
  `preserve_provider_state_on_error=preserve_primary_state` (`:615-624`). If the primary was skipped
  by the circuit breaker and nothing else answered, a synthetic transient error is returned:
  `content=f"Primary model '{primary_model}' circuit open and no fallbacks available"`,
  `finish_reason="error"`, `error_should_retry=True`, and `error_retry_after_s` = remaining cooldown
  (`:625-641`).
- `_should_fallback(response)` (`:671-712`) order: arrearage → `True`; authentication kind/tokens →
  `True`; non-fallbackable kinds (`content_filter`, `refusal`, `context_length`, `invalid_request`) →
  `False`; status `401/403` → `True`; authentication text tokens → `True`; `error_should_retry is
  False` → `False`; status `{400,404,422}` → `False`; `error_should_retry is True` → `True`; status
  `{408,409,429}` or `5xx` → `True`; fallbackable kinds
  (`timeout`,`connection`,`server_error`,`rate_limit`,`overloaded`) → `True`; else token scan of
  `kind`/`error_type`/`error_code`/`text` against `_FALLBACK_ERROR_TOKENS` (which includes `"empty"`,
  `"insufficient_quota"`, `"billing_hard_limit"`, `"balance"`, `"out of credits"`, etc.,
  `:73-96`).

### 9.5 Error text that surfaces to the user

| Situation | Text | Line |
|---|---|---|
| Provider error, non-arrearage | `clean or spec.error_message or "Sorry, I encountered an error calling the AI model."` | `runner.py:65`, `runner.py:719` |
| Provider error, arrearage | billing/quota message | `runner.py:66-69`, `runner.py:716-717` |
| Blank final response after retries | `"I completed the tool steps but couldn't produce a final answer. Please try again or narrow the task."` | `nanobot/utils/runtime.py:19-22`, `runner.py:738` |
| Max iterations exhausted | rendered `agent/max_iterations_message.md` (or `spec.max_iterations_message`) | `runner.py:1258-1268` |
| Unhandled exception in `_dispatch` | `"Sorry, I encountered an error."` | `turn_delivery.py:341` |
| Stream stalled | `"Error calling LLM: stream stalled for more than {n} seconds"` | `openai_compat_provider.py:2191-2199` |
| Empty choices from API | `"Error: API returned empty choices."` (finish_reason `error`, error_kind `empty`) | `openai_compat_provider.py:1567-1570`, `:1637-1642` |
| HTTP error body | `"Error: {body[:500]}"` or `"Error calling LLM: {e}"` | `openai_compat_provider.py:1903-1909` |
| Unexpected exception in `chat`/`chat_stream` | `"Error calling LLM: {detail}"` | `base.py:995-1004` |
| Persisted placeholder for a failed model turn | `"[Assistant reply unavailable due to model error.]"` | `runner.py:70`, `runner.py:1370-1374` |
| Retry notification text | `"Model request failed, retry in {n}s (attempt {a})."` / `"persistent retry"` variant | `base.py:1703-1708` |
| Retry exhaustion | `"Model request failed after {n} attempts, giving up."` / `"Persistent retry stopped after {n} identical errors."` | `base.py:1858-1861`, `:1879-1882` |
| Tool error suffix | `"\n\n[Analyze the error above and try a different approach.]"` | `execution.py:22` |
| SSRF boundary note | non-bypassable security boundary text | `execution.py:30-37` |
| Workspace escalation | "refusing repeated workspace-bypass attempts" text | `nanobot/utils/runtime.py:192-200` |

---

## 10. GOTCHAS — what silently breaks a naive Go port

**10.1 `stop_reason = "tool_error"` is read but never written.** Delivery maps `"tool_error"` to
`("failed","tool")` (`turn_delivery.py:68`) and `_assemble_outbound` excludes it from
`StreamedResponseEvent` (`loop.py:1735`), but no assignment of `"tool_error"` exists in `runner.py`.
Observed runner assignments are only `completed`, `error`, `empty_final_response`, `max_iterations`.
A port that invents a `tool_error` stop reason changes observable behavior; a port that removes the
branch changes nothing today.

**10.2 The runner bypasses `ToolRegistry.execute`.** `_execute_tool_call` calls `tools.prepare_call`
and then `tool.execute(**params)` directly (`execution.py:136-174`). The hint-appending wrapper at
`registry.py:187-201` only runs when `prepare_call` is unavailable. Error strings therefore come from
`execution.py` (`_with_retry_hint(f"Error: {type(exc).__name__}: {exc}")`), not from
`registry.execute`'s `"Error executing {name}: ..."` form.

**10.3 The runner always calls the streaming entry point.** Even when the hook does not want
streaming, `_request_model` calls `provider.chat_stream_with_retry(...)` with no delta callbacks
(`runner.py:992-996`), which routes through `_safe_chat_stream` → `chat_stream`
(`base.py:1500-1503`, `base.py:1601`). A Go port that implements only a non-streaming `chat` path
would silently change wire behavior (`stream: true` + `stream_options.include_usage`), usage
extraction, and stall detection.

**10.4 Tool calls with `finish_reason == "stop"` DO execute.** `should_execute_tools` accepts
`"stop"` (`base.py:602-608`). Conversely, calls under `refusal`, `content_filter`, or `error` are
dropped with only a warning log (`runner.py:581-586`). A naive port that keys tool execution solely on
`finish_reason == "tool_calls"` loses real behavior.

**10.5 `ToolResult` is a `str` subclass with an `is_error` flag.** Any port that models tool output as
a struct must still route error results through the same string-based path (retry hint, 120-char event
detail, `persist_text` offloading). Note `normalize_tool_result` checks `isinstance(result, str)`
first, so `ToolResult` values take the string branch.

**10.6 `arguments` may legitimately be a non-dict.** `parse_tool_arguments` returns the *original
string* for malformed JSON and for JSON `null` (`base.py:117-130`), and the registry rejects non-dict
params with a specific user-facing message (`registry.py:132-139`). Silently coercing to `{}` would
execute tools with empty parameters.

**10.7 Duplicate/empty tool-call IDs are actively repaired in three places** — non-streaming parse
(`openai_compat_provider.py:1605-1625`), streaming aggregation (`:1816-1823`), and request
sanitization for Mistral (`:673-684`, `:749-758`). Without this, `tool` messages can collide and the
provider rejects the next request.

**10.8 Message persistence has destructive filters.** Empty assistant messages are dropped
(`loop.py:2211-2212`); tool results whose `tool_call_id` is missing, undeclared, or already fulfilled
are dropped (`loop.py:2213-2227`); `image_url` data-URL blocks are replaced by placeholders
(`loop.py:2090-2115`). A port that persists the runner's message list verbatim will corrupt session
history and can permanently wedge a session with unmatched tool calls.

**10.9 Role alternation rewrites history.** `_enforce_role_alternation` merges consecutive same-role
messages (assistant: replaces on incoming tool calls, skips on existing tool calls; user: concatenates
text or merges block lists), **pops all trailing assistant messages**, recovers the last popped
assistant as a `user` message when only system messages would remain, and inserts a synthetic user
message `"(conversation continued)"` if the first non-system message would be a bare assistant message
(`base.py:1123-1197`; constant at `base.py:620`). This runs on every OpenAI-compatible request
(`openai_compat_provider.py:795`).

**10.10 Empty content is rewritten, not dropped.** `_sanitize_empty_content` turns an empty string
content into `None` for assistant messages with tool calls and into `"(empty)"` otherwise, removes
empty text blocks, and strips `_meta` keys (`base.py:813-873`). It also scrubs lone UTF-16 surrogates
from every string leaf (`base.py:870-873`) — Go must replicate this or JSON encoding of some inputs
will fail differently.

**10.11 Retry budget is 4 total attempts in standard mode, not 3.** `_CHAT_RETRY_DELAYS = (1,2,4)`
gives delays for attempts 1–3, and the loop only gives up when `attempt > len(delays)`, i.e. on
attempt 4 (`base.py:1873`). Delays are capped at the last element for any later attempt
(`base.py:1895`).

**10.12 `retry_after` adds a 1-second buffer** and, when present, *replaces* the backoff schedule
rather than being combined with it (`base.py:31`, `base.py:1894-1896`).

**10.13 Two different timeouts.** The HTTP request timeout is 120 s
(`openai_compat_provider.py:132`) while the per-stream-chunk idle timeout is 90 s
(`base.py:29`), and the streaming call overrides the SDK timeout with the idle timeout
(`openai_compat_provider.py:2125`).

**10.14 A stream without a finish reason is an error.** `completed` is derived only from
`choices[0].finish_reason` (`openai_compat_provider.py:2143-2144`) and a missing one raises
`ConnectionError` (`:2188-2189`). A port that accepts a clean EOF as success will hang sessions in
`stop` with truncated content.

**10.15 Provider-hosted tool events must be closed on error.** Active hosted tool calls are tracked by
`call_id` and only closed with `phase="error"` after the provider returns its final error response
(`runner.py:941-953`, `runner.py:1019-1029`) — the comment explains that `chat_stream_with_retry` may
recover internally, so closing earlier would be wrong.

**10.16 Injection accounting is two-dimensional.** At most `_MAX_INJECTIONS_PER_TURN = 3` messages
per drain, and at most `_MAX_INJECTION_CYCLES = 5` *real* injection cycles per turn; the extra
`wait_at_terminal` drain does not increment the cycle counter (`runner.py:73-74`, `runner.py:177-194`,
`runner.py:289-295`). Caller-requested continuations (the goal callback) also do not increment the
counter (`runner.py:193-194`, `runner.py:220-221`).

**10.17 Leftover pending messages are re-published, not dropped.** `_dispatch`'s `finally` drains the
per-session pending queue back onto the bus (`loop.py:1489-1513`). Skipping this loses user messages
silently, or (worse) leaves them in a queue that a later task believes it owns.

**10.18 Cancellation has two distinct behaviors.** Normal `/stop` materializes partial context by
restoring the runtime checkpoint; gateway shutdown deliberately does not, so the recovery coordinator
can offer "Continue" (`loop.py:1454-1462`, `loop.py:1381-1389`).

**10.19 Tool definition ordering is normalized, not registration-ordered.** `get_definitions` sorts
built-ins and `mcp_`-prefixed tools separately and concatenates (`registry.py:86-108`). Any prompt
cache marker indices computed over the tool list (`base.py:889-906`) depend on this ordering.

**10.20 `subagent` results are hidden history.** Subagent follow-ups are persisted as `assistant`
messages tagged `injected_event="subagent_result"` and de-duplicated by `subagent_task_id`
(`loop.py:2274-2301`), and injected rows carry `HIDDEN_HISTORY_META` plus `injected_event`
(`loop.py:1044-1054`). Losing these tags changes what the model sees and can duplicate results.

**10.21 `suppress_response` depends on captured tool-side deliveries.**
`capture_message_deliveries()` (`loop.py:1980`) plus the route comparison at `loop.py:2003-2008`
decide whether the final assistant text is suppressed. A port without this will double-post messages
when a `message` tool already replied.

**10.22 Error conversion happens twice.** `OpenAICompatProvider.chat` catches broad exceptions
(`openai_compat_provider.py:2012-2013`) and `_safe_chat`/`_safe_chat_stream` catch again
(`base.py:1288-1289`, `base.py:1433-1434`). Which layer wins determines whether provider-specific
error metadata (`error_status_code`, `error_retry_after_s`, `x-should-retry`) is populated; a port
with a single conversion point can lose the metadata the retry policy depends on.

**10.23 `_SENTINEL` default semantics.** `chat_with_retry`/`chat_stream_with_retry` distinguish
"argument omitted" from "argument explicitly `None`": both `_SENTINEL` and `None` fall back to
`self.generation` for `max_tokens`/`temperature`, but only `_SENTINEL` does for `reasoning_effort`
(`base.py:1448-1468`, `base.py:1517-1541`). Go has no equivalent distinction without pointers or a
similar sentinel.

**10.24 Prompt-cache markers mutate message/tool shapes.** `_apply_cache_control` converts a string
system message into a block list with `cache_control` and marks the second-to-last message and up to
two tool entries (`openai_compat_provider.py:640-671`). This only runs for specs with
`supports_prompt_caching` and Anthropic-style model names (`:942-945`).

---

## 11. HIGHEST-RISK COMPATIBILITY GAPS

Ranked by likelihood of producing silent behavioral divergence in a Go port.

1. **Streaming-only wire path (§10.3).** The runner never uses non-streaming `chat`. Reproducing
   `stream: true` + `stream_options.include_usage` + per-chunk idle timeout + "no finish reason is an
   error" is mandatory for fidelity.

2. **Tool-result persistence filters (§10.8).** Dropping invalid/duplicate tool results and empty
   assistant messages is what keeps sessions from wedging permanently. Omitting it produces a
   session that fails on every subsequent request.

3. **Role alternation + empty-content rewriting (§10.9, §10.10).** These mutate every outbound request
   body. Providers that reject consecutive same-role messages or trailing assistant messages will fail
   immediately; the failure surfaces as a provider error, not as a local exception.

4. **Retry classification and budget (§9.1, §9.2, §10.11–10.13).** The exact precedence
   (`error_should_retry` → status → kind → text markers), the "unknown 429 retries" default, the
   4-attempt budget, `retry_after + 1`, and the streamed-content guard together determine latency and
   cost characteristics. Any one of them mis-ported changes observable retry behavior.

5. **Arrearage vs transient 429 separation (§9.3).** Conflating quota exhaustion with rate limiting
   causes infinite retries against a hard-billing failure, or suppresses retries that should happen.

6. **Tool-call ID repair and Gemini thought signatures (§10.7, §6.2).** Both are provider-specific
   correctness workarounds that only fail on specific upstreams, and they fail hard (whole request
   rejected) rather than degrading.

7. **Injection accounting and re-publication (§10.16, §10.17).** Off-by-one in the cycle counters
   either loops forever or silently drops user follow-ups mid-turn; the two-dimensional cap is easy to
   collapse into one.

8. **Cancellation duality (§10.18).** Materializing partial context on `/stop` but not on gateway
   shutdown is a deliberate product behavior that is invisible in a naive implementation.

9. **`should_execute_tools` including `finish_reason == "stop"` (§10.4).** A port that requires
   `tool_calls` as the finish reason will drop legitimate tool calls from providers that report
   `stop`.

10. **Fallback chain semantics (§9.4).** Per-candidate retry before advancing, no failover after
    content was streamed (except timeouts), circuit breaker half-open after 60 s, and only notifying
    the model observer on a *successful* fallback. Collapsing this into "try each provider once"
    changes both user-visible output duplication and recovery timing.

11. **`ProviderConversationState` round-tripping (§4.6).** Opaque state must survive persistence,
    de-duplicate pending messages across turns, and be dropped when the provider cannot resume it.
    Losing `pending_messages` duplicates input to Responses-API providers; losing the capability check
    sends stale state to a switched provider.

12. **`LLMUsage` invariants (§4.4).** `reported + estimated == total`, `total >= input + output`,
    cache sums only when both sides are known, and `context_tokens` taking the right operand. These
    feed budget/compaction decisions; violations raise `ValueError` in Python and would silently pass
    in Go.

---

## Appendix A — Explicit UNVERIFIED items

1. **`Authorization` header construction.** The API key is passed to `AsyncOpenAI`
   (`openai_compat_provider.py:604-612`); the resulting `Authorization: Bearer ...` header is OpenAI
   SDK behavior and is **not present in this repository**. UNVERIFIED in-repo.
2. **`nanobot/providers/openai_responses/*` internals** — `ResponsesStreamCapture`,
   `consume_sdk_stream`, `parse_response_output`, `prepare_responses_input`,
   `build_responses_state`, `resolve_compact_threshold`, `is_compaction_compatibility_error` were not
   read. The Responses API path is documented in §6.5 only at the call-site level.
3. **Non-OpenAI provider implementations** (`anthropic_provider.py`, `bedrock_provider.py`,
   `azure_openai_provider.py`, `github_copilot_provider.py`, `openai_codex_provider.py`,
   `xai_grok_provider.py`) were not read; only the base contract and factory wiring are specified.
4. **`nanobot/runtime_context.py`** (`RuntimeContextBlock`, `append_runtime_context`,
   `resolve_runtime_context`, the exact metadata marker shapes) was not read; §2 cites only the call
   sites in `loop.py`/`context.py`.
5. **Compaction/context-governance internals.** `ContextGovernor.prepare_request`,
   `prepare_messages_for_model`, `summarize_provider_compaction`, `request_messages`,
   `accept_request`, and the auto-compaction/summary machinery were not fully read. Only
   `normalize_tool_result` (§7.4) and the `TOOL_RESULT_OFFLOAD_EXEMPT_TOOLS` constant were verified.
6. **`nanobot/session/manager.py` persistence format.** The JSONL on-disk layout, atomic-write
   mechanics, `save_runtime_checkpoint` file layout, and TTL/auto-compaction triggers were not read in
   full; §2 cites `Session.add_message`, `Session.get_history`, `SessionPolicy`, and the save call
   sites only.
7. **Channel-side rendering.** How each channel consumes `OutboundMessage.event`,
   `ProgressEvent.tool_events`, `FileEditEvent`, and stream segment ids was not read beyond
   `channels/manager.py:775-800` and `channels/base.py:305-321`.
8. **`nanobot/command/builtin.py` handler bodies** other than `cmd_stop` and `cmd_restart` were not
   read; only the registration table (`builtin.py:1074-1102`) is specified.
9. **`ProviderSpec` field inventory.** `nanobot/providers/registry.py` (821 lines) was only sampled;
   the spec fields referenced in §6 (`thinking_style`, `model_overrides`, `reasoning_effort_remap`,
   `implicit_reasoning_models`, `supports_prompt_caching`, `supports_max_completion_tokens`,
   `responses_models`, `responses_default_tools`, `strip_model_prefix`, `strip_model_prefixes`,
   `strip_history_reasoning_content`, `reasoning_as_content`, `extract_thinking_blocks`, `is_local`,
   `is_direct`, `is_oauth`, `is_transcription_only`, `default_api_base`, `default_extra_headers`,
   `backend`) are verified by their use sites, not by a full read of the definition.
10. **`llm_usage` subsystem** (`bind_llm_usage_source`, `LLMCallRecord`, `source_from_session_key`)
    was not read; §3.2 cites the call site only.
