package domain

type Hook string

const (
	HookAfterCreate  Hook = "after_create"
	HookBeforeRun    Hook = "before_run"
	HookAfterRun     Hook = "after_run"
	HookBeforeRemove Hook = "before_remove"
)

type Workspace struct {
	Path            string `json:"path"`
	Key             string `json:"workspace_key"`
	CreatedNow      bool   `json:"created_now"`
	Owned           bool   `json:"owned"`
	Root            string `json:"-"`
	RootIdentity    string `json:"-"`
	IssueID         string `json:"-"`
	IssueIdentifier string `json:"-"`
}
