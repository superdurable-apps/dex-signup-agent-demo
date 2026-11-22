package signup_test

// A connector tool's refusal on a retried attempt of its tool Step, through the Go connector tool source with the real
// Slack and Gmail connectors and fence over a fake transport: after an attempt that may have sent, the call's outcome
// is unknown, never failed, so the model is never told that nothing was sent, and a new call is refused instead of
// posting or sending again. Dex Agents 0.6.14, which the TypeScript harnesses run, records the same when the earlier
// attempt's checkpoint holds a claim or a send. On a retried attempt that finds no checkpoint, the Go harness follows
// sdkgo's fence, which assumes each earlier attempt sent once since Dex may lose the latest checkpoint, and records the
// refusal unknown; Dex Agents 0.6.14 records it failed. The Go harness is stricter there.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/singledispatchtest"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/agents/go/core/connectortools"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
)

// retryHarness is the six connector tools in the Go connector tool source over the real connectors and fake providers.
type retryHarness struct {
	source *connectortools.ConnectorToolSource
	stored *storedMessages
	stub   *signuptest.ReviewAPIStub
	slack  *fakeProvider
	gmail  *fakeProvider
}

func newRetryHarness(t *testing.T) retryHarness {
	t.Helper()
	stub := signuptest.StartReviewAPIStub()
	t.Cleanup(stub.Stop)
	stub.AddPendingAccount(signuptest.PendingAccount{AccountID: "acct1", Email: "ada@example.com", DisplayName: text("Ada")})
	api, err := signup.NewReviewAPIClient(stub.BaseURL(), stub.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	stored := &storedMessages{}
	sessions := signup.NewReviewSessions()
	sessions.Connect(nil, "SignupReviewEinoAgentFlow", stored)
	slackProvider, gmailProvider := &fakeProvider{}, &fakeProvider{}
	slackClient, err := slack.New(slack.Config{Endpoint: "https://slack.example/api"}, sdkgo.StaticCredentialProvider[slack.Credentials]{
		slackConnection: {BotToken: sdkgo.NewSecretString("xoxb-test"), UserToken: sdkgo.NewSecretString("xoxp-test")},
	}, slack.WithHTTPClient(&http.Client{Transport: slackProvider}))
	if err != nil {
		t.Fatal(err)
	}
	slackTools, err := slack.NewConnection(slackClient, slackConnection)
	if err != nil {
		t.Fatal(err)
	}
	gmailClient, err := gmail.New(gmail.Config{Endpoint: "https://gmail.example/gmail/v1"}, sdkgo.StaticCredentialProvider[gmail.Credentials]{
		gmailConnection: {AuthMethodID: "google-oauth", AccessToken: sdkgo.NewSecretString("ya29.test"), PrimaryEmail: "hello@example.com"},
	}, gmail.WithHTTPClient(&http.Client{Transport: gmailProvider}))
	if err != nil {
		t.Fatal(err)
	}
	gmailTools, err := gmail.NewConnection(gmailClient, gmailConnection)
	if err != nil {
		t.Fatal(err)
	}
	source, err := signup.SignupConnectorToolSource(&signup.Services{Sessions: sessions, ReviewAPI: api, SlackChannelID: "CREVIEWS01", HarnessLabel: "eino"}, slackTools, gmailTools)
	if err != nil {
		t.Fatal(err)
	}
	return retryHarness{source: source, stored: stored, stub: stub, slack: slackProvider, gmail: gmailProvider}
}

// toolStep is one tool call's Step as the session runs it: each attempt gets the checkpoint the attempts before it saved.
type toolStep struct {
	harness    retryHarness
	name       string
	callID     string
	step       *singledispatchtest.Context
	checkpoint *string
	attempts   int
}

func (harness retryHarness) toolStep(name, stepExecutionID string) *toolStep {
	return &toolStep{harness: harness, name: name, callID: "call-" + stepExecutionID, step: singledispatchtest.NewContext("signup-review-acct1", stepExecutionID)}
}

func (call *toolStep) attempt(t *testing.T, arguments string) (dexagents.DeferredToolOutcome, error) {
	t.Helper()
	if call.attempts > 0 {
		call.step.AdvanceAttempt()
	}
	call.attempts++
	turn := dexagents.AgentTurnContext{ToolPolicies: signup.SignupConnectorToolPolicies()}
	deferred, err := call.harness.source.DeferredToolCall(turn, dexagents.SourceToolCall{ToolCallID: call.callID, ToolName: call.name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return call.harness.source.ExecuteDeferredToolCall(call.step, dexagents.DeferredToolExecution{
		FlowID: "signup-review-acct1", Attempt: int64(call.step.Attempt()), StepContext: call.step, HeartbeatCheckpoint: call.checkpoint,
		ToolCall:  dexagents.AgentToolCall{ToolCallID: call.callID, ToolName: call.name, Arguments: arguments, ToolSourceReference: deferred.ToolSourceReference},
		Heartbeat: func() error { return nil },
		SaveHeartbeatCheckpoint: func(checkpoint string) error {
			call.checkpoint = &checkpoint
			return nil
		},
	})
}

// storeResult stores a finished call's tool result as the session does: the output, or the fixed text of an unknown
// outcome.
func (harness retryHarness) storeResult(name, output string) {
	sequence := int64(len(harness.stored.messages) + 1)
	harness.stored.messages = append(harness.stored.messages, dexagents.AgentMessage{Sequence: sequence, Role: dexagents.MessageRoleTool, ToolName: &name, ToolOutput: &output})
}

func requireRetry(t *testing.T, outcome dexagents.DeferredToolOutcome, err error) {
	t.Helper()
	var toolError *dexagents.ToolExecutionError
	var unknown *dexagents.ToolOutcomeUnknownError
	if err == nil || errors.As(err, &toolError) || errors.As(err, &unknown) {
		t.Fatalf("the first attempt does not ask for a retry: %#v, %v", outcome, err)
	}
}

func requireUnknown(t *testing.T, outcome dexagents.DeferredToolOutcome, err error, text string) {
	t.Helper()
	var unknown *dexagents.ToolOutcomeUnknownError
	if !errors.As(err, &unknown) || !strings.Contains(unknown.Message, text) {
		t.Fatalf("the outcome is not unknown: %#v, %v", outcome, err)
	}
}

const lostAnswer = "connection reset after the request was written"

func TestASendWelcomeEmailRefusedOnARetryAfterAPossibleSendIsUnknownAndNeverSentAgain(t *testing.T) {
	harness := newRetryHarness(t)
	harness.stub.DecideElsewhere("acct1", "active")
	harness.storeResult("post_review", `{"draft":{"number":1},"email":{"body":"Hi Ada","subject":"Welcome"},"thread":{"ts":"1710000100.000100"}}`)
	harness.gmail.answer = func(request recordedRequest) (*http.Response, error) {
		if isGmailSend(request) {
			return nil, errors.New(lostAnswer)
		}
		return jsonResponse(200, map[string]any{}), nil
	}
	send := harness.toolStep("send_welcome_email", "step-send")
	outcome, err := send.attempt(t, `{}`)
	requireRetry(t, outcome, err)
	if sends := harness.gmail.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}
	// The retry's checks fail before its send: the ReviewAPI is busy for a moment.
	harness.stub.SetFailingGets(1)
	outcome, err = send.attempt(t, `{}`)
	requireUnknown(t, outcome, err, "not sent: ")
	if sends := harness.gmail.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}
	// The admins reply send: a new call reads the unknown outcome and refuses, so the applicant gets one email.
	harness.storeResult("send_welcome_email", dexagents.OutcomeUnknownToolResult)
	outcome, err = harness.toolStep("send_welcome_email", "step-send-again").attempt(t, `{}`)
	var toolError *dexagents.ToolExecutionError
	if !errors.As(err, &toolError) || !strings.HasSuffix(toolError.Message, "refused: the welcome email for draft 1 may already have been sent in this review: an earlier send's outcome stayed unknown after Gmail checked Sent Mail and tried up to 3 times; the admins check Sent Mail") {
		t.Fatalf("the new call is not refused: %#v, %v", outcome, err)
	}
	if sends := harness.gmail.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("the email was sent again: %d sends", len(sends))
	}
}

