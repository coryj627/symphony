package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

type Adapter struct {
	config        tracker.GitHubConfig
	token         []byte
	client        *http.Client
	logger        *slog.Logger
	origin        apiOrigin
	collectionURL *url.URL

	stateMu   sync.Mutex
	pageCache map[string]cachedPage
	tokenMu   sync.RWMutex
}

func New(config tracker.GitHubConfig, token []byte, client *http.Client, logger *slog.Logger) (*Adapter, error) {
	endpoint, origin, err := parseEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Owner) == "" || strings.TrimSpace(config.Repository) == "" {
		return nil, configError("GitHub owner and repository are required")
	}
	if len(token) == 0 {
		return nil, authError("GitHub credential is missing")
	}
	collectionURL, err := appendEscapedPath(endpoint, "repos", config.Owner, config.Repository, "issues")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	config.ActiveStates = append([]string(nil), config.ActiveStates...)
	config.TerminalStates = append([]string(nil), config.TerminalStates...)
	return &Adapter{
		config:        config,
		token:         append([]byte(nil), token...),
		client:        cloneHTTPClient(client, origin),
		logger:        logger,
		origin:        origin,
		collectionURL: collectionURL,
		pageCache:     make(map[string]cachedPage),
	}, nil
}

func (adapter *Adapter) Kind() string { return "github" }

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
	return "Bearer " + string(adapter.token)
}

func (adapter *Adapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	wanted := supportedStates(states)
	if len(wanted) == 0 {
		return emptyIssues(), nil
	}

	adapter.stateMu.Lock()
	defer adapter.stateMu.Unlock()
	pages, stagedCache, err := adapter.fetchStatePages(ctx)
	if err != nil {
		return emptyIssues(), err
	}
	issues := make([]domain.Issue, 0)
	for _, page := range pages {
		var records []json.RawMessage
		if !decodeOneJSON(page.body, &records) || records == nil {
			return emptyIssues(), payloadError("GitHub issue page was malformed")
		}
		for index, record := range records {
			issue, pullRequest, err := normalizeIssueRecord(record, adapter.config)
			if pullRequest {
				continue
			}
			if err != nil {
				adapter.logger.WarnContext(ctx, "github_issue_omitted",
					slog.Int("page", page.number),
					slog.Int("index", index),
					slog.String("reason", "malformed_required_record"),
				)
				continue
			}
			if _, found := wanted[tracker.NormalizeState(issue.State)]; found {
				issues = append(issues, issue)
			}
		}
	}
	adapter.pageCache = stagedCache
	return issues, nil
}

func (adapter *Adapter) FetchIssuesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	if len(ids) == 0 {
		return emptyIssues(), nil
	}
	numbers, err := adapter.validateDispatchIDs(ids)
	if err != nil {
		return emptyIssues(), err
	}
	issues := make([]domain.Issue, 0, len(numbers))
	for _, number := range numbers {
		requestURL, err := appendEscapedPath(adapter.collectionURL, number)
		if err != nil {
			return emptyIssues(), configError("GitHub issue ID was invalid")
		}
		response, err := adapter.request(ctx, requestURL, "", false, true)
		if err != nil {
			return emptyIssues(), err
		}
		if response.status == http.StatusNotFound {
			continue
		}
		var identity struct {
			Number json.RawMessage `json:"number"`
		}
		if !decodeOneJSON(response.body, &identity) {
			return emptyIssues(), payloadError("GitHub issue response was malformed")
		}
		returnedNumber, ok := requiredPositiveNumber(identity.Number)
		if !ok || returnedNumber != number {
			return emptyIssues(), payloadError("GitHub issue response did not match the requested issue")
		}
		issue, pullRequest, err := normalizeIssueRecord(response.body, adapter.config)
		if err != nil {
			if pullRequest {
				continue
			}
			return emptyIssues(), payloadError("GitHub issue response was malformed")
		}
		if pullRequest {
			continue
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

func (adapter *Adapter) AgentTools(tracker.Session) []domain.ToolSpec {
	return []domain.ToolSpec{}
}

func (adapter *Adapter) ExecuteAgentTool(context.Context, domain.ToolCall, tracker.Session) domain.ToolResult {
	return domain.ToolUnavailableResult()
}

func (adapter *Adapter) SecretEnvironmentNames() []string {
	return append([]string(nil), adapter.config.SecretEnvironmentNames()...)
}

func (adapter *Adapter) statePageURL(page int) *url.URL {
	requestURL := *adapter.collectionURL
	requestURL.RawQuery = "state=all&per_page=100&page=" + strconv.Itoa(page)
	return &requestURL
}

func supportedStates(states []string) map[string]struct{} {
	wanted := make(map[string]struct{}, len(states))
	for _, state := range states {
		normalized := tracker.NormalizeState(state)
		if normalized == "open" || normalized == "closed" {
			wanted[normalized] = struct{}{}
		}
	}
	return wanted
}

func (adapter *Adapter) validateDispatchIDs(ids []string) ([]string, error) {
	prefix := "github:" + strings.ToLower(adapter.config.Owner) + "/" + strings.ToLower(adapter.config.Repository) + "#"
	seen := make(map[string]struct{}, len(ids))
	numbers := make([]string, 0, len(ids))
	for _, id := range ids {
		if !strings.HasPrefix(id, prefix) {
			return nil, configError("GitHub issue ID was invalid or outside the configured repository")
		}
		numberText := strings.TrimPrefix(id, prefix)
		if numberText == "" || strings.HasPrefix(numberText, "+") || (len(numberText) > 1 && numberText[0] == '0') {
			return nil, configError("GitHub issue ID was invalid or outside the configured repository")
		}
		number, err := strconv.ParseUint(numberText, 10, 64)
		if err != nil || number == 0 || dispatchID(adapter.config, strconv.FormatUint(number, 10)) != id {
			return nil, configError("GitHub issue ID was invalid or outside the configured repository")
		}
		canonical := strconv.FormatUint(number, 10)
		if _, found := seen[canonical]; found {
			continue
		}
		seen[canonical] = struct{}{}
		numbers = append(numbers, canonical)
	}
	return numbers, nil
}

func emptyIssues() []domain.Issue { return []domain.Issue{} }
