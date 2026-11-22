package signup

// How a connector tool runs inside its tool Step, as src/signup-agent.ts write() builds one: each tool is an
// application Mutation over the connector operation it writes with. Its definition and idempotency key are the
// operation's, with the tool's failed branch added, so sdkgo gives it the Call of a plain connector tool call of that
// operation: the same Call ID, idempotency key, send cap and single-dispatch fence. Its Invoke runs the tool's checks,
// then the operation's Invoke once with that Call, reads the attempt through the attempt's accessors (Kind, Branch,
// Value, Failure) and returns the tool's own attempt. The Go connector tool source runs it like any connector
// operation (connectortools.NewConnectorTool over sdkgo.NewMutationAgentTool).
//
// Why a Mutation of the application's and not the connector's own agent tool: the Go connector tool source offers an
// operation with that operation's input and result, and has no hook for a tool's own invoke, as the TypeScript
// core's connectorTool has; these tools take their own arguments, run their own checks and return their own results.
// A Go Invoke cannot return an error either, so a tool's refusal or definite failure is the failed branch, which the
// tools list as rejected, where the TypeScript tools throw a ToolExecutionError.

import (
	"encoding/json"
	"errors"
	"fmt"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/singledispatch"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/agents/go/core/connectortools"
)

// OutcomeOf reads one attempt of a connector operation as a tool does: its kind, branch, value as JSON, failure,
// retry delay and receipt. A value that does not encode as JSON is an error.
func OutcomeOf[OUT any](attempt sdkgo.MutationAttempt[OUT]) (ConnectorOutcome, error) {
	outcome := ConnectorOutcome{Kind: attempt.Kind(), Branch: attempt.Branch(), Failure: attempt.Failure(), RetryAfter: attempt.RetryAfter(), Receipt: attempt.Receipt()}
	if outcome.Kind == sdkgo.AttemptKindBranch || outcome.Kind == sdkgo.AttemptKindUncertain {
		value, err := json.Marshal(attempt.Value())
		if err != nil {
			return ConnectorOutcome{}, err
		}
		outcome.Value = value
	}
	return outcome, nil
}

// attempt is the outcome as the tool's own attempt, with the operation's branch, value, failure and receipt.
func (outcome ConnectorOutcome) attempt() sdkgo.MutationAttempt[json.RawMessage] {
	switch {
	case outcome.Kind == sdkgo.AttemptKindBranch:
		return sdkgo.NewMutationBranch(outcome.Branch, outcome.Value, outcome.Failure, outcome.Receipt)
	case outcome.Kind == sdkgo.AttemptKindUncertain && outcome.Failure != nil:
		return sdkgo.NewMutationUncertain(outcome.Value, *outcome.Failure, outcome.Receipt)
	case outcome.Kind == sdkgo.AttemptKindRetry && outcome.Failure != nil:
		return sdkgo.NewMutationRetry[json.RawMessage](*outcome.Failure, outcome.RetryAfter)
	default:
		// A zero attempt, which sdkgo records as the operation's defect.
		return sdkgo.MutationAttempt[json.RawMessage]{}
	}
}

// connectorWrite is the connector operation a tool writes with, on its connection, with the operation's input type
// erased.
type connectorWrite struct {
	definition     sdkgo.MutationDefinition
	connection     sdkgo.ConnectionRef
	idempotencyKey func(sdkgo.CallID) sdkgo.IdempotencyKey
	// invoke runs one attempt of the operation with the tool's Call and input, and reads it.
	invoke func(call sdkgo.Call, input any) (ConnectorOutcome, error)
}

// writeWith is operation on connection as a tool's connectorWrite. The idempotency key is the operation's for the
// Call ID, asked without an input as write() asks it in TypeScript: the Slack and Gmail operations derive it from the
// Call ID alone.
func writeWith[IN, OUT any](operation sdkgo.Mutation[IN, OUT], connection sdkgo.ConnectionRef) connectorWrite {
	return connectorWrite{
		definition: operation.Definition(), connection: connection,
		idempotencyKey: func(callID sdkgo.CallID) sdkgo.IdempotencyKey {
			var noInput IN
			return operation.IdempotencyKey(callID, noInput)
		},
		invoke: func(call sdkgo.Call, input any) (ConnectorOutcome, error) {
			return OutcomeOf(operation.Invoke(call, input.(IN)))
		},
	}
}

