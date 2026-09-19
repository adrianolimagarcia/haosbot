package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var weekdayNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

type fieldMatcher struct {
	any bool
	set map[int]struct{}
}

type cronSpec struct {
	minute     fieldMatcher
	hour       fieldMatcher
	day        fieldMatcher
	month      fieldMatcher
	weekday    fieldMatcher
	dayAny     bool
	weekdayAny bool
	loc        *time.Location
}

func ValidateSchedule(s Schedule) error {
	switch s.Kind {
	case KindAt:
		if s.TZ != "" {
			return fmt.Errorf("tz can only be used with cron schedules")
		}
		if s.AtMS == nil || *s.AtMS <= 0 {
			return fmt.Errorf("at schedule requires atMs > 0")
		}
	case KindEvery:
		if s.TZ != "" {
			return fmt.Errorf("tz can only be used with cron schedules")
		}
		if s.EveryMS == nil || *s.EveryMS <= 0 {
			return fmt.Errorf("every schedule requires everyMs > 0")
		}
	case KindCron:
		if strings.TrimSpace(s.Expr) == "" {
			return fmt.Errorf("cron schedule requires expr")
		}
		_, err := parseCron(s.Expr, s.TZ)
		return err
	default:
		return fmt.Errorf("unsupported schedule kind %q", s.Kind)
	}
	return nil
}

func NextRun(s Schedule, after time.Time) (*int64, error) {
	if err := ValidateSchedule(s); err != nil {
		return nil, err
	}
	switch s.Kind {
	case KindAt:
		if *s.AtMS <= after.UnixMilli() {
			return nil, nil
		}
		v := *s.AtMS
		return &v, nil
	case KindEvery:
		v := after.UnixMilli() + *s.EveryMS
		return &v, nil
	case KindCron:
		spec, err := parseCron(s.Expr, s.TZ)
		if err != nil {
			return nil, err
		}
		next, ok := spec.next(after)
		if !ok {
			return nil, fmt.Errorf("cron expression has no occurrence within search horizon")
		}
		v := next.UnixMilli()
		return &v, nil
	}
	return nil, nil
}

func parseCron(expr, tz string) (*cronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron expression must contain 5 fields: minute hour day month weekday")
	}
	loc := time.Local
	if strings.TrimSpace(tz) != "" {
		var err error
		loc, err = time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("unknown timezone %q: %w", tz, err)
		}
	}

	minute, err := parseField(parts[0], 0, 59, nil, false)
	if err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseField(parts[1], 0, 23, nil, false)
	if err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	day, err := parseField(parts[2], 1, 31, nil, false)
	if err != nil {
		return nil, fmt.Errorf("day: %w", err)
	}
	month, err := parseField(parts[3], 1, 12, monthNames, false)
	if err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	weekday, err := parseField(parts[4], 0, 7, weekdayNames, true)
	if err != nil {
		return nil, fmt.Errorf("weekday: %w", err)
	}

	return &cronSpec{
		minute: minute, hour: hour, day: day, month: month, weekday: weekday,
		dayAny: parts[2] == "*", weekdayAny: parts[4] == "*", loc: loc,
	}, nil
}

func parseField(raw string, min, max int, names map[string]int, normalizeSunday bool) (fieldMatcher, error) {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	out := fieldMatcher{set: map[int]struct{}{}}
	if raw == "*" {
		out.any = true
		return out, nil
	}
	for _, item := range strings.Split(raw, ",") {
		if item == "" {
			return out, fmt.Errorf("empty list item")
		}
		base, step, err := splitStep(item)
		if err != nil {
			return out, err
		}
		start, end := min, max
		if base != "*" {
			if strings.Contains(base, "-") {
				pair := strings.Split(base, "-")
				if len(pair) != 2 {
					return out, fmt.Errorf("invalid range %q", base)
				}
				start, err = parseFieldValue(pair[0], min, max, names)
				if err != nil {
					return out, err
				}
				end, err = parseFieldValue(pair[1], min, max, names)
				if err != nil {
					return out, err
				}
				if start > end {
					return out, fmt.Errorf("descending range %q", base)
				}
			} else {
				start, err = parseFieldValue(base, min, max, names)
				if err != nil {
					return out, err
				}
				end = start
			}
		}
		for v := start; v <= end; v += step {
			n := v
			if normalizeSunday && n == 7 {
				n = 0
			}
			out.set[n] = struct{}{}
		}
	}
	if len(out.set) == 0 {
		return out, fmt.Errorf("field selects no values")
	}
	return out, nil
}

func splitStep(item string) (string, int, error) {
	parts := strings.Split(item, "/")
	if len(parts) > 2 {
		return "", 0, fmt.Errorf("invalid step %q", item)
	}
	step := 1
	if len(parts) == 2 {
		n, err := strconv.Atoi(parts[1])
		if err != nil || n <= 0 {
			return "", 0, fmt.Errorf("invalid step %q", parts[1])
		}
		step = n
	}
	return parts[0], step, nil
}

func parseFieldValue(raw string, min, max int, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToUpper(raw)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("value %q outside %d..%d", raw, min, max)
	}
	return v, nil
}

func (m fieldMatcher) match(v int) bool {
	if m.any {
		return true
	}
	_, ok := m.set[v]
	return ok
}

func (s *cronSpec) matches(t time.Time) bool {
	t = t.In(s.loc)
	if !s.minute.match(t.Minute()) || !s.hour.match(t.Hour()) || !s.month.match(int(t.Month())) {
		return false
	}
	dom := s.day.match(t.Day())
	dow := s.weekday.match(int(t.Weekday()))
	switch {
	case s.dayAny && s.weekdayAny:
		return true
	case s.dayAny:
		return dow
	case s.weekdayAny:
		return dom
	default:
		// Traditional cron semantics: day-of-month and day-of-week are ORed
		// when both fields are restricted.
		return dom || dow
	}
}

func (s *cronSpec) next(after time.Time) (time.Time, bool) {
	cur := after.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	limit := cur.AddDate(5, 0, 0)
	for cur.Before(limit) {
		if s.matches(cur) {
			return cur, true
		}
		cur = cur.Add(time.Minute)
	}
	return time.Time{}, false
}
