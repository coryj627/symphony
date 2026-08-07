package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	webassets "github.com/coryj627/symphony/go/web"
)

const templateRoot = "templates"

// Renderer executes the shared application shell with route-specific content.
type Renderer struct {
	templates map[string]*template.Template
}

// NewRenderer parses every embedded page with the shared shell and partials.
func NewRenderer() (*Renderer, error) {
	pageNames := []string{"overview", "issues", "issue", "activity", "configuration", "logs", "unauthorized"}
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
func NewPageHandler() (http.Handler, error) {
	renderer, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(webassets.Files)))
	mux.HandleFunc("GET /{$}", renderPage(renderer, "overview", func(*http.Request) Page {
		return Page{
			Title: "Overview — Symphony", Route: "/", Heading: "Overview", Mode: "configure",
			Content: overviewContent{Repository: "Repository not selected"},
		}
	}))
	mux.HandleFunc("GET /issues", renderPage(renderer, "issues", func(*http.Request) Page {
		return Page{Title: "Issues — Symphony", Route: "/issues", Heading: "Issues", Mode: "configure", Content: issuesContent{}}
	}))
	mux.HandleFunc("GET /issues/{identifier}", renderPage(renderer, "issue", func(request *http.Request) Page {
		identifier := strings.TrimSpace(request.PathValue("identifier"))
		return Page{
			Title: identifier + " — Symphony", Route: "/issues", Heading: "Issue " + identifier, Mode: "configure",
			Content: issueContent{Identifier: identifier},
		}
	}))
	mux.HandleFunc("GET /activity", renderPage(renderer, "activity", func(*http.Request) Page {
		return Page{Title: "Activity — Symphony", Route: "/activity", Heading: "Activity", Mode: "configure", Content: activityContent{}}
	}))
	mux.HandleFunc("GET /configuration", renderPage(renderer, "configuration", func(*http.Request) Page {
		return Page{Title: "Configuration — Symphony", Route: "/configuration", Heading: "Configuration", Mode: "configure", Content: configurationContent{}}
	}))
	mux.HandleFunc("GET /logs", renderPage(renderer, "logs", func(*http.Request) Page {
		return Page{Title: "Logs — Symphony", Route: "/logs", Heading: "Logs", Mode: "configure", Content: logsContent{}}
	}))
	return mux, nil
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
