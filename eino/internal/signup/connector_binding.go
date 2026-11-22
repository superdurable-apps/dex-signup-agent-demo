package signup

// How a connector tool runs inside its tool Step: each tool is an application Mutation that wraps the connector
// operation it writes with. The Mutation runs the tool's checks, then the operation's one invocation, with the
// connector's own call identity, idempotency key and single-dispatch fence, and returns the tool's result as its
// branch. The Go connector tool source runs it like any connector operation (connectortools.NewConnectorTool).
//
// Why a wrapping Mutation instead of the TypeScript tools' own invoke: Go's sdkgo.MutationAttempt is opaque (its
// fields are unexported), so a tool cannot read the attempt of the operation it calls through Invoke. It runs the
// operation through sdkgo.RunMutation instead, which returns the branch, value and failure. sdkgo refuses a second
// running call of the same operation in one Step attempt, so the wrapping Mutation has its own operation identity:
// the connector's ID and the tool's name, such as slack postReview. The wrapped call keeps the connector operation's
// identity, so its Call ID, idempotency key and fence are exactly those of a plain connector tool call.
// WORKAROUND(dex-bugs.md#a-custom-go-connector-tool-cannot-read-the-attempt-of-the-operation-it-wraps)

import (
	"encoding/json"
	"errors"
	"fmt"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/sdk-go/dex"
)

// RunConnectorAttempt runs one attempt of operation in the tool Step's context ctx, with the connector's call
// identity (derived from the Step's FlowID and StepExecutionID), its idempotency key and fence, and the call's send
// cap, and reads the attempt as a tool does: its kind, branch, value and failure.
func RunConnectorAttempt[IN, OUT any](ctx dex.Context, operation sdkgo.Mutation[IN, OUT], connection sdkgo.ConnectionRef, input IN, maxSends int32) (ConnectorOutcome, error) {
	var options []sdkgo.RunOption
	if maxSends > 1 && operation.Definition().RepeatOnUnknown == sdkgo.RepeatOnUnknownLimited {
		options = append(options, sdkgo.WithMaxSends(maxSends))
	}
	result, err := sdkgo.RunMutation(ctx, operation, connection, input, options...)
	if err != nil {
		var retry *sdkgo.RetryError
		if !errors.As(err, &retry) {
			return ConnectorOutcome{}, err
		}
		failure := retry.Failure
		outcome := ConnectorOutcome{Kind: "retry", Failure: &failure}
		var retryAfter *dex.RetryAfterError
		if errors.As(err, &retryAfter) {
			outcome.RetryAfter = retryAfter.After
		}
		return outcome, nil
	}
	value, err := json.Marshal(result.Value)
	if err != nil {
		return ConnectorOutcome{}, err
	}
	kind := "branch"
	if result.Branch == sdkgo.UncertainBranchID {
		kind = "uncertain"
	}
	return ConnectorOutcome{Kind: kind, Branch: result.Branch, Value: value, Failure: result.Failure}, nil
}

// FailedBranch is the branch of a tool's own refusal or definite failure, and the failure's message is the tool's
// text, such as "refused: call post_review first". It is not named refused: the source's text for it,
// "<connector> <operation> returned <branch>: ...", must not read as a refusal when the tool's text is another
// failure, such as send_welcome_email's "send_welcome_email failed and nothing was sent: ...".
const FailedBranch sdkgo.BranchID = "failed"

// signupMutation is one connector tool as an sdkgo Mutation.
type signupMutation struct {
	tool       connectorTool
	definition sdkgo.MutationDefinition
	// write runs the wrapped connector operation once with input, inside the Step of ctx, with the call's send cap.
	write func(ctx dex.Context, input any, maxSends int32) (ConnectorOutcome, error)
}

func (mutation signupMutation) Definition() sdkgo.MutationDefinition { return mutation.definition }

func (mutation signupMutation) IdempotencyKey(callID sdkgo.CallID, _ map[string]any) sdkgo.IdempotencyKey {
	return sdkgo.IdempotencyKey(callID)
}

func (mutation signupMutation) failure(kind sdkgo.FailureKind, message string) *sdkgo.Failure {
	operation := mutation.definition.Operation
	return &sdkgo.Failure{Kind: kind, Provider: operation.ConnectorID, Operation: operation.OperationID, Message: message}
}

