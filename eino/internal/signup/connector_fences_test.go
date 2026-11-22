package signup_test

// The released Go Slack and Gmail connector operations with the real Connector SDK fence, over a fake provider
// transport and a fake tool Step: sdkgo's singledispatchtest.Context, whose attempts share one heartbeat checkpoint
// that the next attempt reads, as the session's tool Step keeps it. Each attempt is one attempt of that Step. Port of
// test/connector-fences.test.ts.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/singledispatchtest"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

var (
	slackConnection = sdkgo.ConnectionRef{Provider: "slack", Name: "review-slack"}
	gmailConnection = sdkgo.ConnectionRef{Provider: "google", Name: "welcome-gmail"}
	replyInput      = slack.PostThreadReplyInput{ChannelID: "C0REVIEW", ThreadTimestamp: "1710000100.000100", Text: "Approved."}
	emailInput      = gmail.SendMessageInput{To: []string{"ada@example.com"}, Subject: "Welcome to Dex Cloud", TextBody: "Hi Ada"}
)

// fakeProvider records each request and answers it with answer.
type fakeProvider struct {
	mutex    sync.Mutex
	requests []recordedRequest
	answer   func(request recordedRequest) (*http.Response, error)
}

type recordedRequest struct {
	method string
	url    string
	body   []byte
}

func (provider *fakeProvider) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	recorded := recordedRequest{method: request.Method, url: request.URL.String(), body: body}
	provider.mutex.Lock()
	provider.requests = append(provider.requests, recorded)
	provider.mutex.Unlock()
	return provider.answer(recorded)
}

func (provider *fakeProvider) matching(matches func(recordedRequest) bool) []recordedRequest {
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	var found []recordedRequest
	for _, request := range provider.requests {
		if matches(request) {
			found = append(found, request)
		}
	}
	return found
}

func jsonResponse(status int, body any) *http.Response {
	encoded, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(encoded))}
}

func slackOperations(t *testing.T, provider *fakeProvider) slack.Operations {
	t.Helper()
	client, err := slack.New(slack.Config{Endpoint: "https://slack.example/api"}, sdkgo.StaticCredentialProvider[slack.Credentials]{
		slackConnection: {BotToken: sdkgo.NewSecretString("xoxb-test"), UserToken: sdkgo.NewSecretString("xoxp-test")},
	}, slack.WithHTTPClient(&http.Client{Transport: provider}))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := slack.NewConnection(client, slackConnection)
	if err != nil {
		t.Fatal(err)
	}
	return connection.Operations()
}

func gmailOperations(t *testing.T, provider *fakeProvider) gmail.Operations {
	t.Helper()
	client, err := gmail.New(gmail.Config{Endpoint: "https://gmail.example/gmail/v1"}, sdkgo.StaticCredentialProvider[gmail.Credentials]{
		gmailConnection: {AuthMethodID: "google-oauth", AccessToken: sdkgo.NewSecretString("ya29.test"), PrimaryEmail: "hello@example.com"},
	}, gmail.WithHTTPClient(&http.Client{Transport: provider}))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := gmail.NewConnection(client, gmailConnection)
	if err != nil {
		t.Fatal(err)
	}
	return connection.Operations()
}