func TestAPostReviewRefusedOnARetryAfterAPossiblePostIsUnknownAndNeverPostedAgain(t *testing.T) {
	harness := newRetryHarness(t)
	harness.slack.answer = func(request recordedRequest) (*http.Response, error) {
		if isPost(request) {
			return nil, errors.New(lostAnswer)
		}
		return jsonResponse(200, map[string]any{"ok": true, "messages": []any{}}), nil
	}
	review := harness.toolStep("post_review", "step-review")
	outcome, err := review.attempt(t, reviewArguments)
	requireRetry(t, outcome, err)
	harness.stub.SetFailingGets(1)
	outcome, err = review.attempt(t, reviewArguments)
	requireUnknown(t, outcome, err, "not sent: ")
	if posts := harness.slack.matching(isPost); len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	// The model calls post_review again: the unknown outcome refuses it, so the admins see one review.
	harness.storeResult("post_review", dexagents.OutcomeUnknownToolResult)
	outcome, err = harness.toolStep("post_review", "step-review-again").attempt(t, reviewArguments)
	var toolError *dexagents.ToolExecutionError
	if !errors.As(err, &toolError) || !strings.HasSuffix(toolError.Message, "refused: an earlier post_review may have posted the review (its outcome stayed unknown after the connector checked and tried up to 3 times); never post it again, the admins decide in Dex Cloud admin") {
		t.Fatalf("the new call is not refused: %#v, %v", outcome, err)
	}
	if posts := harness.slack.matching(isPost); len(posts) != 1 {
		t.Fatalf("the review was posted again: %d posts", len(posts))
	}
}

