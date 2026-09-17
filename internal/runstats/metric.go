package runstats

// Metric combines lifetime moments with fast and slow exponential trends.
type Metric struct {
	AllTime Statistics
	Fast    Exponential
	Slow    Exponential
}

func NewMetric() Metric {
	return Metric{AllTime: New(), Fast: NewExponential(0.80), Slow: NewExponential(0.98)}
}

func (m *Metric) Push(value float64) {
	m.AllTime.Push(value)
	m.Fast.Push(value)
	m.Slow.Push(value)
}
