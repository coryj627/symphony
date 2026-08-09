package web

import (
	"context"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
)

type csrfContextKey struct{}

// MethodPolicy reports the exact ordered methods for a static route without
// invoking application dependencies.
type MethodPolicy interface {
	AllowedMethods(*http.Request) ([]string, bool)
}

// RequestErrorResponder optionally renders request-class-aware safe errors.
// ErrorResponder remains compatible for existing handlers.
type RequestErrorResponder interface {
	RespondRequestError(http.ResponseWriter, *http.Request, int)
}

// CSRFToken returns the current session's CSRF value for trusted handlers that
// render forms. Session and cookie values are never placed in request context.
func CSRFToken(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(csrfContextKey{}).(string)
	return value, ok && value != ""
}

func (s *Server) protectedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validHost(r.Host) {
			s.respondError(w, r, http.StatusBadRequest, "bad request")
			return
		}

		if token, present := bootstrapCandidate(r); present {
			if r.Method != http.MethodGet || r.URL.Path != "/" || !s.bootstrap.exchange(token, s.now) {
				s.respondError(w, r, http.StatusUnauthorized, "unauthorized")
				return
			}
			rawSession, err := s.sessions.issue()
			if err != nil {
				s.respondError(w, r, http.StatusInternalServerError, "internal server error")
				return
			}
			setSessionCookie(w, rawSession)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		// Error documents use the same local stylesheet as authenticated pages.
		// Static GET/HEAD requests carry no application state and remain bound by
		// the loopback Host check and the standard response security headers.
		if isCanonicalPublicStaticRequest(r) {
			setSecurityHeaders(w.Header())
			s.handler.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			s.respondError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		session, ok := s.sessions.authenticate(cookie.Value)
		if !ok {
			s.respondError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}

		setSecurityHeaders(w.Header())
		if policy, ok := s.handler.(MethodPolicy); ok {
			allowed, defined := policy.AllowedMethods(r)
			if defined && !methodAllowed(r.Method, allowed) {
				w.Header().Set("Allow", strings.Join(allowed, ", "))
				s.respondError(w, r, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if !defined {
				ctx := context.WithValue(r.Context(), csrfContextKey{}, session.csrf)
				s.handler.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if status, message := authorizeMethod(r, s.sessions, session); status != 0 {
			if status == http.StatusMethodNotAllowed {
				w.Header().Set("Allow", "GET, HEAD, POST")
			}
			s.respondError(w, r, status, message)
			return
		}
		ctx := context.WithValue(r.Context(), csrfContextKey{}, session.csrf)
		s.handler.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isCanonicalPublicStaticRequest(request *http.Request) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return false
	}
	if request.URL.RawPath != "" || request.URL.EscapedPath() != request.URL.Path {
		return false
	}
	const prefix = "/static/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return false
	}
	asset := strings.TrimPrefix(request.URL.Path, prefix)
	if asset == "" || strings.HasSuffix(asset, "/") || strings.Contains(asset, "\\") {
		return false
	}
	return path.Clean(request.URL.Path) == request.URL.Path
}

func (s *Server) respondError(w http.ResponseWriter, request *http.Request, status int, fallback string) {
	setSecurityHeaders(w.Header())
	if responder, ok := s.errorResponder.(RequestErrorResponder); ok {
		responder.RespondRequestError(w, request, status)
		return
	}
	if s.errorResponder != nil {
		s.errorResponder.RespondError(w, status)
		return
	}
	http.Error(w, fallback, status)
}

func bootstrapCandidate(r *http.Request) (string, bool) {
	values, present := r.URL.Query()["access_token"]
	if !present || len(values) != 1 || values[0] == "" {
		return "", present
	}
	return values[0], true
}

func (s *Server) validHost(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port != strconv.Itoa(int(s.boundPort.Load())) {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}
