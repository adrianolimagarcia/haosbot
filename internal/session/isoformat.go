package session

import (
	"time"
	"unicode/utf8"
)

// datetime.fromisoformat acceptance.
//
// session_summary_from_metadata keeps the stored "last_active" string only when
// datetime.fromisoformat accepts it, and substitutes the fallback otherwise. So
// the ACCEPT/REJECT decision is observable, and it has to match CPython's, not
// merely "parse or not" in Go's stricter time.Parse sense: the reference accepts
// "20260102", "2026-W01-1", "2026-01-02X03:04:05" and "2026-01-02T24:00:00"
// (all of which Go's layouts reject) and rejects "2026-01-02T03:04:05z" and
// " 2026-01-02" (which Go's parser would accept after trimming).
//
// This is a direct port of datetime_fromisoformat and its helpers
// (Modules/_datetimemodule.c, CPython 3.14.7: parse_isoformat_date at :940,
// parse_hh_mm_ss_ff at :1004, parse_isoformat_time at :1089,
// _find_isoformat_datetime_separator at :5811, datetime_fromisoformat at :5910),
// which is what the C accelerator implements. The pure-Python reference in
// Lib/datetime.py is only a stub in 3.14, so the C source is the specification.
//
// The port works on BYTES, like the C code: the separator search and the
// date/time parse index the UTF-8 encoding, while the initial length check
// counts CODE POINTS. Only the accept/reject decision is reproduced; the parsed
// value is discarded because the reference discards it too (it validates the
// string and then keeps the original text).

// fromisoformatOK reports whether datetime.fromisoformat(s) would succeed.
func fromisoformatOK(s string) bool {
	// _sanitize_isoformat_str returns NULL for fewer than 7 code points, which
	// datetime_fromisoformat turns into a ValueError.
	if utf8.RuneCountInString(s) < 7 {
		return false
	}

	// The reference replaces a surrogate character at byte-offset-candidate
	// positions 7, 8 or 10 with 'T' and then requires the whole string to be
	// UTF-8 encodable. A Go string cannot carry a lone surrogate (encoding/json
	// and the UTF-8 decoders used by this port replace it with U+FFFD), so that
	// branch is unreachable here; a U+FFFD separator fails the parse anyway.
	b := []byte(s)

	sepLoc := findISOFormatDatetimeSeparator(b)

	// separator_location is a Py_ssize_t; -1 becomes (size_t)-1 when it is
	// passed to parse_isoformat_date, which makes every `(p - dtstr) < len`
	// test succeed. Modelling it as a separate "unbounded" flag keeps the
	// signed comparison `len > separator_location` below intact.
	dateLen := sepLoc
	unbounded := sepLoc < 0
	if unbounded {
		dateLen = 0
	}

	year, month, day := 0, 0, 0
	rv := parseISOFormatDate(b, dateLen, unbounded, &year, &month, &day)

	hour, minute, second, microsecond := 0, 0, 0, 0
	tzoffset, tzusec := 0, 0

	if rv == 0 && len(b) > sepLoc {
		p := sepLoc + utf8CharLen(at(b, sepLoc))
		if p > len(b) {
			p = len(b)
		}
		rv = parseISOFormatTime(b, p, &hour, &minute, &second, &microsecond, &tzoffset, &tzusec)
	}
	if rv < 0 {
		return false
	}

	// tzinfo_from_isoformat_results (Modules/_datetimemodule.c:1664) builds a
	// timezone, which new_timezone (:1451) rejects unless the offset is
	// strictly between -24h and +24h. Note the asymmetry at exactly -24h: the
	// normalised delta is (days=-1, seconds=0, microseconds=0) and the guard
	// `GET_TD_MICROSECONDS(offset) < 1` fires, so -24:00:00 is rejected just
	// like +24:00:00.
	if rv == 1 {
		total := int64(tzoffset)*1_000_000 + int64(tzusec)
		const dayMicro = int64(86_400_000_000)
		if total <= -dayMicro || total >= dayMicro {
			return false
		}
	}

	// CPython rolls 24:00:00 over to midnight of the next day, but only when
	// the date is otherwise well formed; anything else falls through to
	// check_time_args, which rejects hour 24.
	if hour == 24 && month >= 1 && month <= 12 {
		dInMonth := daysInMonth(year, month)
		if day <= dInMonth {
			if minute == 0 && second == 0 && microsecond == 0 {
				hour = 0
				day++
				if day > dInMonth {
					day = 1
					month++
					if month > 12 {
						month = 1
						year++
					}
				}
			} else {
				return false
			}
		}
	}

	return checkDateArgs(year, month, day) && checkTimeArgs(hour, minute, second, microsecond)
}

