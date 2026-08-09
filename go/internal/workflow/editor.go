package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var (
	ErrSaveConflict    = errors.New("workflow_save_conflict")
	ErrInvalidWorkflow = errors.New("invalid_workflow")
	ErrInvalidSave     = errors.New("invalid_save_command")
)

type FieldError struct {
	Field   string
	Code    string
	Message string
}

type SafeError struct {
	Code    string
	Message string
}

type ValidationResult struct {
	Valid        bool
	FieldErrors  []FieldError
	GlobalErrors []SafeError
}

type InvalidWorkflowError struct {
	Validation ValidationResult
}

func (err *InvalidWorkflowError) Error() string { return ErrInvalidWorkflow.Error() }
func (err *InvalidWorkflowError) Unwrap() error { return ErrInvalidWorkflow }

// StructuredPatch is the supported structured-editor surface. Pointer fields
// distinguish an omitted form value from an explicit zero or empty value.
// Provider-owned mappings are patched one known leaf at a time so unknown
// provider extension keys remain untouched.
type StructuredPatch struct {
	TrackerKind           *string
	ProviderOwner         *string
	ProviderRepository    *string
	ProviderProjectSlug   *string
	ProviderEndpoint      *string
	ProviderCredentialRef *string
	ProviderAssignee      *string
	TrackerRequiredLabels *[]string
	TrackerActiveStates   *[]string
	TrackerTerminalStates *[]string

	PollingIntervalMS *int
	WorkspaceRoot     *string

	HookAfterCreate  *string
	HookBeforeRun    *string
	HookAfterRun     *string
	HookBeforeRemove *string
	HookTimeoutMS    *int

	AgentMaxConcurrent     *int
	AgentMaxTurns          *int
	AgentMaxRetryBackoffMS *int

	CodexCommand        *string
	CodexApprovalPolicy *any
	CodexThreadSandbox  *string
	CodexTurnTimeoutMS  *int
	CodexReadTimeoutMS  *int
	CodexStallTimeoutMS *int

	ServerPort                      *int
	ServerOperatorResponseTimeoutMS *int
}

type SaveCommand struct {
	BaseDigest string
	RawSource  []byte
	Patch      *StructuredPatch
}

