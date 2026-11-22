package signup_test

// The six connector tools in the Go connector tool source: each call pins the connectors' declared send cap of 3 and
// gets an attempt per send, asks for no approval, and runs as the source runs a connector call. Port of the cap test
// of test/signup-agent.test.ts.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/connectors/slack/go/slackmock"
	"github.com/superdurable/dex-connectors-library/sdkgo/singledispatchtest"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/agents/go/core/connectortools"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

func connectorSource(t *testing.T, harness bindingHarness) *connectortools.ConnectorToolSource {
	t.Helper()
	sessions := signup.NewReviewSessions()
	sessions.Connect(nil, "SignupReviewEinoAgentFlow", harness.stored)
	api, err := signup.NewReviewAPIClient(harness.stub.BaseURL(), harness.stub.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	source, err := signup.SignupConnectorToolSource(&signup.Services{Sessions: sessions, ReviewAPI: api, SlackChannelID: "CREVIEWS01", HarnessLabel: "eino"}, harness.slack.Connection(), harness.gmail.Connection())
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestEachConnectorToolCallCarriesTheConnectorsDeclaredCapOf3(t *testing.T) {
	source := connectorSource(t, newBindingHarness(t))
	names := source.DeclaredToolNames()
	if strings.Join(names, ",") != strings.Join(signup.ConnectorToolNames, ",") {
		t.Fatalf("tools = %v", names)
	}
	turn := dexagents.AgentTurnContext{ToolPolicies: signup.SignupConnectorToolPolicies()}
	for index, name := range signup.ConnectorToolNames {
		deferred, err := source.DeferredToolCall(turn, dexagents.SourceToolCall{ToolCallID: "call-" + name, ToolName: name, Arguments: "{}"})
		if err != nil {
			t.Fatal(err)
		}
		// Dex Agents passes this pinned cap to the connector as the call's send cap on every attempt.
		var reference map[string]any
		if deferred.ToolSourceReference == nil || json.Unmarshal([]byte(*deferred.ToolSourceReference), &reference) != nil || reference["tool"] != name || reference["maxSends"] != float64(3) {
			t.Fatalf("%s: reference %v", name, deferred.ToolSourceReference)
		}
		if deferred.Policy.RequiresApproval == nil || *deferred.Policy.RequiresApproval || deferred.Policy.Effect != dexagents.ToolEffectNonIdempotentWrite {
			t.Fatalf("%s: policy %+v", name, deferred.Policy)
		}
		options, err := source.ToolCallStepOptions(dexagents.AgentToolCall{ToolName: name, ToolSourceReference: deferred.ToolSourceReference, DeferralOrder: int64(index)})
		if err != nil || options == nil || options.BoundedResendExecuteRetry == nil || options.BoundedResendExecuteRetry.MaximumAttempts < 3 {
			t.Fatalf("%s's tool Step gets no attempt per send: %+v, %v", name, options, err)
		}
	}
	// The tools the model sees: the tools' own descriptions and argument schemas.
	listed := source.CachedTools()
	for _, tool := range listed {
		if tool.Name == "reject_account" && (!strings.Contains(string(tool.InputSchema), "The reviewer's reason for the rejection.") || tool.Description != "Reject the account in Dex Cloud with the reviewer's reason; the tool posts the outcome in the review thread.") {
			t.Fatalf("reject_account = %+v", tool)
		}
	}
}

func TestTheSourceRunsAToolsRefusalAsAFailureWithTheToolsTextAndASuccessAsItsResult(t *testing.T) {
	harness := newBindingHarness(t)
	source := connectorSource(t, harness)
	turn := dexagents.AgentTurnContext{ToolPolicies: signup.SignupConnectorToolPolicies()}
	run := func(step *singledispatchtest.Context, name, arguments string) (dexagents.DeferredToolOutcome, error) {
		deferred, err := source.DeferredToolCall(turn, dexagents.SourceToolCall{ToolCallID: "call-1", ToolName: name, Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		return source.ExecuteDeferredToolCall(step, dexagents.DeferredToolExecution{
			FlowID: "signup-review-acct1", Attempt: int64(step.Attempt()), StepContext: step,
			ToolCall:  dexagents.AgentToolCall{ToolCallID: "call-1", ToolName: name, Arguments: arguments, ToolSourceReference: deferred.ToolSourceReference},
			Heartbeat: func() error { return nil }, SaveHeartbeatCheckpoint: func(string) error { return nil },
		})
	}
	// No review post yet: reply_in_thread refuses with its own text, inside the source's text for the failed branch of
	// the operation it writes with.
	_, err := run(singledispatchtest.NewContext("signup-review-acct1", "step-1"), "reply_in_thread", `{"text":"hi"}`)
	var toolError *dexagents.ToolExecutionError
	if !errorsAs(err, &toolError) || toolError.Message != "slack postThreadReply returned failed: VALIDATION: refused: call post_review first" {
		t.Fatalf("err = %v", err)
	}
	// The source checks the schema itself first.
	if _, err := run(singledispatchtest.NewContext("signup-review-acct1", "step-2"), "reply_in_thread", `{"note":"hi"}`); !errorsAs(err, &toolError) || !strings.HasPrefix(toolError.Message, "invalid arguments for reply_in_thread") {
		t.Fatalf("err = %v", err)
	}
	harness.slack.PostChannelMessage().Default(slackmock.PostChannelMessageSent(slack.PostMessageOutput{Message: slack.Message{ChannelID: "CREVIEWS01", Timestamp: "1710000100.000100"}}, nil))
	outcome, err := run(singledispatchtest.NewContext("signup-review-acct1", "step-3"), "post_review", reviewArguments)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := outcome.(*dexagents.ToolExecutionResult)
	// The result text has sorted keys and no spaces, as in every core, so the records read it.
	if !ok || !strings.Contains(result.Output, `"draft":{"number":1}`) || !strings.Contains(result.Output, `"thread":{"ts":"1710000100.000100"}`) {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func errorsAs[T error](err error, target *T) bool { return errors.As(err, target) }
