package signuptest

// A Gmail API stand-in for the tests that run send_welcome_email on the real Gmail connector: users.messages.send
// and the Sent Mail search by Message-ID that the connector's fence reads back.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
)

// GmailSend is one message the stand-in received.
type GmailSend struct {
	MessageID string
	To        string
	Subject   string
	Raw       string
	// GmailID is the ID the stand-in gave the message.
	GmailID string
}

// GmailAPI is the stand-in. HoldNextSend makes the next send record the message as sent and then wait without an
// answer, as a provider whose answer is lost: the caller's connection then ends.
type GmailAPI struct {
	server   *httptest.Server
	mutex    sync.Mutex
	sends    []GmailSend
	holdNext bool
	held     chan struct{}
	release  chan struct{}
}

// StartGmailAPI starts the stand-in.
func StartGmailAPI() *GmailAPI {
	api := &GmailAPI{held: make(chan struct{}, 8), release: make(chan struct{})}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	return api
}

// Endpoint is the Gmail API endpoint of the stand-in, the welcome-gmail connection's endpoint setting.
func (api *GmailAPI) Endpoint() string { return api.server.URL + "/gmail/v1" }

// Stop releases a held send and stops the stand-in.
func (api *GmailAPI) Stop() {
	api.mutex.Lock()
	select {
	case <-api.release:
	default:
		close(api.release)
	}
	api.mutex.Unlock()
	api.server.CloseClientConnections()
	api.server.Close()
}

// HoldNextSend holds the answer to the next send; Held signals when a send is held.
func (api *GmailAPI) HoldNextSend() {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.holdNext = true
}

// Held is signalled each time a send is held.
func (api *GmailAPI) Held() <-chan struct{} { return api.held }

// Sends are the messages received, oldest first.
func (api *GmailAPI) Sends() []GmailSend {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return append([]GmailSend(nil), api.sends...)
}

var (
	messageIDHeader = regexp.MustCompile(`(?mi)^Message-ID: *(.*?)\r?$`)
	toHeader        = regexp.MustCompile(`(?mi)^To: *(.*?)\r?$`)
	subjectHeader   = regexp.MustCompile(`(?mi)^Subject: *(.*?)\r?$`)
	searchMessageID = regexp.MustCompile(`rfc822msgid:(\S+)`)
)

func headerOf(pattern *regexp.Regexp, raw string) string {
	if match := pattern.FindStringSubmatch(raw); match != nil {
		return match[1]
	}
	return ""
}

func (api *GmailAPI) serve(response http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/users/me/messages/send"):
		var body struct {
			Raw string `json:"raw"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(body.Raw, "="))
		if err != nil {
			decoded, _ = base64.URLEncoding.DecodeString(body.Raw)
		}
		raw := string(decoded)
		api.mutex.Lock()
		send := GmailSend{MessageID: headerOf(messageIDHeader, raw), To: headerOf(toHeader, raw), Subject: headerOf(subjectHeader, raw), Raw: raw, GmailID: fmt.Sprintf("gmail-%d", len(api.sends)+1)}
		api.sends = append(api.sends, send)
		hold := api.holdNext
		api.holdNext = false
		api.mutex.Unlock()
		if hold {
			api.held <- struct{}{}
			select {
			case <-api.release:
			case <-request.Context().Done():
			}
			return
		}
		writeJSON(response, map[string]any{"id": send.GmailID, "threadId": "thread-" + send.GmailID, "labelIds": []string{"SENT"}})
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/users/me/messages"):
		wanted := ""
		if match := searchMessageID.FindStringSubmatch(request.URL.Query().Get("q")); match != nil {
			wanted = strings.Trim(match[1], "<>")
		}
		messages := []any{}
		api.mutex.Lock()
		for _, send := range api.sends {
			if wanted != "" && strings.Trim(send.MessageID, "<>") == wanted {
				messages = append(messages, map[string]any{"id": send.GmailID, "threadId": "thread-" + send.GmailID})
			}
		}
		api.mutex.Unlock()
		writeJSON(response, map[string]any{"messages": messages, "resultSizeEstimate": len(messages)})
	default:
		response.WriteHeader(http.StatusNotFound)
		writeJSON(response, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
	}
}
