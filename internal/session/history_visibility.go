package session

import (
	"bytes"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// Visibility markers for persisted history messages.
//
// Mirrors nanobot/session/history_visibility.py and the marker constants it
// depends on. The reference reaches is_automation_history_message through
// nanobot/session/automation_turns.py, which lazily imports the cron and
// local-trigger specs; the only value those specs contribute here is their
// legacy_history_meta_key (nanobot/cron/session_turns.py:15 defines
// CRON_HISTORY_META, the local-trigger spec defines none), so the port names
// that one key directly instead of pulling in the whole spec registry.
const (
	// HiddenHistoryMeta is HIDDEN_HISTORY_META (history_visibility.py:12): the
	// message key marking a persisted message that is not a chat turn.
	HiddenHistoryMeta = "_hidden_history"

	// AutomationHistoryMeta is AUTOMATION_HISTORY_META
	// (session/automation_turns.py:12).
	AutomationHistoryMeta = "_automation_turn"

	// CronHistoryMeta is CRON_HISTORY_META (cron/session_turns.py:15), the
	// legacy marker that predates AutomationHistoryMeta. Only the exact bool
	// true counts, exactly as in is_automation_history_message.
	CronHistoryMeta = "_cron_turn"

	// ChannelDeliveryMeta is the "_channel_delivery" key. The reference has no
	// named constant for it: it is spelled out literally at
	// session/manager.py:374 and utils/helpers.py:473. Both uses are Python
	// TRUTHINESS tests (`if sliced[i - 1].get("_channel_delivery")`), not
	// identity tests against True, which is why markerTruthy below exists.
	ChannelDeliveryMeta = "_channel_delivery"
)

// IsHiddenHistoryMessage reports whether a persisted message should be kept out
// of the visible chat history.
//
// Mirrors is_hidden_history_message (history_visibility.py:18-20):
//
//	return _has_hidden_history_marker(message) or is_automation_history_message(message)
func IsHiddenHistoryMessage(m core.Message) bool {
	return hasHiddenHistoryMarker(m) || isAutomationHistoryMessage(m)
}

// hasHiddenHistoryMarker mirrors _has_hidden_history_marker
// (history_visibility.py:15-19): the marker counts when it is exactly the bool
// true OR any Mapping. Any other value — false, 0, "", a list, null — does not.
func hasHiddenHistoryMarker(m core.Message) bool {
	return markerIsTrueOrMapping(m, HiddenHistoryMeta)
}

// isAutomationHistoryMessage mirrors is_automation_history_message
// (automation_turns.py:78-88): the current marker accepts true or a Mapping,
// and each automation spec additionally honours its legacy marker, but only
// when that legacy marker is exactly true.
func isAutomationHistoryMessage(m core.Message) bool {
	if markerIsTrueOrMapping(m, AutomationHistoryMeta) {
		return true
	}
	return markerIsTrue(m, CronHistoryMeta)
}

// markerIsTrueOrMapping reports `marker is True or isinstance(marker, Mapping)`.
func markerIsTrueOrMapping(m core.Message, key string) bool {
	raw, ok := m.Extra(key)
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	// A JSON object is the Go shape of a decoded Python Mapping.
	if trimmed[0] == '{' {
		return true
	}
	return bytes.Equal(trimmed, []byte("true"))
}

// markerIsTrue reports `marker is True`.
func markerIsTrue(m core.Message, key string) bool {
	raw, ok := m.Extra(key)
	if !ok {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(raw), []byte("true"))
}

// markerTruthy reports Python truthiness of the value stored under key.
//
// Used for the "_channel_delivery" marker, which the reference tests with
// `if ... .get("_channel_delivery")`. A missing key, null, false, 0, 0.0, ""
// and an empty list/object are all falsy; everything else is truthy.
func markerTruthy(m core.Message, key string) bool {
	raw, ok := m.Extra(key)
	if !ok {
		return false
	}
	return pyTruthy(raw)
}
