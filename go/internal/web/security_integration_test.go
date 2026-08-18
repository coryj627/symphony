package web

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUntrustedLoopbackRequestCannotBootstrapOrMutate(t *testing.T) {
	t.Run("bootstrap capability", func(t *testing.T) {
		tests := []struct {
			name       string
			origin     string
			fetchSite  string
			host       string
			extraQuery bool
			want       int
		}{
			{name: "foreign origin", origin: "https://evil.example", want: http.StatusUnauthorized},
			{name: "cross-site fetch metadata", fetchSite: "cross-site", want: http.StatusUnauthorized},
			{name: "unexpected query data", extraQuery: true, want: http.StatusUnauthorized},
			{name: "foreign host", host: "evil.example", want: http.StatusBadRequest},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				const capability = "loopback-authority-bootstrap"
				var applicationCalls atomic.Int32
				server := startTestServer(t, bootstrapFromValue(capability), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					applicationCalls.Add(1)
				}))

				bootstrapURL, err := url.Parse(server.bound.URL)
				if err != nil {
					t.Fatal("parse bootstrap URL")
				}
				if test.extraQuery {
					query := bootstrapURL.Query()
					query.Set("return_to", "/activity")
					bootstrapURL.RawQuery = query.Encode()
				}
				request, err := http.NewRequest(http.MethodGet, bootstrapURL.String(), nil)
				if err != nil {
					t.Fatal("create bootstrap request")
				}
				if test.origin != "" {
					request.Header.Set("Origin", test.origin)
				}
				if test.fetchSite != "" {
					request.Header.Set("Sec-Fetch-Site", test.fetchSite)
				}
				if test.host != "" {
					request.Host = test.host + ":" + strconv.Itoa(server.bound.Port)
				}
				response, err := server.client.Do(request)
				if err != nil {
					t.Fatal("perform bootstrap request")
				}
				response.Body.Close()
				if response.StatusCode != test.want {
					t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
				}
				if len(response.Cookies()) != 0 {
					t.Fatal("rejected bootstrap request issued a session cookie")
				}
				if got := applicationCalls.Load(); got != 0 {
					t.Fatalf("application calls = %d, want 0", got)
				}

				// A rejected request must not consume the one-time capability.
				_ = exchange(t, server)
			})
		}

		t.Run("native browser launch", func(t *testing.T) {
			server := startTestServer(t, bootstrapFromValue("native-browser-bootstrap"), http.NotFoundHandler())
			request, err := http.NewRequest(http.MethodGet, server.bound.URL, nil)
			if err != nil {
				t.Fatal("create native browser request")
			}
			request.Header.Set("Sec-Fetch-Dest", "document")
			request.Header.Set("Sec-Fetch-Mode", "navigate")
			request.Header.Set("Sec-Fetch-Site", "none")
			request.Header.Set("Sec-Fetch-User", "?1")
			response, err := server.client.Do(request)
			if err != nil {
				t.Fatal("perform native browser request")
			}
			response.Body.Close()
			if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" || len(response.Cookies()) != 1 {
				t.Fatalf("native browser launch status = %d, location = %q, cookies = %d", response.StatusCode, response.Header.Get("Location"), len(response.Cookies()))
			}
		})
	})

	t.Run("authenticated mutation", func(t *testing.T) {
		var mutationCalls atomic.Int32
		server := startTestServer(t, bootstrapFromValue("loopback-authority-mutation"), http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/csrf" {
				csrf, _ := CSRFToken(request.Context())
				_, _ = io.WriteString(w, csrf)
				return
			}
			mutationCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		cookie := exchange(t, server)
		csrf := csrfFromHandler(t, server, cookie)
		boundURL, err := url.Parse(server.bound.URL)
		if err != nil {
			t.Fatal("parse bound URL")
		}
		origin := boundURL.Scheme + "://" + boundURL.Host

		tests := []struct {
			name      string
			csrf      string
			origin    string
			fetchSite string
			host      string
			want      int
		}{
			{name: "missing CSRF", origin: origin, want: http.StatusForbidden},
			{name: "wrong CSRF", csrf: "wrong-csrf", origin: origin, want: http.StatusForbidden},
			{name: "foreign origin", csrf: csrf, origin: "https://evil.example", want: http.StatusForbidden},
			{name: "cross-site fetch metadata", csrf: csrf, origin: origin, fetchSite: "cross-site", want: http.StatusForbidden},
			{name: "foreign host", csrf: csrf, origin: origin, host: "evil.example", want: http.StatusBadRequest},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				mutationURL := *boundURL
				mutationURL.Path = "/api/v1/mutate"
				mutationURL.RawQuery = ""
				request, err := http.NewRequest(http.MethodPost, mutationURL.String(), strings.NewReader("{}"))
				if err != nil {
					t.Fatal("create mutation request")
				}
				request.AddCookie(cookie)
				request.Header.Set("Content-Type", "application/json")
				if test.csrf != "" {
					request.Header.Set("X-CSRF-Token", test.csrf)
				}
				if test.origin != "" {
					request.Header.Set("Origin", test.origin)
				}
				if test.fetchSite != "" {
					request.Header.Set("Sec-Fetch-Site", test.fetchSite)
				}
				if test.host != "" {
					request.Host = test.host + ":" + strconv.Itoa(server.bound.Port)
				}
				response, err := server.client.Do(request)
				if err != nil {
					t.Fatal("perform mutation request")
				}
				response.Body.Close()
				if response.StatusCode != test.want {
					t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
				}
				if got := mutationCalls.Load(); got != 0 {
					t.Fatalf("mutation calls = %d, want 0", got)
				}
			})
		}

		headers := http.Header{
			"Content-Type":   {"application/json"},
			"Origin":         {origin},
			"Sec-Fetch-Site": {"same-origin"},
			"X-CSRF-Token":   {csrf},
		}
		response := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/mutate", strings.NewReader("{}"), headers)
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("trusted mutation status = %d, want %d", response.StatusCode, http.StatusNoContent)
		}
		if got := mutationCalls.Load(); got != 1 {
			t.Fatalf("mutation calls = %d, want 1", got)
		}
	})
}
