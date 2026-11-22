package signup_test

// The connector tools as sdkgo AgentTools over the connector mocks, in a fake tool Step: each tool's checks, its one
// wrapped connector call under the connector's own call identity, and the branch its result takes.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	"github.com/superdurable/dex-connectors-library/connectors/google/gmail/go/gmailmock"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/connectors/slack/go/slackmock"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/singledispatchtest"
	dexagents "github.com/superdurable/dex/agents/go/core"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
)

// storedMessages answers the session reads with fixed messages.
type storedMessages struct{ messages []dexagents.AgentMessage }

func (stored *storedMessages) GetAgentSnapshot(context.Context, string) (dexagents.AgentSnapshot, error) {
	return dexagents.AgentSnapshot{Messages: stored.messages}, nil
}

func (stored *storedMessages) ListAgentMessages(context.Context, string, *int64, int64) (dexagents.AgentMessagesPage, error) {
	return dexagents.AgentMessagesPage{}, nil
}

type bindingHarness struct {
	tools  map[string]sdkgo.AgentTool
	slack  *slackmock.Mock
	gmail  *gmailmock.Mock
	stored *storedMessages
	stub   *signuptest.ReviewAPIStub
}

func newBindingHarness(t *testing.T) bindingHarness {
	t.Helper()
	stub := signuptest.StartReviewAPIStub()
	t.Cleanup(stub.Stop)
	stub.AddPendingAccount(signuptest.PendingAccount{AccountID: "acct1", Email: "ada@example.com", DisplayName: text("Ada <Lovelace>")})
	api, err := signup.NewReviewAPIClient(stub.BaseURL(), stub.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	stored := &storedMessages{}
	sessions := signup.NewReviewSessions()
	sessions.Connect(nil, "SignupReviewEinoAgentFlow", stored)
	slackMock, gmailMock := slackmock.New(t, signup.ReviewSlackConnection), gmailmock.New(t, signup.WelcomeGmailConnection)
	services := &signup.Services{Sessions: sessions, ReviewAPI: api, Profiles: signuptest.FixtureProfileSources{}, SlackChannelID: "CREVIEWS01", HarnessLabel: "eino"}
	agentTools, err := signup.SignupConnectorAgentTools(services, slackMock.Connection(), gmailMock.Connection())
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]sdkgo.AgentTool{}
	for _, tool := range agentTools {
		tools[tool.Name] = tool.AgentTool
	}
	return bindingHarness{tools: tools, slack: slackMock, gmail: gmailMock, stored: stored, stub: stub}
}

