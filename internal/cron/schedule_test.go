package cron

import (
	"testing"
	"time"
)

func TestNextRunEvery(t *testing.T) {
	ms := int64(30_000)
	base := time.Unix(1_700_000_000, 0)
	next, err := NextRun(Schedule{Kind: KindEvery, EveryMS: &ms}, base)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || *next != base.UnixMilli()+ms {
		t.Fatalf("next=%v want %d", next, base.UnixMilli()+ms)
	}
}

func TestNextRunCronWithTimezone(t *testing.T) {
	base := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	next, err := NextRun(Schedule{
		Kind: KindCron,
		Expr: "0 9 * * 1-5",
		TZ: "America/Sao_Paulo",
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("expected next run")
	}
	got := time.UnixMilli(*next).In(mustLocation(t, "America/Sao_Paulo"))
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Fatalf("local next run=%s want 09:00", got)
	}
	if got.Weekday() == time.Saturday || got.Weekday() == time.Sunday {
		t.Fatalf("next run landed on weekend: %s", got)
	}
}

func TestValidateCronRejectsBadTimezoneAndExpression(t *testing.T) {
	cases := []Schedule{
		{Kind: KindCron, Expr: "bad cron", TZ: "UTC"},
		{Kind: KindCron, Expr: "0 9 * * *", TZ: "Mars/Olympus"},
		{Kind: KindEvery, TZ: "UTC", EveryMS: int64Ptr(1000)},
	}
	for _, tc := range cases {
		if err := ValidateSchedule(tc); err == nil {
			t.Fatalf("ValidateSchedule(%+v) unexpectedly succeeded", tc)
		}
	}
}

func TestCronDayOfMonthAndWeekUseOrSemantics(t *testing.T) {
	spec, err := parseCron("0 9 1 * MON", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	// Monday that is not the first.
	monday := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	if !spec.matches(monday) {
		t.Fatal("restricted DOM/DOW should match when weekday matches")
	}
	// First of month that is not Monday.
	first := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if !spec.matches(first) {
		t.Fatal("restricted DOM/DOW should match when day-of-month matches")
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func int64Ptr(v int64) *int64 { return &v }


func TestCronSingleValueStepExpandsToFieldMaximum(t *testing.T) {
	spec, err := parseCron("5/15 * * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	for _, minute := range []int{5, 20, 35, 50} {
		at := time.Date(2026, 9, 19, 12, minute, 0, 0, time.UTC)
		if !spec.matches(at) {
			t.Fatalf("5/15 should match minute %d", minute)
		}
	}
	for _, minute := range []int{0, 6, 19, 21, 55} {
		at := time.Date(2026, 9, 19, 12, minute, 0, 0, time.UTC)
		if spec.matches(at) {
			t.Fatalf("5/15 should not match minute %d", minute)
		}
	}
}
