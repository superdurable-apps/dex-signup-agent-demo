package signup

// The Eino harness's model: an OpenAI-compatible Chat Completions endpoint through eino-ext components/model/openai,
// or an Anthropic-compatible Messages endpoint through eino-ext components/model/claude.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The output limit of the endpoint's model, of either protocol, as the pi harness sends for a custom endpoint.
const einoMaxTokens = 16_384

// einoRequestTimeout bounds one model request.
const einoRequestTimeout = 5 * time.Minute

// withoutAuthorization drops an Authorization header: the endpoint's key goes only as the protocol's own header, so a
// token the Anthropic Go SDK reads from the process environment (ANTHROPIC_AUTH_TOKEN) never reaches the endpoint.
type withoutAuthorization struct{ next http.RoundTripper }

func (transport withoutAuthorization) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Header.Get("Authorization") != "" {
		request = request.Clone(request.Context())
		request.Header.Del("Authorization")
	}
	return transport.next.RoundTrip(request)
}

// NewEinoChatModel is the model of selection. The key is sent as the protocol's header (Authorization: Bearer for
// openai, x-api-key for anthropic) and never logged.
//
// The Anthropic Go SDK under eino-ext's claude model retries a failed request twice inside the model call, and
// eino-ext offers no option to turn that off; those retries happen inside one RunModel Step.
func NewEinoChatModel(ctx context.Context, selection EinoModelSelection) (model.ToolCallingChatModel, error) {
	maxTokens := einoMaxTokens
	switch selection.Protocol {
	case EinoProtocolOpenAI:
		chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL: selection.BaseURL, APIKey: selection.APIKey, Model: selection.ModelID, MaxTokens: &maxTokens,
			HTTPClient: &http.Client{Timeout: einoRequestTimeout},
		})
		if err != nil {
			return nil, err
		}
		return withoutReplayedReasoning{next: chatModel}, nil
	case EinoProtocolAnthropic:
		baseURL := selection.BaseURL
		return claude.NewChatModel(ctx, &claude.Config{
			BaseURL: &baseURL, APIKey: selection.APIKey, Model: selection.ModelID, MaxTokens: maxTokens,
			HTTPClient: &http.Client{Timeout: einoRequestTimeout, Transport: withoutAuthorization{next: http.DefaultTransport}},
		})
	default:
		return nil, fmt.Errorf("SIGNUP_AGENT_EINO_PROTOCOL must be openai or anthropic")
	}
}

// withoutReplayedReasoning leaves the reasoning of earlier responses out of each request of an OpenAI-compatible
// model, as the pi harness does for a custom endpoint (withoutReplayedThinking): eino-ext sends an assistant message's
// ReasoningContent back as reasoning_content, which an endpoint may reject. The text, the tool calls and the tool
// results stay, and the session keeps the reasoning it stored: only the request leaves it out.
type withoutReplayedReasoning struct{ next model.ToolCallingChatModel }

func (chatModel withoutReplayedReasoning) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	return chatModel.next.Generate(ctx, withoutReasoning(input), options...)
}

func (chatModel withoutReplayedReasoning) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return chatModel.next.Stream(ctx, withoutReasoning(input), options...)
}

func (chatModel withoutReplayedReasoning) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	next, err := chatModel.next.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return withoutReplayedReasoning{next: next}, nil
}

// GetType and IsCallbacksEnabled are the wrapped model's, so Eino's callbacks see the eino-ext model.
func (chatModel withoutReplayedReasoning) GetType() string {
	componentType, _ := components.GetType(chatModel.next)
	return componentType
}

func (chatModel withoutReplayedReasoning) IsCallbacksEnabled() bool {
	return components.IsCallbacksEnabled(chatModel.next)
}

// withoutReasoning is the messages with copies of the assistant messages that carry reasoning, without it.
func withoutReasoning(messages []*schema.Message) []*schema.Message {
	var result []*schema.Message
	for index, message := range messages {
		if message == nil || message.Role != schema.Assistant || message.ReasoningContent == "" {
			continue
		}
		if result == nil {
			result = append([]*schema.Message(nil), messages...)
		}
		copied := *message
		copied.ReasoningContent = ""
		result[index] = &copied
	}
	if result == nil {
		return messages
	}
	return result
}
