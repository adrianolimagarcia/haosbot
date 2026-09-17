// Package runstats provides allocation-free online statistics for telemetry.
//
// The higher-order moment recurrence is based on the parallel/online formulas
// described by Philippe Pébay (SAND2008-6212) and used by python-runstats.
package runstats

import "math"

// Statistics accumulates count, extrema, mean, variance, skewness and kurtosis
// without retaining samples. The zero value is ready for use.
type Statistics struct {
	count float64
	mean  float64
	m2    float64
	m3    float64
	m4    float64
	min   float64
	max   float64
}

// New returns an empty accumulator.
func New() Statistics { return Statistics{min: math.NaN(), max: math.NaN()} }

// Push adds one sample. It performs no allocation.
func (s *Statistics) Push(x float64) {
	if s.count == 0 {
		s.min, s.max = x, x
	} else {
		if x < s.min {
			s.min = x
		}
		if x > s.max {
			s.max = x
		}
	}

	delta := x - s.mean
	deltaN := delta / (s.count + 1)
	deltaN2 := deltaN * deltaN
	term := delta * deltaN * s.count
	s.count++
	s.mean += deltaN
	s.m4 += term*deltaN2*(s.count*s.count-3*s.count+3) + 6*deltaN2*s.m2 - 4*deltaN*s.m3
	s.m3 += term*deltaN*(s.count-2) - 3*deltaN*s.m2
	s.m2 += term
}

// Merge combines other into s. It is equivalent to pushing both datasets in
// sequence, up to floating-point rounding, and enables lock-free worker-local
// aggregation. Other is not modified.
func (s *Statistics) Merge(other Statistics) {
	if other.count == 0 {
		return
	}
	if s.count == 0 {
		*s = other
		return
	}

	n1, n2 := s.count, other.count
	n := n1 + n2
	delta := other.mean - s.mean
	delta2 := delta * delta
	delta3 := delta2 * delta
	delta4 := delta3 * delta

	m2 := s.m2 + other.m2 + delta2*n1*n2/n
	m3 := s.m3 + other.m3 + delta3*n1*n2*(n1-n2)/(n*n) + 3*delta*(n1*other.m2-n2*s.m2)/n
	m4 := s.m4 + other.m4 + delta4*n1*n2*(n1*n1-n1*n2+n2*n2)/(n*n*n) + 6*delta2*(n1*n1*other.m2+n2*n2*s.m2)/(n*n) + 4*delta*(n1*other.m3-n2*s.m3)/n

	s.mean += delta * n2 / n
	s.count = n
	s.m2, s.m3, s.m4 = m2, m3, m4
	if other.min < s.min {
		s.min = other.min
	}
	if other.max > s.max {
		s.max = other.max
	}
}

// Count returns the number of samples.
func (s Statistics) Count() uint64 { return uint64(s.count) }
func (s Statistics) Mean() float64 { return s.mean }
func (s Statistics) Min() float64  { return s.min }
func (s Statistics) Max() float64  { return s.max }

// Variance returns the unbiased sample variance, or NaN for fewer than two
// samples.
func (s Statistics) Variance() float64 {
	if s.count < 2 {
		return math.NaN()
	}
	return s.m2 / (s.count - 1)
}

func (s Statistics) StdDev() float64 { return math.Sqrt(s.Variance()) }

// Skewness returns the moment coefficient (not the adjusted Fisher-Pearson
// estimator), or NaN when it is undefined.
func (s Statistics) Skewness() float64 {
	if s.count < 2 || s.m2 == 0 {
		return math.NaN()
	}
	return math.Sqrt(s.count) * s.m3 / math.Pow(s.m2, 1.5)
}

// Kurtosis returns excess kurtosis (normal distribution = 0), or NaN when it
// is undefined.
func (s Statistics) Kurtosis() float64 {
	if s.count < 2 || s.m2 == 0 {
		return math.NaN()
	}
	return s.count*s.m4/(s.m2*s.m2) - 3
}