// Invoke runs one attempt of the tool.
func (mutation signupMutation) Invoke(call sdkgo.Call, arguments map[string]any) sdkgo.MutationAttempt[json.RawMessage] {
	if problems := mutation.tool.validate(arguments); len(problems) > 0 {
		return sdkgo.NewMutationBranch[json.RawMessage](FailedBranch, nil,
			mutation.failure(sdkgo.FailureValidation, fmt.Sprintf("invalid arguments for %s: %s", mutation.tool.name, joinProblems(problems))), sdkgo.Receipt{})
	}
	write := func(input any) (ConnectorOutcome, error) { return mutation.write(call.Context, input, call.MaxSends()) }
	result, err := mutation.tool.attempt(call.Context, call.Context.FlowID(), arguments, write)
	if err != nil {
		var toolError *dexagents.ToolExecutionError
		var unknown *outcomeUnknownError
		switch {
		case errors.As(err, &toolError):
			kind := sdkgo.FailureProviderRejection
			if isRefusalText(toolError.Message) {
				kind = sdkgo.FailureValidation
			}
			return sdkgo.NewMutationBranch[json.RawMessage](FailedBranch, nil, mutation.failure(kind, toolError.Message), sdkgo.Receipt{})
		case errors.As(err, &unknown):
			return sdkgo.NewMutationUncertain[json.RawMessage](nil, *mutation.failure(sdkgo.FailureAvailability, unknown.message), sdkgo.Receipt{})
		default:
			// A defect of the wrapped call: Dex records outcome_unknown, as the connector tools record a defect.
			return sdkgo.NewMutationBranch[json.RawMessage](sdkgo.DefectBranchID, nil, mutation.failure(sdkgo.FailureLocalDefect, messageOf(err)), sdkgo.Receipt{})
		}
	}
	if result.pass != nil {
		outcome := *result.pass
		switch outcome.Kind {
		case "retry":
			return sdkgo.NewMutationRetry[json.RawMessage](*outcome.Failure, outcome.RetryAfter)
		case "uncertain":
			return sdkgo.NewMutationUncertain(outcome.Value, *outcome.Failure, sdkgo.Receipt{})
		default:
			return sdkgo.NewMutationBranch(outcome.Branch, outcome.Value, outcome.Failure, sdkgo.Receipt{})
		}
	}
	value, err := json.Marshal(result.value)
	if err != nil {
		return sdkgo.NewMutationBranch[json.RawMessage](sdkgo.DefectBranchID, nil, mutation.failure(sdkgo.FailureLocalDefect, "the result does not encode as JSON"), sdkgo.Receipt{})
	}
	return sdkgo.NewMutationBranch(sentBranchOf(mutation.definition), json.RawMessage(value), nil, sdkgo.Receipt{})
}

// isRefusalText reports a tool's own refusal: nothing was tried.
func isRefusalText(message string) bool {
	for _, start := range []string{RefusedPrefix, "not sent: ", "invalid arguments"} {
		if len(message) >= len(start) && message[:len(start)] == start {
			return true
		}
	}
	return false
}

func joinProblems(problems []string) string {
	text := ""
	for index, problem := range problems {
		if index > 0 {
			text += "; "
		}
		text += problem
	}
	return text
}

// sentBranchOf is the definition's required branch, "sent" for every wrapped operation.
func sentBranchOf(definition sdkgo.MutationDefinition) sdkgo.BranchID {
	for _, branch := range definition.Branches {
		if !branch.Optional {
			return branch.ID
		}
	}
	return "sent"
}

// toolDefinition is the wrapping Mutation's definition: the wrapped operation's branches, Step defaults and send cap,
// under the tool's own operation identity, and the refused branch.
func toolDefinition(wrapped sdkgo.MutationDefinition, connectorID, operationID string) sdkgo.MutationDefinition {
	definition := wrapped
	definition.Operation = sdkgo.OperationRef{ConnectorID: connectorID, OperationID: operationID}
	definition.Branches = append(append([]sdkgo.BranchDefinition(nil), wrapped.Branches...),
		sdkgo.BranchDefinition{ID: FailedBranch, Description: "The tool refused the call, or the call definitely failed.", Optional: true})
	return definition
}

