package web

import (
	"context"
	"crypto/rand"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	webassets "github.com/coryj627/symphony/go/web"
)

const templateRoot = "templates"

// Renderer executes the shared application shell with route-specific content.
type Renderer struct {
	templates map[string]*template.Template
}

// PageHandler serves the application pages and their semantic error documents.
type PageHandler struct {
	renderer            *Renderer
	mux                 *http.ServeMux
	configService       *app.ConfigService
	mode                string
	queries             app.RuntimeQueries
	commands            app.RuntimeCommands
	logs                LogQueries
	logger              *slog.Logger
	events              eventStreamConfig
	resolveDependencies pageDependencyResolver
	errorSeed           [32]byte
	errorCounter        atomic.Uint64
}

type pageDependencies struct {
	queries  app.RuntimeQueries
	commands app.RuntimeCommands
	logs     LogQueries
}

type resolvedPageDependencies struct {
	pageDependencies
	scenario string
}

type pageDependenciesContextKey struct{}

type pageDependencyResolver func(*http.Request, pageDependencies) (pageDependencies, string, bool)

// LogQueries is the narrow immutable log-query surface consumed by pages.
type LogQueries interface {
	Query(context.Context, observability.LogQuery) (observability.LogPage, error)
}

// PageOptions supplies the live application state used by server-rendered and
// JSON routes. Compatibility constructors install safe empty dependencies.
type PageOptions struct {
	Configuration *app.ConfigService
	Mode          string
	Queries       app.RuntimeQueries
	Commands      app.RuntimeCommands
	Logs          LogQueries
	Logger        *slog.Logger
}

// NewRenderer parses every embedded page with the shared shell and partials.
func NewRenderer() (*Renderer, error) {
	pageNames := []string{"overview", "issues", "issue", "activity", "configuration", "logs", "unauthorized", "error"}
	templates := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		parsed, err := template.ParseFS(
			webassets.Files,
			templateRoot+"/base.html",
			templateRoot+"/partials/*.html",
			templateRoot+"/"+name+".html",
		)
		if err != nil {
			return nil, fmt.Errorf("parse %s page: %w", name, err)
		}
		templates[name] = parsed
	}
	return &Renderer{templates: templates}, nil
}

