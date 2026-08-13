package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/coryj627/symphony/go/internal/buildinfo"
	codexschema "github.com/coryj627/symphony/go/schema/codex"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type cachedRequestSchema struct {
	schema *jsonschema.Schema
	err    error
}

var requestSchemaCache sync.Map

func validatePinnedRequestParams(filename string, raw json.RawMessage) error {
	loaded, _ := requestSchemaCache.LoadOrStore(filename, sync.OnceValue(func() cachedRequestSchema {
		contents, err := fs.ReadFile(codexschema.Files, buildinfo.CodexVersion+"/"+filename)
		if err != nil {
			return cachedRequestSchema{err: err}
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(contents))
		if err != nil {
			return cachedRequestSchema{err: err}
		}
		compiler := jsonschema.NewCompiler()
		const resource = "https://symphony.local/codex-request-schema.json"
		if err := compiler.AddResource(resource, document); err != nil {
			return cachedRequestSchema{err: err}
		}
		schema, err := compiler.Compile(resource)
		return cachedRequestSchema{schema: schema, err: err}
	}))
	compiled := loaded.(func() cachedRequestSchema)()
	if compiled.err != nil {
		return fmt.Errorf("load pinned Codex request schema: %w", compiled.err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return compiled.schema.Validate(value)
}