func (store *FileStore) Save(ctx context.Context, command SaveCommand) (Snapshot, error) {
	if !store.beginOperation() {
		return Snapshot{}, ErrStoreClosed
	}
	defer store.endOperation()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if (command.RawSource == nil) == (command.Patch == nil) {
		return Snapshot{}, ErrInvalidSave
	}
	if err := store.pathMu.acquire(ctx, store.stopping); err != nil {
		return Snapshot{}, err
	}
	defer store.pathMu.release()

	currentSource, err := os.ReadFile(store.path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return Snapshot{}, fmt.Errorf("reload workflow before save: %w", err)
	}
	currentDigest := ""
	if !missing {
		currentDigest = digestSource(currentSource)
	}
	if currentDigest != command.BaseDigest {
		return Snapshot{}, ErrSaveConflict
	}

	candidate := command.RawSource
	if command.Patch != nil {
		candidate, err = patchStructuredSource(store.path, currentSource, command.Patch)
		if err != nil {
			result := safeValidation(err)
			return Snapshot{}, &InvalidWorkflowError{Validation: result}
		}
	}
	snapshot, validation, candidateErr := store.snapshotFromSource(candidate)
	if candidateErr != nil || !validation.Valid {
		latest, readErr := os.ReadFile(store.path)
		latestDigest := ""
		if readErr == nil {
			latestDigest = digestSource(latest)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return Snapshot{}, fmt.Errorf("recheck workflow after validation: %w", readErr)
		}
		if latestDigest != command.BaseDigest {
			return Snapshot{}, ErrSaveConflict
		}
		return Snapshot{}, &InvalidWorkflowError{Validation: validation}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	select {
	case <-store.stopping:
		return Snapshot{}, ErrStoreClosed
	default:
	}

	replaceErr := atomicReplaceChecked(store.path, candidate, command.BaseDigest, store.atomic)
	if replaceErr != nil && !errors.Is(replaceErr, ErrDurabilityUncertain) {
		return Snapshot{}, replaceErr
	}
	visible, readErr := os.ReadFile(store.path)
	if readErr != nil || digestSource(visible) != snapshot.Digest {
		_, _ = store.loadStableLocked(ctx)
		return Snapshot{}, errors.Join(ErrSaveConflict, replaceErr)
	}
	select {
	case <-store.stopping:
		return Snapshot{}, ErrStoreClosed
	default:
	}
	if !store.installSnapshotIfOpen(snapshot, true) {
		return snapshot, ErrStoreClosed
	}
	if replaceErr != nil {
		return snapshot, replaceErr
	}
	return snapshot, nil
}

func digestSource(source []byte) string {
	digest := sha256.Sum256(source)
	return fmt.Sprintf("%x", digest)
}

func patchStructuredSource(path string, source []byte, patch *StructuredPatch) ([]byte, error) {
	definition, err := Parse(path, source)
	if err != nil {
		return nil, err
	}
	if definition.FrontMatter == nil || len(definition.FrontMatter.Content) != 1 || definition.FrontMatter.Content[0].Kind != yaml.MappingNode {
		return nil, ErrFrontMatterNotMap
	}
	root := definition.FrontMatter.Content[0]

	setStringPointer(root, []string{"tracker", "kind"}, patch.TrackerKind)
	setStringPointer(root, []string{"tracker", "provider", "owner"}, patch.ProviderOwner)
	setStringPointer(root, []string{"tracker", "provider", "repository"}, patch.ProviderRepository)
	setStringPointer(root, []string{"tracker", "provider", "project_slug"}, patch.ProviderProjectSlug)
	setStringPointer(root, []string{"tracker", "provider", "endpoint"}, patch.ProviderEndpoint)
	setStringPointer(root, []string{"tracker", "provider", "credential_ref"}, patch.ProviderCredentialRef)
	setStringPointer(root, []string{"tracker", "provider", "assignee"}, patch.ProviderAssignee)
	setStringSlicePointer(root, []string{"tracker", "required_labels"}, patch.TrackerRequiredLabels)
	setStringSlicePointer(root, []string{"tracker", "active_states"}, patch.TrackerActiveStates)
	setStringSlicePointer(root, []string{"tracker", "terminal_states"}, patch.TrackerTerminalStates)

	setIntPointer(root, []string{"polling", "interval_ms"}, patch.PollingIntervalMS)
	setStringPointer(root, []string{"workspace", "root"}, patch.WorkspaceRoot)
	setStringPointer(root, []string{"hooks", "after_create"}, patch.HookAfterCreate)
	setStringPointer(root, []string{"hooks", "before_run"}, patch.HookBeforeRun)
	setStringPointer(root, []string{"hooks", "after_run"}, patch.HookAfterRun)
	setStringPointer(root, []string{"hooks", "before_remove"}, patch.HookBeforeRemove)
	setIntPointer(root, []string{"hooks", "timeout_ms"}, patch.HookTimeoutMS)
	setIntPointer(root, []string{"agent", "max_concurrent_agents"}, patch.AgentMaxConcurrent)
	setIntPointer(root, []string{"agent", "max_turns"}, patch.AgentMaxTurns)
	setIntPointer(root, []string{"agent", "max_retry_backoff_ms"}, patch.AgentMaxRetryBackoffMS)
	setStringPointer(root, []string{"codex", "command"}, patch.CodexCommand)
	if err := setAnyPointer(root, []string{"codex", "approval_policy"}, patch.CodexApprovalPolicy); err != nil {
		return nil, err
	}
	setStringPointer(root, []string{"codex", "thread_sandbox"}, patch.CodexThreadSandbox)
	setIntPointer(root, []string{"codex", "turn_timeout_ms"}, patch.CodexTurnTimeoutMS)
	setIntPointer(root, []string{"codex", "read_timeout_ms"}, patch.CodexReadTimeoutMS)
	setIntPointer(root, []string{"codex", "stall_timeout_ms"}, patch.CodexStallTimeoutMS)
	setIntPointer(root, []string{"server", "port"}, patch.ServerPort)
	setIntPointer(root, []string{"server", "operator_response_timeout_ms"}, patch.ServerOperatorResponseTimeoutMS)

	frontMatter, err := encodeFrontMatter(definition.FrontMatter)
	if err != nil {
		return nil, fmt.Errorf("encode workflow front matter: %w", err)
	}
	prompt, delimiterEnding, hadFrontMatter, splitErr := exactPromptSuffix(source)
	if splitErr != nil {
		return nil, splitErr
	}
	if !hadFrontMatter {
		delimiterEnding = []byte("\n")
		prompt = source
	}
	openingEnding := []byte("\n")
	if firstEnd, ending := lineEnd(source, 0); hadFrontMatter && firstEnd >= len(ending) && len(ending) > 0 {
		openingEnding = ending
	}
	result := make([]byte, 0, len(frontMatter)+len(prompt)+16)
	result = append(result, "---"...)
	result = append(result, openingEnding...)
	result = append(result, frontMatter...)
	if len(result) == 0 || result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, "---"...)
	result = append(result, delimiterEnding...)
	result = append(result, prompt...)
	return result, nil
}

