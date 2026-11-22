package signup

// Application configuration from environment variables: .env locally (the process loads it into a copy of its
// environment). The Eino harness runs only locally. Secrets are never logged.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the configuration of the local process.
type Config struct {
	ReviewAPIURL string
	ReviewAPIKey string
	// Channel is the review channel and the reviewers: SLACK_REVIEW_CHANNEL and SIGNUP_REVIEWER_SLACK_IDS.
	Channel ReviewChannel
	// Model is the Eino harness's model endpoint.
	Model EinoModelSelection
	// GitHubToken raises the GitHub API rate limit; empty without one.
	GitHubToken   string
	WatchInterval time.Duration
	// ReviewSince: only sign-ups registered at or after this time are reviewed.
	ReviewSince time.Time
	// WorkerSlots are the local Worker's execution slots: a running turn or tool call takes one; a waiting review none.
	WorkerSlots int
	// BlobCacheDirectory is the local blob cache.
	BlobCacheDirectory string
	// ConnectorConfigFile is DEX_CONNECTOR_CONFIG_FILE, the connections.json of Dex Web.
	ConnectorConfigFile string
	Dex                 DexOptions
}

// DexOptions is how the process reaches Dex: local dexcli dev by default, or a remote server with an API key.
type DexOptions struct {
	ServerAddress string
	TLS           bool
	APIKey        string
	TokenURL      string
	ServerID      string
	TokenCAFile   string
}

// EinoEndpointProtocol is SIGNUP_AGENT_EINO_PROTOCOL: the API of the endpoint at SIGNUP_AGENT_EINO_BASE_URL.
type EinoEndpointProtocol string

const (
	// EinoProtocolOpenAI is an OpenAI-compatible Chat Completions endpoint (eino-ext components/model/openai).
	EinoProtocolOpenAI EinoEndpointProtocol = "openai"
	// EinoProtocolAnthropic is an Anthropic-compatible Messages endpoint (eino-ext components/model/claude).
	EinoProtocolAnthropic EinoEndpointProtocol = "anthropic"
)

// EinoModelSelection is the model endpoint of the Eino harness. Eino has no model catalog, so the endpoint is always
// named: its protocol, base URL, model ID and key.
type EinoModelSelection struct {
	Protocol EinoEndpointProtocol
	ModelID  string
	BaseURL  string
	APIKey   string
}

// String never shows the key.
func (selection EinoModelSelection) String() string {
	return fmt.Sprintf("%s %s at %s", selection.Protocol, selection.ModelID, selection.BaseURL)
}

// legacyOpenAICompatiblePrefix is the pi harness's model prefix; it names nothing for Eino.
const legacyOpenAICompatiblePrefix = "openai-compatible/"

type endpointPath struct {
	path    string
	suffix  *regexp.Regexp
	example string
}

// endpointPaths are the paths the client appends to the base URL of each protocol.
var endpointPaths = map[EinoEndpointProtocol]endpointPath{
	EinoProtocolOpenAI:    {path: "/chat/completions", suffix: regexp.MustCompile(`/chat/completions/?$`), example: "https://llm.example.com/v1"},
	EinoProtocolAnthropic: {path: "/v1/messages", suffix: regexp.MustCompile(`/messages/?$`), example: "https://llm.example.com/anthropic"},
}

