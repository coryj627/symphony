package orchestrator

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }
func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}
func (*fakeClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }
func (clock *fakeClock) NewTimer(time.Duration) Timer {
	timer := &fakeTimer{ch: make(chan time.Time, 4)}
	clock.mu.Lock()
	clock.timers = append(clock.timers, timer)
	clock.mu.Unlock()
	return timer
}

func (clock *fakeClock) lastTimer(t *testing.T) *fakeTimer {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		clock.mu.Lock()
		if len(clock.timers) > 0 {
			timer := clock.timers[len(clock.timers)-1]
			clock.mu.Unlock()
			return timer
		}
		clock.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timer was not created")
	return nil
}

type fakeTimer struct {
	mu      sync.Mutex
	ch      chan time.Time
	stopped bool
}

func (timer *fakeTimer) C() <-chan time.Time { return timer.ch }
func (timer *fakeTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}
func (timer *fakeTimer) forceFire(at time.Time) { timer.ch <- at }