// Render writes one complete HTML document.
func (r *Renderer) Render(w http.ResponseWriter, templateName string, page Page) error {
	parsed, ok := r.templates[templateName]
	if !ok {
		return fmt.Errorf("unknown page template %q", templateName)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return parsed.ExecuteTemplate(w, "base", page)
}

// NewPageHandler returns the placeholder shell routes used until live view
// models are supplied by later phases.
func NewPageHandler() (*PageHandler, error) {
	return newPageHandlerWithEntropy(PageOptions{Mode: "configure"}, rand.Reader)
}

// NewConfiguredPageHandler composes the protected shell with live
// configuration state and mutations.
func NewConfiguredPageHandler(service *app.ConfigService, mode string) (*PageHandler, error) {
	return newPageHandlerWithEntropy(PageOptions{Configuration: service, Mode: mode}, rand.Reader)
}

// NewPageHandlerWithOptions composes live queue, command, log, and
// configuration dependencies into one protected page handler.
func NewPageHandlerWithOptions(options PageOptions) (*PageHandler, error) {
	return newPageHandlerWithEntropy(options, rand.Reader)
}

func newPageHandlerWithEntropy(options PageOptions, entropy io.Reader) (*PageHandler, error) {
	if options.Mode == "" {
		options.Mode = "configure"
	}
	if options.Queries == nil || options.Commands == nil {
		empty := emptyPageRuntime{}
		if options.Queries == nil {
			options.Queries = empty
		}
		if options.Commands == nil {
			options.Commands = empty
		}
	}
	if options.Logs == nil {
		options.Logs = emptyLogQueries{}
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	renderer, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	staticFiles, err := newStaticFileSystem()
	if err != nil {
		return nil, err
	}
	var seed [32]byte
	if _, err := io.ReadFull(entropy, seed[:]); err != nil {
		return nil, fmt.Errorf("create API correlation seed: %w", err)
	}
	mux := http.NewServeMux()
	handler := &PageHandler{
		renderer: renderer, mux: mux, configService: options.Configuration, mode: options.Mode,
		queries: options.Queries, commands: options.Commands, logs: options.Logs, logger: options.Logger, events: newEventStreamConfig(), resolveDependencies: newPageDependencyResolver(), errorSeed: seed,
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))
	mux.HandleFunc("GET /api/v1/state", handler.stateAPI)
	mux.HandleFunc("GET /api/v1/events", handler.eventsAPI)
	mux.HandleFunc("POST /api/v1/refresh", handler.refreshAPI)
	mux.HandleFunc("GET /api/v1/{issue_identifier}", handler.issueAPI)
	mux.HandleFunc("GET /{$}", handler.overviewHTML)
	mux.HandleFunc("GET /issues", handler.issuesHTML)
	mux.HandleFunc("GET /issues/{identifier}", handler.issueHTML)
	mux.HandleFunc("GET /activity", handler.activityHTML)
	if options.Configuration == nil {
		mux.HandleFunc("GET /configuration", renderPage(renderer, "configuration", func(*http.Request) Page {
			return Page{Title: "Configuration — Symphony", Route: "/configuration", Heading: "Configuration", Mode: options.Mode, Status: "Configuration has not been loaded.", Content: configurationContent{Errors: map[string]string{}}}
		}))
		for _, route := range []string{"/api/v1/config/validate", "/api/v1/config/save", "/api/v1/config/credential", "/api/v1/config/credential/delete"} {
			mux.HandleFunc("POST "+route, func(w http.ResponseWriter, _ *http.Request) {
				handler.writeAPIError(w, "runtime_unavailable")
			})
		}
	} else {
		registerConfigurationRoutes(handler, options.Configuration, options.Mode)
	}
	mux.HandleFunc("GET /logs", handler.logsHTML)
	mux.HandleFunc("/", handler.renderFallback)
	return handler, nil
}

func newStaticFileSystem() (fs.FS, error) {
	staticFiles, err := fs.Sub(webassets.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded static subtree: %w", err)
	}
	return staticFiles, nil
}

// ServeHTTP dispatches a page/static request through the semantic page mux.
func (h *PageHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(w.Header())
	if allowed, defined := h.AllowedMethods(request); defined && !methodAllowed(request.Method, allowed) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		if isAPIRequest(request) {
			h.writeAPIError(w, "method_not_allowed")
		} else {
			h.RespondError(w, http.StatusMethodNotAllowed)
		}
		return
	}
	if isAPIRequest(request) {
		if _, defined := h.AllowedMethods(request); !defined {
			h.writeAPIError(w, "not_found")
			return
		}
	}
	if strings.HasPrefix(request.URL.Path, "/static/") {
		h.mux.ServeHTTP(w, request)
		return
	}
	base := pageDependencies{queries: h.queries, commands: h.commands, logs: h.logs}
	dependencies, scenario, ok := h.resolveDependencies(request, base)
	if !ok {
		h.RespondRequestError(w, request, http.StatusNotFound)
		return
	}
	resolved := resolvedPageDependencies{pageDependencies: dependencies, scenario: scenario}
	ctx := context.WithValue(request.Context(), pageDependenciesContextKey{}, resolved)
	h.mux.ServeHTTP(w, request.WithContext(ctx))
}

func (h *PageHandler) dependencies(request *http.Request) resolvedPageDependencies {
	if resolved, ok := request.Context().Value(pageDependenciesContextKey{}).(resolvedPageDependencies); ok {
		return resolved
	}
	return resolvedPageDependencies{pageDependencies: pageDependencies{queries: h.queries, commands: h.commands, logs: h.logs}}
}

func requestScenario(request *http.Request) string {
	if resolved, ok := request.Context().Value(pageDependenciesContextKey{}).(resolvedPageDependencies); ok {
		return resolved.scenario
	}
	return ""
}

