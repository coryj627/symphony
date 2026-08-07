//go:build e2e

package web

import "net/http"

// EnableE2ERoutes registers fixed, state-free documents that exercise shell
// states which are otherwise reached only after later-phase mutations.
func (h *PageHandler) EnableE2ERoutes() {
	h.mux.HandleFunc("GET /__e2e/flash", renderPage(h.renderer, "overview", func(*http.Request) Page {
		return Page{
			Title:   "Overview — Symphony",
			Route:   "/",
			Heading: "Overview",
			Mode:    "configure",
			Status:  "Scheduler configuration is ready.",
			Flash:   "Configuration saved.",
			Content: overviewContent{Repository: "Repository not selected"},
		}
	}))
}
