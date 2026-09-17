// Package telegram ports the portable core of the nanobot Telegram channel:
// nanobot/channels/telegram/runtime.py, validation.py and manifest.py at commit
// 1bb712d3488915ca4ed9ccc1a93067ff722f5ab9 (v0.3.5).
//
// SCOPE — what is here and what is deliberately not.
//
// Ported (all pure, no I/O): the module constants, _split_telegram_markdown and
// its HTML-limit re-split, the Markdown→Telegram-HTML renderer (including the
// pipe-table box renderer and the East Asian Width measure it needs),
// TelegramConfig with both of its validators, the allowed-sender policy
// (TelegramChannel.is_allowed plus the group-policy table), the command aliases
// and their display/normalisation helpers, validation.py's setup validator, and
// the SETUP_SPEC descriptor as a static table.
//
// NOT ported, because it is the transport phase: the getUpdates long poll, the
// webhook HTTP server, sendMessage/editMessageText/sendChatAction, media
// transfer, inline keyboards, rich messages and streaming edits, and everything
// that exists only because python-telegram-bot is the transport
// (_LivenessTrackedRequest, Application lifecycle, restart backoff, poll-stale
// watching). See TRANSPORT-INVENTORY.md for the precise remaining surface.
//
// The package deliberately has NO dependency outside the standard library plus
// this module's own packages: python-telegram-bot has no Go equivalent, so the
// Bot API will be spoken over net/http in the transport phase.
package telegram

// Module constants, port of runtime.py:44-70.
//
// The three message limits are CHARACTER counts, not byte counts: Python's
// len(str) counts code points. Every use of these values in this package goes
// through utf8.RuneCountInString or a []rune, never len(string).
const (
	// MaxMessageLen is TELEGRAM_MAX_MESSAGE_LEN: the split budget for raw
	// markdown, a 96-character safety margin below Telegram's real 4096 limit
	// so a mid-stream plain-text edit can never overflow.
	MaxMessageLen = 4000
	// HTMLMaxLen is TELEGRAM_HTML_MAX_LEN: Telegram's true message limit, used
	// as the budget for the RENDERED HTML on stream end.
	HTMLMaxLen = 4096
	// RichMaxLen is TELEGRAM_RICH_MAX_LEN: the Bot API rich-message limit. The
	// reference comments that raw markdown is counted conservatively here.
	RichMaxLen = 32768
	// ReplyContextMaxLen is TELEGRAM_REPLY_CONTEXT_MAX_LEN, defined as
	// TELEGRAM_MAX_MESSAGE_LEN.
	ReplyContextMaxLen = MaxMessageLen
	// RichDraftMinInterval is TELEGRAM_RICH_DRAFT_MIN_INTERVAL: 40 draft
	// updates per 30 seconds per chat.
	RichDraftMinInterval = 0.75
	// CompactionNoticesMax bounds in-flight compaction notices in case a
	// terminal phase never arrives.
	CompactionNoticesMax = 64
	// PollStaleSeconds is the stall threshold for the getUpdates long poll. A
	// healthy poll completes every ~10s even with no traffic.
	PollStaleSeconds = 120.0
	// PollWatchInterval is how often the stall watcher samples.
	PollWatchInterval = 1.0
	// RestartBackoffInitialSeconds is the first restart delay.
	RestartBackoffInitialSeconds = 5.0
	// RestartBackoffMaxSeconds caps the restart delay.
	RestartBackoffMaxSeconds = 300.0
	// AppRestartSendWaitSeconds is how long a send waits out a rebuild; short
	// because ChannelManager dispatches every channel from one serial loop.
	AppRestartSendWaitSeconds = 2.0
	// SendMaxRetries is _SEND_MAX_RETRIES.
	SendMaxRetries = 3
	// SendRetryBaseDelay is _SEND_RETRY_BASE_DELAY in seconds, doubled each
	// retry.
	SendRetryBaseDelay = 0.5
	// StreamEditIntervalDefault is _STREAM_EDIT_INTERVAL_DEFAULT: the minimum
	// seconds between editMessageText calls.
	StreamEditIntervalDefault = 0.6
)
