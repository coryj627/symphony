package cli

import "testing"

func TestParseDefaultsToRunAndCurrentWorkflow(t *testing.T) {
	got, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeRun || got.WorkflowPath != "./WORKFLOW.md" || got.Port != 0 || got.PortSet {
		t.Fatalf("unexpected defaults: %#v", got)
	}
}

func TestParseConfigureAndOverrides(t *testing.T) {
	got, err := Parse([]string{"configure", "C:/work/WORKFLOW.md", "--port", "43127", "--data-dir", "C:/state", "--open"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeConfigure || got.WorkflowPath != "C:/work/WORKFLOW.md" || got.Port != 43127 || !got.PortSet || got.DataDir != "C:/state" || !got.OpenBrowser {
		t.Fatalf("unexpected options: %#v", got)
	}
}

func TestParseTracksExplicitEphemeralPort(t *testing.T) {
	got, err := Parse([]string{"--port", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 0 || !got.PortSet {
		t.Fatalf("unexpected port options: %#v", got)
	}
}

func TestParseRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"first.md", "second.md"},
		{""},
		{"--port", "-1"},
		{"--port", "65536"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("Parse(%q) succeeded", args)
		}
	}
}
