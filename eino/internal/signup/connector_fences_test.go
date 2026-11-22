package signup_test

// The released Go Slack and Gmail connector operations with the real Connector SDK fence, over a fake provider
// transport and a fake tool Step: sdkgo's singledispatchtest.Context, whose attempts share one heartbeat checkpoint
// that the next attempt reads, as the session's tool Step keeps it. Each attempt is one attempt of that Step. Port of
// test/connector-fences.test.ts.

import (
	"bytes"
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
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
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
	isPost      = func(request recordedRequest) bool { return strings.HasSuffix(request.url, "/chat.postMessage") }
	isGmailSend = func(request recordedRequest) bool {
		return request.method == http.MethodPost && strings.Contains(request.url, "/messages/send")
	}
	isSentMailSearch = func(request recordedRequest) bool {
		target, _ := url.Parse(request.url)
		return request.method == http.MethodGet && strings.HasSuffix(target.Path, "/users/me/messages")
	}
	isHeaderRead = func(request recordedRequest) bool {
		target, _ := url.Parse(request.url)
		return request.method == http.MethodGet && strings.Contains(target.Path, "/users/me/messages/") && target.Query().Get("format") == "metadata"
	}
)

// gmailAnswer answers the Gmail connector as Gmail does over mailbox: send answers a messages.send request with the
// message it carries; every other request reads the mailbox.
func gmailAnswer(mailbox *signuptest.GmailMailbox, send func(raw string) (*http.Response, error)) func(recordedRequest) (*http.Response, error) {
	return func(request recordedRequest) (*http.Response, error) {
		if isGmailSend(request) {
			raw, err := signuptest.DecodeSendBody(request.body)
			if err != nil {
				return jsonResponse(400, map[string]any{"error": map[string]any{"code": 400}}), nil
			}
			return send(raw)
		}
		target, err := url.Parse(request.url)
		if err != nil {
			return nil, err
		}
		status, body := mailbox.Answer(request.method, target)
		return jsonResponse(status, body), nil
	}
}

// accept is Gmail accepting a send: the message is in Sent Mail, under Gmail's own Message-ID.
func accept(mailbox *signuptest.GmailMailbox) func(string) (*http.Response, error) {
	return func(raw string) (*http.Response, error) {
		return jsonResponse(200, signuptest.SendAnswer(mailbox.Send(raw))), nil
	}
}

// requireReadBackByDexMessageID checks that every Sent Mail search listed candidates to the recipient, never by
// Message-ID, and that Gmail had replaced the Message-ID of each Sent copy, so only X-Dex-Message-ID could match.
func requireReadBackByDexMessageID(t *testing.T, mailbox *signuptest.GmailMailbox) {
	t.Helper()
	for _, query := range mailbox.Searches() {
		if query != "in:sent newer_than:2d to:ada@example.com" {
			t.Fatalf("a Sent Mail search is %q", query)
		}
	}
	for _, send := range mailbox.Sends() {
		if send.MessageID == "" || send.DexMessageID != send.MessageID || send.SentMessageID == send.MessageID {
			t.Fatalf("the Sent copy does not model Gmail's Message-ID rewrite: %+v", send)
		}
	}
}

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

func TestGmailReadsSentMailBackByXDexMessageIDBeforeItWouldResend(t *testing.T) {
	mailbox := &signuptest.GmailMailbox{}
	late := ""
	provider := &fakeProvider{}
	provider.answer = gmailAnswer(mailbox, func(raw string) (*http.Response, error) {
		late = raw
		return jsonResponse(500, map[string]any{"error": map[string]any{"code": 500}}), nil
	})
	send := gmailOperations(t, provider).SendMessage()
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	if kind := kindOf(attemptOutcome(t, step, send, gmailConnection, emailInput)); kind != "retry" {
		t.Fatalf("first attempt = %s", kind)
	}
	// The 500 lost the answer, but the message reached Sent Mail after the attempt's read-back, with Gmail's own
	// Message-ID: the next attempt finds it by its X-Dex-Message-ID.
	mailbox.Send(late)
	step.AdvanceAttempt()
	second := attemptOutcome(t, step, send, gmailConnection, emailInput)
	if kindOf(second) != "sent" {
		t.Fatalf("second attempt = %s (%v)", kindOf(second), second.Failure)
	}
	var value gmail.SendMessageOutput
	_ = json.Unmarshal(second.Value, &value)
	if value.MessageID != "gmail-1" {
		t.Fatalf("message ID = %q", value.MessageID)
	}
	if sends := provider.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}
	if reads := provider.matching(isHeaderRead); len(reads) == 0 {
		t.Fatal("no candidate's headers were read")
	}
	requireReadBackByDexMessageID(t, mailbox)
}

