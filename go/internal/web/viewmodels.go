package web

// Page is the common view model supplied to every rendered Symphony page.
type Page struct {
	Title     string
	Route     string
	Heading   string
	Mode      string
	Flash     string
	Status    string
	CSRFToken string
	Content   any
}

type overviewContent struct {
	Repository string
}

type issuesContent struct{}

type issueContent struct {
	Identifier string
}

type activityContent struct{}

type configurationContent struct{}

type logsContent struct{}

type errorContent struct {
	Instruction string
}
