package codex

import (
	"context"
	"encoding/json"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

type dynamicToolCallParams struct {
	Arguments any     `json:"arguments"`
	CallID    string  `json:"callId"`
	Namespace *string `json:"namespace"`
	ThreadID  string  `json:"threadId"`
	Tool      string  `json:"tool"`
	TurnID    string  `json:"turnId"`
}

func dynamicToolNameSet(tools []DynamicToolSpec) map[string]struct{} {
	set := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		set[tool.Name] = struct{}{}
	}
	return set
}

func (session *liveAgentSession) handleServerRequest(ctx context.Context, request ServerRequest) bool {
	if request.Method != "item/tool/call" {
		return false
	}
	if request.ID.Token() == "" || validatePinnedRequestParams("DynamicToolCallParams.json", request.Params) != nil {
		session.rejectDynamicTool(request)
		return true
	}
	var params dynamicToolCallParams
	if decodeRequestParams(request.Params, &params) != nil || params.CallID == "" ||
		!session.protocol.matchesActiveTurn(params.ThreadID, params.TurnID) {
		session.rejectDynamicTool(request)
		return true
	}
	if _, advertised := session.dynamicTools[params.Tool]; !advertised || session.executeTool == nil {
		session.rejectDynamicTool(request)
		return true
	}
	call := domain.ToolCall{Name: params.Tool, Arguments: params.Arguments}
	if call.Validate() != nil {
		session.respondDynamicTool(request, domain.ToolFailure("invalid_arguments", "The dynamic tool arguments are invalid."))
		return true
	}
	trackerSession, err := session.trackerSession.Clone()
	if err != nil {
		session.respondDynamicTool(request, domain.ToolFailure("tool_session_invalid", "The captured tracker session is unavailable."))
		return true
	}
	result := executeAgentToolSafely(ctx, session.executeTool, call, trackerSession)
	if result.Validate() != nil {
		result = domain.ToolFailure("invalid_tool_result", "The tracker tool returned an invalid result.")
	}
	session.respondDynamicTool(request, result)
	return true
}

func executeAgentToolSafely(ctx context.Context, execute AgentToolExecutor, call domain.ToolCall, session tracker.Session) (result domain.ToolResult) {
	defer func() {
		if recover() != nil {
			result = domain.ToolFailure("tool_panic", "The tracker tool failed unexpectedly.")
		}
	}()
	return execute(ctx, call, session)
}

func (session *liveAgentSession) respondDynamicTool(request ServerRequest, result domain.ToolResult) {
	encoded, err := json.Marshal(result)
	if err != nil {
		result = domain.ToolFailure("invalid_tool_result", "The tracker tool result could not be encoded.")
		encoded, _ = json.Marshal(result)
	}
	response := DynamicToolCallResponse{
		Success: result.Success,
		ContentItems: []DynamicToolCallContentItem{{
			Type: "inputText", Text: string(encoded),
		}},
	}
	_ = session.protocol.RespondRequest(request.ID, response)
}

func (session *liveAgentSession) rejectDynamicTool(request ServerRequest) {
	_ = session.protocol.RejectRequest(request.ID, rpcInvalidParams, "The Codex dynamic tool request is invalid.")
}
