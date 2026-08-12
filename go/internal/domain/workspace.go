package domain

type HookName string

const (
	HookNameAfterCreate  HookName = "after_create"
	HookNameBeforeRun    HookName = "before_run"
	HookNameAfterRun     HookName = "after_run"
	HookNameBeforeRemove HookName = "before_remove"
)

type Hook struct {
	Name   HookName
	Script string
}

var (
	HookAfterCreate  = Hook{Name: HookNameAfterCreate}
	HookBeforeRun    = Hook{Name: HookNameBeforeRun}
	HookAfterRun     = Hook{Name: HookNameAfterRun}
	HookBeforeRemove = Hook{Name: HookNameBeforeRemove}
)

func (hook Hook) WithScript(script string) Hook {
	hook.Script = script
	return hook
}

type Workspace struct {
	Path            string `json:"path"`
	Key             string `json:"workspace_key"`
	CreatedNow      bool   `json:"created_now"`
	Owned           bool   `json:"owned"`
	Root            string `json:"-"`
	RootIdentity    string `json:"-"`
	PathIdentity    string `json:"-"`
	IssueID         string `json:"-"`
	IssueIdentifier string `json:"-"`
}
