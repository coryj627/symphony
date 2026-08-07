package secrets

const defaultServicePrefix = "symphony"

type Ref struct {
	WorkflowID  string
	TrackerKind string
}

func (r Ref) Service() string {
	return defaultServicePrefix + "/workflow/" + r.WorkflowID
}

func (r Ref) Account() string {
	return "tracker/" + r.TrackerKind
}