// AllowedMethods returns the exact ordered method set for one canonical route
// without touching application dependencies.
func (h *PageHandler) AllowedMethods(request *http.Request) ([]string, bool) {
	escaped := request.URL.EscapedPath()
	if !canonicalRoutePath(escaped) {
		return nil, false
	}
	switch escaped {
	case "/", "/issues", "/activity", "/configuration", "/logs", "/api/v1/state", "/api/v1/events":
		return []string{http.MethodGet, http.MethodHead}, true
	case "/api/v1/refresh", "/api/v1/config/validate", "/api/v1/config/save", "/api/v1/config/credential", "/api/v1/config/credential/delete":
		return []string{http.MethodPost}, true
	}
	if strings.HasPrefix(escaped, "/static/") {
		return []string{http.MethodGet, http.MethodHead}, true
	}
	if canonicalEscapedSegment(request, "/issues/") || canonicalEscapedSegment(request, "/api/v1/") {
		return []string{http.MethodGet, http.MethodHead}, true
	}
	return nil, false
}

func methodAllowed(method string, allowed []string) bool {
	for _, candidate := range allowed {
		if method == candidate {
			return true
		}
	}
	return false
}

func isAPIRequest(request *http.Request) bool {
	return request.URL.Path == "/api/v1" || strings.HasPrefix(request.URL.Path, "/api/v1/")
}

func canonicalRoutePath(escaped string) bool {
	if escaped == "" || escaped[0] != '/' || (escaped != "/" && strings.HasSuffix(escaped, "/")) || strings.Contains(escaped, "//") || strings.Contains(escaped, "\\") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(escaped, "/"), "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "." || decoded == ".." || strings.ContainsRune(decoded, '\\') {
			return false
		}
	}
	return true
}

func canonicalEscapedSegment(request *http.Request, prefix string) bool {
	escaped := request.URL.EscapedPath()
	if !strings.HasPrefix(escaped, prefix) || !strings.HasPrefix(request.URL.Path, prefix) {
		return false
	}
	escapedSuffix := strings.TrimPrefix(escaped, prefix)
	decodedSuffix := strings.TrimPrefix(request.URL.Path, prefix)
	return decodedSuffix != "" && !strings.ContainsRune(escapedSuffix, '/') && url.PathEscape(decodedSuffix) == escapedSuffix
}

func (h *PageHandler) renderFallback(w http.ResponseWriter, request *http.Request) {
	status := http.StatusNotFound
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", "GET, HEAD")
	}
	h.respondHTMLRequestError(w, request, status)
}

// RespondError renders a safe, state-free error document for the protected
// server boundary and authenticated page mux.
func (h *PageHandler) RespondError(w http.ResponseWriter, status int) {
	h.respondError(w, status, "")
}

func (h *PageHandler) respondError(w http.ResponseWriter, status int, scenario string) {
	templateName := "error"
	page := Page{Mode: "error", Scenario: scenario}
	switch status {
	case http.StatusUnauthorized:
		templateName = "unauthorized"
		page.Title = "Authorization required — Symphony"
		page.Heading = "Authorization required"
		page.Status = "This browser session is missing or no longer valid."
		page.Content = errorContent{
			Instruction: "Return to the terminal and open the newest Symphony launch URL.",
		}
	case http.StatusNotFound:
		page.Title = "Page not found — Symphony"
		page.Heading = "Page not found"
		page.Status = "The requested page is not available."
		page.Content = errorContent{
			Instruction: "Use the primary navigation to choose an available page.",
		}
	case http.StatusMethodNotAllowed:
		page.Title = "Method not allowed — Symphony"
		page.Heading = "Method not allowed"
		page.Status = "That request method is not available for this page."
		page.Content = errorContent{
			Instruction: "Return to the previous page and use the available controls.",
		}
	default:
		page.Title = "Request could not be completed — Symphony"
		page.Heading = "Request could not be completed"
		page.Status = "Symphony could not complete that request."
		page.Content = errorContent{
			Instruction: "Return to the previous page and try an available action.",
		}
	}
	setSecurityHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.renderer.Render(w, templateName, page)
}

