package runstats

import "math"

// Exponential tracks exponentially weighted mean and variance. Decay must be
// in [0,1); values close to one retain a longer history.
type Exponential struct {
	Decay    float64
	mean     float64
	variance float64
	count    uint64
}

func NewExponential(decay float64) Exponential {
	if decay < 0 {
		decay = 0
	}
	if decay >= 1 {
		decay = math.Nextafter(1, 0)
	}
	return Exponential{Decay: decay}
}

func (s *Exponential) Push(x float64) {
	alpha := 1 - s.Decay
	if s.count == 0 {
		s.mean = x
		s.variance = 0
		s.count = 1
		return
	}
	diff := x - s.mean
	s.variance += alpha * (s.Decay*diff*diff - s.variance)
	s.mean += alpha * diff
	s.count++
}

func (s Exponential) Count() uint64     { return s.count }
func (s Exponential) Mean() float64     { return s.mean }
func (s Exponential) Variance() float64 { return s.variance }
func (s Exponential) StdDev() float64   { return math.Sqrt(s.variance) }