func TestARefusalOnARetryAfterAnAttemptThatSentNothingStaysAFailure(t *testing.T) {
	harness := newRetryHarness(t)
	// Slack refuses the post with a rate limit, so nothing was posted, and the fence records it.
	harness.slack.answer = func(recordedRequest) (*http.Response, error) {
		response := jsonResponse(429, map[string]any{"ok": false, "error": "ratelimited"})
		response.Header.Set("Retry-After", "1")
		return response, nil
	}
	review := harness.toolStep("post_review", "step-review")
	outcome, err := review.attempt(t, reviewArguments)
	requireRetry(t, outcome, err)
	harness.stub.SetFailingGets(1)
	outcome, err = review.attempt(t, reviewArguments)
	var toolError *dexagents.ToolExecutionError
	if !errors.As(err, &toolError) || !strings.HasSuffix(toolError.Message, "not sent: ReviewAPI get: busy") {
		t.Fatalf("the refusal is not a failure: %#v, %v (checkpoint %s)", outcome, err, checkpointText(review.checkpoint))
	}
}

// The first attempt ran, but Dex lost its checkpoint: Dex stores checkpoints batched and lossy. The retry's refusal is
// unknown, as sdkgo's fence would assume a possible send.
func TestARefusalOnARetryThatFindsNoCheckpointIsUnknown(t *testing.T) {
	harness := newRetryHarness(t)
	review := harness.toolStep("post_review", "step-review")
	review.attempts = 1 // the next attempt is attempt 2, with no checkpoint
	harness.stub.SetFailingGets(1)
	outcome, err := review.attempt(t, reviewArguments)
	requireUnknown(t, outcome, err, "not sent: ReviewAPI get: busy (an earlier attempt of this call may have sent it)")
	if posts := harness.slack.matching(isPost); len(posts) != 0 {
		t.Fatalf("posts = %d", len(posts))
	}
}