func encodeFrontMatter(document *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	if bytes.HasPrefix(encoded, []byte("---\n")) {
		encoded = encoded[4:]
	}
	return append([]byte(nil), encoded...), nil
}

func exactPromptSuffix(source []byte) (prompt, delimiterEnding []byte, hadFrontMatter bool, err error) {
	firstEnd, firstEnding := lineEnd(source, 0)
	if firstEnd < 0 || string(source[:firstEnd-len(firstEnding)]) != "---" {
		return source, nil, false, nil
	}
	position := firstEnd
	for position <= len(source) {
		end, ending := lineEnd(source, position)
		if end < 0 {
			end = len(source)
			ending = nil
		}
		contentEnd := end - len(ending)
		if contentEnd >= position && string(source[position:contentEnd]) == "---" {
			return source[end:], append([]byte(nil), ending...), true, nil
		}
		if end >= len(source) {
			break
		}
		position = end
	}
	return nil, nil, true, ErrWorkflowParse
}

func lineEnd(source []byte, start int) (int, []byte) {
	if start > len(source) {
		return -1, nil
	}
	index := bytes.IndexByte(source[start:], '\n')
	if index < 0 {
		return len(source), nil
	}
	end := start + index + 1
	if end >= 2 && source[end-2] == '\r' {
		return end, []byte("\r\n")
	}
	return end, []byte("\n")
}

func setStringPointer(root *yaml.Node, path []string, value *string) {
	if value == nil {
		return
	}
	node := mappingPath(root, path)
	if editableScalarValue(node) == *value {
		return
	}
	prepareEditableNode(root, node)
	node.Kind = yaml.ScalarNode
	if node.Tag == "" || strings.HasPrefix(node.Tag, "!!") {
		node.Tag = "!!str"
	}
	node.Value = *value
}

func setIntPointer(root *yaml.Node, path []string, value *int) {
	if value == nil {
		return
	}
	node := mappingPath(root, path)
	if editableScalarValue(node) == strconv.Itoa(*value) {
		return
	}
	prepareEditableNode(root, node)
	node.Kind = yaml.ScalarNode
	if node.Tag == "" || strings.HasPrefix(node.Tag, "!!") {
		node.Tag = "!!int"
	}
	node.Value = strconv.Itoa(*value)
}

