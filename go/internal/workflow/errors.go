package workflow

import "errors"

var (
	ErrMissingWorkflow   = errors.New("missing_workflow_file")
	ErrWorkflowParse     = errors.New("workflow_parse_error")
	ErrFrontMatterNotMap = errors.New("workflow_front_matter_not_a_map")
	ErrTemplateParse     = errors.New("template_parse_error")
	ErrTemplateRender    = errors.New("template_render_error")
)
