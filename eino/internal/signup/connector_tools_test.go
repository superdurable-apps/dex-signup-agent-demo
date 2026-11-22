package signup_test

// The send caps of the connector tools and the decision notes' states: port of test/signup-agent.test.ts. The
// connectors' fence behaviour itself is in connector_fences_test.go.

import (
	"reflect"
	"sort"
	"testing"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	dexagents "github.com/superdurable/dex/agents/go/core"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

func TestTheConnectorToolsSessionPoliciesSetNoSendCapOnlyNoApproval(t *testing.T) {
	policies := signup.SignupConnectorToolPolicies()
	var names []string
	for name, policy := range policies {
		names = append(names, name)
		if policy.RequiresApproval == nil || *policy.RequiresApproval {
			t.Fatalf("%s asks for Dex approval", name)
		}
		// The policy sets the effect and the approval only, so each call keeps the connector's declared send cap.
		if policy.Effect != dexagents.ToolEffectNonIdempotentWrite {
			t.Fatalf("%s has the effect %s", name, policy.Effect)
		}
	}
	sort.Strings(names)
	want := append([]string(nil), signup.ConnectorToolNames...)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("policies = %v", names)
	}
}

func TestTheConnectorsDeclareASendCapOf3(t *testing.T) {
	for _, check := range []struct {
		name       string
		definition sdkgo.MutationDefinition
	}{
		{"postChannelMessage", slack.PostChannelMessageDefinition},
		{"postThreadReply", slack.PostThreadReplyDefinition},
		{"sendMessage", gmail.SendMessageDefinition},
	} {
		if check.definition.RepeatOnUnknown != sdkgo.RepeatOnUnknownLimited || check.definition.MaxSends != 3 {
			t.Fatalf("%s declares %s with maxSends %d", check.name, check.definition.RepeatOnUnknown, check.definition.MaxSends)
		}
	}
}

func TestADecisionToolsNoteIsPostedNotPostedOrUnknownFromTheSlackAttempt(t *testing.T) {
	failure := sdkgo.Failure{Kind: sdkgo.FailureTransport, Provider: "slack", Operation: "postThreadReply", Message: "x"}
	posted := slack.PostMessageOutput{Message: slack.Message{ChannelID: "C", Timestamp: "1710000100.000200"}}
	for _, check := range []struct {
		attempt sdkgo.MutationAttempt[slack.PostMessageOutput]
		want    signup.ConfirmationState
	}{
		{sdkgo.NewMutationBranch("sent", posted, nil, sdkgo.Receipt{}), signup.ConfirmationPosted},
		// Slack selects these only when nothing of the call went out.
		{sdkgo.NewMutationBranch("providerRejected", slack.PostMessageOutput{}, &failure, sdkgo.Receipt{}), signup.ConfirmationNotPosted},
		{sdkgo.NewMutationBranch(sdkgo.DefectBranchID, slack.PostMessageOutput{}, &failure, sdkgo.Receipt{}), signup.ConfirmationNotPosted},
		// A retry may follow a lost answer, and an uncertain post may be in the thread.
		{sdkgo.NewMutationRetry[slack.PostMessageOutput](failure, 0), signup.ConfirmationUnknown},
		{sdkgo.NewMutationUncertain(slack.PostMessageOutput{}, failure, sdkgo.Receipt{}), signup.ConfirmationUnknown},
	} {
		// The tool reads the attempt through its accessors.
		outcome, err := signup.OutcomeOf(check.attempt)
		if err != nil {
			t.Fatal(err)
		}
		if got := signup.ConfirmationStateOf(outcome); got != check.want {
			t.Fatalf("%+v: %s, want %s", outcome, got, check.want)
		}
	}
}

func TestADecisionMadeEarlierInTheReviewReportsItsNoteAsPostedOnlyWhenItWasPosted(t *testing.T) {
	if signup.RepeatedConfirmation("posted") != signup.ConfirmationPosted {
		t.Fatal("posted is not kept")
	}
	// The model may have posted the outcome itself since, or its turn failed first: never repost.
	for _, earlier := range []any{"notPosted", "unknown", nil} {
		if signup.RepeatedConfirmation(earlier) != signup.ConfirmationUnknown {
			t.Fatalf("%v is not unknown", earlier)
		}
	}
}
