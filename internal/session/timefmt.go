package session

import (
	"fmt"
	"strings"
	"time"
)

// Timestamp handling.
//
// The reference writes every timestamp as datetime.now().isoformat():
// naive LOCAL time, no timezone suffix, and either six fractional digits or
// none at all (manager.py:281-282, 317, 1323-1324).
//
// Two details matter for compatibility:
//
//   - Never emit RFC3339 with "Z" or an offset. Python >= 3.11 reads such a
//     value back, but datetime.fromisoformat(x).isoformat() then returns the
//     offset form, so the runtime-checkpoint fingerprint
//     (base_updated_at == session.updated_at.isoformat(), manager.py:1282)
//     stops matching and Python silently deletes the sidecar.
//   - Fractional seconds are normalised, not preserved: isoformat() drops
//     ".000000" entirely and pads anything else to exactly six digits, so
//     "…05.1" must be written back as "…05.100000" (verified on CPython
//     3.14.7).

// formatNaive renders t the way datetime.isoformat() does for a naive
// datetime: local wall-clock time, no suffix, six fractional digits or none.
//
// Go keeps nanoseconds while datetime keeps microseconds, so the value is
// truncated to microseconds, matching fromisoformat's handling of extra
// fractional digits.
func formatNaive(t time.Time) string {
	base := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		return base + fmt.Sprintf(".%06d", us)
	}
	return base
}

// formatWithOffset renders t the way datetime.isoformat() does for an aware
// datetime, including "+00:00" for UTC where Go's RFC3339 would write "Z".
func formatWithOffset(t time.Time) string {
	_, seconds := t.Zone()
	return formatNaive(t) + formatOffset(seconds)
}

// formatOffset renders a zone offset in seconds as ±HH:MM.
func formatOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	return fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, (seconds%3600)/60)
}

// nowTimestamp returns the current time, the Go equivalent of datetime.now().
// Its wall-clock value is what gets written, so it is always local.
func nowTimestamp() time.Time { return time.Now() }

// parseTimestamp parses a stored timestamp and returns it together with the
// string datetime.fromisoformat(value).isoformat() would produce.
//
// Python's strict load path raises for an unparseable value and its repair
// path suppresses that error and substitutes datetime.now(); callers here
// treat an unparseable value as absent and fall back to now, which matches the
// repair path (the only path that survives a bad value in the reference).
func parseTimestamp(s string) (time.Time, string, bool) {
	if s == "" {
		return time.Time{}, "", false
	}
	trimmed := strings.TrimSpace(s)

	// Offset-aware forms: Python >= 3.11 accepts both "Z" and "+HH:MM".
	// These are normalised back to the offset form, exactly as isoformat()
	// would render them.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04Z07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return t, formatWithOffset(t), true
		}
	}

	// Naive forms: local time, no suffix.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, trimmed, time.Local); err == nil {
			return t, formatNaive(t), true
		}
	}
	return time.Time{}, "", false
}