// FailedBranch is the branch of a tool's own refusal or definite failure, and the failure's message is the tool's
// text, such as "refused: call post_review first". It is not named refused: the source's text for it,
// "<connector> <operation> returned <branch>: ...", must not read as a refusal when the tool's text is another
// failure, such as send_welcome_email's "send_welcome_email failed and nothing was sent: ...". The tools list it as
// rejected, and the Go connector tool source records a branch as listed, also after a send in the same attempt (a
// decision's note, or a send Gmail rejected). sdkgo's own AgentToolAttempt.Outcome marks it unknown then, since the
// tool's Call is its write's and the fence saw the send; the source does not read that outcome.
const FailedBranch sdkgo.BranchID = "failed"

// signupMutation is one connector tool as an sdkgo Mutation over its connector write.
type signupMutation struct {
	tool       connectorTool
	definition sdkgo.MutationDefinition
	write      connectorWrite
}

func (mutation signupMutation) Definition() sdkgo.MutationDefinition { return mutation.definition }

func (mutation signupMutation) IdempotencyKey(callID sdkgo.CallID, _ map[string]any) sdkgo.IdempotencyKey {
	return mutation.write.idempotencyKey(callID)
}

func (mutation signupMutation) failure(kind sdkgo.FailureKind, message string) *sdkgo.Failure {
	operation := mutation.definition.Operation
	return &sdkgo.Failure{Kind: kind, Provider: operation.ConnectorID, Operation: operation.OperationID, Message: message}
}

// Invoke runs one attempt of the tool. Its connector write is the operation's one Invoke with this Call; a second
// would start the call's fence over, so the tool gets an error instead.
func (mutation signupMutation) Invoke(call sdkgo.Call, arguments map[string]any) sdkgo.MutationAttempt[json.RawMessage] {
	if problems := mutation.tool.validate(arguments); len(problems) > 0 {
		return mutation.refused(call, sdkgo.FailureValidation, fmt.Sprintf("invalid arguments for %s: %s", mutation.tool.name, joinProblems(problems)))
	}
	hasWritten := false
	write := func(input any) (ConnectorOutcome, error) {
		if hasWritten {
			return ConnectorOutcome{}, errors.New("a connector tool makes one connector write per attempt")
		}
		hasWritten = true
		return mutation.write.invoke(call, input)
	}
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
			return mutation.refused(call, kind, toolError.Message)
		case errors.As(err, &unknown):
			return sdkgo.NewMutationUncertain[json.RawMessage](nil, *mutation.failure(sdkgo.FailureAvailability, unknown.message), sdkgo.Receipt{})
		default:
			// A defect of the tool or its write: Dex records outcome_unknown, as the connector tools record a defect.
			return sdkgo.NewMutationBranch[json.RawMessage](sdkgo.DefectBranchID, nil, mutation.failure(sdkgo.FailureLocalDefect, messageOf(err)), sdkgo.Receipt{})
		}
	}
	if result.pass != nil {
		return result.pass.attempt()
	}
	value, err := json.Marshal(result.value)
	if err != nil {
		return sdkgo.NewMutationBranch[json.RawMessage](sdkgo.DefectBranchID, nil, mutation.failure(sdkgo.FailureLocalDefect, "the result does not encode as JSON"), sdkgo.Receipt{})
	}
	return sdkgo.NewMutationBranch(sentBranchOf(mutation.definition), json.RawMessage(value), nil, sdkgo.Receipt{})
}

