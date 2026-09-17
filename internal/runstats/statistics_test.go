package runstats

import (
	"math"
	"testing"
)

func TestStatisticsMoments(t *testing.T) {
	var s Statistics
	for _, x := range []float64{1, 2, 3, 4, 5} {
		s.Push(x)
	}
	checks := []struct {
		name      string
		got, want float64
	}{
		{"mean", s.Mean(), 3}, {"variance", s.Variance(), 2.5}, {"stddev", s.StdDev(), math.Sqrt(2.5)},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > 1e-14 {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
	if s.Count() != 5 || s.Min() != 1 || s.Max() != 5 {
		t.Fatalf("metadata mismatch")
	}
}

func TestStatisticsMerge(t *testing.T) {
	var all, a, b Statistics
	for i, x := range []float64{-10, 1, 4, 8, 100, 2} {
		all.Push(x)
		if i < 3 {
			a.Push(x)
		} else {
			b.Push(x)
		}
	}
	a.Merge(b)
	for _, pair := range [][2]float64{{a.Mean(), all.Mean()}, {a.Variance(), all.Variance()}, {a.Skewness(), all.Skewness()}, {a.Kurtosis(), all.Kurtosis()}} {
		if math.Abs(pair[0]-pair[1]) > 1e-12 {
			t.Fatalf("merge mismatch: %v != %v", pair[0], pair[1])
		}
	}
}

func TestEmptyAndConstant(t *testing.T) {
	s := New()
	if !math.IsNaN(s.Min()) || !math.IsNaN(s.Max()) || !math.IsNaN(s.Variance()) {
		t.Fatal("empty values must be NaN")
	}
	s.Push(3)
	s.Push(3)
	if s.Variance() != 0 || !math.IsNaN(s.Skewness()) {
		t.Fatal("constant moments mismatch")
	}
}
