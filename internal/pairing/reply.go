package pairing

import (
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// FormatPairingReply returns the pairing-code message sent to unrecognised DM
// senders. Port of format_pairing_reply (store.py:285-293).
func FormatPairingReply(code string) string {
	return "Hi! This is your private nanobot.\n\n" +
		"Your pairing code is: `" + code + "`\n\n" +
		"Open the nanobot WebUI and enter this code to pair your chat account.\n" +
		"Without the WebUI, approve it from an already paired chat with " +
		"`/pairing approve " + code + "`."
}

// FormatExpiry returns a human-readable expiry string ("120s" or "expired").
// Port of format_expiry (store.py:296-299).
//
// int() truncates toward zero, which is what a Go float-to-int conversion does
// as well, so a remaining time of 0.5s reports "expired" in both runtimes.
func (s *Store) FormatExpiry(expiresAt float64) string {
	remaining := int64(expiresAt - s.clock())
	if remaining > 0 {
		return strconv.FormatInt(remaining, 10) + "s"
	}
	return "expired"
}

// HandlePairingCommand executes a pairing subcommand and returns the reply
// text. Port of handle_pairing_command (store.py:302-313).
//
// The reference converts a transient OSError into a user-facing message rather
// than lying ("invalid code") or silently rewriting the store from an empty
// view. Every error the subcommand can produce in this port is a store I/O
// failure, so any error maps to that message; an unexpected error is logged as
// well, because it would mean an invariant above is broken.
func (s *Store) HandlePairingCommand(channel, subcommandText string) string {
	out, err := s.handlePairingSubcommand(channel, subcommandText)
	if err != nil {
		s.log().Warn("Pairing store unavailable", "error", err)
		return "The pairing store is temporarily unavailable. Please try again."
	}
	return out
}

func (s *Store) handlePairingSubcommand(channel, subcommandText string) (string, error) {
	parts := pySplit(subcommandText)
	sub := "list"
	if len(parts) > 0 {
		sub = parts[0]
	}
	arg := ""
	hasArg := len(parts) > 1
	if hasArg {
		arg = parts[1]
	}

	switch sub {
	case "list":
		pending := s.ListPending()
		if len(pending) == 0 {
			return "No pending pairing requests.", nil
		}
		lines := []string{"Pending pairing requests:"}
		for _, item := range pending {
			// gcPending has already dropped entries whose expires_at is not a
			// number, so the zero default is unreachable for stored data.
			expiresAt, _ := numericValue(item.ExpiresAt)
			lines = append(lines, "- `"+item.Code+"` | "+pyStr(item.Channel)+" | "+
				pyStr(item.SenderID)+" | "+s.FormatExpiry(expiresAt))
		}
		return strings.Join(lines, "\n"), nil

	case "approve":
		if !hasArg {
			return "Usage: `/pairing approve <code>`", nil
		}
		result, ok, err := s.ApproveCode(arg)
		if err != nil {
			return "", err
		}
		if !ok {
			return "Invalid or expired pairing code: `" + arg + "`", nil
		}
		return "Approved pairing code `" + arg + "` — " + result.SenderID +
			" can now access " + result.Channel, nil

	case "deny":
		if !hasArg {
			return "Usage: `/pairing deny <code>`", nil
		}
		denied, err := s.DenyCode(arg)
		if err != nil {
			return "", err
		}
		if denied {
			return "Denied pairing code `" + arg + "`", nil
		}
		return "Pairing code `" + arg + "` not found or already expired", nil

	case "revoke":
		switch len(parts) {
		case 2:
			revoked, err := s.Revoke(channel, parts[1])
			if err != nil {
				return "", err
			}
			if revoked {
				return "Revoked " + arg + " from " + channel, nil
			}
			return arg + " was not in the approved list for " + channel, nil
		case 3:
			revoked, err := s.Revoke(parts[1], parts[2])
			if err != nil {
				return "", err
			}
			if revoked {
				return "Revoked " + parts[2] + " from " + parts[1], nil
			}
			return parts[2] + " was not in the approved list for " + parts[1], nil
		}
		return "Usage: `/pairing revoke <user_id>` or `/pairing revoke <channel> <user_id>`", nil
	}

	return "Unknown pairing command.\n" +
		"Usage: `/pairing [list|approve <code>|deny <code>|revoke <user_id>|revoke <channel> <user_id>]`", nil
}

// pySplit ports str.split() with no argument: split on runs of whitespace and
// discard leading and trailing whitespace.
//
// It uses textutil.PyIsSpace rather than unicode.IsSpace because Python's
// whitespace set includes U+001C..U+001F, which Go's does not — a subcommand
// such as "revoke\x1calice" splits in Python and would not with the Go
// definition.
func pySplit(s string) []string {
	out := []string{}
	start := -1
	for i, r := range s {
		if textutil.PyIsSpace(r) {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}
