// Package pairing ports nanobot/pairing: the DM sender-approval store used by
// BaseChannel.is_allowed.
//
// Mirrors upstream/nanobot/nanobot/pairing/store.py (367 lines) and
// nanobot/pairing/__init__.py at commit 1bb712d3 (v0.3.5).
//
// The store is a single JSON file at <data dir>/pairing.json holding approved
// senders per channel and the pending pairing codes. It is designed for
// private-assistant scale: small file, one lock, no external database.
//
// COMPATIBILITY CONTRACT — the on-disk file is shared with the Python runtime,
// so it is reproduced byte for byte, not merely "as valid JSON":
//
//   - top-level key order is "approved" then "pending";
//   - keys inside both objects keep Python dict insertion order, which is why
//     this package carries its own ordered JSON reader/writer instead of using
//     encoding/json (Go maps are unordered);
//   - approved lists are sorted (Python's sorted(), i.e. code-point order);
//   - numbers are re-encoded through Python's repr rules, so an integer literal
//     stays an integer ("5") while a float keeps its ".0" ("9000000000.0") —
//     encoding/json would emit "9000000000" and change the value's JSON type;
//   - output is json.dumps(payload, indent=2, ensure_ascii=False): two-space
//     indent, no trailing newline, non-ASCII passed through unescaped, and no
//     HTML escaping (encoding/json escapes <, > and & by default).
//
// See json.go for the value model and store.go for the operations.
package pairing

import "sync"

// Metadata keys used by channels and commands to tag pairing-related messages.
// Port of nanobot/pairing/__init__.py:19-20.
const (
	// PairingCodeMetaKey is the metadata key carrying a pairing code on the
	// outbound message that delivers it.
	PairingCodeMetaKey = "_pairing_code"
	// PairingCommandMetaKey is the metadata key marking a pairing command.
	PairingCommandMetaKey = "_pairing_command"
)

// Store constants, mirroring store.py:27-29.
const (
	// DefaultTTLSeconds is _TTL_DEFAULT_S: ten minutes.
	DefaultTTLSeconds = 600

	// CodeLength is _CODE_LENGTH (store.py:28): the number of characters
	// DRAWN. The rendered code is nine characters, because a dash is inserted
	// after the fourth, e.g. "ABCD-EFGH".
	CodeLength = 8

	// CodeAlphabet is _ALPHABET (store.py:27): string.ascii_uppercase +
	// string.digits. Exported so a validator or UI can describe the code space
	// without restating the literal.
	CodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// defaultStore backs the package-level functions.
//
// It is the Go equivalent of the module globals in store.py: the reference
// resolves its path on EVERY call (store.py:32-33) rather than caching it, so
// the default store keeps an empty path meaning "resolve now" instead of
// freezing $HOME at init time.
var (
	defaultStoreOnce sync.Once
	defaultStore     *Store
)

// Default returns the process-wide store used by the package-level functions.
//
// Callers that need an isolated store (tests, or a deployment rooted outside
// the default data directory) should use NewStore instead.
func Default() *Store {
	defaultStoreOnce.Do(func() { defaultStore = NewStore("") })
	return defaultStore
}

// ---------------------------------------------------------------------------
// Package-level operations
//
// These mirror the module-level functions of store.py, which are what
// BaseChannel and the CLI actually call. Each delegates to Default().
// ---------------------------------------------------------------------------

// GenerateCode returns an active pairing code for senderID on channel, using
// the default TTL. Port of generate_code (store.py:113).
func GenerateCode(channel, senderID string) (string, error) {
	return Default().GenerateCode(channel, senderID, DefaultTTLSeconds)
}

// ApproveCode approves a pending pairing code. Port of approve_code
// (store.py:144).
func ApproveCode(code string) (Approval, bool, error) { return Default().ApproveCode(code) }

// DenyCode rejects and discards a pending pairing code. Port of deny_code
// (store.py:165).
func DenyCode(code string) (bool, error) { return Default().DenyCode(code) }

// IsApproved reports whether senderID has been approved on channel.
// Port of is_approved (store.py:182).
func IsApproved(channel, senderID string) bool { return Default().IsApproved(channel, senderID) }

// ListPending returns all non-expired pending pairing requests.
// Port of list_pending (store.py:194).
func ListPending() []PendingRequest { return Default().ListPending() }

// Revoke removes an approved sender from channel. Port of revoke (store.py:209).
func Revoke(channel, senderID string) (bool, error) { return Default().Revoke(channel, senderID) }

// RevokeChannel removes all approved sender IDs for channel.
// Port of revoke_channel (store.py:229).
func RevokeChannel(channel string) (int, error) { return Default().RevokeChannel(channel) }

// ClearChannel removes approved senders and pending requests for channel.
// Port of clear_channel (store.py:245).
func ClearChannel(channel string) (ClearResult, error) { return Default().ClearChannel(channel) }

// GetApproved returns all approved sender IDs for channel, sorted.
// Port of get_approved (store.py:275).
func GetApproved(channel string) []string { return Default().GetApproved(channel) }

// FormatExpiry renders a human-readable expiry string ("120s" or "expired").
// Port of format_expiry (store.py:296).
func FormatExpiry(expiresAt float64) string { return Default().FormatExpiry(expiresAt) }

// HandlePairingCommand executes a pairing subcommand and returns the reply
// text. Port of handle_pairing_command (store.py:302).
func HandlePairingCommand(channel, subcommandText string) string {
	return Default().HandlePairingCommand(channel, subcommandText)
}
