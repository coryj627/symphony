//go:build !e2e

package cli

import "github.com/coryj627/symphony/go/internal/secrets"

func newVaultStore() secrets.Store { return secrets.NewKeyring("symphony") }
