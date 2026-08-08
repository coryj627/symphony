package web

import "github.com/coryj627/symphony/go/internal/app"

// Page is the common view model supplied to every rendered Symphony page.
type Page struct {
	Title                string
	Route                string
	Heading              string
	Mode                 string
	Flash                string
	FlashKind            string
	Status               string
	CSRFToken            string
	FocusTarget          string
	ErrorSummary         []PageError
	ErrorSummaryInDialog bool
	Content              any
}

type PageError struct {
	ControlID string
	Message   string
}

type overviewContent struct {
	Repository string
	Mode       string
}

type issuesContent struct{}

type issueContent struct {
	Identifier string
}

type activityContent struct{}

type configurationContent struct {
	View               app.ConfigView
	Values             app.ConfigValues
	RawSource          string
	CurrentSource      string
	Credential         app.CredentialState
	Errors             map[string]string
	DeleteConfirmation bool
}

type logsContent struct{}

type errorContent struct {
	Instruction string
}
