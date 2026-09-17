package telegram

import (
	"encoding/json"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/pairing"
)

// ChannelName is the runtime identity of this channel, used for pairing-store
// lookups and for the session keys the manager derives.
const ChannelName = "telegram"

// GroupPolicies is GROUP_POLICIES (channels/_manifest.py:10).
//
// The reference value is a frozenset, so it has no meaningful order; it is
// stored SORTED here because every consumer that serialises it sorts — the setup
// payload the WebUI reads is alphabetical, and the differential test compares
// against `sorted(GROUP_POLICIES)`.
//
// The manifest advertises all three, but TelegramConfig's annotation only
// accepts "open" and "mention" — "allowlist" is in the table because the shared
// constructor is used by other channels too, and because a configuration
// written for one of those channels must be REJECTED here rather than silently
// treated as "mention". The differential test covers that rejection.
var GroupPolicies = []string{"allowlist", "mention", "open"}

// DirectGroupPolicies is DIRECT_GROUP_POLICIES (channels/_manifest.py:11). It is
// not used by the Telegram manifest, which uses the full GROUP_POLICIES set;
// it is exported so the shared manifest constructors have a Go home.
var DirectGroupPolicies = []string{"mention", "open"}

// SenderPolicy is TelegramChannel.is_allowed as a standalone, constructible
// value. Port of runtime.py:574-593.
//
// The override exists for TELEGRAM'S LEGACY allowlist format, where a sender id
// is "12345|username" and either half may appear in allow_from. It runs only
// after the base check fails, and it reads the allowlist differently from the
// base check — with `getattr(config, "allow_from", [])`, which means:
//
//   - a DICT config never reaches the legacy branch at all, because a dict has
//     no `allow_from` attribute and getattr returns the empty default. In
//     production this is invisible: TelegramChannel.__init__ validates the dict
//     into a TelegramConfig first. The differential corpus exercises both
//     shapes, and the dict shape really does deny "123|alice";
//   - an OBJECT config without the attribute behaves the same way.
//
// The policy therefore takes a channels.Section, which is exactly the
// dict-or-object distinction internal/channels already models.
type SenderPolicy struct {
	base    *channels.Base
	section channels.Section
}

// NewSenderPolicy builds the policy for a channel whose configuration section
// is section. store supplies DM pairing approvals; a nil store uses the
// process-wide store, matching the reference's module-level is_approved.
func NewSenderPolicy(section channels.Section, store *pairing.Store) SenderPolicy {
	opts := []channels.Option{channels.WithName(ChannelName)}
	if store != nil {
		opts = append(opts, channels.WithPairingStore(store))
	}
	return SenderPolicy{base: channels.NewBase(nil, section, nil, opts...), section: section}
}

// NewSenderPolicyForConfig builds the policy from an already validated Config,
// which is the production shape: the legacy branch needs an object with an
// `allow_from` attribute.
func NewSenderPolicyForConfig(cfg Config, store *pairing.Store) SenderPolicy {
	return NewSenderPolicy(
		channels.NewObjectSection(channels.ObjectSection{AllowFrom: cfg.AllowFrom}),
		store,
	)
}

// IsAllowed reports whether senderID may talk to the bot.
//
// Order of decisions (runtime.py:576-593), all preserved:
//
//  1. the base check: "*" in the allowlist, then exact membership, then the
//     pairing store;
//  2. the legacy check, skipped entirely when the config has no `allow_from`
//     attribute, when the allowlist is falsy, or when it contains "*";
//  3. the sender must contain EXACTLY ONE "|";
//  4. the id half must be all digits (Python's str.isdigit, which accepts
//     superscripts and other numeric-but-not-decimal characters) and the
//     username half must be non-empty;
//  5. either half may match the allowlist.
func (p SenderPolicy) IsAllowed(senderID string) bool {
	if p.base != nil && p.base.IsAllowed(senderID) {
		return true
	}

	allowList := p.legacyAllowList()
	if !pyTruthy(allowList) || pyContains(allowList, "*") {
		return false
	}

	if strings.Count(senderID, "|") != 1 {
		return false
	}
	sid, username, _ := strings.Cut(senderID, "|")
	if !pyIsDigitString(sid) || username == "" {
		return false
	}
	return pyContains(allowList, sid) || pyContains(allowList, username)
}

// legacyAllowList is `getattr(self.config, "allow_from", [])`.
//
// The DEFAULT differs from the base check's `getattr(config, "allow_from", None)
// or []`: here the default is the empty list, and a present-but-falsy attribute
// is returned as-is. Both end up falsy for every realistic input, but the two
// expressions are not the same and the reference really does use both.
func (p SenderPolicy) legacyAllowList() any {
	if p.section.IsMap() {
		// getattr() on a dict instance never finds a config key.
		return []any{}
	}
	obj, ok := p.section.Object()
	if !ok {
		return []any{}
	}
	return obj.AllowFrom
}

// ---------------------------------------------------------------------------
// Python value semantics
//
// internal/channels has both of these, but unexported, and this package must
// not modify that file. The implementations are deliberately identical to
// section.go's containsToken and pyTruthy; the differential corpus covers the
// legacy branch through both.
// ---------------------------------------------------------------------------

