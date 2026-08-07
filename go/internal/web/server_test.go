package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type runningTestServer struct {
	server *Server
	bound  Bound
	client *http.Client
	logs   *bytes.Buffer
}

func startTestServer(t *testing.T, bootstrap Bootstrap, handler http.Handler) *runningTestServer {
	t.Helper()
	logs := new(bytes.Buffer)
	server, err := NewServer(Options{
		Port:      0,
		Bootstrap: bootstrap,
		Handler:   handler,
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := server.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &runningTestServer{
		server: server,
		bound:  bound,
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		logs: logs,
	}
}

func request(t *testing.T, client *http.Client, method, rawURL string, body io.Reader, headers http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func exchange(t *testing.T, server *runningTestServer) *http.Cookie {
	t.Helper()
	res := request(t, server.client, http.MethodGet, server.bound.URL, nil, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("bootstrap response: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatal("session cookie not set")
	return nil
}

func authenticatedRequest(t *testing.T, server *runningTestServer, cookie *http.Cookie, method, path string, body io.Reader, headers http.Header) *http.Response {
	t.Helper()
	parsed, err := url.Parse(server.bound.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = path
	parsed.RawQuery = ""
	req, err := http.NewRequest(method, parsed.String(), body)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	res, err := server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestBootstrapExchangesCapabilityAndRedirectsCleanURL(t *testing.T) {
	server := startTestServer(t, bootstrapFromValue("known-capability"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("application handler called during bootstrap")
	}))
	res := request(t, server.client, http.MethodGet, server.bound.URL, nil, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("unexpected response: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, candidate := range res.Cookies() {
		if candidate.Name == sessionCookieName {
			cookie = candidate
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Domain != "" || cookie.Path != "/" || cookie.Secure {
		t.Fatalf("unsafe cookie: %#v", cookie)
	}
	if strings.Contains(res.Header.Get("Location"), "access_token") {
		t.Fatalf("redirect retained capability: %q", res.Header.Get("Location"))
	}
}

func TestBootstrapRejectsInvalidExpiredAndReusedCapabilities(t *testing.T) {
	tests := []struct {
		name             string
		bootstrap        Bootstrap
		requested        string
		firstUse         bool
		expireAfterStart bool
	}{
		{name: "invalid", bootstrap: bootstrapFromValue("valid-capability"), requested: "invalid-capability"},
		{name: "expired", bootstrap: bootstrapFromValue("expired-capability"), requested: "expired-capability", expireAfterStart: true},
		{name: "reused", bootstrap: bootstrapFromValue("single-use-capability"), requested: "single-use-capability", firstUse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := startTestServer(t, tt.bootstrap, http.NotFoundHandler())
			if tt.expireAfterStart {
				tt.bootstrap.state.mu.Lock()
				tt.bootstrap.state.expires = time.Now().Add(-time.Second)
				tt.bootstrap.state.mu.Unlock()
			}
			if tt.firstUse {
				first := request(t, server.client, http.MethodGet, server.bound.URL, nil, nil)
				if first.StatusCode != http.StatusSeeOther {
					t.Fatalf("first use: got %d", first.StatusCode)
				}
			}
			parsed, err := url.Parse(server.bound.URL)
			if err != nil {
				t.Fatal(err)
			}
			query := parsed.Query()
			query.Set("access_token", tt.requested)
			parsed.RawQuery = query.Encode()
			res := request(t, server.client, http.MethodGet, parsed.String(), nil, nil)
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("got %d", res.StatusCode)
			}
		})
	}
}

func TestBootstrapConcurrentExchangeIsSingleUse(t *testing.T) {
	server := startTestServer(t, bootstrapFromValue("concurrent-capability"), http.NotFoundHandler())
	const attempts = 16
	statuses := make(chan int, attempts)
	var requests sync.WaitGroup
	for range attempts {
		requests.Add(1)
		go func() {
			defer requests.Done()
			req, err := http.NewRequest(http.MethodGet, server.bound.URL, nil)
			if err != nil {
				statuses <- 0
				return
			}
			res, err := server.client.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			res.Body.Close()
			statuses <- res.StatusCode
		}()
	}
	requests.Wait()
	close(statuses)
	accepted := 0
	for status := range statuses {
		switch status {
		case http.StatusSeeOther:
			accepted++
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d concurrent exchanges, want 1", accepted)
	}
}

func TestNewBootstrapUsesOnlyRandomOrExplicitE2EMode(t *testing.T) {
	const shortValue = "too-short"
	const explicitValue = "0123456789abcdef0123456789abcdef"
	t.Setenv("SYMPHONY_E2E_BOOTSTRAP_TOKEN", shortValue)
	first, err := NewBootstrap()
	if err != nil {
		if err.Error() != "e2e bootstrap token must be at least 32 characters" {
			t.Fatalf("unexpected bootstrap error: %v", err)
		}
		t.Setenv("SYMPHONY_E2E_BOOTSTRAP_TOKEN", explicitValue)
		second, err := NewBootstrap()
		if err != nil {
			t.Fatal(err)
		}
		got, err := second.value()
		if err != nil {
			t.Fatal(err)
		}
		if got != explicitValue {
			t.Fatalf("e2e bootstrap = %q, want explicit value", got)
		}
		return
	}

	firstValue, err := first.value()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYMPHONY_E2E_BOOTSTRAP_TOKEN", explicitValue)
	second, err := NewBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	secondValue, err := second.value()
	if err != nil {
		t.Fatal(err)
	}
	if firstValue == shortValue || secondValue == explicitValue || firstValue == secondValue {
		t.Fatalf("production bootstrap used deterministic input: first=%q second=%q", firstValue, secondValue)
	}
}

func TestMissingSessionDoesNotInvokeApplicationHandler(t *testing.T) {
	var called atomic.Bool
	server := startTestServer(t, bootstrapFromValue("missing-session-capability"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called.Store(true)
	}))
	parsed, _ := url.Parse(server.bound.URL)
	parsed.RawQuery = ""
	res := request(t, server.client, http.MethodGet, parsed.String(), nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", res.StatusCode)
	}
	if called.Load() {
		t.Fatal("application handler was invoked")
	}
}

func TestLoopbackListenersShareSelectedPort(t *testing.T) {
	server := startTestServer(t, bootstrapFromValue("dual-stack-capability"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	cookie := exchange(t, server)
	res := authenticatedRequest(t, server, cookie, http.MethodGet, "/", nil, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("IPv4 response: %d", res.StatusCode)
	}

	ipv6URL := "http://[::1]:" + strconv.Itoa(server.bound.Port) + "/"
	req, err := http.NewRequest(http.MethodGet, ipv6URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	res, err = server.client.Do(req)
	if err != nil {
		var netErr *net.OpError
		if errors.As(err, &netErr) {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("IPv6 response: %d", res.StatusCode)
	}
}

func TestLoopbackRejectsWildcardAndWrongPortHosts(t *testing.T) {
	server := startTestServer(t, bootstrapFromValue("host-capability"), http.NotFoundHandler())
	cookie := exchange(t, server)
	parsed, _ := url.Parse(server.bound.URL)
	for _, host := range []string{"0.0.0.0:" + strconv.Itoa(server.bound.Port), "[::]:" + strconv.Itoa(server.bound.Port), "127.0.0.1:1", "attacker.example:" + strconv.Itoa(server.bound.Port)} {
		req, err := http.NewRequest(http.MethodGet, parsed.Scheme+"://"+parsed.Host+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.AddCookie(cookie)
		res, err := server.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("host %q: got %d", host, res.StatusCode)
		}
	}
}

func TestCancellationClosesEveryListenerAndShutdownIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server, err := NewServer(Options{Port: 0, Bootstrap: bootstrapFromValue("shutdown-capability"), Handler: http.NotFoundHandler()})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := server.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.Port)), 20*time.Millisecond)
		if dialErr != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener remained open after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for range 2 {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		err = server.Shutdown(shutdownCtx)
		shutdownCancel()
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}
}

func TestIPv6BindFailureRollsBackIPv4Listener(t *testing.T) {
	ipv6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ipv6.Close()
	port := ipv6.Addr().(*net.TCPAddr).Port
	server, err := NewServer(Options{Port: port, Bootstrap: bootstrapFromValue("rollback-capability")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded despite occupied IPv6 loopback port")
	}
	ipv4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("IPv4 listener leaked after IPv6 bind failure: %v", err)
	}
	ipv4.Close()
}

func TestSensitiveValuesAreAbsentFromCapturedLogs(t *testing.T) {
	const capability = "canary-bootstrap-capability"
	server := startTestServer(t, bootstrapFromValue(capability), http.NotFoundHandler())
	parsed, _ := url.Parse(server.bound.URL)
	query := parsed.Query()
	query.Set("access_token", "wrong-canary-capability")
	parsed.RawQuery = query.Encode()
	request(t, server.client, http.MethodGet, parsed.String(), nil, nil)
	cookie := exchange(t, server)
	parsed.RawQuery = ""
	req, _ := http.NewRequest(http.MethodGet, parsed.String(), nil)
	req.AddCookie(cookie)
	res, err := server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	logged := server.logs.String()
	for _, sensitive := range []string{capability, "wrong-canary-capability", cookie.Value, "access_token", "symphony_session"} {
		if strings.Contains(logged, sensitive) {
			t.Fatalf("logs retained %q: %s", sensitive, logged)
		}
	}
}
