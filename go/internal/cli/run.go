package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var start = func(context.Context, Options, io.Writer, io.Writer) error {
	return errors.New("application startup is not configured")
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := Parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := start(ctx, opts, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ctx.Err() == nil {
		fmt.Fprintln(stderr, "application stopped before context shutdown")
		return 1
	}
	return 0
}
