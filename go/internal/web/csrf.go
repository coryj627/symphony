package web

import (
	"mime"
	"net/http"
	"net/url"
	"strings"
)

func authorizeMethod(r *http.Request, sessions *sessionStore, session authenticatedSession) (int, string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return 0, ""
	case http.MethodPost:
		// Continue with the state-changing request checks below.
	default:
		return http.StatusMethodNotAllowed, "method not allowed"
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && mediaType != "application/x-www-form-urlencoded") {
		return http.StatusUnsupportedMediaType, "unsupported media type"
	}
	if !sameOriginSignals(r) {
		return http.StatusForbidden, "forbidden"
	}

	candidate := r.Header.Get("X-CSRF-Token")
	if candidate == "" && mediaType == "application/x-www-form-urlencoded" {
		if err := r.ParseForm(); err != nil {
			return http.StatusBadRequest, "invalid form"
		}
		candidate = r.PostForm.Get("csrf_token")
	}
	if candidate == "" || !sessions.verifyCSRF(session, candidate) {
		return http.StatusForbidden, "forbidden"
	}
	return 0, ""
}

func sameOriginSignals(r *http.Request) bool {
	fetchSite := strings.ToLower(r.Header.Get("Sec-Fetch-Site"))
	switch fetchSite {
	case "", "none", "same-origin":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// Referrer-Policy: no-referrer intentionally gives native Chromium form
	// submissions an opaque Origin. Accept that narrow browser case only when
	// independently supplied fetch metadata says the navigation is same-origin;
	// the protected Host, session, content type, method, and CSRF checks still
	// run before dispatch.
	if origin == "null" {
		return fetchSite == "same-origin"
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Host != r.Host || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return true
}
