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
	"github.com/cloudwego/eino/components/model"
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
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL: selection.BaseURL, APIKey: selection.APIKey, Model: selection.ModelID, MaxTokens: &maxTokens,
			HTTPClient: &http.Client{Timeout: einoRequestTimeout},
		})
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