// at reads byte i of the NUL-terminated C buffer the reference parses. Reading
// at or past the end yields 0, which is exactly what the reference's
// NUL-terminated buffer yields on the paths that reach those reads.
func at(b []byte, i int) byte {
	if i < 0 || i >= len(b) {
		return 0
	}
	return b[i]
}

// utf8CharLen mirrors the inline UTF-8 advance in datetime_fromisoformat.
func utf8CharLen(c byte) int {
	if c&0x80 == 0 {
		return 1
	}
	switch c & 0xf0 {
	case 0xe0:
		return 3
	case 0xf0:
		return 4
	default:
		return 2
	}
}

// isASCIIDigit mirrors is_digit (Modules/_datetimemodule.c:921).
func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// parseDigits mirrors parse_digits (Modules/_datetimemodule.c:926).
func parseDigits(b []byte, p int, v *int, n int) (int, bool) {
	for i := 0; i < n; i++ {
		tmp := int(at(b, p)) - '0'
		p++
		if tmp > 9 || tmp < 0 {
			return 0, false
		}
		*v = *v*10 + tmp
	}
	return p, true
}

// findISOFormatDatetimeSeparator mirrors _find_isoformat_datetime_separator.
//
// Returns -1 for the one ambiguous length the C code gives up on; callers must
// treat that as an unbounded date length, exactly as the (size_t)-1 conversion
// does in C.
func findISOFormatDatetimeSeparator(b []byte) int {
	length := len(b)
	if length == 7 {
		return 7
	}

	const dateSeparator = '-'
	const weekIndicator = 'W'

	if at(b, 4) == dateSeparator {
		if at(b, 5) == weekIndicator {
			if length < 8 {
				return -1
			}
			if length > 8 && at(b, 8) == dateSeparator {
				if length == 9 {
					return -1
				}
				if length > 10 && isASCIIDigit(at(b, 10)) {
					return 8
				}
				return 10
			}
			return 8
		}
		return 10
	}

	if at(b, 4) == weekIndicator {
		idx := 7
		for ; idx < length; idx++ {
			if !isASCIIDigit(b[idx]) {
				break
			}
		}
		if idx < 9 {
			return idx
		}
		if idx%2 == 0 {
			return 7
		}
		return 8
	}
	return 8
}

// parseISOFormatDate mirrors parse_isoformat_date. length is the separator
// offset; unbounded stands in for the (size_t)-1 the C code passes when
// findISOFormatDatetimeSeparator returns -1.
func parseISOFormatDate(b []byte, length int, unbounded bool, year, month, day *int) int {
	p := 0
	var ok bool
	p, ok = parseDigits(b, p, year, 4)
	if !ok {
		return -1
	}

	usesSeparator := at(b, p) == '-'
	if usesSeparator {
		p++
	}

	if at(b, p) == 'W' {
		p++
		isoWeek := 0
		p, ok = parseDigits(b, p, &isoWeek, 2)
		if !ok {
			return -3
		}

		// The default applies only when the day component is ABSENT ("2026-W01").
		// When it is present it must be read into a ZEROED accumulator:
		// parseDigits accumulates (*v = *v*10 + digit), so seeding it with 1 made
		// every week date parse as 11..17, fail isoToYMD's 1..7 range check, and
		// reject the entire YYYY-Www-D form.
		isoDay := 1
		if unbounded || p < length {
			if usesSeparator {
				if at(b, p) != '-' {
					return -2
				}
				p++
			}
			isoDay = 0
			// p is not read again: the function returns straight after.
			_, ok = parseDigits(b, p, &isoDay, 1)
			if !ok {
				return -4
			}
		}
		return isoToYMD(*year, isoWeek, isoDay, year, month, day)
	}

	p, ok = parseDigits(b, p, month, 2)
	if !ok {
		return -1
	}
	if usesSeparator {
		if at(b, p) != '-' {
			return -2
		}
		p++
	}
	if _, ok = parseDigits(b, p, day, 2); !ok {
		return -1
	}
	return 0
}

