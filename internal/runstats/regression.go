package runstats

import "math"

// Regression incrementally fits y = slope*x + intercept.
type Regression struct {
	n     float64
	meanX float64
	meanY float64
	sxx   float64
	syy   float64
	sxy   float64
}

func NewRegression() Regression { return Regression{} }

func (r *Regression) Push(x, y float64) {
	r.n++
	dx := x - r.meanX
	r.meanX += dx / r.n
	dy := y - r.meanY
	r.meanY += dy / r.n
	r.sxx += dx * (x - r.meanX)
	r.syy += dy * (y - r.meanY)
	r.sxy += dx * (y - r.meanY)
}

func (r Regression) Count() uint64 { return uint64(r.n) }
func (r Regression) Slope() float64 {
	if r.n < 2 || r.sxx == 0 {
		return math.NaN()
	}
	return r.sxy / r.sxx
}
func (r Regression) Intercept() float64 {
	if r.n < 1 {
		return math.NaN()
	}
	return r.meanY - r.Slope()*r.meanX
}
func (r Regression) Correlation() float64 {
	if r.n < 2 || r.sxx == 0 || r.syy == 0 {
		return math.NaN()
	}
	return r.sxy / math.Sqrt(r.sxx*r.syy)
}

// Merge combines two independently accumulated regressions.
func (r *Regression) Merge(other Regression) {
	if other.n == 0 {
		return
	}
	if r.n == 0 {
		*r = other
		return
	}
	n := r.n + other.n
	dx, dy := other.meanX-r.meanX, other.meanY-r.meanY
	r.sxx += other.sxx + dx*dx*r.n*other.n/n
	r.syy += other.syy + dy*dy*r.n*other.n/n
	r.sxy += other.sxy + dx*dy*r.n*other.n/n
	r.meanX += dx * other.n / n
	r.meanY += dy * other.n / n
	r.n = n
}
