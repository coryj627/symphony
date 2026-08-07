package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func csrfFromHandler(t *testing.T, server *runningTestServer, cookie *http.Cookie) string {
	t.Helper()
	res := authenticatedRequest(t, server, cookie, http.MethodGet, "/csrf", nil, nil)
	value, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal("read CSRF response")
	}
	if res.StatusCode != http.StatusOK || len(value) == 0 {
		t.Fatalf("CSRF endpoint status = %d or value was empty", res.StatusCode)
	}
	return string(value)
}

func csrfTestServer(t *testing.T) (*runningTestServer, *http.Cookie, string) {
	t.Helper()
	server := startTestServer(t, bootstrapFromValue("mutation-capability"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/csrf" {
			csrf, _ := CSRFToken(r.Context())
			_, _ = io.WriteString(w, csrf)
			return
		}
		if r.Header.Get("X-Body-Observed") == "form" && r.FormValue("sentinel") != "present" {
			http.Error(w, "form body consumed", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("X-Body-Observed") == "json" {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"sentinel":"present"}` {
				http.Error(w, "JSON body consumed", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	cookie := exchange(t, server)
	return server, cookie, csrfFromHandler(t, server, cookie)
}

func TestMutationRejectsCrossSiteOrigin(t *testing.T) {
	server, cookie, csrf := csrfTestServer(t)
	headers := http.Header{
		"Content-Type": {"application/json"},
		"X-CSRF-Token": {csrf},
		"Origin":       {"https://attacker.example"},
	}
	res := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/validate", strings.NewReader("{}"), headers)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestMutationAcceptsExactLoopbackOrigin(t *testing.T) {
	server, cookie, csrf := csrfTestServer(t)
	parsed, err := url.Parse(server.bound.URL)
	if err != nil {
		t.Fatal("parse bound URL")
	}
	headers := http.Header{
		"Content-Type": {"application/json"},
		"X-CSRF-Token": {csrf},
		"Origin":       {parsed.Scheme + "://" + parsed.Host},
	}
	res := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/validate", strings.NewReader("{}"), headers)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestMutationRejectsNullOrigin(t *testing.T) {
	server, cookie, csrf := csrfTestServer(t)
	headers := http.Header{
		"Content-Type": {"application/json"},
		"X-CSRF-Token": {csrf},
		"Origin":       {"null"},
	}
	res := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/validate", strings.NewReader("{}"), headers)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestMutationRejectsCrossSiteFetchMetadata(t *testing.T) {
	server, cookie, csrf := csrfTestServer(t)
	headers := http.Header{
		"Content-Type":   {"application/json"},
		"X-CSRF-Token":   {csrf},
		"Sec-Fetch-Site": {"cross-site"},
	}
	res := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/validate", strings.NewReader("{}"), headers)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestMutationRejectsMissingAndWrongCSRF(t *testing.T) {
	server, cookie, _ := csrfTestServer(t)
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "wrong", token: "wrong-csrf-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"Content-Type": {"application/json"}}
			if tc.token != "" {
				headers.Set("X-CSRF-Token", tc.token)
			}
			res := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/validate", strings.NewReader("{}"), headers)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("got %d", res.StatusCode)
			}
		})
	}
}

func TestMutationAcceptsHeaderAndFormCSRFWithoutConsumingHandlerBody(t *testing.T) {
	server, cookie, csrf := csrfTestServer(t)
	jsonHeaders := http.Header{
		"Content-Type":    {"application/json; charset=utf-8"},
		"X-CSRF-Token":    {csrf},
		"X-Body-Observed": {"json"},
	}
	res := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/validate", strings.NewReader(`{"sentinel":"present"}`), jsonHeaders)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("header token: got %d", res.StatusCode)
	}

	form := url.Values{"csrf_token": {csrf}, "sentinel": {"present"}}
	formHeaders := http.Header{
		"Content-Type":    {"application/x-www-form-urlencoded"},
		"X-Body-Observed": {"form"},
	}
	res = authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/save", strings.NewReader(form.Encode()), formHeaders)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("form token: got %d", res.StatusCode)
	}
}

func TestMutationRejectsUnsupportedMethodAndContentType(t *testing.T) {
	server, cookie, csrf := csrfTestServer(t)
	res := authenticatedRequest(t, server, cookie, http.MethodDelete, "/api/v1/config/save", nil, nil)
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD, POST" {
		t.Fatalf("method response: %d Allow=%q", res.StatusCode, res.Header.Get("Allow"))
	}
	headers := http.Header{"Content-Type": {"text/plain"}, "X-CSRF-Token": {csrf}}
	res = authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/save", strings.NewReader("plain"), headers)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content type response: %d", res.StatusCode)
	}
}
