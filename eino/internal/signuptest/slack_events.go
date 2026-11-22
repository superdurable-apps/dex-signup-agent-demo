package signuptest

// Slack's side of the reply Trigger. Reviewer replies arrive as Events API envelopes on a Socket Mode stand-in: a
// local server that answers apps.connections.open with its own WebSocket URL and serves a real Socket Mode
// WebSocket, so the application's own runner, durable inbox and target handle them. The Go Slack connector has no
// public dialer option; its public hooks are the connection's endpoint setting and the URL that
// apps.connections.open returns. This is the port of test/support/slack-events.ts.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ReviewChannelID is the review channel of the tests.
const ReviewChannelID = "CREVIEWS01"

// AppBotUserID is this app's own bot user.
const AppBotUserID = "UBOT0000001"

// slackPingInterval is how often the stand-in pings each connection, as Slack does, so the runner's read deadline
// never ends a quiet connection.
const slackPingInterval = 10 * time.Second

type socket struct {
	connection *websocket.Conn
	writes     sync.Mutex
	closed     chan struct{}
}

func (current *socket) write(value any) error {
	current.writes.Lock()
	defer current.writes.Unlock()
	return current.connection.WriteJSON(value)
}

// SlackEvents is Slack's side of Socket Mode: envelopes go to the open connection, or wait for the next one, as Slack
// holds them. It also serves the Web API calls of the real Slack connector, chat.postMessage and
// conversations.replies, for the tests that run the tools on the real connector instead of its mock.
type SlackEvents struct {
	server       *httptest.Server
	mutex        sync.Mutex
	acknowledged []string
	current      *socket
	pending      []map[string]any
	envelopes    int
	events       int
	connections  int
	posts        []SlackPost
}

// SlackPost is one chat.postMessage request the stand-in received.
type SlackPost struct {
	Channel     string
	ThreadTS    string
	Text        string
	ClientMsgID string
	// TS is the timestamp the stand-in gave the message.
	TS string
}

// Posts are the chat.postMessage requests, oldest first.
func (events *SlackEvents) Posts() []SlackPost {
	events.mutex.Lock()
	defer events.mutex.Unlock()
	return append([]SlackPost(nil), events.posts...)
}

// postMessage records a post and answers it with a new timestamp: root posts get the next of 1720000000.000100,
// 1720000100.000100, ..., replies their root's seconds and a counter.
func (events *SlackEvents) postMessage(response http.ResponseWriter, request *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(request.Body).Decode(&body)
	text, _ := body["text"].(string)
	channel, _ := body["channel"].(string)
	threadTS, _ := body["thread_ts"].(string)
	clientMsgID, _ := body["client_msg_id"].(string)
	events.mutex.Lock()
	ts := fmt.Sprintf("%d.000100", 1720000000+100*len(events.posts))
	if threadTS != "" {
		ts = fmt.Sprintf("%s.%06d", strings.Split(threadTS, ".")[0], 900+len(events.posts))
	}
	events.posts = append(events.posts, SlackPost{Channel: channel, ThreadTS: threadTS, Text: text, ClientMsgID: clientMsgID, TS: ts})
	events.mutex.Unlock()
	message := map[string]any{"ts": ts, "text": text, "user": AppBotUserID}
	if threadTS != "" {
		message["thread_ts"] = threadTS
	}
	writeJSON(response, map[string]any{"ok": true, "channel": channel, "ts": ts, "message": message})
}

// replies answers conversations.replies with the posts of the thread, so a read-back finds a post.
func (events *SlackEvents) replies(response http.ResponseWriter, request *http.Request) {
	threadTS := request.URL.Query().Get("ts")
	events.mutex.Lock()
	messages := []any{}
	for _, post := range events.posts {
		if post.TS == threadTS || post.ThreadTS == threadTS {
			messages = append(messages, map[string]any{"ts": post.TS, "thread_ts": threadTS, "text": post.Text, "user": AppBotUserID})
		}
	}
	events.mutex.Unlock()
	writeJSON(response, map[string]any{"ok": true, "messages": messages, "has_more": false})
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}

// StartSlackEvents starts the stand-in.
func StartSlackEvents() *SlackEvents {
	events := &SlackEvents{}
	upgrader := websocket.Upgrader{}
	events.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/apps.connections.open":
			response.Header().Set("content-type", "application/json")
			_, _ = fmt.Fprintf(response, `{"ok":true,"url":"ws://%s/socket"}`, request.Host)
		case "/socket":
			events.serveSocket(upgrader, response, request)
		case "/chat.postMessage":
			events.postMessage(response, request)
		case "/conversations.replies":
			events.replies(response, request)
		default:
			response.Header().Set("content-type", "application/json")
			_, _ = fmt.Fprint(response, `{"ok":false,"error":"unknown_method"}`)
		}
	}))
	return events
}

// Endpoint is the Slack Web API endpoint of the stand-in, the review-slack connection's endpoint setting.
func (events *SlackEvents) Endpoint() string { return events.server.URL }

// Stop stops the stand-in.
func (events *SlackEvents) Stop() {
	events.mutex.Lock()
	current := events.current
	events.mutex.Unlock()
	if current != nil {
		_ = current.connection.Close()
	}
	events.server.CloseClientConnections()
	events.server.Close()
}

// Acknowledged is the number of envelopes the runner acknowledged.
func (events *SlackEvents) Acknowledged() int {
	events.mutex.Lock()
	defer events.mutex.Unlock()
	return len(events.acknowledged)
}

