package web

import (
	"net/http"
	"testing"
)

func TestSecurityHeadersProtectAuthenticatedDocumentsAndAPIs(t *testing.T) {
	server := startTestServer(t, bootstrapFromValue("headers-capability"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	cookie := exchange(t, server)
	for _, path := range []string{"/", "/api/v1/config/validate"} {
		res := authenticatedRequest(t, server, cookie, http.MethodGet, path, nil, nil)
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: got %d", path, res.StatusCode)
		}
		want := map[string]string{
			"Content-Security-Policy":      "default-src 'none'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'",
			"X-Content-Type-Options":       "nosniff",
			"Referrer-Policy":              "no-referrer",
			"Cache-Control":                "no-store",
			"Cross-Origin-Resource-Policy": "same-origin",
		}
		for name, value := range want {
			if got := res.Header.Get(name); got != value {
				t.Errorf("%s %s: got %q, want %q", path, name, got, value)
			}
		}
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: unexpected CORS header %q", path, got)
		}
	}
}
