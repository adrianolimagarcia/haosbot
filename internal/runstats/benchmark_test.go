package runstats

import "testing"

func BenchmarkPush(b *testing.B) {
	var s Statistics
	for i := 0; i < b.N; i++ {
		s.Push(float64(i))
	}
	_ = s
}

func BenchmarkMetricPush(b *testing.B) {
	m := NewMetric()
	for i := 0; i < b.N; i++ {
		m.Push(float64(i))
	}
	_ = m
}
