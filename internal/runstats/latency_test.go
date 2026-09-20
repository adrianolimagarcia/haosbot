package runstats

import (
	"testing"
	"time"
)

type fakeClock struct { t time.Time }
func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLatencyRecorderPhases(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := NewLatencyRecorder(clock.Now)
	clock.Advance(7*time.Millisecond); r.ProviderStart()
	clock.Advance(43*time.Millisecond); r.FirstByte()
	clock.Advance(70*time.Millisecond); r.Finish()
	got := r.Snapshot()
	if got.PreProvider != 7*time.Millisecond { t.Fatalf("pre-provider=%s", got.PreProvider) }
	if got.TTFB != 50*time.Millisecond { t.Fatalf("TTFB=%s", got.TTFB) }
	if got.Total != 120*time.Millisecond { t.Fatalf("total=%s", got.Total) }
}

func TestLatencyRecorderMilestonesAreIdempotent(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := NewLatencyRecorder(clock.Now)
	clock.Advance(time.Millisecond); r.ProviderStart()
	clock.Advance(time.Millisecond); r.ProviderStart()
	clock.Advance(time.Millisecond); r.FirstByte()
	clock.Advance(time.Millisecond); r.FirstByte()
	clock.Advance(time.Millisecond); r.Finish()
	clock.Advance(time.Millisecond); r.Finish()
	got := r.Snapshot()
	if got.PreProvider != time.Millisecond || got.TTFB != 3*time.Millisecond || got.Total != 5*time.Millisecond {
		t.Fatalf("non-idempotent milestones: %+v", got)
	}
}

func BenchmarkLatencyRecorder(b *testing.B) {
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < b.N; i++ {
		n := 0
		r := NewLatencyRecorder(func() time.Time { n++; return base.Add(time.Duration(n)) })
		r.ProviderStart(); r.FirstByte(); r.Finish(); _ = r.Snapshot()
	}
}
