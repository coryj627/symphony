package secrets

import (
	"context"
	"errors"
	"runtime"
	"strings"
)

const nativeKeyringBackend = "native-keyring"

var (
	errKeyringPut    = errors.New("native keyring put failed")
	errKeyringGet    = errors.New("native keyring get failed")
	errKeyringDelete = errors.New("native keyring delete failed")
)

type keyringClient interface {
	Set(context.Context, string, string, string) error
	Get(context.Context, string, string) (string, error)
	Delete(context.Context, string, string) error
}

type keyringStore struct {
	servicePrefix string
	client        keyringClient
	supported     bool
}

func NewKeyring(servicePrefix string) Store {
	return newKeyring(servicePrefix, newSystemKeyringClient())
}

func newKeyring(servicePrefix string, client keyringClient) Store {
	return newKeyringForPlatform(servicePrefix, runtime.GOOS, client)
}

func newKeyringForPlatform(servicePrefix, goos string, client keyringClient) Store {
	servicePrefix = strings.TrimRight(servicePrefix, "/")
	if servicePrefix == "" {
		servicePrefix = defaultServicePrefix
	}
	return &keyringStore{
		servicePrefix: servicePrefix,
		client:        client,
		supported:     goos == "darwin" || goos == "windows",
	}
}

func (s *keyringStore) Put(ctx context.Context, ref Ref, value []byte) error {
	if !s.supported {
		return ErrUnsupportedPlatform
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	valueCopy := append([]byte(nil), value...)
	password := string(valueCopy)
	clear(valueCopy)
	if err := s.client.Set(ctx, s.service(ref), ref.Account(), password); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errKeyringPut
	}
	return nil
}

func (s *keyringStore) Get(ctx context.Context, ref Ref) ([]byte, error) {
	if !s.supported {
		return nil, ErrUnsupportedPlatform
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	value, err := s.client.Get(ctx, s.service(ref), ref.Account())
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if err != nil {
		return nil, errKeyringGet
	}
	return append([]byte(nil), value...), nil
}

func (s *keyringStore) Delete(ctx context.Context, ref Ref) error {
	if !s.supported {
		return ErrUnsupportedPlatform
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.client.Delete(ctx, s.service(ref), ref.Account())
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err != nil {
		return errKeyringDelete
	}
	return nil
}

func (s *keyringStore) Status(ctx context.Context, ref Ref) Status {
	status := Status{Backend: nativeKeyringBackend}
	if !s.supported {
		status.ErrorCode = "unsupported_platform"
		return status
	}
	if err := ctx.Err(); err != nil {
		status.ErrorCode = "canceled"
		return status
	}

	_, err := s.client.Get(ctx, s.service(ref), ref.Account())
	if errors.Is(err, ErrNotFound) {
		status.ErrorCode = "not_found"
		return status
	}
	if errors.Is(err, context.Canceled) {
		status.ErrorCode = "canceled"
		return status
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status.ErrorCode = "deadline_exceeded"
		return status
	}
	if err != nil {
		status.ErrorCode = "backend_error"
		return status
	}
	status.Present = true
	return status
}

func (s *keyringStore) service(ref Ref) string {
	return s.servicePrefix + "/workflow/" + ref.WorkflowID
}