// parseHHMMSSFF mirrors parse_hh_mm_ss_ff.
//
// Returns a negative code on failure, 0 when the parse ends exactly at the end
// of the time portion, and 1 when characters remain.
func parseHHMMSSFF(b []byte, start, end int, hour, minute, second, microsecond *int) int {
	*hour, *minute, *second, *microsecond = 0, 0, 0, 0
	p := start
	vals := [3]*int{hour, minute, second}
	hasSeparator := true

	reachedFraction := false
	for i := 0; i < 3; i++ {
		var ok bool
		p, ok = parseDigits(b, p, vals[i], 2)
		if !ok {
			return -3
		}

		c := at(b, p)
		p++
		if i == 0 {
			hasSeparator = c == ':'
		}

		switch {
		case c == '.' || c == ',':
			if i < 2 {
				return -3 // decimal mark on hour or minute
			}
			if p >= end {
				return -3 // decimal mark not followed by any digit
			}
			reachedFraction = true
		case p >= end:
			if c != 0 {
				return 1
			}
			return 0
		case hasSeparator && c == ':':
			if i == 2 {
				return -4
			}
			continue
		case !hasSeparator:
			p--
		default:
			return -4
		}
		if reachedFraction {
			break
		}
	}

	remaining := end - p
	toParse := remaining
	if remaining >= 6 {
		toParse = 6
	}

	var ok bool
	p, ok = parseDigits(b, p, microsecond, toParse)
	if !ok {
		return -3
	}

	correction := [5]int{100000, 10000, 1000, 100, 10}
	if toParse < 6 {
		*microsecond *= correction[toParse-1]
	}

	for isASCIIDigit(at(b, p)) {
		p++ // skip truncated digits
	}

	if at(b, p) != 0 {
		return 1
	}
	return 0
}

// parseISOFormatTime mirrors parse_isoformat_time.
//
// start is the byte offset of the time portion; the portion always runs to the
// end of the string, because datetime_fromisoformat subtracts the consumed
// prefix from the total length before calling this.
func parseISOFormatTime(b []byte, start int, hour, minute, second, microsecond, tzoffset, tzmicrosecond *int) int {
	pEnd := len(b)

	// Scan for the first timezone introducer, exactly like the C do/while: the
	// character at the cursor is tested before each increment, and the cursor
	// can end up one past pEnd when the time portion is empty.
	tzinfoPos := start
	for {
		c := at(b, tzinfoPos)
		if c == 'Z' || c == '+' || c == '-' {
			break
		}
		tzinfoPos++
		if tzinfoPos >= pEnd {
			break
		}
	}

	rv := parseHHMMSSFF(b, start, tzinfoPos, hour, minute, second, microsecond)
	if rv < 0 {
		return rv
	}
	if tzinfoPos == pEnd {
		if rv == 1 {
			return -5
		}
		return 0
	}

	if at(b, tzinfoPos) == 'Z' {
		*tzoffset = 0
		*tzmicrosecond = 0
		if at(b, tzinfoPos+1) != 0 {
			return -5
		}
		return 1
	}

	tzsign := 1
	if at(b, tzinfoPos) == '-' {
		tzsign = -1
	}
	tzinfoPos++

	tzhour, tzminute, tzsecond := 0, 0, 0
	rv = parseHHMMSSFF(b, tzinfoPos, pEnd, &tzhour, &tzminute, &tzsecond, tzmicrosecond)
	*tzoffset = tzsign * (tzhour*3600 + tzminute*60 + tzsecond)
	*tzmicrosecond *= tzsign

	if rv != 0 {
		return -5
	}
	return 1
}