// RespondRequestError keeps HTML error documents compatible while providing
// one stable JSON envelope for every protected API-boundary failure.
func (h *PageHandler) RespondRequestError(w http.ResponseWriter, request *http.Request, status int) {
	if isAPIRequest(request) {
		h.writeAPIError(w, apiErrorKeyForStatus(status))
		return
	}
	h.respondHTMLRequestError(w, request, status)
}

func (h *PageHandler) respondHTMLRequestError(w http.ResponseWriter, request *http.Request, status int) {
	h.respondError(w, status, requestScenario(request))
}

func (h *PageHandler) renderHTML(w http.ResponseWriter, templateName string, page Page) error {
	setSecurityHeaders(w.Header())
	return h.renderer.Render(w, templateName, page)
}

func renderPage(renderer *Renderer, templateName string, build func(*http.Request) Page) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		csrfToken, ok := CSRFToken(request.Context())
		if !ok {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		page := build(request)
		page.CSRFToken = csrfToken
		page.Scenario = requestScenario(request)
		if err := renderer.Render(w, templateName, page); err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

func configureLivePage(page *Page, route string, cursor domain.EventCursor, filters url.Values) {
	formattedCursor, ok := formatValidEventCursor(cursor)
	if !ok {
		return
	}
	stateValues := make(url.Values)
	for key, values := range filters {
		if len(values) == 1 {
			stateValues.Set(key, values[0])
		}
	}
	if page.Scenario != "" {
		stateValues.Set("__e2e_scenario", page.Scenario)
	}
	stateURL := "/api/v1/state"
	if encoded := stateValues.Encode(); encoded != "" {
		stateURL += "?" + encoded
	}
	eventValues := url.Values{"after": {formattedCursor}}
	if page.Scenario != "" {
		eventValues.Set("__e2e_scenario", page.Scenario)
	}
	eventsURL := "/api/v1/events?" + eventValues.Encode()
	if len(stateURL) > 1024 || len(eventsURL) > 1024 {
		return
	}
	page.LiveRoute = route
	page.EventCursorID = formattedCursor
	page.StateURL = stateURL
	page.EventsURL = eventsURL
}

type emptyPageRuntime struct{}

const emptyPageRuntimeEpoch = "00000000000000000000000000000000"

var emptyPageRuntimeEvents = make(chan struct{})

func (emptyPageRuntime) Snapshot(context.Context) (domain.Snapshot, error) {
	snapshot := domain.EmptySnapshot()
	snapshot.GeneratedAt = time.Now().UTC()
	snapshot.EventCursor = domain.EventCursor{Epoch: emptyPageRuntimeEpoch}
	return snapshot, nil
}
func (emptyPageRuntime) Issue(context.Context, string) (domain.IssueDetail, error) {
	return domain.IssueDetail{}, app.ErrIssueNotFound
}
func (emptyPageRuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	current := domain.EventCursor{Epoch: emptyPageRuntimeEpoch}
	return domain.EventPage{Events: []domain.Event{}, LatestCursor: current, Reset: cursor != current}, nil
}
func (emptyPageRuntime) RecentEvents(context.Context, int) (domain.EventPage, error) {
	return domain.EventPage{Events: []domain.Event{}, LatestCursor: domain.EventCursor{Epoch: emptyPageRuntimeEpoch}}, nil
}
func (emptyPageRuntime) SubscribeEvents(domain.EventCursor) <-chan struct{} {
	return emptyPageRuntimeEvents
}
func (emptyPageRuntime) Refresh(context.Context) (domain.RefreshReceipt, error) {
	return domain.RefreshReceipt{Operations: []string{}}, app.ErrUnavailableInPhase
}
func (emptyPageRuntime) SetScheduler(context.Context, bool) error { return app.ErrUnavailableInPhase }
func (emptyPageRuntime) Respond(context.Context, domain.OperatorResponse) error {
	return app.ErrUnavailableInPhase
}

type emptyLogQueries struct{}

func (emptyLogQueries) Query(context.Context, observability.LogQuery) (observability.LogPage, error) {
	return observability.LogPage{Records: []observability.LogRecord{}}, nil
}
