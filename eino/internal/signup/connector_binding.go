package signup

// How a connector tool runs its one fenced connector write inside its tool Step.

import (
	"encoding/json"
	"errors"

	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex/sdk-go/dex"
)

// RunConnectorAttempt runs one attempt of operation in the tool Step's context ctx, with the connector's call
// identity (derived from the Step's FlowID and StepExecutionID), its idempotency key and fence, and the call's send
// cap, and reads the attempt as a tool does: its kind, branch, value and failure. Go's sdkgo.MutationAttempt is
// opaque, so the attempt is read from the result that sdkgo.RunMutation returns.
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
