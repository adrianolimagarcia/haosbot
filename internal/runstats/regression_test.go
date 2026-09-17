package runstats

import (
	"math"
	"testing"
)

func TestRegression(t *testing.T) {
	var r Regression
	for x := 1.0; x <= 5; x++ {
		r.Push(x, 2*x+3)
	}
	if math.Abs(r.Slope()-2) > 1e-14 || math.Abs(r.Intercept()-3) > 1e-14 || math.Abs(r.Correlation()-1) > 1e-14 {
		t.Fatalf("unexpected regression: slope=%v intercept=%v corr=%v", r.Slope(), r.Intercept(), r.Correlation())
	}
}

func TestRegressionMerge(t *testing.T) {
	var a, b, all Regression
	for i := 1.0; i <= 10; i++ {
		all.Push(i, 3*i-2)
		if i <= 5 {
			a.Push(i, 3*i-2)
		} else {
			b.Push(i, 3*i-2)
		}
	}
	a.Merge(b)
	if math.Abs(a.Slope()-all.Slope()) > 1e-14 || math.Abs(a.Intercept()-all.Intercept()) > 1e-14 {
		t.Fatal("merge mismatch")
	}
}
