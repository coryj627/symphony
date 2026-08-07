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
	state  *bootstrapState
	launch *bootstrapLaunch
}

type bootstrapState struct {
	mu      sync.Mutex
	digest  [sha256.Size]byte
	expires time.Time
	used    bool
}

type bootstrapLaunch struct {
	mu    sync.Mutex
	value string
}

func bootstrapFromValue(value string) Bootstrap {
	return bootstrapFromValueUntil(value, time.Now().Add(bootstrapLifetime))
}

func bootstrapFromValueUntil(value string, expires time.Time) Bootstrap {
	return Bootstrap{
		state: &bootstrapState{
			digest:  sha256.Sum256([]byte(value)),
			expires: expires,
		},
		launch: &bootstrapLaunch{value: value},
	}
}

func (b Bootstrap) validate() error {
	if b.state == nil || b.launch == nil {
		return errors.New("bootstrap capability is required")
	}
	b.launch.mu.Lock()
	defer b.launch.mu.Unlock()
	if b.launch.value == "" {
		return errors.New("bootstrap capability is required")
	}
	return nil
}

func (b Bootstrap) takeLaunch() (string, error) {
	if b.state == nil || b.launch == nil {
		return "", errors.New("bootstrap capability is required")
	}
	b.launch.mu.Lock()
	defer b.launch.mu.Unlock()
	if b.launch.value == "" {
		return "", errors.New("bootstrap capability is unavailable")
	}
	value := b.launch.value
	b.launch.value = ""
	return value, nil
}

func (b Bootstrap) exchange(candidate string, now func() time.Time) bool {
	if b.state == nil {
		return false
	}
	candidateDigest := sha256.Sum256([]byte(candidate))
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	valid := !b.state.used && now().Before(b.state.expires) &&
		subtle.ConstantTimeCompare(candidateDigest[:], b.state.digest[:]) == 1
	if valid {
		b.state.used = true
	}
	return valid
}
