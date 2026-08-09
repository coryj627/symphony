//go:build e2e

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLiveFixtureCapacityAllocatesAnAtomicIsolatedScenarioSetPerClient(t *testing.T) {
	resolver := newPageDependencyResolver()
	resolve := func(client int, scenario string) (pageDependencies, bool) {
		request := httptest.NewRequest(http.MethodGet, "/issues?__e2e_scenario="+scenario, nil)
		request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, fmt.Sprintf("e2e-client-%d", client)))
		dependencies, resolvedScenario, ok := resolver(request, pageDependencies{})
		if ok && resolvedScenario != scenario {
			t.Fatalf("client %d scenario = %q, want %q", client, resolvedScenario, scenario)
		}
		return dependencies, ok
	}

	var first, last pageDependencies
	for client := range maximumE2ELiveClients {
		for _, scenario := range e2eLiveScenarios {
			dependencies, ok := resolve(client, scenario)
			if !ok {
				t.Fatalf("client %d was rejected for %q before capacity", client, scenario)
			}
			if scenario == "live-focus" && client == 0 {
				first = dependencies
			}
			if scenario == "live-focus" && client == maximumE2ELiveClients-1 {
				last = dependencies
			}
		}
	}
	if first.queries == last.queries {
		t.Fatal("separate client identities shared a mutable live-focus runtime")
	}

	lastClient := maximumE2ELiveClients - 1
	seen := make(map[*liveE2ERuntime]struct{}, len(e2eLiveScenarios))
	for _, scenario := range e2eLiveScenarios {
		dependencies, ok := resolve(lastClient, scenario)
		if !ok {
			t.Fatalf("last accepted client lost atomic access to %q", scenario)
		}
		runtime, ok := dependencies.queries.(*liveE2ERuntime)
		if !ok {
			t.Fatalf("%q page runtime = %T, want *liveE2ERuntime", scenario, dependencies.queries)
		}
		if runtime.scenario != scenario {
			t.Fatalf("%q runtime scenario = %q", scenario, runtime.scenario)
		}
		if _, duplicate := seen[runtime]; duplicate {
			t.Fatalf("last accepted client shared a runtime across scenarios at %q", scenario)
		}
		seen[runtime] = struct{}{}
	}

	for _, scenario := range e2eLiveScenarios {
		if _, ok := resolve(maximumE2ELiveClients, scenario); ok {
			t.Fatalf("over-capacity client unexpectedly allocated %q", scenario)
		}
	}
}
