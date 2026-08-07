package web

import (
	"net/http"
	"testing"
)

func TestSessionRejectsUnknownCookie(t *testing.T) {
	server := startTestServer(t, bootstrapFromValue("session-capability"), http.NotFoundHandler())
	exchange(t, server)
	unknown := &http.Cookie{Name: sessionCookieName, Value: "unknown-session", Path: "/"}
	res := authenticatedRequest(t, server, unknown, http.MethodGet, "/", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestAuthenticatedHandlerReceivesSessionCSRFValue(t *testing.T) {
	var csrf string
	server := startTestServer(t, bootstrapFromValue("csrf-context-capability"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		csrf, ok = CSRFToken(r.Context())
		if !ok || csrf == "" {
			http.Error(w, "missing CSRF context", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	cookie := exchange(t, server)
	res := authenticatedRequest(t, server, cookie, http.MethodGet, "/", nil, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d", res.StatusCode)
	}
	if csrf == "" || csrf == cookie.Value {
		t.Fatal("handler received an unsafe CSRF value")
	}
}
