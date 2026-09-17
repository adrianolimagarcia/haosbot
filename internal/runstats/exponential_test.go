package runstats

import (
	"math"
	"testing"
)

func TestExponential(t *testing.T) {
	e := NewExponential(0.5)
	e.Push(10)
	e.Push(20)
	if math.Abs(e.Mean()-15) > 1e-14 {
		t.Fatalf("mean=%v", e.Mean())
	}
	if e.Variance() < 0 || e.StdDev() < 0 {
		t.Fatal("variance must be non-negative")
	}
}
