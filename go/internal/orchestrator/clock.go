package orchestrator

import "time"

type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type RealClock struct{}

func (RealClock) Now() time.Time                             { return time.Now() }
func (RealClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }
func (RealClock) NewTimer(delay time.Duration) Timer         { return realTimer{timer: time.NewTimer(delay)} }

type realTimer struct{ timer *time.Timer }

func (timer realTimer) C() <-chan time.Time { return timer.timer.C }
func (timer realTimer) Stop() bool          { return timer.timer.Stop() }
