package signup_test

// The configuration and the Eino model rules: the Eino counterpart of test/pi-model.test.ts's configuration cases.

import (
	"strings"
	"testing"
	"time"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

func environment(extra map[string]string) map[string]string {
	values := map[string]string{
		"REVIEW_API_URL": "https://review-api.example", "REVIEW_API_KEY": "srk_1_secret", "SIGNUP_REVIEW_SINCE": "2026-11-13T00:00:00Z",
		"SLACK_REVIEW_CHANNEL": "C0123", "SIGNUP_REVIEWER_SLACK_IDS": "U1, U2,,", "DEX_CONNECTOR_CONFIG_FILE": "/tmp/connections.json",
		"SIGNUP_AGENT_EINO_BASE_URL": "https://llm.example.com/v1", "SIGNUP_AGENT_EINO_MODEL": "meta-llama/Llama-3", "SIGNUP_AGENT_EINO_API_KEY": "sk-endpoint-secret",
	}
	for key, value := range extra {
		if value == "<unset>" {
			delete(values, key)
		} else {
			values[key] = value
		}
	}
	return values
}

func TestTheConfigurationReadsEveryVariableWithItsDefaults(t *testing.T) {
	config, err := signup.LoadConfig(environment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if config.ReviewSince != time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) || config.WatchInterval != 30*time.Second || config.WorkerSlots != 64 {
		t.Fatalf("config = %+v", config)
	}
	if len(config.Channel.ReviewerIDs) != 2 || !config.Channel.ReviewerIDs["U1"] || !config.Channel.ReviewerIDs["U2"] {
		t.Fatalf("reviewers = %v", config.Channel.ReviewerIDs)
	}
	if config.Model != (signup.EinoModelSelection{Protocol: signup.EinoProtocolOpenAI, ModelID: "meta-llama/Llama-3", BaseURL: "https://llm.example.com/v1", APIKey: "sk-endpoint-secret"}) {
		t.Fatalf("model = %+v", config.Model)
	}
	if config.Dex.ServerAddress != "127.0.0.1:8801" || config.BlobCacheDirectory != ".dex-blob-cache" {
		t.Fatalf("dex = %+v", config.Dex)
	}
	// The model's description never shows its key.
	if strings.Contains(config.Model.String(), "secret") {
		t.Fatalf("model string = %s", config.Model)
	}
	anthropic, err := signup.LoadConfig(environment(map[string]string{"SIGNUP_AGENT_EINO_PROTOCOL": "anthropic", "SIGNUP_AGENT_EINO_BASE_URL": "https://llm.example.com/anthropic", "SIGNUP_AGENT_EINO_MODEL": "claude-opus-5-5"}))
	if err != nil || anthropic.Model.Protocol != signup.EinoProtocolAnthropic || anthropic.Model.ModelID != "claude-opus-5-5" {
		t.Fatalf("anthropic = %+v, %v", anthropic.Model, err)
	}
	harness, err := signup.LoadConfig(environment(map[string]string{"SIGNUP_AGENT_HARNESS": "eino"}))
	if err != nil || harness.Model.ModelID == "" {
		t.Fatalf("eino harness: %v", err)
	}
}

func TestBadCombinationsStopTheStartWithAMessageThatSaysWhatToSet(t *testing.T) {
	for _, check := range []struct {
		extra map[string]string
		want  string
	}{
		{map[string]string{"SIGNUP_AGENT_HARNESS": "pi"}, "SIGNUP_AGENT_HARNESS is pi, but this is the Eino process: unset it or set it to eino"},
		{map[string]string{"REVIEW_API_KEY": "<unset>"}, "REVIEW_API_KEY is required; see .env.example"},
		{map[string]string{"SIGNUP_REVIEW_SINCE": "yesterday"}, "SIGNUP_REVIEW_SINCE must be an RFC 3339 time, such as "},
		{map[string]string{"SIGNUP_AGENT_EINO_BASE_URL": "<unset>"}, "SIGNUP_AGENT_EINO_BASE_URL is required: set it to the endpoint's base URL"},
		{map[string]string{"SIGNUP_AGENT_EINO_MODEL": "<unset>"}, "SIGNUP_AGENT_EINO_MODEL is required with SIGNUP_AGENT_EINO_BASE_URL"},
		{map[string]string{"SIGNUP_AGENT_EINO_API_KEY": "<unset>"}, "SIGNUP_AGENT_EINO_API_KEY is required; see .env.example"},
		{map[string]string{"SIGNUP_AGENT_EINO_PROTOCOL": "gemini"}, "SIGNUP_AGENT_EINO_PROTOCOL must be openai or anthropic"},
		{map[string]string{"SIGNUP_AGENT_EINO_MODEL": "openai-compatible/qwen"}, "has the pi harness's prefix openai-compatible/"},
		{map[string]string{"SIGNUP_AGENT_EINO_BASE_URL": "ftp://llm.example.com"}, "SIGNUP_AGENT_EINO_BASE_URL must be an http(s) URL with a host"},
		{map[string]string{"SIGNUP_AGENT_EINO_BASE_URL": "https://llm.example.com/v1?key=1"}, "must not have a query or a fragment"},
		{map[string]string{"SIGNUP_AGENT_EINO_BASE_URL": "https://llm.example.com/v1/chat/completions"}, "must not end in /chat/completions: with SIGNUP_AGENT_EINO_PROTOCOL=openai the client appends /chat/completions"},
		{map[string]string{"SIGNUP_AGENT_EINO_PROTOCOL": "anthropic", "SIGNUP_AGENT_EINO_BASE_URL": "https://llm.example.com/v1/messages"}, "must not end in /messages: with SIGNUP_AGENT_EINO_PROTOCOL=anthropic the client appends /v1/messages"},
		{map[string]string{"SIGNUP_WATCH_INTERVAL_SECONDS": "0"}, "SIGNUP_WATCH_INTERVAL_SECONDS must be a positive whole number of seconds"},
		{map[string]string{"SIGNUP_WORKER_SLOTS": "many"}, "SIGNUP_WORKER_SLOTS must be a positive whole number of slots"},
		{map[string]string{"DEX_API_KEY": "key"}, "DEX_TOKEN_URL is required with DEX_API_KEY"},
		{map[string]string{"DEX_CONNECTOR_CONFIG_FILE": "<unset>"}, "DEX_CONNECTOR_CONFIG_FILE is required; see .env.example"},
	} {
		_, err := signup.LoadConfig(environment(check.extra))
		if err == nil || !strings.Contains(err.Error(), check.want) {
			t.Fatalf("%v: err = %v, want %q", check.extra, err, check.want)
		}
		// No message shows a key.
		if strings.Contains(err.Error(), "sk-endpoint-secret") || strings.Contains(err.Error(), "srk_1_secret") {
			t.Fatalf("%v: the message shows a key: %v", check.extra, err)
		}
	}
}