// isoToYMD mirrors iso_to_ymd (Modules/_datetimemodule.c:598). The return codes
// differ from the other helpers only in that they are offset by -3 by the
// caller; every non-zero value is a failure, so the exact code is not
// observable.
func isoToYMD(isoYear, isoWeek, isoDay int, year, month, day *int) int {
	if isoYear < 1 || isoYear > 9999 {
		return -4
	}
	if isoWeek <= 0 || isoWeek >= 53 {
		outOfRange := true
		if isoWeek == 53 {
			firstWeekday := weekday(isoYear, 1, 1)
			if firstWeekday == 3 || (firstWeekday == 2 && isLeap(isoYear)) {
				outOfRange = false
			}
		}
		if outOfRange {
			return -2
		}
	}
	if isoDay <= 0 || isoDay >= 8 {
		return -3
	}

	day1 := isoWeek1Monday(isoYear)
	dayOffset := (isoWeek-1)*7 + isoDay - 1
	ordToYMD(day1+dayOffset, year, month, day)
	return 0
}

// isLeap mirrors is_leap (Modules/_datetimemodule.c:422).
func isLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// daysInMonth mirrors days_in_month. month must be in 1..12.
func daysInMonth(year, month int) int {
	table := [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if month == 2 && isLeap(year) {
		return 29
	}
	return table[month]
}

// ymdToOrd mirrors ymd_to_ord: 01-Jan-0001 is day 1.
func ymdToOrd(year, month, day int) int {
	y := year - 1
	return y*365 + y/4 - y/100 + y/400 + daysBeforeMonth(year, month) + day
}

// daysBeforeMonth mirrors days_before_month.
func daysBeforeMonth(year, month int) int {
	table := [13]int{0, 0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334}
	days := table[month]
	if month > 2 && isLeap(year) {
		days++
	}
	return days
}

// weekday mirrors weekday: Monday is 0.
func weekday(year, month, day int) int {
	return (ymdToOrd(year, month, day) + 6) % 7
}

// isoWeek1Monday mirrors iso_week1_monday.
func isoWeek1Monday(year int) int {
	firstDay := ymdToOrd(year, 1, 1)
	firstWeekday := (firstDay + 6) % 7
	week1Monday := firstDay - firstWeekday
	if firstWeekday > 3 {
		week1Monday += 7
	}
	return week1Monday
}

// ordToYMD mirrors ord_to_ymd via the proleptic Gregorian calendar Go's
// time package implements (identical to Python's date for every ordinal the
// reference can reach here). The only consumer re-validates the result with
// check_date_args, so an out-of-range ordinal that lands past 9999-12-31 is
// rejected by the caller exactly as it is in the reference.
func ordToYMD(ordinal int, year, month, day *int) {
	t := time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, ordinal-1)
	*year = t.Year()
	*month = int(t.Month())
	*day = t.Day()
}

// checkDateArgs mirrors check_date_args.
func checkDateArgs(year, month, day int) bool {
	if year < 1 || year > 9999 {
		return false
	}
	if month < 1 || month > 12 {
		return false
	}
	if day < 1 || day > daysInMonth(year, month) {
		return false
	}
	return true
}

// checkTimeArgs mirrors check_time_args.
func checkTimeArgs(hour, minute, second, microsecond int) bool {
	return hour >= 0 && hour <= 23 &&
		minute >= 0 && minute <= 59 &&
		second >= 0 && second <= 59 &&
		microsecond >= 0 && microsecond <= 999999
}