// endpointBaseURL checks SIGNUP_AGENT_EINO_BASE_URL: an http(s) URL with a host and no query or fragment, before the
// protocol's path.
func endpointBaseURL(value string, protocol EinoEndpointProtocol) (string, error) {
	paths := endpointPaths[protocol]
	invalid := func(reason string) error {
		return fmt.Errorf("SIGNUP_AGENT_EINO_BASE_URL %s; give the base URL only, such as %s", reason, paths.example)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return "", invalid("is not a URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", invalid("must be an http(s) URL with a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(value, "?") || strings.Contains(value, "#") {
		return "", invalid("must not have a query or a fragment")
	}
	if paths.suffix.MatchString(parsed.Path) {
		return "", invalid(fmt.Sprintf("must not end in %s: with SIGNUP_AGENT_EINO_PROTOCOL=%s the client appends %s",
			strings.TrimPrefix(paths.path, "/v1"), protocol, paths.path))
	}
	return value, nil
}

// einoModelSelection reads SIGNUP_AGENT_EINO_MODEL, SIGNUP_AGENT_EINO_BASE_URL, SIGNUP_AGENT_EINO_API_KEY and
// SIGNUP_AGENT_EINO_PROTOCOL. They follow the pi harness's custom-endpoint rules: the base URL, the model and the key
// are required, the protocol is openai unless set to anthropic, and the base URL is the part before the protocol's
// path. The model ID is the endpoint's, as it is, slashes included; the pi harness's openai-compatible/ prefix is
// refused, since Eino has no catalog it could be told apart from.
func einoModelSelection(optional func(string) string, required func(string) (string, error)) (EinoModelSelection, error) {
	protocol := EinoProtocolOpenAI
	if configured := optional("SIGNUP_AGENT_EINO_PROTOCOL"); configured != "" {
		if configured != string(EinoProtocolOpenAI) && configured != string(EinoProtocolAnthropic) {
			return EinoModelSelection{}, errors.New("SIGNUP_AGENT_EINO_PROTOCOL must be openai or anthropic, the API of the endpoint at SIGNUP_AGENT_EINO_BASE_URL")
		}
		protocol = EinoEndpointProtocol(configured)
	}
	configuredBaseURL := optional("SIGNUP_AGENT_EINO_BASE_URL")
	if configuredBaseURL == "" {
		return EinoModelSelection{}, fmt.Errorf("SIGNUP_AGENT_EINO_BASE_URL is required: set it to the endpoint's base URL, such as %s (%s) or %s (%s), and SIGNUP_AGENT_EINO_MODEL and SIGNUP_AGENT_EINO_API_KEY",
			endpointPaths[EinoProtocolOpenAI].example, EinoProtocolOpenAI, endpointPaths[EinoProtocolAnthropic].example, EinoProtocolAnthropic)
	}
	model := optional("SIGNUP_AGENT_EINO_MODEL")
	if model == "" {
		return EinoModelSelection{}, errors.New("SIGNUP_AGENT_EINO_MODEL is required with SIGNUP_AGENT_EINO_BASE_URL: set it to the endpoint's model ID, such as qwen3.8-flash-next")
	}
	if strings.HasPrefix(model, legacyOpenAICompatiblePrefix) {
		return EinoModelSelection{}, fmt.Errorf("SIGNUP_AGENT_EINO_MODEL %s has the pi harness's prefix %s: set SIGNUP_AGENT_EINO_MODEL to the endpoint's model ID alone", model, legacyOpenAICompatiblePrefix)
	}
	baseURL, err := endpointBaseURL(configuredBaseURL, protocol)
	if err != nil {
		return EinoModelSelection{}, err
	}
	apiKey, err := required("SIGNUP_AGENT_EINO_API_KEY")
	if err != nil {
		return EinoModelSelection{}, err
	}
	return EinoModelSelection{Protocol: protocol, ModelID: model, BaseURL: baseURL, APIKey: apiKey}, nil
}

var rfc3339Start = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)

// reviewSince reads SIGNUP_REVIEW_SINCE, which is required, so a first start never reviews the whole backlog.
func reviewSince(value string, now time.Time) (time.Time, error) {
	since, err := time.Parse(time.RFC3339Nano, value)
	if !rfc3339Start.MatchString(value) || err != nil {
		return time.Time{}, fmt.Errorf("SIGNUP_REVIEW_SINCE must be an RFC 3339 time, such as %s", now.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	return since, nil
}

var positiveWholeNumber = regexp.MustCompile(`^[1-9]\d{0,5}$`)

func positiveInteger(value, name, unit string) (int, error) {
	if !positiveWholeNumber.MatchString(value) {
		return 0, fmt.Errorf("%s must be a positive whole number of %s", name, unit)
	}
	return strconv.Atoi(value)
}

// LoadConfig reads the configuration from environment, a copy of the process environment with .env applied. A bad
// value stops the start with a message that says what to set.
func LoadConfig(environment map[string]string) (Config, error) {
	return loadConfigAt(environment, time.Now())
}

func loadConfigAt(environment map[string]string, now time.Time) (Config, error) {
	optional := func(name string) string { return strings.TrimSpace(environment[name]) }
	required := func(name string) (string, error) {
		if value := optional(name); value != "" {
			return value, nil
		}
		return "", fmt.Errorf("%s is required; see .env.example", name)
	}
	if harness := optional("SIGNUP_AGENT_HARNESS"); harness != "" && harness != "eino" {
		return Config{}, fmt.Errorf("SIGNUP_AGENT_HARNESS is %s, but this is the Eino process: unset it or set it to eino (make run-eino does), or run the %s harness with npm start", harness, harness)
	}
	var config Config
	var err error
	if config.ReviewAPIURL, err = required("REVIEW_API_URL"); err != nil {
		return Config{}, err
	}
	since, err := required("SIGNUP_REVIEW_SINCE")
	if err != nil {
		return Config{}, err
	}
	if config.ReviewSince, err = reviewSince(since, now); err != nil {
		return Config{}, err
	}
	if config.ReviewAPIKey, err = required("REVIEW_API_KEY"); err != nil {
		return Config{}, err
	}
	channelID, err := required("SLACK_REVIEW_CHANNEL")
	if err != nil {
		return Config{}, err
	}
	reviewers, err := required("SIGNUP_REVIEWER_SLACK_IDS")
	if err != nil {
		return Config{}, err
	}
	config.Channel = ReviewChannel{ChannelID: channelID, ReviewerIDs: map[string]bool{}}
	for _, id := range strings.Split(reviewers, ",") {
		if id = strings.TrimSpace(id); id != "" {
			config.Channel.ReviewerIDs[id] = true
		}
	}
	if config.Model, err = einoModelSelection(optional, required); err != nil {
		return Config{}, err
	}
	config.GitHubToken = optional("GITHUB_TOKEN")
	interval := optional("SIGNUP_WATCH_INTERVAL_SECONDS")
	if interval == "" {
		interval = "30"
	}
	seconds, err := positiveInteger(interval, "SIGNUP_WATCH_INTERVAL_SECONDS", "seconds")
	if err != nil {
		return Config{}, err
	}
	config.WatchInterval = time.Duration(seconds) * time.Second
	slots := optional("SIGNUP_WORKER_SLOTS")
	if slots == "" {
		slots = "64"
	}
	if config.WorkerSlots, err = positiveInteger(slots, "SIGNUP_WORKER_SLOTS", "slots"); err != nil {
		return Config{}, err
	}
	config.BlobCacheDirectory = optional("DEX_BLOB_CACHE_DIR")
	if config.BlobCacheDirectory == "" {
		config.BlobCacheDirectory = ".dex-blob-cache"
	}
	if config.ConnectorConfigFile, err = required("DEX_CONNECTOR_CONFIG_FILE"); err != nil {
		return Config{}, err
	}
	if config.Dex, err = dexOptions(optional); err != nil {
		return Config{}, err
	}
	return config, nil
}

// dexOptions reads DEX_FLOW_SERVICE_ADDRESS, DEX_TLS, and the API key exchange (DEX_API_KEY, DEX_TOKEN_URL,
// DEX_SERVER_ID, DEX_TOKEN_CA_FILE), as src/dex.ts does.
func dexOptions(optional func(string) string) (DexOptions, error) {
	options := DexOptions{ServerAddress: optional("DEX_FLOW_SERVICE_ADDRESS"), APIKey: optional("DEX_API_KEY"), TokenCAFile: optional("DEX_TOKEN_CA_FILE")}
	if options.ServerAddress == "" {
		options.ServerAddress = "127.0.0.1:8801"
	}
	tls := optional("DEX_TLS")
	options.TLS = tls == "1" || tls == "true"
	if options.APIKey != "" {
		for _, name := range []string{"DEX_TOKEN_URL", "DEX_SERVER_ID"} {
			if optional(name) == "" {
				return DexOptions{}, fmt.Errorf("%s is required with DEX_API_KEY", name)
			}
		}
		options.TokenURL, options.ServerID = optional("DEX_TOKEN_URL"), optional("DEX_SERVER_ID")
	}
	return options, nil
}