// pyTruthy reproduces Python's bool() for the values a configuration or a
// decoded JSON object can hold.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int32:
		return t != 0
	case int64:
		return t != 0
	case uint:
		return t != 0
	case uint64:
		return t != 0
	case float64:
		return t != 0
	case float32:
		return t != 0
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			// A non-numeric json.Number cannot occur from encoding/json, but a
			// caller could construct one; a non-empty string is truthy.
			return t.String() != ""
		}
		return f != 0
	case []any:
		return len(t) != 0
	case []string:
		return len(t) != 0
	case map[string]any:
		return len(t) != 0
	}
	// Python objects and any other container are truthy.
	return true
}

// pyContains reproduces Python's `token in value`.
//
// A list matches only STRING elements equal to token (Python's == between a str
// and an int is False), a string is a SUBSTRING test, and a dict is a KEY test.
// A value Python cannot iterate over raises TypeError out of is_allowed and
// kills the handler; Go has no exception channel, so that case reports "no
// match" and the sender is denied — failing closed.
func pyContains(value any, token string) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == token {
				return true
			}
		}
		return false
	case []string:
		for _, item := range v {
			if item == token {
				return true
			}
		}
		return false
	case string:
		return strings.Contains(v, token)
	case map[string]any:
		_, ok := v[token]
		return ok
	case map[string]string:
		_, ok := v[token]
		return ok
	}
	return false
}

// ---------------------------------------------------------------------------
// Group policy
// ---------------------------------------------------------------------------

// MentionEntity is the subset of telegram.MessageEntity the mention check reads.
type MentionEntity struct {
	// Type is entity.type: "mention", "text_mention", or anything else.
	Type string
	// Offset and Length are CHARACTER offsets into the message text.
	Offset *int
	Length *int
	// UserID is entity.user.id for a "text_mention".
	UserID *int64
}

// GroupMessageInput is the subset of telegram.Message the group decision reads.
type GroupMessageInput struct {
	// ChatType is message.chat.type; "private" short-circuits the policy.
	ChatType string
	// Text is message.text, empty when the message has no text.
	Text string
	// Entities is message.entities.
	Entities []MentionEntity
	// Caption is message.caption.
	Caption string
	// CaptionEntities is message.caption_entities.
	CaptionEntities []MentionEntity
	// ReplyToUserID is message.reply_to_message.from_user.id, nil when the
	// message is not a reply or the reply has no author.
	ReplyToUserID *int64
}

// IsGroupMessageForBot decides whether the bot should answer a group message.
// Port of _is_group_message_for_bot (runtime.py:1798-1823).
//
// botID and botUsername are what _ensure_bot_identity returns; the caller
// resolves them (in the reference by calling getMe once and caching). A nil or
// ZERO botID is falsy in Python and therefore disables the reply-to-bot rule —
// bot_id 0 is not a real Telegram id, but `bool(bot_id and ...)` would reject it
// and this port does too.
func IsGroupMessageForBot(in GroupMessageInput, groupPolicy string, botID *int64, botUsername string) bool {
	if in.ChatType == "private" || groupPolicy == "open" {
		return true
	}
	if botUsername != "" {
		if HasMentionEntity(in.Text, in.Entities, botUsername, botID) {
			return true
		}
		if HasMentionEntity(in.Caption, in.CaptionEntities, botUsername, botID) {
			return true
		}
	}
	if botID == nil || *botID == 0 || in.ReplyToUserID == nil {
		return false
	}
	return *in.ReplyToUserID == *botID
}

// HasMentionEntity checks Telegram mention entities against the bot username.
// Port of _has_mention_entity (runtime.py:1774-1796).
//
// The fallback `handle in text.lower()` is a plain SUBSTRING test, so a message
// containing "@nanobot" anywhere counts even when Telegram sent no entity — the
// reference deliberately accepts that, and a bot username that is a substring of
// another word will therefore also match.
//
// DIVERGENCE (documented, not silent): `text.lower()` is Python's full Unicode
// lower-casing, which maps U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE) to the
// two-character sequence "i" + U+0307; Go's strings.ToLower maps it to "i". Bot
// usernames are ASCII, so this only changes the fallback substring search for
// text containing U+0130. It is not reproduced because Go's standard library
// exposes no simple-mapping-plus-SpecialCasing lower-caser.
func HasMentionEntity(text string, entities []MentionEntity, botUsername string, botID *int64) bool {
	handle := strings.ToLower("@" + botUsername)
	runes := []rune(text)
	for _, entity := range entities {
		switch entity.Type {
		case "text_mention":
			if entity.UserID != nil && botID != nil && *entity.UserID == *botID {
				return true
			}
			continue
		case "mention":
		default:
			continue
		}
		if entity.Offset == nil || entity.Length == nil {
			continue
		}
		// Python slices with clamping; Go panics out of range.
		start := *entity.Offset
		if start < 0 {
			start = 0
		}
		if start > len(runes) {
			start = len(runes)
		}
		end := start + *entity.Length
		if end < start {
			end = start
		}
		if end > len(runes) {
			end = len(runes)
		}
		if strings.ToLower(string(runes[start:end])) == handle {
			return true
		}
	}
	return strings.Contains(strings.ToLower(text), handle)
}