// toolMetadata is the wrapping Mutation's agent metadata. Its branch outcomes are those of the TypeScript
// application's connector tools: a connector's defect is outcome_unknown, as Dex Agents 0.6.14 records it, so a write
// that may have gone out is never told to the model as failed.
func toolMetadata(definition sdkgo.MutationDefinition, description, schema string) sdkgo.AgentToolMetadata {
	return sdkgo.AgentToolMetadata{
		Operation: definition.Operation, Kind: sdkgo.AgentOperationKindMutation,
		ToolName:    definition.Operation.ConnectorID + "_" + lowerCamelToSnake(definition.Operation.OperationID),
		Description: description, RepeatOnUnknown: definition.RepeatOnUnknown, MaxSends: definition.MaxSends,
		BranchOutcomes: map[sdkgo.BranchID]sdkgo.AgentBranchOutcome{
			"providerRejected": sdkgo.AgentBranchOutcomeRejected, sdkgo.UncertainBranchID: sdkgo.AgentBranchOutcomeUnknown,
			sdkgo.DefectBranchID: sdkgo.AgentBranchOutcomeUnknown, FailedBranch: sdkgo.AgentBranchOutcomeRejected,
		},
		InputType: "arguments", InputSchema: json.RawMessage(schema), Branches: definition.Branches, StepDefaults: definition.StepDefaults,
	}
}

// lowerCamelToSnake is sdkgo's tool-name rule for the operation IDs this application uses (no acronyms).
func lowerCamelToSnake(name string) string {
	converted := []rune{}
	for index, character := range name {
		if character >= 'A' && character <= 'Z' {
			if index > 0 {
				converted = append(converted, '_')
			}
			character += 'a' - 'A'
		}
		converted = append(converted, character)
	}
	return string(converted)
}

// toolOperationIDs are the wrapping Mutations' operation IDs, from the tool names.
var toolOperationIDs = map[string]string{
	"post_review": "postReview", "post_draft": "postDraft", "reply_in_thread": "replyInThread",
	"approve_account": "approveAccount", "reject_account": "rejectAccount", "send_welcome_email": "sendWelcomeEmail",
}

// SignupConnectorAgentTool is one connector tool as an sdkgo.AgentTool, with the tool's name.
type SignupConnectorAgentTool struct {
	Name        string
	Description string
	AgentTool   sdkgo.AgentTool
}

// SignupConnectorAgentTools are the six connector tools as sdkgo.AgentTools over the Slack and Gmail connections:
// post_review on Slack postChannelMessage; post_draft, reply_in_thread, approve_account and reject_account on Slack
// postThreadReply; send_welcome_email on Gmail sendMessage.
func SignupConnectorAgentTools(services *Services, slackConnection slack.Connection, gmailConnection gmail.Connection) ([]SignupConnectorAgentTool, error) {
	slackOperations, gmailOperations := slackConnection.Operations(), gmailConnection.Operations()
	if slackOperations == nil || gmailOperations == nil {
		return nil, errors.New("the Slack and Gmail connections are required")
	}
	slackReference, gmailReference := slackConnection.Reference(), gmailConnection.Reference()
	writes := map[string]struct {
		wrapped    sdkgo.MutationDefinition
		connection sdkgo.ConnectionRef
		write      func(ctx dex.Context, input any, maxSends int32) (ConnectorOutcome, error)
	}{
		"postChannelMessage": {slack.PostChannelMessageDefinition, slackReference, func(ctx dex.Context, input any, maxSends int32) (ConnectorOutcome, error) {
			return RunConnectorAttempt(ctx, slackOperations.PostChannelMessage(), slackReference, input.(slack.PostChannelMessageInput), maxSends)
		}},
		"postThreadReply": {slack.PostThreadReplyDefinition, slackReference, func(ctx dex.Context, input any, maxSends int32) (ConnectorOutcome, error) {
			return RunConnectorAttempt(ctx, slackOperations.PostThreadReply(), slackReference, input.(slack.PostThreadReplyInput), maxSends)
		}},
		"sendMessage": {gmail.SendMessageDefinition, gmailReference, func(ctx dex.Context, input any, maxSends int32) (ConnectorOutcome, error) {
			return RunConnectorAttempt(ctx, gmailOperations.SendMessage(), gmailReference, input.(gmail.SendMessageInput), maxSends)
		}},
	}
	var tools []SignupConnectorAgentTool
	for _, tool := range signupConnectorTools(services) {
		wrapped := writes[tool.operationID]
		definition := toolDefinition(wrapped.wrapped, wrapped.wrapped.Operation.ConnectorID, toolOperationIDs[tool.name])
		agentTool, err := sdkgo.NewMutationAgentTool[map[string]any, json.RawMessage](
			signupMutation{tool: tool, definition: definition, write: wrapped.write}, wrapped.connection, toolMetadata(definition, tool.description, tool.schema))
		if err != nil {
			return nil, fmt.Errorf("connector tool %s: %w", tool.name, err)
		}
		tools = append(tools, SignupConnectorAgentTool{Name: tool.name, Description: tool.description, AgentTool: agentTool})
	}
	return tools, nil
}
