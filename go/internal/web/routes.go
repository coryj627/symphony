package web

import (
	"context"
	"net"
	"net/http"
	"strconv"
)

type csrfContextKey struct{}

// CSRFToken returns the current session's CSRF value for trusted handlers that
// render forms. Session and cookie values are never placed in request context.
func CSRFToken(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(csrfContextKey{}).(string)
	return value, ok && value != ""
}

func (s *Server) protectedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validHost(r.Host) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if token, present := bootstrapCandidate(r); present {
			if r.Method != http.MethodGet || r.URL.Path != "/" || !s.bootstrap.exchange(token, s.now) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			rawSession, err := s.sessions.issue()
			if err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			setSessionCookie(w, rawSession)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		session, ok := s.sessions.authenticate(cookie.Value)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		setSecurityHeaders(w.Header())
		if status, message := authorizeMethod(r, s.sessions, session); status != 0 {
			if status == http.StatusMethodNotAllowed {
				w.Header().Set("Allow", "GET, HEAD, POST")
			}
			http.Error(w, message, status)
			return
		}
		ctx := context.WithValue(r.Context(), csrfContextKey{}, session.csrf)
		s.handler.ServeHTTP(w, r.WithContext(ctx))
	})
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