// refused is the tool's own refusal or definite failure, with its text: the failed branch, or uncertain when an
// earlier attempt of the tool Step may have sent the call's write. A retried attempt runs the tool's checks again
// before its write, so a check that fails then, such as a ReviewAPI read while it is busy, says nothing about the
// earlier attempt's send. Recorded as failed, the model would read that nothing was sent, and a new call, with a new
// call identity whose fence knows none of the earlier sends, could post the review or send the email again.
//
// The Go connector tool source leaves this to the tool. singledispatch.MaximumPriorSends reads the bound on earlier
// sends that the write's fence computes, for the tool's own Call ID: the write is the operation's Invoke with the
// tool's Call, so its fence uses that ID (sdkgo.NextCallID would name another call). Attempt 1 has no earlier send. A
// retried attempt that finds no checkpoint counts one possible send per earlier attempt, as the fence does: Dex stores
// checkpoints batched and lossy, so attempt 1 may have sent and lost its claim. That is stricter than Dex Agents
// 0.6.14, which the TypeScript harnesses run (hasEarlierSendCheckpoint): the refusal is uncertain here, failed there.
// When the read fails, on a checkpoint that does not decode or an entry with an impossible send count, the bound is
// unknown and the refusal is uncertain too. sdkgo suggests a Retry then, but the checkpoint is the text the session
// repeats in every later attempt: each retry would read the same, and the write's fence would fail closed on it, until
// the attempts ran out and the session recorded outcome_unknown anyway.
func (mutation signupMutation) refused(call sdkgo.Call, kind sdkgo.FailureKind, message string) sdkgo.MutationAttempt[json.RawMessage] {
	if priorSends, err := singledispatch.MaximumPriorSends(call.Context, call.ID); err != nil || priorSends > 0 {
		return sdkgo.NewMutationUncertain[json.RawMessage](nil, *mutation.failure(kind, message+" (an earlier attempt of this call may have sent it)"), sdkgo.Receipt{})
	}
	return sdkgo.NewMutationBranch[json.RawMessage](FailedBranch, nil, mutation.failure(kind, message), sdkgo.Receipt{})
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

// sentBranchOf is the definition's required branch, "sent" for every operation the tools write with.
func sentBranchOf(definition sdkgo.MutationDefinition) sdkgo.BranchID {
	for _, branch := range definition.Branches {
		if !branch.Optional {
			return branch.ID
		}
	}
	return "sent"
}

// toolDefinition is the tool's definition: the operation's, with its identity, branches, Step defaults and send cap,
// and the failed branch.
func toolDefinition(operation sdkgo.MutationDefinition) sdkgo.MutationDefinition {
	definition := operation
	definition.Branches = append(append([]sdkgo.BranchDefinition(nil), operation.Branches...),
		sdkgo.BranchDefinition{ID: FailedBranch, Description: "The tool refused the call, or the call definitely failed.", Optional: true})
	return definition
}

// toolMetadata is the tool's agent metadata. Its branch outcomes are those of the TypeScript application's connector
// tools: a connector's defect is outcome_unknown, as Dex Agents 0.6.14 records it, so a write that may have gone out
// is never told to the model as failed.
func toolMetadata(definition sdkgo.MutationDefinition, description, schema string) sdkgo.AgentToolMetadata {
	return sdkgo.AgentToolMetadata{
		Operation: definition.Operation, Kind: sdkgo.AgentOperationKindMutation,
		ToolName:    connectortools.DefaultConnectorToolName(definition.Operation),
		Description: description, RepeatOnUnknown: definition.RepeatOnUnknown, MaxSends: definition.MaxSends,
		BranchOutcomes: map[sdkgo.BranchID]sdkgo.AgentBranchOutcome{
			"providerRejected": sdkgo.AgentBranchOutcomeRejected, sdkgo.UncertainBranchID: sdkgo.AgentBranchOutcomeUnknown,
			sdkgo.DefectBranchID: sdkgo.AgentBranchOutcomeUnknown, FailedBranch: sdkgo.AgentBranchOutcomeRejected,
		},
		InputType: "arguments", InputSchema: json.RawMessage(schema), Branches: definition.Branches, StepDefaults: definition.StepDefaults,
	}
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
	writes := map[string]connectorWrite{
		"postChannelMessage": writeWith(slackOperations.PostChannelMessage(), slackReference),
		"postThreadReply":    writeWith(slackOperations.PostThreadReply(), slackReference),
		"sendMessage":        writeWith(gmailOperations.SendMessage(), gmailReference),
	}
	var tools []SignupConnectorAgentTool
	for _, tool := range signupConnectorTools(services) {
		write := writes[tool.operationID]
		mutation := signupMutation{tool: tool, definition: toolDefinition(write.definition), write: write}
		agentTool, err := sdkgo.NewMutationAgentTool[map[string]any, json.RawMessage](
			mutation, write.connection, toolMetadata(mutation.definition, tool.description, tool.schema))
		if err != nil {
			return nil, fmt.Errorf("connector tool %s: %w", tool.name, err)
		}
		tools = append(tools, SignupConnectorAgentTool{Name: tool.name, Description: tool.description, AgentTool: agentTool})
	}
	return tools, nil
}