// A retry whose checkpoint cannot be read does not know the earlier attempts' sends: a checkpoint text that does not
// decode, or the call's entry with more sends than its attempts allow. The retry's refusal is unknown, never failed. A
// Retry would not help: the session repeats the checkpoint in every later attempt, and the write's fence fails closed
// on it.
func TestARefusalOnARetryWhoseCheckpointCannotBeReadIsUnknown(t *testing.T) {
	t.Run("the checkpoint does not decode", func(t *testing.T) {
		harness := newRetryHarness(t)
		review := harness.toolStep("post_review", "step-review")
		review.attempts = 1 // the next attempt is attempt 2
		broken := `{"singleDispatchMarkers":`
		review.checkpoint = &broken
		harness.stub.SetFailingGets(1)
		outcome, err := review.attempt(t, reviewArguments)
		requireUnknown(t, outcome, err, "not sent: ReviewAPI get: busy (an earlier attempt of this call may have sent it)")
		if posts := harness.slack.matching(isPost); len(posts) != 0 {
			t.Fatalf("posts = %d", len(posts))
		}
	})
	t.Run("the call's entry counts an impossible send", func(t *testing.T) {
		// As in TestARefusalOnARetryAfterAnAttemptThatSentNothingStaysAFailure, Slack refuses the post with a rate
		// limit and the fence records no send; the checkpoint then claims one send before attempt 1, which no attempt
		// could have made.
		harness := newRetryHarness(t)
		harness.slack.answer = func(recordedRequest) (*http.Response, error) {
			response := jsonResponse(429, map[string]any{"ok": false, "error": "ratelimited"})
			response.Header.Set("Retry-After", "1")
			return response, nil
		}
		review := harness.toolStep("post_review", "step-review")
		outcome, err := review.attempt(t, reviewArguments)
		requireRetry(t, outcome, err)
		recorded := checkpointText(review.checkpoint)
		if strings.Count(recorded, `"sendCount":0`) != 1 {
			t.Fatalf("the checkpoint does not record one entry without a send: %s", recorded)
		}
		impossible := strings.Replace(recorded, `"sendCount":0`, `"sendCount":1`, 1)
		review.checkpoint = &impossible
		harness.stub.SetFailingGets(1)
		outcome, err = review.attempt(t, reviewArguments)
		requireUnknown(t, outcome, err, "not sent: ReviewAPI get: busy (an earlier attempt of this call may have sent it)")
	})
}

// A tool's own definite failure after its write, in the same attempt, stays a failure, as the TypeScript tools'
// ToolExecutionError is recorded. The tool's Call is its write's, so the attempt's fence claimed the send that reached
// the provider, and sdkgo's own agent-tool classification would mark the failed branch unknown; the Go connector tool
// source records a branch as the tool lists it.
func TestAToolsDefiniteFailureAfterItsWriteInTheSameAttemptStaysAFailure(t *testing.T) {
	// Gmail rejects the send after the fence's claim: nothing was sent.
	harness := newRetryHarness(t)
	harness.stub.DecideElsewhere("acct1", "active")
	harness.storeResult("post_review", `{"draft":{"number":1},"email":{"body":"Hi Ada","subject":"Welcome"},"thread":{"ts":"1710000100.000100"}}`)
	harness.gmail.answer = func(request recordedRequest) (*http.Response, error) {
		if isGmailSend(request) {
			return jsonResponse(400, map[string]any{"error": map[string]any{"code": 400, "message": "Invalid To header"}}), nil
		}
		return jsonResponse(404, map[string]any{}), nil
	}
	send := harness.toolStep("send_welcome_email", "step-send")
	outcome, err := send.attempt(t, `{}`)
	var toolError *dexagents.ToolExecutionError
	if !errors.As(err, &toolError) || !strings.HasPrefix(toolError.Message, "gmail sendMessage returned failed: PROVIDER_REJECTION: send_welcome_email failed and nothing was sent: ") {
		t.Fatalf("the rejected send is not a failure: %#v, %v", outcome, err)
	}
	if sends := harness.gmail.matching(isGmailSend); len(sends) != 1 {
		t.Fatalf("sends = %d", len(sends))
	}

	// The ReviewAPI refuses the approval, and the tool posts the failure in the thread.
	harness = newRetryHarness(t)
	harness.stub.SetRefusesSuppressedMail(true)
	harness.storeResult("post_review", `{"draft":{"number":1},"email":{"body":"Hi Ada","subject":"Welcome"},"thread":{"ts":"1710000100.000100"}}`)
	harness.slack.answer = func(request recordedRequest) (*http.Response, error) {
		return jsonResponse(200, map[string]any{"ok": true, "channel": "CREVIEWS01", "ts": "1710000100.000900", "message": map[string]any{"ts": "1710000100.000900", "text": "Approval failed."}}), nil
	}
	approve := harness.toolStep("approve_account", "step-approve")
	outcome, err = approve.attempt(t, `{}`)
	if !errors.As(err, &toolError) || toolError.Message != "slack postThreadReply returned failed: PROVIDER_REJECTION: approve_account failed: the review API refused the decision: ReviewAPI approve: suppressApprovedMail is not supported yet (thread note: posted)" {
		t.Fatalf("the refused approval is not a failure: %#v, %v", outcome, err)
	}
	if posts := harness.slack.matching(isPost); len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
}

func checkpointText(checkpoint *string) string {
	if checkpoint == nil {
		return "none"
	}
	return *checkpoint
}
