# Telegram transport phase — remaining inventory

Reference: `upstream/nanobot/nanobot/channels/telegram/runtime.py` @
`1bb712d3488915ca4ed9ccc1a93067ff722f5ab9` (v0.3.5), 2231 lines.

This document is the deliverable that accompanies the pure-core port in
`internal/channels/telegram/`. It lists **precisely** what is not in that
package and why, so the transport phase can be planned against a closed list
rather than against a re-read of the Python.

Everything below is `PROPOSED`/`NOT IMPLEMENTED` — none of it is claimed as
built. Line ranges are from the frozen file and were taken from the module's own
AST, not from reading.

---

## 1. Why there is a boundary at all

`nanobot/channels/telegram/runtime.py` cannot be imported without
`python-telegram-bot`:

```
$ .tools/venv/bin/python -c "import nanobot.channels.telegram.runtime"
ModuleNotFoundError: No module named 'telegram'
```

The reference's transport *is* that library. There is no Go equivalent, and this
port may not add a dependency (stdlib only). The split is therefore:

* **pure core** — every function whose result depends only on its arguments.
  Ported, and pinned by 5281 differential cases against the reference process.
* **transport phase** — everything that needs a live Bot API connection, an
  asyncio task tree, or PTB's object model. Not ported.

The boundary is not "easy vs hard": it is "decidable from arguments" vs
"observable only against api.telegram.org".

---

## 2. Ported, for contrast

| Reference symbol | Lines | Go |
| --- | --- | --- |
| module constants | 44–73, 388–390 | `constants.go` |
| `_split_telegram_markdown` | 102–185 | `SplitMarkdown` |
| `_escape_telegram_html` | 188–190 | `EscapeHTML` |
| `_tool_hint_to_telegram_blockquote` | 193–195 | `ToolHintBlockquote` |
| `_strip_md` | 198–204 | `StripMarkdownInline` |
| `_strip_md_block` | 207–232 | `StripMarkdownBlock` |
| `_render_table_box` | 235–264 | `RenderTableBox` |
| `_markdown_to_telegram_html` | 267–354 | `MarkdownToHTML` |
| `_split_telegram_markdown_html_chunks` | 357–380 | `SplitMarkdownHTMLChunks` |
| `_split_telegram_markdown_html` | 383–385 | `SplitMarkdownHTML` |
| `TelegramConfig` + both validators | 413–465 | `Config`, `ParseConfig` |
| `_TELEGRAM_COMMAND_ALIASES` | 468–472 | `CommandAliases` |
| `_TELEGRAM_DISPLAY_COMMAND_RE` | 474 | `DisplayCommandReSource` + hand-written scanner |
| `_telegram_command_text` | 479–500 | `DisplayCommandText` |
| `TelegramChannel.is_allowed` | 574–591 | `SenderPolicy.IsAllowed` |
| `TelegramChannel._normalize_telegram_command` | 594–601 | `NormalizeCommand` |
| `TELEGRAM_BUS_SLASH_COMMAND_RE` | 539–542 | `BusSlashCommandRe` |
| `TelegramChannel._has_mention_entity` | 1777–1800 | `HasMentionEntity` |
| `TelegramChannel._is_group_message_for_bot` | 1802–1827 | `IsGroupMessageForBot` |
| `TelegramChannel.default_config` | 545–546 | `DefaultConfig` |
| `validation.py` in full | 1–165 | `Validate` and its helpers |
| `manifest.py` `SETUP_SPEC` | 8–33 | `SETUP_SPEC` |

Two class attributes that are data rather than behaviour are also ported:
`TelegramChannel.name` (→ `ChannelName`) and `GROUP_POLICIES`
(→ `GroupPolicies`).

`TelegramChannel.BOT_COMMANDS` (508–527) is **not** ported. It is pure data and
could be, but its only consumer is `set_my_commands`, which is transport. It
belongs with the transport phase; carrying it now would be a table nobody reads.

---

## 3. Remaining surface

### 3.1 PTB machinery — exists only because the transport is a library

| Symbol | Lines | Notes |
| --- | --- | --- |
| `TelegramApplication` TypeAlias | 62 | `Application[Any × 6]` |
| `_LivenessTrackedRequest` | 76–99 | Wraps PTB's `BaseRequest` pool to time each `getUpdates` round trip. Has no Go analogue: `net/http` gives the round trip directly. |
| `TelegramChannel.__init__` | 548–567 | 14 instance attributes; 5 of them (`_typing_tasks`, `_media_group_*`, `_inbound_workers`, `_stream_bufs`, `_compaction_notices`) are asyncio task registries. |
| `_require_app` | 569–572 | Raises when `self._app is None`. |
| `_StreamBuf` | 394–400 | Per-chat streaming accumulator: `text`, `message_id`, `draft_id`, `last_edit`, `stream_id`. |
| `_QueuedTelegramUpdate` | 404–410 | `kind`, `update`, `context`, `sort_key`. Holds a PTB `Update` and `ContextTypes.DEFAULT_TYPE`, so it cannot be ported without the object model. |

