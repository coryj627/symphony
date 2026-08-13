package github

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	maxGitHubIdempotencyEntries = 64
	maxGitHubIdempotencyBytes   = 8 << 20
)

type githubIdempotencyRecord struct {
	digest [32]byte
	result []byte
}

func (adapter *Adapter) executeIdempotentComment(ctx context.Context, call parsedGitHubToolCall, session tracker.Session) domain.ToolResult {
	if err := ctx.Err(); err != nil {
		return githubToolFailure("canceled")
	}
	key := session.ToolScopeID() + "\x00" + call.idempotencyKey
	adapter.toolMu.Lock()
	defer adapter.toolMu.Unlock()
	if adapter.idempotencyCache == nil {
		adapter.idempotencyCache = make(map[string]githubIdempotencyRecord)
	}
	if cached, found := adapter.idempotencyCache[key]; found {
		if cached.digest != call.digest {
			return githubToolFailure("idempotency_key_reused")
		}
		result, ok := decodeCachedGitHubToolResult(cached.result)
		if !ok {
			return githubToolFailure("idempotency_cache_invalid")
		}
		return result
	}
	if len(adapter.idempotencyCache) >= maxGitHubIdempotencyEntries {
		return githubToolFailure("idempotency_cache_full")
	}
	result := adapter.executeGitHubToolRequest(ctx, call)
	encoded, err := json.Marshal(result)
	if err != nil || adapter.idempotencyBytes+len(encoded) > maxGitHubIdempotencyBytes {
		return githubToolFailure("idempotency_cache_full")
	}
	adapter.idempotencyCache[key] = githubIdempotencyRecord{digest: call.digest, result: bytes.Clone(encoded)}
	adapter.idempotencyBytes += len(encoded)
	return result
}

func decodeCachedGitHubToolResult(encoded []byte) (domain.ToolResult, bool) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var result domain.ToolResult
	if err := decoder.Decode(&result); err != nil || result.Validate() != nil {
		return domain.ToolResult{}, false
	}
	return result, true
}
