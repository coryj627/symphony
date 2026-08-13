package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type pipeTransport struct {
	stdoutReader *io.PipeReader
	stdinWriter  *io.PipeWriter
	serverInput  *bufio.Reader
	serverOutput *io.PipeWriter
	closeOnce    sync.Once
}

func canonicalTestDirectory(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.EqualFold(strings.SplitN(entry, "=", 2)[0], name) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func newPipeTransport(t *testing.T, options RouterOptions) (*Router, *pipeTransport) {
	t.Helper()
	stdoutReader, serverOutput := io.Pipe()
	serverInputRaw, stdinWriter := io.Pipe()
	transport := &pipeTransport{
		stdoutReader: stdoutReader,
		stdinWriter:  stdinWriter,
		serverInput:  bufio.NewReader(serverInputRaw),
		serverOutput: serverOutput,
	}
	t.Cleanup(func() { _ = transport.Close() })
	return NewRouter(transport, options), transport
}

func (transport *pipeTransport) Read(target []byte) (int, error) {
	return transport.stdoutReader.Read(target)
}

func (transport *pipeTransport) Write(source []byte) (int, error) {
	return transport.stdinWriter.Write(source)
}

func (transport *pipeTransport) Close() error {
	transport.closeOnce.Do(func() {
		_ = transport.stdoutReader.Close()
		_ = transport.stdinWriter.Close()
		_ = transport.serverOutput.Close()
	})
	return nil
}

func (transport *pipeTransport) readRequest(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	type result struct {
		line []byte
		err  error
	}
	resultChannel := make(chan result, 1)
	go func() {
		line, err := transport.serverInput.ReadBytes('\n')
		resultChannel <- result{line: line, err: err}
	}()
	select {
	case got := <-resultChannel:
		if got.err != nil {
			t.Fatalf("read request: %v", got.err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got.line, &fields); err != nil {
			t.Fatalf("decode request %q: %v", got.line, err)
		}
		return fields
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading router request")
		return nil
	}
}

func (transport *pipeTransport) sendJSON(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	transport.sendRaw(t, append(encoded, '\n'))
}

func (transport *pipeTransport) sendRaw(t *testing.T, value []byte) {
	t.Helper()
	if _, err := transport.serverOutput.Write(value); err != nil {
		t.Fatalf("write server output: %v", err)
	}
}

type callResult struct {
	value map[string]any
	err   error
}

func startCall(ctx context.Context, router *Router, method string) <-chan callResult {
	result := make(chan callResult, 1)
	go func() {
		var value map[string]any
		err := router.Call(ctx, method, map[string]any{"test": true}, &value)
		result <- callResult{value: value, err: err}
	}()
	return result
}
