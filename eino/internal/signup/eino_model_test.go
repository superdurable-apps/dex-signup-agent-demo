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

// The openai protocol leaves the reasoning of earlier responses out of each request, as the pi harness does for a
// custom endpoint: an OpenAI-compatible endpoint may reject reasoning_content in the messages it gets. The text, the
// tool calls and the tool results stay.
func TestTheOpenAIProtocolLeavesTheReasoningOfEarlierResponsesOutOfTheRequest(t *testing.T) {
	var mutex sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		contents, _ := io.ReadAll(request.Body)
		var body map[string]any
		_ = json.Unmarshal(contents, &body)
		mutex.Lock()
		bodies = append(bodies, body)
		mutex.Unlock()
		if body["stream"] == true {
			response.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(response, `data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"I think a lot"}}]}`+"\n\n"+
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
			return
		}
		response.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(response, `{"id":"c1","object":"chat.completion","model":"deepseek-reasoner","choices":[{"index":0,"message":{"role":"assistant","content":"done","reasoning_content":"I think a lot"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(server.Close)
	chatModel, err := signup.NewEinoChatModel(context.Background(), signup.EinoModelSelection{Protocol: signup.EinoProtocolOpenAI, ModelID: "deepseek-reasoner", BaseURL: server.URL + "/v1", APIKey: "sk-endpoint-key"})
	if err != nil {
		t.Fatal(err)
	}
	chatModel, err = chatModel.WithTools([]*schema.ToolInfo{{Name: "github_user", Desc: "Look up a GitHub user."}})
	if err != nil {
		t.Fatal(err)
	}
	earlier := &schema.Message{Role: schema.Assistant, Content: "Looking", ReasoningContent: "I think a lot",
		ToolCalls: []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "github_user", Arguments: `{"login":"ada"}`}}}}
	history := []*schema.Message{schema.UserMessage("hi"), earlier, schema.ToolMessage(`{"login":"ada"}`, "call-1")}
	for _, call := range []func() (*schema.Message, error){
		func() (*schema.Message, error) { return chatModel.Generate(context.Background(), history) },
		func() (*schema.Message, error) {
			stream, err := chatModel.Stream(context.Background(), history)
			if err != nil {
				return nil, err
			}
			defer stream.Close()
			var chunks []*schema.Message
			for {
				chunk, err := stream.Recv()
				if err == io.EOF {
					return schema.ConcatMessages(chunks)
				}
				if err != nil {
					return nil, err
				}
				chunks = append(chunks, chunk)
			}
		},
	} {
		answer, err := call()
		if err != nil {
			t.Fatal(err)
		}
		// The session keeps the reasoning it got: only the request leaves it out.
		if answer.ReasoningContent != "I think a lot" {
			t.Fatalf("answer = %+v", answer)
		}
	}
	if earlier.ReasoningContent != "I think a lot" {
		t.Fatal("the stored message lost its reasoning")
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d", len(bodies))
	}
	for _, body := range bodies {
		messages := body["messages"].([]any)
		assistant := messages[1].(map[string]any)
		if _, has := assistant["reasoning_content"]; has || assistant["content"] != "Looking" || assistant["tool_calls"] == nil {
			t.Fatalf("assistant message = %v", assistant)
		}
		if tool := messages[2].(map[string]any); tool["tool_call_id"] != "call-1" {
			t.Fatalf("tool message = %v", tool)
		}
	}
}
