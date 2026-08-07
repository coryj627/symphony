//go:build e2e

package cli

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/coryj627/symphony/go/internal/web"
)

func init() {
	start = startE2E
}

// startE2E is a temporary phase-one composition adapter. It deliberately
// exposes only configure mode and starts no application or scheduler services.
func startE2E(ctx context.Context, options Options, _, _ io.Writer) error {
	if options.Mode != ModeConfigure {
		return errors.New("e2e startup requires configure mode")
	}
	bootstrap, err := web.NewBootstrap()
	if err != nil {
		return err
	}
	handler, err := web.NewPageHandler()
	if err != nil {
		return err
	}
	handler.EnableE2ERoutes()
	server, err := web.NewServer(web.Options{
		Port:           options.Port,
		Bootstrap:      bootstrap,
		Handler:        handler,
		ErrorResponder: handler,
	})
	if err != nil {
		return err
	}
	if _, err := server.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