// attemptOutcome runs one attempt of operation in the Step context, with the call identity, idempotency key and send
// cap the session's tool Step gives it, and reads it as a tool does.
func attemptOutcome[IN, OUT any](t *testing.T, step *singledispatchtest.Context, operation sdkgo.Mutation[IN, OUT], connection sdkgo.ConnectionRef, input IN) signup.ConnectorOutcome {
	t.Helper()
	outcome, err := signup.RunConnectorAttempt(step, operation, connection, input, 3)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func kindOf(outcome signup.ConnectorOutcome) string {
	if outcome.Kind == "branch" {
		return string(outcome.Branch)
	}
	return outcome.Kind
}

var (
	isPost           = func(request recordedRequest) bool { return strings.HasSuffix(request.url, "/chat.postMessage") }
	isGmailSend      = func(request recordedRequest) bool { return request.method == http.MethodPost && strings.Contains(request.url, "/messages/send") }
	isSentMailSearch = func(request recordedRequest) bool {
		decoded, _ := url.QueryUnescape(request.url)
		return request.method == http.MethodGet && strings.Contains(decoded, "rfc822msgid:")
	}
)

func TestSlackPostThreadReplySendsAtMost3TimesWithOneClientMsgIDWhenEachOutcomeIsUnknown(t *testing.T) {
	provider := &fakeProvider{answer: func(recordedRequest) (*http.Response, error) {
		return nil, errors.New("connection reset after the request was written")
	}}
	reply := slackOperations(t, provider).PostThreadReply()
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	var kinds []string
	for index := 0; index < 4; index++ {
		if index > 0 {
			step.AdvanceAttempt()
		}
		kinds = append(kinds, kindOf(attemptOutcome(t, step, reply, slackConnection, replyInput)))
	}
	// Two retries resend; the third send selects uncertain at the cap; a fourth attempt cannot identify a post to read.
	if strings.Join(kinds, ",") != "retry,retry,uncertain,uncertain" {
		t.Fatalf("kinds = %v", kinds)
	}
	posts := provider.matching(isPost)
	if len(posts) != 3 {
		t.Fatalf("posts = %d", len(posts))
	}
	ids := map[string]bool{}
	for _, post := range posts {
		var body map[string]any
		_ = json.Unmarshal(post.body, &body)
		ids[body["client_msg_id"].(string)] = true
	}
	if len(ids) != 1 {
		t.Fatalf("client_msg_ids = %v", ids)
	}
}

func TestSlackReadsBackAPostWhoseTSWasCheckpointedAndNeverResendsIt(t *testing.T) {
	isReplyReadable := false
	provider := &fakeProvider{}
	provider.answer = func(request recordedRequest) (*http.Response, error) {
		if isPost(request) {
			// Accepted, but the answer lacks the channel: the connector checkpoints the ts and reads the post back.
			return jsonResponse(200, map[string]any{"ok": true, "ts": "1710000100.000900"}), nil
		}
		if isReplyReadable {
			return jsonResponse(200, map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000100.000900", "thread_ts": replyInput.ThreadTimestamp, "text": replyInput.Text, "user": "UBOT"}}}), nil
		}
		return jsonResponse(500, map[string]any{"ok": false, "error": "internal_error"}), nil
	}
	reply := slackOperations(t, provider).PostThreadReply()
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	if kind := kindOf(attemptOutcome(t, step, reply, slackConnection, replyInput)); kind != "retry" {
		t.Fatalf("first attempt = %s", kind)
	}
	isReplyReadable = true
	step.AdvanceAttempt()
	second := attemptOutcome(t, step, reply, slackConnection, replyInput)
	if kindOf(second) != "sent" {
		t.Fatalf("second attempt = %s", kindOf(second))
	}
	if posts := provider.matching(isPost); len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	if signup.ConfirmationStateOf(second) != signup.ConfirmationPosted {
		t.Fatal("the read-back post is not posted")
	}
}

func TestASlackPostSlackRejectsIsADefiniteFailureNeverResentAndANoteThatWasNotPosted(t *testing.T) {
	provider := &fakeProvider{answer: func(recordedRequest) (*http.Response, error) {
		return jsonResponse(200, map[string]any{"ok": false, "error": "not_in_channel"}), nil
	}}
	reply := slackOperations(t, provider).PostThreadReply()
	result := attemptOutcome(t, singledispatchtest.NewContext("signup-review-acct1", "step-1"), reply, slackConnection, replyInput)
	if kindOf(result) != "providerRejected" {
		t.Fatalf("result = %s", kindOf(result))
	}
	if posts := provider.matching(isPost); len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	if signup.ConfirmationStateOf(result) != signup.ConfirmationNotPosted {
		t.Fatal("a rejected note is not notPosted")
	}
}

func TestASlackNoteWhoseAnswerWasLostIsUnknownNotNotPosted(t *testing.T) {
	provider := &fakeProvider{answer: func(recordedRequest) (*http.Response, error) {
		return jsonResponse(500, map[string]any{"ok": false, "error": "internal_error"}), nil
	}}
	reply := slackOperations(t, provider).PostThreadReply()
	result := attemptOutcome(t, singledispatchtest.NewContext("signup-review-acct1", "step-1"), reply, slackConnection, replyInput)
	if result.Kind != "retry" {
		t.Fatalf("result = %s", kindOf(result))
	}
	if signup.ConfirmationStateOf(result) != signup.ConfirmationUnknown {
		t.Fatal("a lost answer is not unknown")
	}
}

func TestGmailReadsBackSentMailByTheSharedMessageIDBeforeItWouldResend(t *testing.T) {
	isInSentMail := false
	provider := &fakeProvider{}
	provider.answer = func(request recordedRequest) (*http.Response, error) {
		switch {
		case isGmailSend(request):
			return jsonResponse(500, map[string]any{"error": map[string]any{"code": 500}}), nil
		case isSentMailSearch(request):
			if isInSentMail {
				return jsonResponse(200, map[string]any{"messages": []any{map[string]any{"id": "m-1", "threadId": "t-1"}}}), nil
			}
			return jsonResponse(200, map[string]any{}), nil
		default:
			return jsonResponse(404, map[string]any{}), nil
		}
	}
	send := gmailOperations(t, provider).SendMessage()
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	if kind := kindOf(attemptOutcome(t, step, send, gmailConnection, emailInput)); kind != "retry" {
		t.Fatalf("first attempt = %s", kind)
	}
	// The 500 lost the answer, but the message reached Sent Mail: the next attempt finds it there.
	isInSentMail = true
	step.AdvanceAttempt()
	second := attemptOutcome(t, step, send, gmailConnection, emailInput)
	if kindOf(second) != "sent" {
		t.Fatalf("second attempt = %s (%v)", kindOf(second), second.Failure)
	}
	var value gmail.SendMessageOutput
	_ = json.Unmarshal(second.Value, &value)
	if value.MessageID != "m-1" {
		t.Fatalf("message ID = %q", value.MessageID)
	}
	if sends := provider.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}
	searched := map[string]bool{}
	for _, request := range provider.matching(isSentMailSearch) {
		decoded, _ := url.QueryUnescape(request.url)
		searched[regexp.MustCompile(`rfc822msgid:(\S+)`).FindStringSubmatch(decoded)[1]] = true
	}
	if len(searched) != 1 {
		t.Fatalf("every search looks for the one Message-ID of the call: %v", searched)
	}
}

func rawMessageIDOf(t *testing.T, request recordedRequest) string {
	t.Helper()
	var body struct {
		Raw string `json:"raw"`
	}
	_ = json.Unmarshal(request.body, &body)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(body.Raw, "="))
	if err != nil {
		raw, _ = base64.URLEncoding.DecodeString(body.Raw)
	}
	match := regexp.MustCompile(`(?m)^Message-ID: (.*?)\r?$`).FindStringSubmatch(string(raw))
	if match == nil {
		return ""
	}
	return match[1]
}

