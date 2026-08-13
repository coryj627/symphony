package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/orchestrator"
)

func TestProductionCodexCompositionContainsTurnAndToolFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario string
		toolFail bool
		want     domain.StopReason
	}{
		{name: "turn failure", scenario: "turn-failed", want: domain.StopReasonFailed},
		{name: "turn interrupted", scenario: "turn-interrupted", want: domain.StopReasonFailed},
		{name: "child exit", scenario: "child-exit", want: domain.StopReasonFailed},
		{name: "tool failure is protocol data", scenario: "tool-failure", toolFail: true, want: domain.StopReasonTerminal},
		{name: "unsupported tool is protocol data", scenario: "unsupported-tool", want: domain.StopReasonTerminal},
		{name: "bounded stderr noise", scenario: "stderr-noise", want: domain.StopReasonTerminal},
		{name: "silent turn stalls", scenario: "stalled", want: domain.StopReasonStalled},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := privateTempDir(t)
			snapshot := codexIntegrationSnapshot(root, buildFakeCodexCommand(t, test.scenario))
			if test.scenario == "stalled" {
				snapshot.Config.Codex.StallTimeout = 100 * time.Millisecond
			}
			issue := validIssue("CODEX-FAIL")
			adapter := &codexIntegrationAdapter{refreshed: terminalIssue(issue), toolFail: test.toolFail}
			redactor := observability.NewRedactor(nil, nil)
			redactor.RegisterSecret([]byte(fullProcessCanary))
			var logs bytes.Buffer
			build, err := ProductionAgentBuilder(redactor, slog.New(slog.NewTextHandler(&logs, nil)))(
				t.Context(), snapshot, adapter, codex.NewRequestBroker(codex.RequestBrokerOptions{}),
			)
			if err != nil {
				t.Fatal(err)
			}
			result := build.Worker.Run(t.Context(), orchestrator.RunRequest{Issue: issue, Workflow: snapshot}, nil)
			if result.Reason != test.want {
				t.Fatalf("result = %+v, want reason %q", result, test.want)
			}
			if test.toolFail && adapter.toolCallCount() != 1 {
				t.Fatalf("tool calls = %d, want 1", adapter.toolCallCount())
			}
			if strings.Contains(logs.String(), fullProcessCanary) {
				t.Fatal("bounded stderr diagnostics retained the registered canary")
			}
		})
	}
}

func TestProductionCodexCompositionRejectsBrokenPreflightProfiles(t *testing.T) {
	for _, test := range []struct{ scenario, code string }{
		{scenario: "incompatible", code: "codex_version_incompatible"},
		{scenario: "malformed", code: "codex_preflight_failed"},
		{scenario: "oversize", code: "codex_preflight_failed"},
	} {
		t.Run(test.scenario, func(t *testing.T) {
			root := privateTempDir(t)
			snapshot := codexIntegrationSnapshot(root, buildFakeCodexCommand(t, test.scenario))
			issue := validIssue("CODEX-INCOMPATIBLE")
			adapter := &codexIntegrationAdapter{refreshed: terminalIssue(issue)}
			_, err := ProductionAgentBuilder(observability.NewRedactor(nil, nil), slog.Default())(
				t.Context(), snapshot, adapter, codex.NewRequestBroker(codex.RequestBrokerOptions{}),
			)
			if err == nil {
				t.Fatalf("%s fake app-server passed preflight", test.scenario)
			}
			requirePrerequisiteCode(t, err, test.code)
		})
	}
}

func TestProductionCodexCompositionExpiresAndCancelsPendingRequests(t *testing.T) {
	for _, test := range []struct {
		name, scenario string
		cancel         bool
		want           domain.StopReason
	}{
		{name: "request expiry", scenario: "request-timeout", want: domain.StopReasonFailed},
		{name: "shutdown request", scenario: "shutdown-request", cancel: true, want: domain.StopReasonOperatorStop},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := privateTempDir(t)
			snapshot := codexIntegrationSnapshot(root, buildFakeCodexCommand(t, test.scenario))
			issue := validIssue("CODEX-REQUEST")
			adapter := &codexIntegrationAdapter{refreshed: terminalIssue(issue)}
			broker := codex.NewRequestBroker(codex.RequestBrokerOptions{Window: 150 * time.Millisecond, WarningLead: 50 * time.Millisecond})
			build, err := ProductionAgentBuilder(observability.NewRedactor(nil, nil), slog.Default())(t.Context(), snapshot, adapter, broker)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan domain.RunResult, 1)
			go func() {
				result <- build.Worker.Run(ctx, orchestrator.RunRequest{Issue: issue, Workflow: snapshot}, nil)
			}()
			if test.cancel {
				deadline := time.Now().Add(5 * time.Second)
				for len(broker.Pending()) == 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if len(broker.Pending()) != 1 {
					t.Fatal("shutdown scenario did not open its operator request")
				}
				cancel()
			}
			select {
			case got := <-result:
				if got.Reason != test.want {
					t.Fatalf("result = %+v, want %q", got, test.want)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("pending request scenario did not finish")
			}
			if len(broker.Pending()) != 0 {
				t.Fatal("pending request survived expiry or shutdown")
			}
		})
	}
}