### 3.2 Application lifecycle

| Symbol | Lines | Notes |
| --- | --- | --- |
| `start` | 603–657 | Builds the `Application`, installs handlers, `add_error_handler`. |
| `_start_app` | 659–768 | `initialize` → `start` → `getMe` → `set_my_commands` → `start_webhook`/`start_polling`. |
| `_is_transient_startup_error` | 771–778 | String-matches PTB's startup errors. |
| `_wait_for_app` | 780–795 | Awaits `self._app_ready`. |
| `_teardown_app` | 817–834 | Under `self._teardown_lock`. |
| `stop` | 836–858 | Cancels typing tasks, drains, stops the updater. |
| `_idle` | 811–815 | `asyncio.Event().wait()`. |
| `_app_ready`, `_teardown_lock` | 565–566 | The restart handshake. |

Restart backoff (`RESTART_BACKOFF_INITIAL_SECONDS`, `RESTART_BACKOFF_MAX_SECONDS`,
`APP_RESTART_SEND_WAIT_SECONDS`) is already in `constants.go` as data but no
code consumes it yet.

### 3.3 Poll liveness

| Symbol | Lines | Notes |
| --- | --- | --- |
| `_note_poll_ok` | 797–800 | Records `time.monotonic()`. |
| `_watch_polling` | 802–809 | Samples every `POLL_WATCH_INTERVAL`, restarts past `POLL_STALE_SECONDS`. |
| `_on_polling_error` | 2131–2137 | PTB error callback. |

### 3.4 Sending

| Symbol | Lines | Bot API |
| --- | --- | --- |
| `send` | 1059–1193 | `sendMessage`, `sendPhoto`, `sendDocument` |
| `call_with_retry` | 1195–1228 | retry/backoff (`_SEND_MAX_RETRIES`, `_SEND_RETRY_BASE_DELAY`) |
| `_send_text` | 1230–1263 | `sendMessage` with `parse_mode=HTML`, reply context, threads |
| `_is_not_modified_error` | 1266–1267 | "message is not modified" |
| `_send_compaction_notice` | 1269–1314 | `sendMessage` + `editMessageText`, bounded by `COMPACTION_NOTICES_MAX` |
| `_format_telegram_error` | 2116–2129 | error text shaping |
| `_get_extension` | 2148–2173 | MIME → file extension |

### 3.5 Streaming edits

| Symbol | Lines | Bot API |
| --- | --- | --- |
| `send_delta` | 1316–1518 | `editMessageText`, `sendMessage` |
| `_flush_stream_overflow` | 1520–1556 | `editMessageText` |
| `_flush_stream_overflow_legacy` | 1558–1616 | `editMessageText`, `sendMessage` |
| `_start_legacy_stream` | 1036–1057 | `sendMessage` |
| `_is_not_modified_error` | 1266–1267 | (shared) |

`_STREAM_EDIT_INTERVAL_DEFAULT` is already a Go constant; the throttle that uses
it is here.

### 3.6 Rich messages (Bot API ≥ 10.1)

| Symbol | Lines | Notes |
| --- | --- | --- |
| `_is_rich_capability_error` | 879–889 | Detects "method not found" to latch off. |
| `_rich_streaming_enabled` | 891–892 | config + latch |
| `_rich_message_payload` | 895–896 | |
| `_mark_rich_unavailable` | 898–903 | |
| `_try_send_rich` | 905–959 | |
| `_new_rich_draft_id` | 962–964 | |
| `_try_send_rich_draft` | 966–995 | `sendMessageDraft`, throttled by `TELEGRAM_RICH_DRAFT_MIN_INTERVAL` |
| `_try_send_stream_rich` | 997–1034 | |

`_rich_send_disabled` is a runtime latch, so this whole group is stateful and
unverifiable offline.

### 3.7 Media

| Symbol | Lines | Bot API |
| --- | --- | --- |
| `_get_media_type` | 861–872 | |
| `_is_remote_media_url` | 875–876 | |
| `_download_message_media` | 1711–1763 | `getFile` + `download_to_drive` |
| `_extract_reply_context` | 1687–1709 | |
| `_flush_media_group` | 2051–2065 | album buffering |
| `_media_group_buffers`, `_media_group_tasks` | 556–557 | |

`_download_message_media` also calls `transcribe_audio`, which is a provider
dependency outside this channel.

### 3.8 Inbound routing

