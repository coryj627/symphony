package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestOrchestratorRuntimeUnavailablePhaseIsTruthfulAndRejectsStart(t *testing.T) {
	engine := &runtimeEngineFake{snapshot: domain.Snapshot{Scheduler: domain.SchedulerStatus{Available: true, Enabled: true, State: "running"}}}
	runtime, err := NewOrchestratorRuntime(OrchestratorRuntimeOptions{Engine: engine, AgentReady: false})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Scheduler.Available || snapshot.Scheduler.Enabled || snapshot.Scheduler.State != "unavailable" || snapshot.Scheduler.Message != "Agent runtime will be enabled in Phase 4." {
		t.Fatalf("scheduler = %#v", snapshot.Scheduler)
	}
	if err := runtime.SetScheduler(context.Background(), true); !errors.Is(err, ErrAgentRuntimeUnavailable) {
		t.Fatalf("start error = %v", err)
	}
	if engine.setCalls() != 0 {
		t.Fatal("unavailable runtime delegated start")
	}
}

func TestOrchestratorRuntimeSerializesReadyStartAndStop(t *testing.T) {
	engine := &runtimeEngineFake{snapshot: domain.Snapshot{Scheduler: domain.SchedulerStatus{Available: true, State: "paused"}}}
	runtime, err := NewOrchestratorRuntime(OrchestratorRuntimeOptions{Engine: engine, AgentReady: true})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for _, enabled := range []bool{true, true, false, false} {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := runtime.SetScheduler(context.Background(), enabled); err != nil {
				t.Errorf("SetScheduler(%t): %v", enabled, err)
			}
		}()
	}
	group.Wait()
	if engine.maxConcurrent != 1 || engine.setCalls() != 4 {
		t.Fatalf("calls = %d, max concurrent = %d", engine.setCalls(), engine.maxConcurrent)
	}
}

type runtimeEngineFake struct {
	mu            sync.Mutex
	snapshot      domain.Snapshot
	setCount      int
	active        int
	maxConcurrent int
}

func (engine *runtimeEngineFake) Snapshot(context.Context) (domain.Snapshot, error) {
	return engine.snapshot.Clone()
}
func (*runtimeEngineFake) Issue(context.Context, string) (domain.IssueDetail, error) {
	return domain.IssueDetail{}, ErrIssueNotFound
}
func (*runtimeEngineFake) EventsAfter(context.Context, domain.EventCursor) (domain.EventPage, error) {
	return domain.EventPage{}, nil
}
func (*runtimeEngineFake) RecentEvents(context.Context, int) (domain.EventPage, error) {
	return domain.EventPage{}, nil
}
func (*runtimeEngineFake) SubscribeEvents(domain.EventCursor) <-chan struct{} {
	return make(chan struct{})
}
func (*runtimeEngineFake) Refresh(context.Context) (domain.RefreshReceipt, error) {
	return domain.RefreshReceipt{}, nil
}
func (engine *runtimeEngineFake) SetScheduler(context.Context, bool) error {
	engine.mu.Lock()
	engine.setCount++
	engine.active++
	if engine.active > engine.maxConcurrent {
		engine.maxConcurrent = engine.active
	}
	engine.mu.Unlock()
	time.Sleep(time.Millisecond)
	engine.mu.Lock()
	engine.active--
	engine.mu.Unlock()
	return nil
}
func (*runtimeEngineFake) Respond(context.Context, domain.OperatorResponse) error {
	return ErrUnavailableInPhase
}
func (engine *runtimeEngineFake) setCalls() int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.setCount
}
