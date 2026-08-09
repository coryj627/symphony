package secrets

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("credential not found")

var ErrUnsupportedPlatform = errors.New("native credential vault unsupported on this platform")

type Store interface {
	Put(context.Context, Ref, []byte) error
	Get(context.Context, Ref) ([]byte, error)
	Delete(context.Context, Ref) error
	Status(context.Context, Ref) Status
}

type Resolver interface {
	Resolve(context.Context, Ref, string) ([]byte, error)
}

type Status struct {
	Present   bool
	Backend   string
	ErrorCode string
}