| Symbol | Lines | Notes |
| --- | --- | --- |
| `on_message` | 1947–1954 | |
| `process_message_update` | 1956–2049 | the main handler |
| `_sender_id` | 1646–1649 | |
| `_send_pairing_code_if_private` | 1651–1662 | |
| `_derive_topic_session_key` | 1665–1670 | |
| `_build_message_metadata` | 1673–1685 | |
| `_remember_thread_context` | 1829–1837 | |
| `_queue_key_for_message` | 1840–1842 | |
| `_sort_key_for_update` | 1845–1850 | |
| `_enqueue_ordered_update` | 1852–1875 | per-session ordering |
| `_drain_ordered_updates` | 1877–1907 | the worker |
| `_forward_command` | 1909–1916 | |
| `_process_forward_command` | 1918–1945 | |
| `_ensure_bot_identity` | 1765–1774 | cached `getMe`; the DECISION it feeds is ported as `IsGroupMessageForBot` |

`_forward_command`/`_process_forward_command` are the only consumers of
`BusSlashCommandRe`; the regex is ported and tested, the dispatch is not.

### 3.9 Typing indicator, reactions, callbacks

| Symbol | Lines | Bot API |
| --- | --- | --- |
| `_start_typing` | 2067–2071 | |
| `_stop_typing` | 2073–2077 | |
| `_typing_loop` | 2105–2113 | `sendChatAction` |
| `_add_reaction` | 2079–2090 | `setMessageReaction` |
| `_remove_reaction` | 2092–2103 | `setMessageReaction` |
| `_build_keyboard` | 2175–2183 | inline keyboards |
| `_safe_callback_data` | 2186–2191 | 64-byte callback budget |
| `_buttons_as_text` | 2194–2196 | |
| `_on_callback_query` | 2198–2231 | `answerCallbackQuery` |

`inlineKeyboards` is a config field in the ported `Config` but nothing consumes
it yet.

### 3.10 Command handlers that reach into the agent

| Symbol | Lines |
| --- | --- |
| `_on_start` | 1618–1632 |
| `_on_help` | 1634–1643 |
| `_on_error` | 2139–2146 |

These publish to the `MessageBus`; they need `internal/core` and the channel
manager's dispatch contract, not the Bot API, so they could land before the
transport proper.

---

## 4. Bot API method inventory

Every method the reference calls, with the call sites:

| Method | Call sites |
| --- | --- |
| `getMe` | 736, 1771 |
| `setMyCommands` | 742 |
| `getUpdates` | via `start_polling` (762) |
| `setWebhook` | via `start_webhook` (750) |
| `sendMessage` | 1050, 1153, 1244, 1254, 1288, 1412, 1599, 1607 |
| `editMessageText` | 1305, 1382, 1399, 1508, 1572, 1584 |
| `sendPhoto` | 1109 |
| `sendDocument` | 1113 |
| `sendChatAction` | 2110 |
| `setMessageReaction` | 2084, 2097 |
| `getFile` | 1741 |
| `answerCallbackQuery` | inside `_on_callback_query` (2198–2231) |
| rich-message draft send | inside `_try_send_rich*` (905–1034) |
| `deleteWebhook` | via PTB's `start_polling` teardown |

`getMe` is the one method already reachable from this package: `GetMe` in
`validate.go` speaks it over `net/http` with a 4-second timeout, because
`validation.py` calls it. That is the only transport code in the ported package,
and it exists to serve a pure validator.

---

## 5. Known constraints for the transport phase

These are recorded now because they shape the design and were discovered while
porting the pure core.

1. **Proxy handling has three separate gaps, and they are not the same gap.**

   `validation.py` accepts `http`, `https`, `socks5` and `socks5h`
   (`_SUPPORTED_PROXY_SCHEMES`), and the channel's dependency list installs
   `socksio`/`python-socks` so PTB can use them.

   *Explicit proxy.* `http.Transport.Proxy` supports `http`, `https` and
   `socks5`; **`socks5h` (remote DNS) has no stdlib spelling**. `GetMe` therefore
   fails with a `TransportError` for a `socks5h` proxy, which `Validate` degrades
   to a `warn` check rather than a false success. The validator still accepts all
   four schemes, exactly as the reference does. Closing this needs either a
   dependency decision or a hand-written SOCKS5 client.

   *No explicit proxy.* This is a distinct case and was **observed in the
   reference source, not assumed**: `_get_me` builds
   `httpx.Client(timeout=4.0)` and only adds `trust_env=False` *when a proxy is
   configured*. So with no proxy, httpx's default `trust_env=True` applies and
   `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` / `ALL_PROXY` are honoured from the
   environment. `GetMe` now uses `http.ProxyFromEnvironment` for that branch,
   which covers the first three. Two gaps remain:

   - `ALL_PROXY` is read by httpx (its mount list is built for `http`, `https`
     and `all`) but **not** by Go's `ProxyFromEnvironment`.
   - `ProxyFromEnvironment` resolves the environment **once per process**
     (`sync.Once`), so a variable exported after the first call is not picked
     up. httpx re-reads it per client. A setup wizard that exports
     `HTTPS_PROXY` and re-validates in the same process will therefore not see
     it.

   *Security note.* The bot token travels in the URL **path**
   (`https://api.telegram.org/bot<token>/getMe`), so whatever proxy is in effect
   sees it. That is the reference's behaviour as well — it is recorded here so
   the transport phase makes the choice deliberately rather than by inheriting
   `ProxyFromEnvironment`.

