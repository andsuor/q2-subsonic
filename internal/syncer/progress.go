package syncer

import (
	"math"
	"time"
)

const (
	emaWindow = 3 * time.Minute  // the moving average's time constant
	etaWarmup = 10 * time.Second // no ETA before this much data
)

// meter is an exponential moving average of bytes per second over ~3 min. weight corrects the
// average's bias toward its zero start, so the first seconds are not read as slow.
type meter struct {
	first, last  time.Time
	pend         int64
	rate, weight float64
}

func (m *meter) add(now time.Time, n int64) {
	if m.first.IsZero() {
		m.first, m.last = now, now
		return
	}
	m.pend += n
	dt := now.Sub(m.last).Seconds()
	if dt <= 0 {
		return
	}
	a := 1 - math.Exp(-dt/emaWindow.Seconds())
	m.rate += a * (float64(m.pend)/dt - m.rate)
	m.weight += a * (1 - m.weight)
	m.pend, m.last = 0, now
}

// eta is remaining bytes over the average rate; 0 for the first 10 s of data or with no rate.
func (m *meter) eta(now time.Time, remaining int64) time.Duration {
	if m.first.IsZero() || now.Sub(m.first) < etaWarmup || m.weight == 0 || m.rate <= 0 || remaining <= 0 {
		return 0
	}
	return time.Duration(float64(remaining) / (m.rate / m.weight) * float64(time.Second))
}

func permille(done, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(min(done*1000/total, 1000))
}