func setStringSlicePointer(root *yaml.Node, path []string, value *[]string) {
	if value == nil {
		return
	}
	node := mappingPath(root, path)
	prepareEditableNode(root, node)
	old := append([]*yaml.Node(nil), node.Content...)
	originalTag := node.Tag
	node.Kind = yaml.SequenceNode
	if originalTag == "" || strings.HasPrefix(originalTag, "!!") {
		node.Tag = "!!seq"
	} else {
		node.Tag = originalTag
	}
	node.Value = ""
	node.Content = make([]*yaml.Node, 0, len(*value))
	assigned := make([]*yaml.Node, len(*value))
	used := make([]bool, len(old))
	for index, item := range *value {
		for oldIndex, child := range old {
			if !used[oldIndex] && editableScalarValue(child) == item {
				assigned[index] = child
				used[oldIndex] = true
				break
			}
		}
	}
	for index := range *value {
		item := (*value)[index]
		var child *yaml.Node
		if assigned[index] != nil {
			child = assigned[index]
			node.Content = append(node.Content, child)
			continue
		}
		for oldIndex, candidate := range old {
			if !used[oldIndex] {
				child = candidate
				used[oldIndex] = true
				break
			}
		}
		if child != nil {
			prepareEditableNode(root, child)
		} else {
			child = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}
		}
		child.Kind = yaml.ScalarNode
		if child.Tag == "" || strings.HasPrefix(child.Tag, "!!") {
			child.Tag = "!!str"
		}
		child.Value = item
		node.Content = append(node.Content, child)
	}
	for index, child := range old {
		if !used[index] {
			detachDroppedAnchors(root, child)
		}
	}
}

func setAnyPointer(root *yaml.Node, path []string, value *any) error {
	if value == nil {
		return nil
	}
	if current := existingMappingPath(root, path); current != nil {
		var decoded any
		if err := current.Decode(&decoded); err == nil && reflect.DeepEqual(decoded, *value) {
			return nil
		}
	}

	node := mappingPath(root, path)
	prepareEditableNode(root, node)
	originalKind, originalTag, originalStyle := node.Kind, node.Tag, node.Style
	head, line, foot := node.HeadComment, node.LineComment, node.FootComment
	var replacement yaml.Node
	if err := replacement.Encode(*value); err != nil {
		return fmt.Errorf("encode structured value: %w", err)
	}
	if replacement.Kind == yaml.DocumentNode && len(replacement.Content) == 1 {
		replacement = *replacement.Content[0]
	}
	if originalKind == replacement.Kind {
		replacement.Style = originalStyle
	}
	if originalTag != "" && !strings.HasPrefix(originalTag, "!!") {
		replacement.Tag = originalTag
	}
	replacement.HeadComment = head
	replacement.LineComment = line
	replacement.FootComment = foot
	*node = replacement
	return nil
}

func existingMappingPath(root *yaml.Node, path []string) *yaml.Node {
	current := root
	for _, key := range path {
		current = mergedMappingValue(current, key, map[*yaml.Node]bool{})
		if current == nil {
			return nil
		}
	}
	return current
}

func detachDroppedAnchors(root, node *yaml.Node) {
	if node.Anchor != "" {
		deAliasConsumers(root, node, cloneYAMLNode(node))
	}
	for _, child := range node.Content {
		detachDroppedAnchors(root, child)
	}
}

func editableScalarValue(node *yaml.Node) string {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias.Value
	}
	return node.Value
}

func prepareEditableNode(root, node *yaml.Node) {
	if node.Kind != yaml.AliasNode || node.Alias == nil {
		if node.Anchor != "" {
			old := cloneYAMLNode(node)
			deAliasConsumers(root, node, old)
			node.Anchor = ""
		}
		return
	}
	head, line, foot := node.HeadComment, node.LineComment, node.FootComment
	copy := cloneYAMLNode(node.Alias)
	copy.Anchor = ""
	copy.Alias = nil
	*node = *copy
	if head != "" {
		node.HeadComment = head
	}
	if line != "" {
		node.LineComment = line
	}
	if foot != "" {
		node.FootComment = foot
	}
}

