package secrets

import (
	"context"
	"sync"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[Ref][]byte
}

func NewMemoryStore() Store {
	return &memoryStore{values: make(map[Ref][]byte)}
}

func (s *memoryStore) Put(_ context.Context, ref Ref, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	clear(s.values[ref])
	s.values[ref] = append([]byte(nil), value...)
	return nil
}

func (s *memoryStore) Get(_ context.Context, ref Ref) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	value, ok := s.values[ref]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (s *memoryStore) Delete(_ context.Context, ref Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.values[ref]; !ok {
		return ErrNotFound
	}
	clear(s.values[ref])
	delete(s.values, ref)
	return nil
}

func (s *memoryStore) Status(_ context.Context, ref Ref) Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.values[ref]
	return Status{Present: ok, Backend: "memory"}
}
