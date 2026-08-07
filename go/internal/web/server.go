package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures the protected loopback HTTP server.
type Options struct {
	Port      int
	Bootstrap Bootstrap
	Handler   http.Handler
	Logger    *slog.Logger
}

// Bound describes the selected IPv4 loopback URL and shared listener port.
type Bound struct {
	URL  string
	Port int
}

// Server owns every loopback listener and serving goroutine.
type Server struct {
	port        int
	bootstrap   Bootstrap
	handler     http.Handler
	logger      *slog.Logger
	sessions    *sessionStore
	now         func() time.Time
	launchValue string

	boundPort atomic.Int64

	mu              sync.Mutex
	started         bool
	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
	serveErr        error
	listeners       []net.Listener
	httpServers     []*http.Server
	serveWG         sync.WaitGroup
	stopCh          chan struct{}
	stopOnce        sync.Once
}

// NewServer constructs a protected server without opening listeners.
func NewServer(options Options) (*Server, error) {
	if options.Port < 0 || options.Port > 65535 {
		return nil, fmt.Errorf("port must be between 0 and 65535")
	}
	if err := options.Bootstrap.validate(); err != nil {
		return nil, err
	}
	sessions, err := newSessionStore()
	if err != nil {
		return nil, fmt.Errorf("create session store: %w", err)
	}
	if options.Handler == nil {
		options.Handler = http.NotFoundHandler()
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	launchValue, err := options.Bootstrap.takeLaunch()
	if err != nil {
		return nil, err
	}
	return &Server{
		port:         options.Port,
		bootstrap:    options.Bootstrap,
		handler:      options.Handler,
		logger:       options.Logger,
		sessions:     sessions,
		now:          time.Now,
		launchValue:  launchValue,
		shutdownDone: make(chan struct{}),
		stopCh:       make(chan struct{}),
	}, nil
}

// Start binds explicit IPv4 loopback first, then IPv6 loopback on the same
// selected port when the platform supports it.
func (s *Server) Start(ctx context.Context) (Bound, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return Bound{}, errors.New("server already started")
	}
	if s.shutdownStarted {
		return Bound{}, errors.New("server is shut down")
	}
	if err := ctx.Err(); err != nil {
		return Bound{}, err
	}
	if s.launchValue == "" {
		return Bound{}, errors.New("bootstrap capability is unavailable")
	}

	ipv4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)))
	if err != nil {
		return Bound{}, fmt.Errorf("bind IPv4 loopback: %w", err)
	}
	selectedPort := ipv4.Addr().(*net.TCPAddr).Port
	listeners := []net.Listener{ipv4}
	ipv6, ipv6Err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(selectedPort)))
	if ipv6Err == nil {
		listeners = append(listeners, ipv6)
	} else if !isIPv6Unavailable(ipv6Err) {
		_ = ipv4.Close()
		return Bound{}, fmt.Errorf("bind IPv6 loopback: %w", ipv6Err)
	}

	query := url.Values{"access_token": {s.launchValue}}
	bound := Bound{
		URL:  "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(selectedPort)) + "/?" + query.Encode(),
		Port: selectedPort,
	}
	s.launchValue = ""
	query = nil

	s.boundPort.Store(int64(selectedPort))
	s.listeners = listeners
	s.started = true
	protected := s.protectedHandler()
	for _, listener := range listeners {
		httpServer := &http.Server{
			Handler:  protected,
			ErrorLog: log.New(fixedLogWriter{logger: s.logger}, "", 0),
		}
		s.httpServers = append(s.httpServers, httpServer)
		s.serveWG.Add(1)
		go s.serve(httpServer, listener)
	}
	go s.stopOnContext(ctx)

	return bound, nil
}

func (s *Server) serve(httpServer *http.Server, listener net.Listener) {
	defer s.serveWG.Done()
	err := httpServer.Serve(listener)
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return
	}
	s.mu.Lock()
	s.serveErr = errors.Join(s.serveErr, err)
	listeners := append([]net.Listener(nil), s.listeners...)
	s.mu.Unlock()
	s.logger.Error("loopback HTTP server stopped unexpectedly")
	for _, current := range listeners {
		_ = current.Close()
	}
}

func (s *Server) stopOnContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	case <-s.stopCh:
	}
}

// Shutdown closes all listeners and waits for every serving goroutine. Calls
// are safe and idempotent, including concurrent calls.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdownStarted {
		done := s.shutdownDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.shutdownErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.shutdownStarted = true
	s.stopOnce.Do(func() { close(s.stopCh) })
	servers := append([]*http.Server(nil), s.httpServers...)
	s.mu.Unlock()

	var shutdownErr error
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
			_ = server.Close()
		}
	}
	waited := make(chan struct{})
	go func() {
		s.serveWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		shutdownErr = errors.Join(shutdownErr, ctx.Err())
		for _, server := range servers {
			_ = server.Close()
		}
		<-waited
	}

	s.mu.Lock()
	s.shutdownErr = errors.Join(shutdownErr, s.serveErr)
	result := s.shutdownErr
	close(s.shutdownDone)
	s.mu.Unlock()
	return result
}

type fixedLogWriter struct {
	logger *slog.Logger
}

func (w fixedLogWriter) Write(value []byte) (int, error) {
	w.logger.Error("loopback HTTP server rejected a request")
	return len(value), nil
}
