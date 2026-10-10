// Package clock abstracts time so time-dependent rules are testable.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time. Inject it instead of calling time.Now in domain and
// application code.
type Clock interface {
	Now() time.Time
}

// System is the real clock (UTC).
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// Default is the system clock.
var Default Clock = System{}

// Fixed is a controllable clock for tests. Safe for concurrent use.
type Fixed struct {
	mu sync.Mutex
	t  time.Time
}

// NewFixed returns a clock frozen at t.
func NewFixed(t time.Time) *Fixed { return &Fixed{t: t} }

func (f *Fixed) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }

// Set moves the clock to t.
func (f *Fixed) Set(t time.Time) { f.mu.Lock(); f.t = t; f.mu.Unlock() }

// Advance moves the clock forward by d.
func (f *Fixed) Advance(d time.Duration) { f.mu.Lock(); f.t = f.t.Add(d); f.mu.Unlock() }

// Age returns the age in full years at `at`.
func Age(birth, at time.Time) int {
	by, bm, bd := birth.Date()
	ay, am, ad := at.Date()
	age := ay - by
	if am < bm || (am == bm && ad < bd) {
		age--
	}
	return age
}

// StartOfDay returns midnight of t's day in t's location.
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// AddMonths adds calendar months, clamping to the end of month (Jan 31 + 1 = Feb 28/29),
// unlike time.AddDate which overflows into the next month.
func AddMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	last := first.AddDate(0, 1, -1).Day()
	if d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}