2. **Character, not byte, budgets.** `TELEGRAM_MAX_MESSAGE_LEN` (4000),
   `TELEGRAM_HTML_MAX_LEN` (4096) and `TELEGRAM_RICH_MAX_LEN` (32768) are
   `len(str)` in the reference. A transport that measures with `len(string)`
   will split at the wrong place for any non-ASCII message. `SplitMarkdown` and
   `SplitMarkdownHTMLChunks` already use `[]rune`/`utf8.RuneCountInString`.

3. **Python `$` matches before a trailing newline.** `BusSlashCommandRe` had to
   become `…\n?$` for RE2. The same trap applies to any new regex the transport
   phase adds.

4. **Python `\w`/`\s` are Unicode.** `pyWordClass`/`pySpaceClass` in
   `unicodetable.go` are the RE2 spellings; Go's own `\w`/`\s` are ASCII-only.

5. **Go's Unicode tables are one release behind.** Go 1.23.5 ships Unicode 15.0;
   the reference runs `unicodedata` 16.0.0. The delta is embedded in
   `unicodetable.go` and checked code point by code point by
   `TestTelegramUnicodeTablesMatchPythonReference`. A toolchain upgrade will
   make some of that table redundant (never wrong — the union is checked both
   ways) and the test will keep it honest.

6. **`_LivenessTrackedRequest` has no Go analogue and is not needed.**
   `net/http`'s `RoundTrip` returns after the round trip; the liveness watcher
   can time it directly instead of wrapping a request pool.

## 6. Phase 1 status (delivered)

`client.go` and `channel.go` implement the transport for polling mode.

**Delivered**

- `BotClient` (`client.go`): `getMe`, `getUpdates`, `sendMessage`,
  `editMessageText`, `setMyCommands`, `deleteWebhook`, `setWebhook`,
  `sendChatAction`, `answerCallbackQuery`, all over stdlib `net/http`.
  `APIError` carries `error_code`, `description` and `parameters`
  (`retry_after`, `migrate_to_chat_id`); 401/404 map to `ErrTokenInvalid`.
- `Channel` (`channel.go`): lifecycle with restart backoff, long polling,
  per-session ordered ingress (`_enqueue_ordered_update` /
  `_drain_ordered_updates` ported faithfully, including the 0.2 s reorder window
  and the `(message_id, update_id)` sort key), group policy, slash command
  normalization, HTML send with plain-text fallback, and compaction notice
  collapse (create on `started`, edit in place on terminal phases).
- `DefaultBotCommands` verified command-by-command against `runtime.py:515-534`
  (18 commands, identical).

**Splitter choice, checked against the reference.** The plain `send_message`
path splits with `_split_telegram_markdown(text, 4000)` and renders each chunk
with `_markdown_to_telegram_html` (runtime.py:1186-1193, `_send_text`). The
HTML-aware splitter `_split_telegram_markdown_html(raw_text,
TELEGRAM_HTML_MAX_LEN)` is used only by the *streaming final-edit* path
(runtime.py:1377). `Channel.Send` therefore uses `SplitMarkdown(text,
MaxMessageLen)` + `MarkdownToHTML`, matching the plain path; `SplitMarkdownHTML`
stays reserved for the streaming phase.

**Two Go-only defects found and fixed during review**

1. *Stranded inbound update.* `_drain_ordered_updates` breaks out of its loop
   when the batch is empty and then cleans up. Python's event loop does not
   yield between that `break` and the cleanup, so no coroutine can enqueue in
   that window. Go goroutines are truly concurrent, so an update arriving in the
   window left the buffer non-empty while the worker entry still existed;
   `_enqueue_ordered_update`'s "worker already present" check then suppressed a
   new worker and the update was lost forever. The cleanup now hands a
   non-empty buffer to a fresh worker. Verified under `-race`.
2. *Uninterruptible shutdown.* `pollLoop` used `time.Sleep` for the `retry_after`
   and 1 s failure backoffs. The reference awaits `asyncio.sleep`, which shutdown
   interrupts; `time.Sleep` would stall `Stop` for the whole `retry_after` window
   (Telegram flood control can ask for 30 s or more). Both waits now go through
   `sleepCtx`.

**Still deferred (Phase 2)**: streaming edits, rich messages
(`sendRichMessage`), media, reactions, webhook mode, and the command handlers
that reach into the agent.
