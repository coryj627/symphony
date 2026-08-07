//go:build e2e

package web

import (
	"errors"
	"os"
)

// NewBootstrap uses an explicit deterministic capability only in e2e builds.
func NewBootstrap() (Bootstrap, error) {
	value := os.Getenv("SYMPHONY_E2E_BOOTSTRAP_TOKEN")
	if len(value) < 32 {
		return Bootstrap{}, errors.New("e2e bootstrap token must be at least 32 characters")
	}
	return bootstrapFromValue(value), nil
}
