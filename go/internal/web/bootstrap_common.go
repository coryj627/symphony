package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"sync"
	"time"
)

const bootstrapLifetime = 5 * time.Minute

// Bootstrap is a one-time capability used to establish the browser session.
// Its fields are deliberately private so callers cannot inspect verifier state.
type Bootstrap struct {
	state *bootstrapState
}

type bootstrapState struct {
	mu      sync.Mutex
	value   string
	digest  [sha256.Size]byte
	expires time.Time
	used    bool
}

func bootstrapFromValue(value string) Bootstrap {
	return bootstrapFromValueUntil(value, time.Now().Add(bootstrapLifetime))
}

func bootstrapFromValueUntil(value string, expires time.Time) Bootstrap {
	return Bootstrap{state: &bootstrapState{
		value:   value,
		digest:  sha256.Sum256([]byte(value)),
		expires: expires,
	}}
}

func (b Bootstrap) validate() error {
	if b.state == nil {
		return errors.New("bootstrap capability is required")
	}
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	if b.state.value == "" {
		return errors.New("bootstrap capability is required")
	}
	return nil
}

func (b Bootstrap) value() (string, error) {
	if b.state == nil {
		return "", errors.New("bootstrap capability is required")
	}
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	if b.state.used || b.state.value == "" || !time.Now().Before(b.state.expires) {
		return "", errors.New("bootstrap capability is unavailable")
	}
	return b.state.value, nil
}

func (b Bootstrap) exchange(candidate string, now time.Time) bool {
	if b.state == nil {
		return false
	}
	candidateDigest := sha256.Sum256([]byte(candidate))
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	valid := !b.state.used && now.Before(b.state.expires) &&
		subtle.ConstantTimeCompare(candidateDigest[:], b.state.digest[:]) == 1
	if valid {
		b.state.used = true
		b.state.value = ""
	}
	return valid
}
