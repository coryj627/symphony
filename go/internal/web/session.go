package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
)

const sessionCookieName = "symphony_session"

type sessionRecord struct {
	digest     [sha256.Size]byte
	csrfDigest [sha256.Size]byte
}

type authenticatedSession struct {
	record sessionRecord
	csrf   string
}

type sessionStore struct {
	mu       sync.RWMutex
	csrfKey  [sha256.Size]byte
	sessions []sessionRecord
}

func newSessionStore() (*sessionStore, error) {
	store := new(sessionStore)
	if _, err := rand.Read(store.csrfKey[:]); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *sessionStore) issue() (string, error) {
	randomValue := make([]byte, 32)
	if _, err := rand.Read(randomValue); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(randomValue)
	record := sessionRecord{digest: sha256.Sum256([]byte(raw))}
	csrf := s.deriveCSRF(record.digest)
	record.csrfDigest = sha256.Sum256([]byte(csrf))

	s.mu.Lock()
	s.sessions = append(s.sessions, record)
	s.mu.Unlock()
	return raw, nil
}

func (s *sessionStore) authenticate(raw string) (authenticatedSession, bool) {
	candidate := sha256.Sum256([]byte(raw))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.sessions {
		if subtle.ConstantTimeCompare(candidate[:], record.digest[:]) == 1 {
			return authenticatedSession{record: record, csrf: s.deriveCSRF(record.digest)}, true
		}
	}
	return authenticatedSession{}, false
}

func (s *sessionStore) verifyCSRF(session authenticatedSession, candidate string) bool {
	digest := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(digest[:], session.record.csrfDigest[:]) == 1
}

func (s *sessionStore) deriveCSRF(sessionDigest [sha256.Size]byte) string {
	mac := hmac.New(sha256.New, s.csrfKey[:])
	_, _ = mac.Write(sessionDigest[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}
