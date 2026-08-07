package secrets

import (
	"context"
	"errors"
	"strings"

	nativekeyring "github.com/zalando/go-keyring"
)

const nativeKeyringBackend = "native-keyring"

var (
	errKeyringPut    = errors.New("native keyring put failed")
	errKeyringGet    = errors.New("native keyring get failed")
	errKeyringDelete = errors.New("native keyring delete failed")
)

type keyringClient interface {
	Set(service, account, password string) error
	Get(service, account string) (string, error)
	Delete(service, account string) error
}

type systemKeyringClient struct{}

func (systemKeyringClient) Set(service, account, password string) error {
	return nativekeyring.Set(service, account, password)
}

func (systemKeyringClient) Get(service, account string) (string, error) {
	return nativekeyring.Get(service, account)
}

func (systemKeyringClient) Delete(service, account string) error {
	return nativekeyring.Delete(service, account)
}

type keyringStore struct {
	servicePrefix string
	client        keyringClient
}

func NewKeyring(servicePrefix string) Store {
	return newKeyring(servicePrefix, systemKeyringClient{})
}

func newKeyring(servicePrefix string, client keyringClient) Store {
	servicePrefix = strings.TrimRight(servicePrefix, "/")
	if servicePrefix == "" {
		servicePrefix = defaultServicePrefix
	}
	return &keyringStore{servicePrefix: servicePrefix, client: client}
}

func (s *keyringStore) Put(ctx context.Context, ref Ref, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	valueCopy := append([]byte(nil), value...)
	password := string(valueCopy)
	clear(valueCopy)
	if err := s.client.Set(s.service(ref), ref.Account(), password); err != nil {
		return errKeyringPut
	}
	return nil
}

func (s *keyringStore) Get(ctx context.Context, ref Ref) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	value, err := s.client.Get(s.service(ref), ref.Account())
	if errors.Is(err, nativekeyring.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, errKeyringGet
	}
	return append([]byte(nil), value...), nil
}

func (s *keyringStore) Delete(ctx context.Context, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.client.Delete(s.service(ref), ref.Account())
	if errors.Is(err, nativekeyring.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return errKeyringDelete
	}
	return nil
}

func (s *keyringStore) Status(ctx context.Context, ref Ref) Status {
	status := Status{Backend: nativeKeyringBackend}
	if err := ctx.Err(); err != nil {
		status.ErrorCode = "canceled"
		return status
	}

	_, err := s.client.Get(s.service(ref), ref.Account())
	if errors.Is(err, nativekeyring.ErrNotFound) {
		status.ErrorCode = "not_found"
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