func TestGmailSendsAtMost3TimesSearchingSentMailBeforeEachResend(t *testing.T) {
	provider := &fakeProvider{answer: func(request recordedRequest) (*http.Response, error) {
		switch {
		case isGmailSend(request):
			return jsonResponse(500, map[string]any{"error": map[string]any{"code": 500}}), nil
		case isSentMailSearch(request):
			return jsonResponse(200, map[string]any{}), nil
		default:
			return jsonResponse(404, map[string]any{}), nil
		}
	}}
	send := gmailOperations(t, provider).SendMessage()
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	var kinds []string
	for index := 0; index < 4; index++ {
		if index > 0 {
			step.AdvanceAttempt()
		}
		kinds = append(kinds, kindOf(attemptOutcome(t, step, send, gmailConnection, emailInput)))
	}
	// The Go Gmail connector selects uncertain once the third send's outcome stays unknown, as Slack does; a later
	// attempt only searches Sent Mail again. Either way Dex records outcome_unknown. (The TypeScript connector keeps
	// asking for a retry that only searches, until the Step's attempts run out.)
	if strings.Join(kinds, ",") != "retry,retry,uncertain,uncertain" {
		t.Fatalf("kinds = %v", kinds)
	}
	sends := provider.matching(isGmailSend)
	if len(sends) != 3 {
		t.Fatalf("sends = %d", len(sends))
	}
	ids := map[string]bool{}
	for _, request := range sends {
		ids[rawMessageIDOf(t, request)] = true
	}
	if len(ids) != 1 || ids[""] {
		t.Fatalf("every resend carries the same Message-ID: %v", ids)
	}
}

