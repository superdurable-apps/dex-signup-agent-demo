package signup_test

// The Eino model of each protocol reaches the configured endpoint with the endpoint's model ID and key, and nothing
// else: the Eino counterpart of test/pi-model.test.ts's custom endpoint cases.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

type endpointRequest struct {
	path    string
	headers http.Header
	body    map[string]any
}

func endpoint(t *testing.T, answer string) (*httptest.Server, func() []endpointRequest) {
	t.Helper()
	var mutex sync.Mutex
	var requests []endpointRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		contents, _ := io.ReadAll(request.Body)
		var body map[string]any
		_ = json.Unmarshal(contents, &body)
		mutex.Lock()
		requests = append(requests, endpointRequest{path: request.URL.Path, headers: request.Header.Clone(), body: body})
		mutex.Unlock()
		response.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(response, answer)
	}))
	t.Cleanup(server.Close)
	return server, func() []endpointRequest {
		mutex.Lock()
		defer mutex.Unlock()
		return append([]endpointRequest(nil), requests...)
	}
}

func TestTheOpenAIProtocolPostsChatCompletionsToTheBaseURLWithTheModelIDAsItIs(t *testing.T) {
	server, requests := endpoint(t, `{"id":"c1","object":"chat.completion","model":"meta-llama/Llama-3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	chatModel, err := signup.NewEinoChatModel(context.Background(), signup.EinoModelSelection{Protocol: signup.EinoProtocolOpenAI, ModelID: "meta-llama/Llama-3", BaseURL: server.URL + "/v1", APIKey: "sk-endpoint-key"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "hello" {
		t.Fatalf("answer = %q", answer.Content)
	}
	sent := requests()
	if len(sent) != 1 || sent[0].path != "/v1/chat/completions" || sent[0].headers.Get("Authorization") != "Bearer sk-endpoint-key" {
		t.Fatalf("requests = %+v", sent)
	}
	if sent[0].body["model"] != "meta-llama/Llama-3" || sent[0].body["max_tokens"] != float64(16384) {
		t.Fatalf("body = %v", sent[0].body)
	}
}

func TestTheAnthropicProtocolPostsMessagesWithTheKeyAsXAPIKeyOnly(t *testing.T) {
	// Anthropic variables of the shell never reach the endpoint.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-not-this-one")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "not-this-token")
	server, requests := endpoint(t, `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	chatModel, err := signup.NewEinoChatModel(context.Background(), signup.EinoModelSelection{Protocol: signup.EinoProtocolAnthropic, ModelID: "claude-opus-5-5", BaseURL: server.URL + "/anthropic", APIKey: "sk-endpoint-key"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "hello" {
		t.Fatalf("answer = %q", answer.Content)
	}
	sent := requests()
	if len(sent) != 1 || sent[0].path != "/anthropic/v1/messages" || sent[0].headers.Get("X-Api-Key") != "sk-endpoint-key" || sent[0].headers.Get("Authorization") != "" {
		t.Fatalf("requests = %+v", sent)
	}
	if sent[0].body["model"] != "claude-opus-5-5" || sent[0].body["max_tokens"] != float64(16384) {
		t.Fatalf("body = %v", sent[0].body)
	}
	for name := range sent[0].headers {
		if values := strings.Join(sent[0].headers.Values(name), " "); strings.Contains(values, "not-this") {
			t.Fatalf("header %s carries a shell variable", name)
		}
	}
}
