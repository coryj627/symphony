package orchestrator

import (
	"sync"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }
func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}
func (*fakeClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }
func (*fakeClock) NewTimer(time.Duration) Timer         { return inertTimer{ch: make(chan time.Time)} }

type inertTimer struct{ ch chan time.Time }

func (timer inertTimer) C() <-chan time.Time { return timer.ch }
func (inertTimer) Stop() bool                { return true }
