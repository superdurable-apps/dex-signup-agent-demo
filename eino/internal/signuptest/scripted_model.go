package signuptest

// A scripted Eino ToolCallingChatModel over DecideNextStep: what the pi harness's ScriptedModelClient and the Claude
// harness's scripted Messages API are for the other harnesses.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ScriptedFailure is the scripted model's failed call: an HTTP 400 the Dex core reads as a permanent model error, so
// the turn fails at once, as a provider refusing the request would make it.
type ScriptedFailure struct {
	// StatusCode is what the Dex core's model-error classification reads.
	StatusCode int
}

func (failure *ScriptedFailure) Error() string {
	return fmt.Sprintf("scripted model failure (HTTP %d)", failure.StatusCode)
}

// ScriptedChatModel answers each model call with DecideNextStep over the request's user messages and the tool results
// since the last one. It records the tools it was offered and every request.
type ScriptedChatModel struct {
	state *scriptedModelState
	tools []*schema.ToolInfo
}

type scriptedModelState struct {
	mutex    sync.Mutex
	offered  map[string]bool
	requests [][]*schema.Message
}

// NewScriptedChatModel returns a scripted model with no tools bound.
func NewScriptedChatModel() *ScriptedChatModel {
	return &ScriptedChatModel{state: &scriptedModelState{offered: map[string]bool{}}}
}

var _ model.ToolCallingChatModel = (*ScriptedChatModel)(nil)

// WithTools returns the model with tools bound, as an Eino ChatModelAgent binds its tools.
func (scripted *ScriptedChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	scripted.state.mutex.Lock()
	for _, tool := range tools {
		scripted.state.offered[tool.Name] = true
	}
	scripted.state.mutex.Unlock()
	return &ScriptedChatModel{state: scripted.state, tools: tools}, nil
}

// OfferedTools are the names of every tool the model was offered, sorted.
func (scripted *ScriptedChatModel) OfferedTools() []string {
	scripted.state.mutex.Lock()
	defer scripted.state.mutex.Unlock()
	names := make([]string, 0, len(scripted.state.offered))
	for name := range scripted.state.offered {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Requests are the messages of every model call, oldest first.
func (scripted *ScriptedChatModel) Requests() [][]*schema.Message {
	scripted.state.mutex.Lock()
	defer scripted.state.mutex.Unlock()
	return append([][]*schema.Message(nil), scripted.state.requests...)
}

// UserTexts are the request's user messages, oldest first.
func UserTexts(input []*schema.Message) []string {
	var texts []string
	for _, message := range input {
		if message.Role == schema.User {
			texts = append(texts, message.Content)
		}
	}
	return texts
}

// ToolResultsSinceLastUserMessage are the tool results after the request's last user message, oldest first.
func ToolResultsSinceLastUserMessage(input []*schema.Message) []SeenResult {
	last := -1
	for index, message := range input {
		if message.Role == schema.User {
			last = index
		}
	}
	var seen []SeenResult
	for _, message := range input[last+1:] {
		if message.Role == schema.Tool {
			seen = append(seen, SeenResult{Name: message.ToolName, Output: message.Content})
		}
	}
	return seen
}

// Generate answers one model call.
func (scripted *ScriptedChatModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	scripted.state.mutex.Lock()
	scripted.state.requests = append(scripted.state.requests, input)
	scripted.state.mutex.Unlock()
	step := DecideNextStep(UserTexts(input), ToolResultsSinceLastUserMessage(input))
	usage := &schema.ResponseMeta{FinishReason: "stop", Usage: &schema.TokenUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}}
	switch step.Kind {
	case "fail":
		return nil, &ScriptedFailure{StatusCode: 400}
	case "answer":
		return &schema.Message{Role: schema.Assistant, Content: step.Text, ResponseMeta: usage}, nil
	}
	message := &schema.Message{Role: schema.Assistant, ResponseMeta: usage}
	usage.FinishReason = "tool_calls"
	for index, call := range step.Calls {
		arguments, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, err
		}
		position := index
		message.ToolCalls = append(message.ToolCalls, schema.ToolCall{Index: &position, Type: "function", Function: schema.FunctionCall{Name: call.Name, Arguments: string(arguments)}})
	}
	return message, nil
}

// Stream answers one model call as a single chunk.
func (scripted *ScriptedChatModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := scripted.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}
