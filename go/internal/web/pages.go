package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	webassets "github.com/coryj627/symphony/go/web"
)

const templateRoot = "templates"

// Renderer executes the shared application shell with route-specific content.
type Renderer struct {
	templates map[string]*template.Template
}

// PageHandler serves the application pages and their semantic error documents.
type PageHandler struct {
	renderer *Renderer
	mux      *http.ServeMux
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
	renderer, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	staticFiles, err := newStaticFileSystem()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	handler := &PageHandler{renderer: renderer, mux: mux}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))
	mux.HandleFunc("GET /{$}", renderPage(renderer, "overview", func(*http.Request) Page {
		return Page{
			Title: "Overview — Symphony", Route: "/", Heading: "Overview", Mode: "configure",
			Status:  "Scheduler configuration is ready.",
			Content: overviewContent{Repository: "Repository not selected"},
		}
	}))
	mux.HandleFunc("GET /issues", renderPage(renderer, "issues", func(*http.Request) Page {
		return Page{Title: "Issues — Symphony", Route: "/issues", Heading: "Issues", Mode: "configure", Status: "No issues are available.", Content: issuesContent{}}
	}))
	mux.HandleFunc("GET /issues/{identifier}", renderPage(renderer, "issue", func(request *http.Request) Page {
		identifier := strings.TrimSpace(request.PathValue("identifier"))
		return Page{
			Title: identifier + " — Symphony", Route: "/issues", Heading: "Issue " + identifier, Mode: "configure",
			Status:  "Issue details are not available yet.",
			Content: issueContent{Identifier: identifier},
		}
	}))
	mux.HandleFunc("GET /activity", renderPage(renderer, "activity", func(*http.Request) Page {
		return Page{Title: "Activity — Symphony", Route: "/activity", Heading: "Activity", Mode: "configure", Status: "No activity has been recorded.", Content: activityContent{}}
	}))
	mux.HandleFunc("GET /configuration", renderPage(renderer, "configuration", func(*http.Request) Page {
		return Page{Title: "Configuration — Symphony", Route: "/configuration", Heading: "Configuration", Mode: "configure", Status: "Configuration has not been loaded.", Content: configurationContent{}}
	}))
	mux.HandleFunc("GET /logs", renderPage(renderer, "logs", func(*http.Request) Page {
		return Page{Title: "Logs — Symphony", Route: "/logs", Heading: "Logs", Mode: "configure", Status: "No log entries are available.", Content: logsContent{}}
	}))
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
	h.mux.ServeHTTP(w, request)
}

func (h *PageHandler) renderFallback(w http.ResponseWriter, request *http.Request) {
	status := http.StatusNotFound
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", "GET, HEAD")
	}
	h.RespondError(w, status)
}

// RespondError renders a safe, state-free error document for the protected
// server boundary and authenticated page mux.
func (h *PageHandler) RespondError(w http.ResponseWriter, status int) {
	templateName := "error"
	page := Page{Mode: "error"}
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

func renderPage(renderer *Renderer, templateName string, build func(*http.Request) Page) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		csrfToken, ok := CSRFToken(request.Context())
		if !ok {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		page := build(request)
		page.CSRFToken = csrfToken
		if err := renderer.Render(w, templateName, page); err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}