func (harness bindingHarness) run(t *testing.T, step *singledispatchtest.Context, name string, arguments string) sdkgo.AgentToolAttempt {
	t.Helper()
	attempt, err := harness.tools[name].RunAttemptWithOptions(step, json.RawMessage(arguments), sdkgo.WithMaxSends(3))
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

const reviewArguments = `{"recommendation":"approve","reasons":["Builds <things>"],"emailSubject":"Welcome","emailBody":"Hi Ada"}`

func TestPostReviewPostsOnceUnderTheConnectorsCallIdentityAndRecordsTheThread(t *testing.T) {
	harness := newBindingHarness(t)
	harness.slack.PostChannelMessage().Default(slackmock.PostChannelMessageSent(slack.PostMessageOutput{Message: slack.Message{ChannelID: "CREVIEWS01", Timestamp: "1710000100.000100"}}, nil))
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	attempt := harness.run(t, step, "post_review", reviewArguments)
	if attempt.Outcome != sdkgo.AgentToolAttemptSucceeded || attempt.Branch != "sent" {
		t.Fatalf("attempt = %+v", attempt)
	}
	var value map[string]any
	_ = json.Unmarshal(attempt.Value, &value)
	if value["thread"].(map[string]any)["ts"] != "1710000100.000100" || value["draft"].(map[string]any)["number"] != float64(1) {
		t.Fatalf("value = %s", attempt.Value)
	}
	if email := value["email"].(map[string]any); email["subject"] != "Welcome" || email["body"] != "Hi Ada" {
		t.Fatalf("email = %v", email)
	}
	calls := harness.slack.PostChannelMessage().Calls()
	if len(calls) != 1 || calls[0].FlowID != "signup-review-acct1" || calls[0].Input.ChannelID != "CREVIEWS01" {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(calls[0].Input.Text, "Ada &lt;Lovelace&gt;") || !strings.Contains(calls[0].Input.Text, "Builds &lt;things&gt;") || !strings.HasSuffix(calls[0].Input.Text, "_Reviewed by eino · Dex session `signup-review-acct1`_") {
		t.Fatalf("text = %q", calls[0].Input.Text)
	}
	// The wrapped call has the connector operation's own call identity, as a plain connector tool call would.
	direct := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	if _, err := signup.RunConnectorAttempt(direct, harness.slack.Connection().Operations().PostChannelMessage(), harness.slack.Connection().Reference(), slack.PostChannelMessageInput{ChannelID: "C", Text: "x"}, 3); err != nil {
		t.Fatal(err)
	}
	if calls := harness.slack.PostChannelMessage().Calls(); calls[1].CallID != calls[0].CallID {
		t.Fatalf("call IDs %s and %s differ", calls[0].CallID, calls[1].CallID)
	}
}

func TestAToolsRefusalIsARejectedBranchWithItsOwnText(t *testing.T) {
	harness := newBindingHarness(t)
	output := `{"draft":{"number":1},"email":{"body":"Hi","subject":"Welcome"},"thread":{"ts":"1710000100.000100"}}`
	name := "post_review"
	harness.stored.messages = []dexagents.AgentMessage{{Sequence: 1, Role: dexagents.MessageRoleTool, ToolName: &name, ToolOutput: &output}}
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	attempt := harness.run(t, step, "post_review", reviewArguments)
	if attempt.Outcome != sdkgo.AgentToolAttemptRejected || attempt.Branch != signup.RefusedBranch || attempt.Failure == nil ||
		attempt.Failure.Message != "refused: the review is already posted (thread 1710000100.000100); use post_draft or reply_in_thread" {
		t.Fatalf("attempt = %+v %+v", attempt, attempt.Failure)
	}
	if calls := harness.slack.PostChannelMessage().Calls(); len(calls) != 0 {
		t.Fatalf("posted %d times", len(calls))
	}
	// Arguments past the zod limits are refused before anything else.
	attempt = harness.run(t, step, "reply_in_thread", `{"text":""}`)
	if attempt.Branch != signup.RefusedBranch || attempt.Failure.Message != "invalid arguments for reply_in_thread: Too small: expected string to have >=1 characters" {
		t.Fatalf("attempt = %+v %+v", attempt, attempt.Failure)
	}
}

func TestAnUnknownOutcomeBelowTheCapAsksForTheStepRetryAndAGmailDefectIsUnknown(t *testing.T) {
	harness := newBindingHarness(t)
	output := `{"draft":{"number":1},"email":{"body":"Hi","subject":"Welcome"},"thread":{"ts":"1710000100.000100"}}`
	name := "post_review"
	harness.stored.messages = []dexagents.AgentMessage{{Sequence: 1, Role: dexagents.MessageRoleTool, ToolName: &name, ToolOutput: &output}}
	harness.stub.DecideElsewhere("acct1", "active")
	failure := sdkgo.Failure{Kind: sdkgo.FailureAvailability, Provider: "google", Operation: "sendMessage", Message: "Gmail send outcome is unknown"}
	harness.gmail.SendMessage().Respond(gmailmock.SendMessageRetry(failure, 0), gmailmock.SendMessageDefect(gmail.SendMessageOutput{}, &sdkgo.Failure{Kind: sdkgo.FailureTransport, Provider: "google", Operation: "sendMessage", Message: "connection reset"}))
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	if attempt := harness.run(t, step, "send_welcome_email", `{}`); attempt.Outcome != sdkgo.AgentToolAttemptRetry {
		t.Fatalf("first attempt = %+v", attempt)
	}
	step.AdvanceAttempt()
	if attempt := harness.run(t, step, "send_welcome_email", `{}`); attempt.Outcome != sdkgo.AgentToolAttemptUnknown || attempt.Branch != sdkgo.DefectBranchID {
		t.Fatalf("second attempt = %+v", attempt)
	}
	sends := harness.gmail.SendMessage().Calls()
	if len(sends) != 2 || sends[1].CallID != sends[0].CallID || sends[0].Input.To[0] != "ada@example.com" {
		t.Fatalf("sends = %+v", sends)
	}
}
