// Package clock provides a testable time abstraction.
package clock

import "time"

// Clock provides the current time. Use Real for production and Fake for tests.
type Clock interface {
	Now() time.Time
}

// Real delegates to time.Now.
type Real struct{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// Fake returns a fixed time, adjustable by tests.
type Fake struct {
	Time time.Time
}

// Now returns the fake's fixed time.
func (f *Fake) Now() time.Time { return f.Time }

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) { f.Time = f.Time.Add(d) }