func TestASendGmailRejectsIsADefiniteFailureAndIsNeverResent(t *testing.T) {
	provider := &fakeProvider{answer: func(request recordedRequest) (*http.Response, error) {
		if isGmailSend(request) {
			return jsonResponse(400, map[string]any{"error": map[string]any{"code": 400, "message": "Invalid To header"}}), nil
		}
		return jsonResponse(404, map[string]any{}), nil
	}}
	send := gmailOperations(t, provider).SendMessage()
	result := attemptOutcome(t, singledispatchtest.NewContext("signup-review-acct1", "step-1"), send, gmailConnection, emailInput)
	if kindOf(result) != "providerRejected" {
		t.Fatalf("result = %s", kindOf(result))
	}
	if sends := provider.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}
}

// dex-connectors-library#870: one fenced connector write per Step. In the TypeScript SDK a second fenced write takes
// over the Step's one checkpoint, so the first write's retry fails closed. The Go fence keeps one marker per call ID
// in the Step's checkpoint, so the email's retry still finds its own marker and reads Sent Mail back. The app makes
// one fenced write per Step either way, which is why send_welcome_email posts no note.
func TestASecondFencedWriteInOneToolStepKeepsTheFirstWritesMarkerInGo(t *testing.T) {
	gmailProvider := &fakeProvider{answer: func(request recordedRequest) (*http.Response, error) {
		if isGmailSend(request) {
			return jsonResponse(200, map[string]any{"id": "m-1", "threadId": "t-1"}), nil
		}
		if isSentMailSearch(request) {
			return jsonResponse(200, map[string]any{"messages": []any{map[string]any{"id": "m-1", "threadId": "t-1"}}}), nil
		}
		return jsonResponse(404, map[string]any{}), nil
	}}
	slackProvider := &fakeProvider{answer: func(recordedRequest) (*http.Response, error) {
		return jsonResponse(200, map[string]any{"ok": true, "channel": replyInput.ChannelID, "ts": "1710000100.000900", "message": map[string]any{"ts": "1710000100.000900", "text": replyInput.Text}}), nil
	}}
	send := gmailOperations(t, gmailProvider).SendMessage()
	reply := slackOperations(t, slackProvider).PostThreadReply()
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	if kind := kindOf(attemptOutcome(t, step, send, gmailConnection, emailInput)); kind != "sent" {
		t.Fatalf("send = %s", kind)
	}
	// In the same first attempt the second write still goes out: no checkpoint was read before it.
	if kind := kindOf(attemptOutcome(t, step, reply, slackConnection, replyInput)); kind != "sent" {
		t.Fatalf("reply = %s", kind)
	}
	// Run again, as after a crash: the email's fence reads the Step's checkpoint.
	step.AdvanceAttempt()
	again := attemptOutcome(t, step, send, gmailConnection, emailInput)
	if kind := kindOf(again); kind != "sent" {
		t.Fatalf("the email's retry selects %s", kind)
	}
	if sends := gmailProvider.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("the email was sent again: %d sends", len(sends))
	}
	if searches := gmailProvider.matching(isSentMailSearch); len(searches) != 1 {
		t.Fatalf("the email's retry searched Sent Mail %d times", len(searches))
	}
}
