package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

type Adapter struct {
	config   tracker.LinearConfig
	token    []byte
	client   *http.Client
	logger   *slog.Logger
	endpoint *url.URL
	tokenMu  sync.RWMutex
}

func New(config tracker.LinearConfig, token []byte, client *http.Client, logger *slog.Logger) (*Adapter, error) {
	endpoint, err := parseEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.ProjectSlug) == "" {
		return nil, configError("Linear project slug is required")
	}
	if len(bytes.TrimSpace(token)) == 0 {
		return nil, authError("Linear credential is missing")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	config.ActiveStates = append([]string(nil), config.ActiveStates...)
	config.TerminalStates = append([]string(nil), config.TerminalStates...)
	return &Adapter{
		config: config, token: append([]byte(nil), token...), client: cloneHTTPClient(client),
		logger: logger, endpoint: endpoint,
	}, nil
}

func (adapter *Adapter) Kind() string { return "linear" }

func (adapter *Adapter) Close() error {
	adapter.tokenMu.Lock()
	clear(adapter.token)
	adapter.token = nil
	adapter.tokenMu.Unlock()
	return nil
}

func (adapter *Adapter) authorization() string {
	adapter.tokenMu.RLock()
	defer adapter.tokenMu.RUnlock()
	return string(adapter.token)
}

func (adapter *Adapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	stateNames := normalizedStateNames(states)
	if len(stateNames) == 0 {
		return emptyIssues(), nil
	}
	if err := adapter.verifyProjectScope(ctx); err != nil {
		return emptyIssues(), err
	}
	pages, err := adapter.fetchStatePages(ctx, stateNames)
	if err != nil {
		return emptyIssues(), err
	}
	wanted := normalizedStateSet(stateNames)
	terminal := normalizedStateSet(adapter.config.TerminalStates)
	issues := make([]domain.Issue, 0)
	seenIDs := make(map[string]struct{})
	seenIdentifiers := make(map[string]struct{})
	for _, page := range pages {
		for index, raw := range page.nodes {
			issue, err := normalizeIssueRecord(raw, adapter.config.ProjectSlug, terminal)
			if err != nil {
				reason := "malformed_required_record"
				if errors.Is(err, errOutOfProjectScope) {
					reason = "out_of_scope"
				}
				adapter.warnOmitted(ctx, page.number, index, reason)
				continue
			}
			if _, requested := wanted[tracker.NormalizeState(issue.State)]; !requested {
				adapter.warnOmitted(ctx, page.number, index, "unexpected_state")
				continue
			}
			if _, duplicate := seenIDs[issue.ID]; duplicate {
				return emptyIssues(), payloadError("Linear state response contained a duplicate issue ID")
			}
			if _, duplicate := seenIdentifiers[issue.Identifier]; duplicate {
				return emptyIssues(), payloadError("Linear state response contained a duplicate issue identifier")
			}
			seenIDs[issue.ID] = struct{}{}
			seenIdentifiers[issue.Identifier] = struct{}{}
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (adapter *Adapter) FetchIssuesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	if len(ids) == 0 {
		return emptyIssues(), nil
	}
	unique, err := uniqueOpaqueIDs(ids)
	if err != nil {
		return emptyIssues(), err
	}
	if err := adapter.verifyProjectScope(ctx); err != nil {
		return emptyIssues(), err
	}
	batches, err := adapter.fetchIDBatches(ctx, unique)
	if err != nil {
		return emptyIssues(), err
	}
	terminal := normalizedStateSet(adapter.config.TerminalStates)
	issues := make([]domain.Issue, 0, len(unique))
	seenIDs := make(map[string]struct{}, len(unique))
	seenIdentifiers := make(map[string]struct{}, len(unique))
	for _, batch := range batches {
		for _, raw := range batch.nodes {
			id, ok := rawIssueID(raw)
			if !ok {
				return emptyIssues(), payloadError("Linear ID response contained a malformed issue ID")
			}
			if _, requested := batch.requested[id]; !requested {
				return emptyIssues(), payloadError("Linear ID response contained an unexpected issue ID")
			}
			if _, duplicate := seenIDs[id]; duplicate {
				return emptyIssues(), payloadError("Linear ID response contained a duplicate issue ID")
			}
			seenIDs[id] = struct{}{}
			issue, err := normalizeIssueRecord(raw, adapter.config.ProjectSlug, terminal)
			if errors.Is(err, errOutOfProjectScope) {
				continue
			}
			if err != nil {
				return emptyIssues(), payloadError("Linear ID response contained a malformed visible issue")
			}
			if _, duplicate := seenIdentifiers[issue.Identifier]; duplicate {
				return emptyIssues(), payloadError("Linear ID response contained a duplicate issue identifier")
			}
			seenIdentifiers[issue.Identifier] = struct{}{}
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (adapter *Adapter) AgentTools(tracker.Session) []domain.ToolSpec {
	return []domain.ToolSpec{linearGraphQLToolSpec()}
}

func (adapter *Adapter) ExecuteAgentTool(ctx context.Context, call domain.ToolCall, _ tracker.Session) domain.ToolResult {
	if call.Name != linearGraphQLToolName {
		return domain.ToolUnavailableResult()
	}
	input, code := parseLinearToolArguments(call.Arguments)
	if code != "" {
		return linearToolFailure(code)
	}
	operation, code := parseLinearGraphQLDocument(input.Query)
	if code != "" {
		return linearToolFailure(code)
	}
	return adapter.executeLinearGraphQL(ctx, input, operation)
}

func (adapter *Adapter) SecretEnvironmentNames() []string {
	return append([]string(nil), adapter.config.SecretEnvironmentNames()...)
}

func (adapter *Adapter) warnOmitted(ctx context.Context, page, index int, reason string) {
	adapter.logger.WarnContext(ctx, "linear_issue_omitted",
		slog.String("operation", "fetch_issues_by_states"),
		slog.Int("page", page),
		slog.Int("index", index),
		slog.String("reason", reason),
	)
}

func rawIssueID(raw json.RawMessage) (string, bool) {
	var record issueRecord
	if !decodeOneJSON(raw, &record) {
		return "", false
	}
	return requiredString(record.ID)
}

func emptyIssues() []domain.Issue { return []domain.Issue{} }