// A Worker killed after Gmail accepted the send: Dex kept only the fence's claim as the Step's checkpoint. Attempt 2,
// on a new Worker, finds the Sent copy by its X-Dex-Message-ID, since Gmail replaced its Message-ID, and sends no
// second email.
func TestAWorkerKilledAfterGmailAcceptedTheSendSendsNoSecondEmail(t *testing.T) {
	mailbox := &signuptest.GmailMailbox{}
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	var claim []byte
	killed := &fakeProvider{}
	killed.answer = func(request recordedRequest) (*http.Response, error) {
		if !isGmailSend(request) {
			return nil, errors.New("the Worker is gone")
		}
		frames := step.Heartbeats()
		claim = frames[len(frames)-1]
		raw, err := signuptest.DecodeSendBody(request.body)
		if err != nil {
			t.Error(err)
		}
		mailbox.Send(raw)
		return nil, errors.New("the Worker was killed after Gmail accepted the send")
	}
	// The attempt's outcome is lost with its Worker.
	_, _ = signup.RunConnectorAttempt(step, gmailOperations(t, killed).SendMessage(), gmailConnection, emailInput, 3)
	if !strings.Contains(string(claim), `"state":"claimed"`) {
		t.Fatalf("the checkpoint at the send is not the fence's claim: %s", claim)
	}
	retried := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	retried.AdvanceAttempt()
	if err := retried.SetPersistedCheckpoint(json.RawMessage(claim)); err != nil {
		t.Fatal(err)
	}
	alive := &fakeProvider{}
	alive.answer = gmailAnswer(mailbox, accept(mailbox))
	second := attemptOutcome(t, retried, gmailOperations(t, alive).SendMessage(), gmailConnection, emailInput)
	if kindOf(second) != "sent" {
		t.Fatalf("attempt 2 = %s (%v)", kindOf(second), second.Failure)
	}
	var value gmail.SendMessageOutput
	_ = json.Unmarshal(second.Value, &value)
	if value.MessageID != "gmail-1" {
		t.Fatalf("message ID = %q", value.MessageID)
	}
	if sends := alive.matching(isGmailSend); len(sends) != 0 {
		t.Fatalf("attempt 2 sent the email again: %d sends", len(sends))
	}
	if sends := mailbox.Sends(); len(sends) != 1 {
		t.Fatalf("Sent Mail holds %d copies", len(sends))
	}
	requireReadBackByDexMessageID(t, mailbox)
}

func rawHeaderOf(t *testing.T, request recordedRequest, name string) string {
	t.Helper()
	raw, err := signuptest.DecodeSendBody(request.body)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `: (.*?)\r?$`).FindStringSubmatch(raw)
	if match == nil {
		return ""
	}
	return match[1]
}

func TestGmailSendsAtMost3TimesSearchingSentMailBeforeEachResend(t *testing.T) {
	mailbox := &signuptest.GmailMailbox{}
	provider := &fakeProvider{}
	provider.answer = gmailAnswer(mailbox, func(string) (*http.Response, error) {
		return jsonResponse(500, map[string]any{"error": map[string]any{"code": 500}}), nil
	})
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
		ids[rawHeaderOf(t, request, "Message-ID")+" "+rawHeaderOf(t, request, "X-Dex-Message-ID")] = true
	}
	for id := range ids {
		if fields := strings.Fields(id); len(ids) != 1 || len(fields) != 2 || fields[0] != fields[1] {
			t.Fatalf("every resend carries the same Message-ID, repeated in X-Dex-Message-ID: %v", ids)
		}
	}
	if len(provider.matching(isSentMailSearch)) == 0 {
		t.Fatal("no resend searched Sent Mail first")
	}
	requireReadBackByDexMessageID(t, mailbox)
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
// in the Step's checkpoint, so the email's retry still finds its own marker and reads the message back by its Gmail
// ID. The app makes one fenced write per Step either way, which is why send_welcome_email posts no note.
func TestASecondFencedWriteInOneToolStepKeepsTheFirstWritesMarkerInGo(t *testing.T) {
	mailbox := &signuptest.GmailMailbox{}
	gmailProvider := &fakeProvider{}
	gmailProvider.answer = gmailAnswer(mailbox, accept(mailbox))
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
	// The retry read the message back by the Gmail ID its marker saved, with no Sent Mail search.
	if reads := mailbox.HeaderReads(); len(reads) != 1 || reads[0] != "gmail-1" {
		t.Fatalf("the email's retry read %v", reads)
	}
	if searches := gmailProvider.matching(isSentMailSearch); len(searches) != 0 {
		t.Fatalf("the email's retry searched Sent Mail %d times", len(searches))
	}
}