// Connections is the number of Socket Mode connections the runner opened.
func (events *SlackEvents) Connections() int {
	events.mutex.Lock()
	defer events.mutex.Unlock()
	return events.connections
}

func (events *SlackEvents) serveSocket(upgrader websocket.Upgrader, response http.ResponseWriter, request *http.Request) {
	connection, err := upgrader.Upgrade(response, request, nil)
	if err != nil {
		return
	}
	current := &socket{connection: connection, closed: make(chan struct{})}
	defer func() {
		close(current.closed)
		_ = connection.Close()
		events.mutex.Lock()
		if events.current == current {
			events.current = nil
		}
		events.mutex.Unlock()
	}()
	if err := current.write(map[string]any{"type": "hello", "num_connections": 1}); err != nil {
		return
	}
	events.mutex.Lock()
	events.current = current
	events.connections++
	pending := events.pending
	events.pending = nil
	events.mutex.Unlock()
	for _, envelope := range pending {
		if err := current.write(envelope); err != nil {
			events.requeue(envelope)
		}
	}
	go func() {
		ticker := time.NewTicker(slackPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-current.closed:
				return
			case <-ticker.C:
				current.writes.Lock()
				err := connection.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second))
				current.writes.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	for {
		var acknowledgement map[string]any
		if err := connection.ReadJSON(&acknowledgement); err != nil {
			return
		}
		if id, ok := acknowledgement["envelope_id"].(string); ok {
			events.mutex.Lock()
			events.acknowledged = append(events.acknowledged, id)
			events.mutex.Unlock()
		}
	}
}

func (events *SlackEvents) requeue(envelope map[string]any) {
	events.mutex.Lock()
	defer events.mutex.Unlock()
	events.pending = append(events.pending, envelope)
}

// Push sends one message event in the review channel, unless message names another, and returns its event ID; an
// empty eventID gets a new one.
func (events *SlackEvents) Push(message map[string]any, eventID string) string {
	events.mutex.Lock()
	events.envelopes++
	if eventID == "" {
		events.events++
		eventID = fmt.Sprintf("Ev%d", events.events)
	}
	event := map[string]any{"type": "message", "channel": ReviewChannelID}
	for key, value := range message {
		event[key] = value
	}
	envelope := map[string]any{
		"envelope_id": fmt.Sprintf("envelope-%d", events.envelopes),
		"type":        "events_api",
		"payload":     map[string]any{"event_id": eventID, "event_time": 1760227200, "team_id": "T1", "event": event},
	}
	current := events.current
	if current == nil {
		events.pending = append(events.pending, envelope)
	}
	events.mutex.Unlock()
	if current != nil {
		if err := current.write(envelope); err != nil {
			events.requeue(envelope)
		}
	}
	return eventID
}

// ReviewThread is a review thread: people reply in it through SlackEvents.
type ReviewThread struct {
	RootTS  string
	events  *SlackEvents
	mutex   sync.Mutex
	seconds int64
}

// NewReviewThread is the thread rooted at rootTS.
func NewReviewThread(events *SlackEvents, rootTS string) *ReviewThread {
	var seconds int64
	_, _ = fmt.Sscanf(strings.Split(rootTS, ".")[0], "%d", &seconds)
	return &ReviewThread{RootTS: rootTS, events: events, seconds: seconds}
}

// SentReply is one reply sent in a thread.
type SentReply struct {
	TS      string
	EventID string
	Message map[string]any
}

// Reply sends a reply in this thread; extra adds or overrides event fields, such as bot_id.
func (thread *ReviewThread) Reply(userID, text string, extra map[string]any) SentReply {
	thread.mutex.Lock()
	thread.seconds++
	ts := fmt.Sprintf("%d.000200", thread.seconds)
	thread.mutex.Unlock()
	message := map[string]any{"user": userID, "text": text, "ts": ts, "thread_ts": thread.RootTS}
	for key, value := range extra {
		message[key] = value
	}
	return SentReply{TS: ts, EventID: thread.events.Push(message, ""), Message: message}
}

// ConnectionsFile writes a Dex Web connections.json in directory with the review-slack connection, whose endpoint is
// slackEndpoint and whose credential has an app_token, so the reply runner uses Socket Mode, and, when gmailEndpoint
// is not empty, the welcome-gmail connection. It returns the file's path; the durable inboxes live beside it.
func ConnectionsFile(directory, slackEndpoint, gmailEndpoint string) (string, error) {
	connections := []map[string]any{{
		"connectorId": "slack", "authMethodId": "oauth2", "modulePath": "slack", "provider": "slack", "connectionName": "review-slack",
		"configuration": map[string]any{"endpoint": slackEndpoint},
		"credentials":   map[string]any{"bot_token": "xoxb-test", "user_token": "xoxp-test", "app_token": "xapp-test"},
	}}
	if gmailEndpoint != "" {
		connections = append(connections, map[string]any{
			"connectorId": "gmail", "authMethodId": "google-oauth", "modulePath": "gmail", "provider": "google", "connectionName": "welcome-gmail",
			"configuration": map[string]any{"endpoint": gmailEndpoint},
			"credentials":   map[string]any{"auth_method": "google-oauth", "access_token": "ya29.test", "primary_email": "hello@example.com"},
		})
	}
	contents, err := json.MarshalIndent(map[string]any{
		"schemaVersion": "connectors.dex.dev/local-connections/v1alpha1", "connections": connections, "triggerBindings": []any{},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, "connections.json")
	return path, os.WriteFile(path, contents, 0o600)
}
