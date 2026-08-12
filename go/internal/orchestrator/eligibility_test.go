package orchestrator

import "testing"

func TestEligibleAppliesProviderNeutralRulesOnly(t *testing.T) {
	cfg := testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, 2)
	view := View{
		RunningIDs:     idSet("1"),
		ClaimedIDs:     idSet("2"),
		RunningByState: map[string]int{"open": 1},
	}

	tests := []struct {
		name         string
		issueID      string
		state        string
		labels       []string
		dispatchable bool
		want         bool
	}{
		{name: "ready", issueID: "3", state: " OPEN ", labels: []string{" SYMPHONY "}, dispatchable: true, want: true},
		{name: "provider rejected", issueID: "3", state: "open", labels: []string{"symphony"}, dispatchable: false, want: false},
		{name: "missing label", issueID: "3", state: "open", labels: []string{"other"}, dispatchable: true, want: false},
		{name: "running", issueID: "1", state: "open", labels: []string{"symphony"}, dispatchable: true, want: false},
		{name: "claimed", issueID: "2", state: "open", labels: []string{"symphony"}, dispatchable: true, want: false},
		{name: "inactive", issueID: "3", state: "paused", labels: []string{"symphony"}, dispatchable: true, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := readyIssue(test.issueID, test.state, test.labels...)
			issue.Dispatchable = test.dispatchable
			if got := Eligible(issue, view, cfg); got != test.want {
				t.Fatalf("Eligible() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEligibleEnforcesTerminalPrecedenceAndConcurrencyCaps(t *testing.T) {
	issue := readyIssue("3", "open", "symphony")

	t.Run("terminal wins over active", func(t *testing.T) {
		cfg := testConfig([]string{"open"}, []string{"OPEN"}, []string{"symphony"}, 2)
		if Eligible(issue, View{RunningByState: map[string]int{}}, cfg) {
			t.Fatal("terminal issue was eligible")
		}
	})

	t.Run("global cap", func(t *testing.T) {
		cfg := testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, 1)
		view := View{RunningIDs: idSet("other"), RunningByState: map[string]int{"open": 1}}
		if Eligible(issue, view, cfg) {
			t.Fatal("issue exceeded global cap")
		}
	})

	t.Run("negative global cap floors at zero", func(t *testing.T) {
		cfg := testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, -1)
		if Eligible(issue, View{RunningByState: map[string]int{}}, cfg) {
			t.Fatal("negative global cap allowed dispatch")
		}
	})

	t.Run("per-state cap", func(t *testing.T) {
		cfg := testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, 3)
		cfg.Agent.MaxConcurrentByState[" OPEN "] = 1
		view := View{RunningIDs: idSet("other"), RunningByState: map[string]int{"open": 1}}
		if Eligible(issue, view, cfg) {
			t.Fatal("issue exceeded per-state cap")
		}
	})

	t.Run("missing per-state cap falls back to global", func(t *testing.T) {
		cfg := testConfig([]string{"open", "triage"}, []string{"closed"}, []string{"symphony"}, 2)
		triage := readyIssue("3", "triage", "symphony")
		view := View{RunningIDs: idSet("other"), RunningByState: map[string]int{"open": 1}}
		if !Eligible(triage, view, cfg) {
			t.Fatal("global fallback rejected an available slot")
		}
	})
}

func TestEligibleRejectsBlankRequiredLabel(t *testing.T) {
	cfg := testConfig([]string{"open"}, []string{"closed"}, []string{"   "}, 1)
	issue := readyIssue("1", "open", "")
	if Eligible(issue, View{RunningByState: map[string]int{}}, cfg) {
		t.Fatal("blank configured label matched")
	}
}