func deAliasConsumers(current, target, old *yaml.Node) {
	for _, child := range current.Content {
		if child.Kind == yaml.AliasNode && child.Alias == target {
			replacement := cloneYAMLNode(old)
			replacement.Anchor = ""
			replacement.Alias = nil
			*child = *replacement
			continue
		}
		deAliasConsumers(child, target, old)
	}
}

func mappingPath(root *yaml.Node, path []string) *yaml.Node {
	current := root
	for index, key := range path {
		prepareEditableNode(root, current)
		var found *yaml.Node
		for child := 0; child+1 < len(current.Content); child += 2 {
			if current.Content[child].Value == key {
				found = current.Content[child+1]
				break
			}
		}
		if found == nil {
			if inherited := mergedMappingValue(current, key, map[*yaml.Node]bool{}); inherited != nil {
				found = cloneYAMLNode(inherited)
				current.Content = append(current.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, found)
			}
		}
		if found == nil {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
			found = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}
			current.Content = append(current.Content, keyNode, found)
		}
		current = found
		if index < len(path)-1 {
			prepareEditableNode(root, current)
		}
		if index < len(path)-1 && current.Kind != yaml.MappingNode {
			current.Kind = yaml.MappingNode
			current.Tag = "!!map"
			current.Value = ""
			current.Content = nil
		}
	}
	return current
}

func mergedMappingValue(mapping *yaml.Node, key string, seen map[*yaml.Node]bool) *yaml.Node {
	if mapping == nil || seen[mapping] {
		return nil
	}
	seen[mapping] = true
	if mapping.Kind == yaml.AliasNode {
		return mergedMappingValue(mapping.Alias, key, seen)
	}
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key && key != "<<" {
			return mapping.Content[i+1]
		}
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "<<" {
			continue
		}
		merge := mapping.Content[i+1]
		candidates := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			candidates = merge.Content
		}
		for _, candidate := range candidates {
			if value := mergedMappingValue(candidate, key, seen); value != nil {
				return value
			}
		}
	}
	return nil
}

func safeValidation(err error) ValidationResult {
	result := ValidationResult{Valid: false, FieldErrors: []FieldError{}, GlobalErrors: []SafeError{}}
	code := "invalid_workflow"
	message := "The workflow is invalid. Review the highlighted settings."
	switch {
	case errors.Is(err, ErrMissingWorkflow):
		code, message = "missing_workflow_file", "The workflow file does not exist."
	case errors.Is(err, ErrFrontMatterNotMap):
		code, message = "workflow_front_matter_not_a_map", "Workflow front matter must be a YAML mapping."
	case errors.Is(err, ErrWorkflowParse):
		code, message = "workflow_parse_error", "The workflow could not be parsed."
	case errors.Is(err, ErrTemplateParse):
		code, message = "template_parse_error", "The workflow prompt template could not be parsed."
	}
	result.GlobalErrors = append(result.GlobalErrors, SafeError{Code: code, Message: message})
	return result
}

func validResult(fieldErrors []FieldError) ValidationResult {
	result := ValidationResult{Valid: len(fieldErrors) == 0, FieldErrors: append([]FieldError(nil), fieldErrors...), GlobalErrors: []SafeError{}}
	if result.FieldErrors == nil {
		result.FieldErrors = []FieldError{}
	}
	return result
}

func snapshotAt(path string, source []byte, definition Definition, config EffectiveConfig) Snapshot {
	return Snapshot{
		Path:       path,
		Source:     string(source),
		Digest:     digestSource(source),
		Definition: definition,
		Config:     config,
		LoadedAt:   time.Now(),
	}
}
