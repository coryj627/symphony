//go:build !e2e

package web

import (
	"crypto/rand"
	"encoding/base64"
)

// NewBootstrap creates a production bootstrap capability from the operating
// system random source. Production builds have no deterministic override.
func NewBootstrap() (Bootstrap, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return Bootstrap{}, err
	}
	return bootstrapFromValue(base64.RawURLEncoding.EncodeToString(value)), nil
}
