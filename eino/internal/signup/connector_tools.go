package signup

// The tools that post to Slack or send the email, as src/signup-agent.ts createSignupConnectorTools defines them: each
// call is one connector call in its own tool Step under the connector's call identity and dispatch fence, so a crash
// never posts or sends twice. Each Step makes at most one fenced connector write, since the writes of a Step share
// its one checkpoint (superdurable/dex-connectors-library#870). approve_account and reject_account are Slack thread
// replies: the ReviewAPI decision, a plain HTTP call without a fence, runs first in the same Step, and the reply says
// how it went. send_welcome_email's one write is the Gmail send; the model posts its outcome with reply_in_thread.
//
// This file holds what each tool decides; connector_binding.go runs it as a connector tool.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	dexagents "github.com/superdurable/dex/agents/go/core"
)

// ConnectorToolNames are the six connector tools.
var ConnectorToolNames = []string{"post_review", "post_draft", "reply_in_thread", "approve_account", "reject_account", "send_welcome_email"}

// SignupConnectorToolPolicies are the session tool policies of the connector tools: no approval, since the reviewers
// approve in Slack and the tools check it. They set no MaxSends, so each call keeps the cap the connector declares:
// Slack's postChannelMessage and postThreadReply and Gmail's sendMessage are repeatOnUnknown limited with maxSends 3,
// so after an unknown outcome the tool Step's retry reads back first and resends only while the call's sends stay
// below 3. The effect still comes from the manifest.
func SignupConnectorToolPolicies() map[string]dexagents.ToolPolicy {
	policies := map[string]dexagents.ToolPolicy{}
	for _, name := range ConnectorToolNames {
		requiresApproval := false
		policies[name] = dexagents.ToolPolicy{Effect: dexagents.ToolEffectNonIdempotentWrite, RequiresApproval: &requiresApproval}
	}
	return policies
}

// ConnectorOutcome is what a tool learns from one attempt of its connector operation.
type ConnectorOutcome struct {
	// Kind is "branch", "uncertain" or "retry".
	Kind       string
	Branch     sdkgo.BranchID
	Value      json.RawMessage
	Failure    *sdkgo.Failure
	RetryAfter time.Duration
}

func (outcome ConnectorOutcome) isSent() bool { return outcome.Kind == "branch" && outcome.Branch == "sent" }

// ConfirmationState is how a tool's own note in the review thread went: posted; notPosted, when nothing went out; or
// unknown, when it may have gone out. The model posts the outcome itself only for notPosted: a repost after unknown
// could duplicate the note.
type ConfirmationState string

// The confirmation states.
const (
	ConfirmationPosted    ConfirmationState = "posted"
	ConfirmationNotPosted ConfirmationState = "notPosted"
	ConfirmationUnknown   ConfirmationState = "unknown"
)

// ConfirmationStateOf is the state of a Slack postThreadReply attempt. Slack selects providerRejected only when no
// earlier send of the call is possible, and defect only before it dispatches, so both are notPosted. A retry may
// follow a send whose answer was lost as well as a request Slack never got, and the attempt does not tell them apart;
// it is unknown, like uncertain.
func ConfirmationStateOf(outcome ConnectorOutcome) ConfirmationState {
	if outcome.isSent() {
		return ConfirmationPosted
	}
	if outcome.Kind == "branch" && (outcome.Branch == "providerRejected" || outcome.Branch == sdkgo.DefectBranchID) {
		return ConfirmationNotPosted
	}
	return ConfirmationUnknown
}

// RepeatedConfirmation is the note's state a decision tool reports when the session already made that decision.
// posted stays posted. Anything else, a missing field of an older record included, is unknown: the model may or may
// not have posted the outcome itself since, as when its turn failed after the tool, so it must not post it again and
// asks the admins to check the thread.
func RepeatedConfirmation(earlier any) ConfirmationState {
	if earlier == string(ConfirmationPosted) {
		return ConfirmationPosted
	}
	return ConfirmationUnknown
}

// connectorResult is what one attempt of a connector tool returns: the operation's outcome as it is, or the sent
// branch with Value.
type connectorResult struct {
	pass  *ConnectorOutcome
	value any
}

func passOutcome(outcome ConnectorOutcome) connectorResult { return connectorResult{pass: &outcome} }
func sentValue(value any) connectorResult               { return connectorResult{value: value} }

// outcomeUnknownError makes the call's outcome unknown without a retry: a decision whose request went out while the
// ReviewAPI stayed busy or unreachable. The model gets only Dex's fixed unknown-outcome text.
type outcomeUnknownError struct{ message string }

func (err *outcomeUnknownError) Error() string { return err.message }

// connectorTool is one connector tool: its name, the connector operation it offers, its description and arguments,
// and what one attempt does. invoke runs the tool's one fenced connector write, inside its Step.
type connectorTool struct {
	name        string
	connector   string // "slack" or "gmail"
	operationID string
	description string
	schema      string
	validate    func(map[string]any) []string
	attempt     func(ctx context.Context, sessionID string, arguments map[string]any, invoke func(input any) (ConnectorOutcome, error)) (connectorResult, error)
}

// beforeDispatch runs the checks before a dispatch: a failure there is definite, since nothing was sent.
func beforeDispatch(err error) error {
	var toolError *dexagents.ToolExecutionError
	if err == nil || errors.As(err, &toolError) {
		return err
	}
	return &dexagents.ToolExecutionError{Message: "not sent: " + messageOf(err)}
}

// withExtra is the operation's value object with extra members.
func withExtra(value json.RawMessage, extra map[string]any) map[string]any {
	merged := map[string]any{}
	var members map[string]json.RawMessage
	if json.Unmarshal(value, &members) == nil {
		for key, member := range members {
			merged[key] = member
		}
	}
	for key, member := range extra {
		merged[key] = member
	}
	return merged
}

func parseObject(text string) map[string]any {
	var value map[string]any
	if json.Unmarshal([]byte(text), &value) != nil || value == nil {
		return map[string]any{}
	}
	return value
}

// isTransientReviewAPIError reports a busy or unreachable ReviewAPI: the request may or may not have been applied.
func isTransientReviewAPIError(err error) bool {
	var apiError *ReviewAPIError
	if errors.As(err, &apiError) {
		return apiError.HTTPStatus == 429 || apiError.HTTPStatus >= 500
	}
	var transport *reviewAPITransportError
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded)
}

// decisionRetryDelay is the wait before the next try of a decision; tests shorten it.
var decisionRetryDelay = func(attempt int) time.Duration { return time.Duration(500*attempt) * time.Millisecond }

// withRetries runs work again after a busy or unreachable ReviewAPI, up to 3 times: decisions carry stable request IDs.
func withRetries(ctx context.Context, work func() (Account, error)) (Account, error) {
	for attempt := 1; ; attempt++ {
		account, err := work()
		if err == nil {
			return account, nil
		}
		if !isTransientReviewAPIError(err) || attempt >= 3 || ctx.Err() != nil {
			return Account{}, err
		}
		timer := time.NewTimer(decisionRetryDelay(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return Account{}, err
		case <-timer.C:
		}
	}
}

// decisionOutcomeUnknown is a decision whose request was sent, and whose outcome a busy or unreachable ReviewAPI
// left unknown.
type decisionOutcomeUnknown struct{ message string }

func (err *decisionOutcomeUnknown) Error() string { return err.message }

// decide approves or rejects the account with the expected revision and a stable request ID, so a retry after an
// applied call that lost its answer finds the account already decided that way, which is success. An account already
// active counts as approved; an account decided the other way is a failure. When the request was sent and the
// ReviewAPI stays busy or unreachable, the outcome is unknown: *decisionOutcomeUnknown.
func decide(ctx context.Context, api *ReviewAPIClient, accountID, decision string) (Account, error) {
	target := "rejected"
	if decision == "approve" {
		target = "active"
	}
	isSent := false
	account, err := withRetries(ctx, func() (Account, error) {
		return decideOnce(ctx, api, accountID, decision, target, func() { isSent = true })
	})
	if err != nil && isSent && isTransientReviewAPIError(err) {
		return Account{}, &decisionOutcomeUnknown{message: messageOf(err)}
	}
	return account, err
}

// decideOnce is one try of decide; markSent runs before the decision request goes out.
func decideOnce(ctx context.Context, api *ReviewAPIClient, accountID, decision, target string, markSent func()) (Account, error) {
	account, err := api.GetAccount(ctx, accountID)
	if err != nil {
		return Account{}, err
	}
	if account.Status == "pending_approval" {
		request := DecisionRequest{AccountID: accountID, RequestID: ReviewSessionID(accountID) + "-" + decision, ExpectedRevision: account.Revision}
		markSent()
		var decided Account
		var decisionErr error
		if decision == "approve" {
			// The platform's own approval mail is left out once the ReviewAPI supports it; until then it refuses this.
			suppress := true
			request.SuppressApprovedMail = &suppress
			decided, decisionErr = api.ApproveAccount(ctx, request)
		} else {
			decided, decisionErr = api.RejectAccount(ctx, request)
		}
		if decisionErr == nil {
			account = decided
		} else {
			var apiError *ReviewAPIError
			if !errors.As(decisionErr, &apiError) || (apiError.Code != "revision_conflict" && apiError.Code != "failed_precondition") {
				return Account{}, decisionErr
			}
			if apiError.CurrentAccount != nil {
				account = *apiError.CurrentAccount
			} else if account, err = api.GetAccount(ctx, accountID); err != nil {
				return Account{}, err
			}
			if apiError.Code == "failed_precondition" && account.Status != target {
				return Account{}, fmt.Errorf("the review API refused the decision: %s", apiError.Message)
			}
		}
	}
	if account.Status != target {
		return Account{}, fmt.Errorf("the account is %s", account.Status)
	}
	return account, nil
}

// confirm posts an outcome in the review thread, best effort: a note that is not posted only logs a warning and never
// changes what the tool did. post makes the tool's one connector write, inside its Step.
//
// A Slack retry here is not taken: the tool's own result decides whether its Step runs again, and running it again
// for its note alone would repeat the decision before it. So the note gets one attempt, and the result says how it
// went, for the model to post the outcome itself only when nothing went out.
func confirm(channelID string, thread ReviewThread, text string, invoke func(input any) (ConnectorOutcome, error)) ConfirmationState {
	outcome, err := invoke(slack.PostThreadReplyInput{ChannelID: channelID, ThreadTimestamp: thread.TS, Text: text})
	if err != nil {
		slog.Warn(fmt.Sprintf("signup review: the note in thread %s is unknown: %s", thread.TS, messageOf(err)))
		return ConfirmationUnknown
	}
	state := ConfirmationStateOf(outcome)
	if state != ConfirmationPosted {
		slog.Warn(fmt.Sprintf("signup review: the note in thread %s is %s", thread.TS, state))
	}
	return state
}

// The JSON Schemas of the connector tools' arguments, limited to the keywords Dex checks, as toolJsonSchema writes
// them; validate checks the rest when the tool runs.
const (
	reviewSchema = `{"type":"object","properties":{"recommendation":{"type":"string","enum":["approve","reject"]},"reasons":{"type":"array","items":{"type":"string"},"description":"Facts you found, or what was missing."},"emailSubject":{"type":"string"},"emailBody":{"type":"string"}},"required":["recommendation","reasons","emailSubject","emailBody"],"additionalProperties":false}`
	draftSchema  = `{"type":"object","properties":{"subject":{"type":"string"},"body":{"type":"string"}},"required":["subject","body"],"additionalProperties":false}`
	replySchema  = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`
	rejectSchema = `{"type":"object","properties":{"reason":{"type":"string","description":"The reviewer's reason for the rejection."}},"required":["reason"],"additionalProperties":false}`
	emptySchema  = `{"type":"object","properties":{},"additionalProperties":false}`
)

var (
	validateReview = objectSchema(enumField("recommendation", "approve", "reject"), stringArrayField("reasons", 1, 6, 1, 500), stringField("emailSubject", 1, 200), stringField("emailBody", 1, 8_000))
	validateDraft  = objectSchema(stringField("subject", 1, 200), stringField("body", 1, 8_000))
	validateReply  = objectSchema(stringField("text", 1, 3_000))
	validateReject = objectSchema(stringField("reason", 1, 500))
	validateEmpty  = objectSchema()
)

func reviewOf(arguments map[string]any) ReviewInput {
	review := ReviewInput{Recommendation: arguments["recommendation"].(string), EmailSubject: arguments["emailSubject"].(string), EmailBody: arguments["emailBody"].(string)}
	for _, reason := range arguments["reasons"].([]any) {
		review.Reasons = append(review.Reasons, reason.(string))
	}
	return review
}

// postMessageTimestamp is the message timestamp of a Slack post's value.
func postMessageTimestamp(value json.RawMessage) string {
	var output slack.PostMessageOutput
	_ = json.Unmarshal(value, &output)
	return output.Message.Timestamp
}

type emailRecord struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type draftNumber struct {
	Number int `json:"number"`
}

// signupConnectorTools are the connector tools' decisions over services.
func signupConnectorTools(services *Services) []connectorTool {
	sessions := services.Sessions
	channelID := services.SlackChannelID
	postReview := func(ctx context.Context, sessionID string, arguments map[string]any, invoke func(any) (ConnectorOutcome, error)) (connectorResult, error) {
		accountID := AccountIDOf(sessionID)
		review := reviewOf(arguments)
		text, err := func() (string, error) {
			existing, err := sessions.Review(ctx, accountID)
			if err != nil {
				return "", err
			}
			if existing.Thread != nil {
				return "", refusal(fmt.Sprintf("the review is already posted (thread %s); use post_draft or reply_in_thread", existing.Thread.TS))
			}
			// A post_review whose outcome stayed unknown after the connector's read-back and resends may have posted the
			// review. A new call is a new call identity whose fence knows none of those sends, so it could post twice.
			if existing.IsReviewPosted {
				return "", refusal("an earlier post_review may have posted the review (its outcome stayed unknown after the connector checked and tried up to 3 times); never post it again, the admins decide in Dex Cloud admin")
			}
			account, err := services.ReviewAPI.GetAccount(ctx, accountID)
			if err != nil {
				return "", err
			}
			return ReviewPost(account, review, services.HarnessLabel, ReviewSessionID(accountID)), nil
		}()
		if err := beforeDispatch(err); err != nil {
			return connectorResult{}, err
		}
		outcome, err := invoke(slack.PostChannelMessageInput{ChannelID: channelID, Text: text})
		if err != nil {
			return connectorResult{}, err
		}
		if !outcome.isSent() {
			return passOutcome(outcome), nil
		}
		thread := ReviewThread{TS: postMessageTimestamp(outcome.Value)}
		// The result is the session's record of the review: its thread, which the other tools and the reply Trigger
		// read, and draft 1. This process knows the thread at once; another one looks it up in this record.
		sessions.RememberThread(thread.TS, accountID)
		return sentValue(withExtra(outcome.Value, map[string]any{
			"thread": map[string]string{"ts": thread.TS}, "draft": draftNumber{Number: 1}, "email": emailRecord{Subject: review.EmailSubject, Body: review.EmailBody},
		})), nil
	}

	postDraft := func(ctx context.Context, sessionID string, arguments map[string]any, invoke func(any) (ConnectorOutcome, error)) (connectorResult, error) {
		accountID := AccountIDOf(sessionID)
		draft := emailRecord{Subject: arguments["subject"].(string), Body: arguments["body"].(string)}
		var thread ReviewThread
		var number int
		var isApproved bool
		err := func() error {
			record, err := sessions.Review(ctx, accountID)
			if err != nil {
				return err
			}
			if record.Thread == nil {
				return refusal("call post_review first")
			}
			thread = *record.Thread
			if record.LatestDraft != nil && record.LatestDraft.Subject == draft.Subject && record.LatestDraft.Body == draft.Body {
				return refusal(fmt.Sprintf("this draft is the same as draft %d, already posted; wait for the reviewers' feedback", record.LatestDraft.Number))
			}
			// The footer follows the account as it is now; a failed read gives the wording for a pending account.
			account, err := services.ReviewAPI.GetAccount(ctx, accountID)
			isApproved = err == nil && account.Status == "active"
			number = 2
			if record.LatestDraft != nil {
				number = record.LatestDraft.Number + 1
			}
			return nil
		}()
		if err := beforeDispatch(err); err != nil {
			return connectorResult{}, err
		}
		outcome, err := invoke(slack.PostThreadReplyInput{ChannelID: channelID, ThreadTimestamp: thread.TS, Text: DraftPost(number, draft.Subject, draft.Body, isApproved)})
		if err != nil {
			return connectorResult{}, err
		}
		if !outcome.isSent() {
			return passOutcome(outcome), nil
		}
		// The result is the session's record of the draft, the one send_welcome_email sends until a newer one.
		return sentValue(withExtra(outcome.Value, map[string]any{"draft": draftNumber{Number: number}, "email": draft})), nil
	}

	replyInThread := func(ctx context.Context, sessionID string, arguments map[string]any, invoke func(any) (ConnectorOutcome, error)) (connectorResult, error) {
		record, err := sessions.Review(ctx, AccountIDOf(sessionID))
		if err == nil && record.Thread == nil {
			err = refusal("call post_review first")
		}
		if err := beforeDispatch(err); err != nil {
			return connectorResult{}, err
		}
		outcome, err := invoke(slack.PostThreadReplyInput{ChannelID: channelID, ThreadTimestamp: record.Thread.TS, Text: EscapeSlack(arguments["text"].(string))})
		if err != nil {
			return connectorResult{}, err
		}
		return passOutcome(outcome), nil
	}

	// decisionTool is approve_account and reject_account: the decision, then its confirmation or failure as this
	// call's thread reply.
	decisionTool := func(decision string) func(context.Context, string, map[string]any, func(any) (ConnectorOutcome, error)) (connectorResult, error) {
		return func(ctx context.Context, sessionID string, arguments map[string]any, invoke func(any) (ConnectorOutcome, error)) (connectorResult, error) {
			accountID := AccountIDOf(sessionID)
			reason := ""
			if decision == "reject" {
				reason = arguments["reason"].(string)
			}
			var thread ReviewThread
			var earlier string
			var hasEarlier bool
			var current Account
			err := func() error {
				record, err := sessions.Review(ctx, accountID)
				if err != nil {
					return err
				}
				if record.Thread == nil {
					return refusal("call post_review first")
				}
				thread = *record.Thread
				earlier, hasEarlier = SucceededOutput(record.History.Records, decision+"_account")
				if hasEarlier {
					// A read only: the account as it is now decides what an earlier success still means.
					current, err = services.ReviewAPI.GetAccount(ctx, accountID)
				}
				return err
			}()
			if err := beforeDispatch(err); err != nil {
				return connectorResult{}, err
			}
			if hasEarlier {
				switch EarlierDecision(decision, current.Status) {
				case "same":
					// This decision already succeeded in this session, as after a failed turn: no second request or
					// confirmation.
					result := parseObject(earlier)
					confirmation := result["confirmation"]
					delete(result, "confirmation")
					delete(result, "confirmationPosted")
					result["accountStatus"] = current.Status
					result["confirmation"] = RepeatedConfirmation(confirmation)
					result["note"] = "already done earlier in this review; nothing was posted again"
					return sentValue(result), nil
				case "changed":
					return sentValue(map[string]any{
						"status": "changed", "accountStatus": current.Status,
						"note": fmt.Sprintf("this review already made this decision, and the account is now %s in Dex Cloud, decided there since; nothing was requested or posted", current.Status),
					}), nil
				}
			}
			account, err := decide(ctx, services.ReviewAPI, accountID, decision)
			if err != nil {
				noun := "Rejection"
				if decision == "approve" {
					noun = "Approval"
				}
				var unknown *decisionOutcomeUnknown
				if errors.As(err, &unknown) {
					// The request went out and the ReviewAPI stayed busy: the account may be decided. Dex records
					// outcome_unknown and never retries the call, and the model is told its outcome is unknown. The model
					// gets only Dex's fixed unknown-outcome text, so the note's state cannot reach it: the prompt has it
					// treat this note as unknown.
					confirm(channelID, thread, fmt.Sprintf("%s outcome unknown: %s. Check the account in Dex Cloud admin; the review continues.", noun, oneLineText(unknown.message, "unknown error")), invoke)
					return connectorResult{}, &outcomeUnknownError{message: fmt.Sprintf("%s_account outcome unknown: %s", decision, unknown.message)}
				}
				state := confirm(channelID, thread, fmt.Sprintf("%s failed: %s.", noun, oneLineText(messageOf(err), "unknown error")), invoke)
				return connectorResult{}, &dexagents.ToolExecutionError{Message: fmt.Sprintf("%s_account failed: %s (thread note: %s)", decision, messageOf(err), state)}
			}
			text := fmt.Sprintf("Rejected %s: %s.", applicantLine(account), oneLineText(reason, "no reason given"))
			status := "rejected"
			if decision == "approve" {
				text = fmt.Sprintf("Approved %s on Dex Cloud. Reply `send` to email the welcome message.", applicantLine(account))
				status = "approved"
			}
			confirmation := confirm(channelID, thread, text, invoke)
			return sentValue(map[string]any{"status": status, "accountStatus": account.Status, "confirmation": confirmation}), nil
		}
	}

	sendWelcomeEmail := func(ctx context.Context, sessionID string, _ map[string]any, invoke func(any) (ConnectorOutcome, error)) (connectorResult, error) {
		accountID := AccountIDOf(sessionID)
		var draft Draft
		var account Account
		err := func() error {
			record, err := sessions.Review(ctx, accountID)
			if err != nil {
				return err
			}
			if record.Thread == nil {
				return refusal("call post_review first")
			}
			if record.LatestDraft == nil {
				return refusal("no draft is posted")
			}
			draft = *record.LatestDraft
			if account, err = services.ReviewAPI.GetAccount(ctx, accountID); err != nil {
				return err
			}
			if account.Status != "active" {
				return refusal(fmt.Sprintf("the account is %s, not approved", account.Status))
			}
			// The session's own records say whether this draft was sent already, for example by a turn that failed
			// after the send, or may have been: a send whose outcome stayed unknown after Gmail's read-back and
			// resends. A new call is a new call identity whose fence knows none of those sends, so it could email the
			// applicant twice. Time travel of the session rewinds the records, so a deliberate re-send stays possible.
			switch DraftSendOf(record.History, draft.Number) {
			case DraftSent:
				return refusal(fmt.Sprintf("the welcome email for draft %d was already sent in this review", draft.Number))
			case DraftSendUnknown:
				return refusal(fmt.Sprintf("the welcome email for draft %d may already have been sent in this review: an earlier send's outcome stayed unknown after Gmail checked Sent Mail and tried up to 3 times; the admins check Sent Mail", draft.Number))
			}
			return nil
		}()
		if err := beforeDispatch(err); err != nil {
			return connectorResult{}, err
		}
		// The Gmail send is this Step's one fenced connector write; the tool posts nothing in the thread. A second
		// fenced write in the Step would share its one checkpoint with the send
		// (superdurable/dex-connectors-library#870), so the model posts the outcome with reply_in_thread, a tool call
		// with its own Step and fence.
		outcome, err := invoke(gmail.SendMessageInput{To: []string{account.Email}, Subject: draft.Subject, TextBody: draft.Body})
		if err != nil {
			return connectorResult{}, err
		}
		if outcome.Kind == "retry" {
			// An unknown outcome below the cap, or a send Gmail asked to retry: the Step's next attempt reads back Sent
			// Mail and resends only while the sends stay below the cap.
			return passOutcome(outcome), nil
		}
		if outcome.Kind == "branch" && outcome.Branch == "providerRejected" {
			// Gmail conclusively rejected the message, so nothing was sent. Raised here, it is recorded as failed.
			reason := "Gmail rejected the message"
			if outcome.Failure != nil {
				reason = fmt.Sprintf("%s (%s)", outcome.Failure.Message, outcome.Failure.Kind)
			}
			return connectorResult{}, &dexagents.ToolExecutionError{Message: "send_welcome_email failed and nothing was sent: " + reason}
		}
		if !outcome.isSent() {
			// Unknown after the cap, or a Gmail defect: Dex records outcome_unknown, and the model gets only Dex's fixed
			// unknown-outcome text; the prompt tells it to send the admins to Sent Mail.
			return passOutcome(outcome), nil
		}
		return sentValue(withExtra(outcome.Value, map[string]any{"draft": draftNumber{Number: draft.Number}})), nil
	}

	return []connectorTool{
		{name: "post_review", connector: "slack", operationID: "postChannelMessage", description: "Post the review to the admins' Slack channel. Once per review.", schema: reviewSchema, validate: validateReview, attempt: postReview},
		{name: "post_draft", connector: "slack", operationID: "postThreadReply", description: "Post a new welcome email draft in the review thread.", schema: draftSchema, validate: validateDraft, attempt: postDraft},
		{name: "reply_in_thread", connector: "slack", operationID: "postThreadReply", description: "Post a short note in the review thread.", schema: replySchema, validate: validateReply, attempt: replyInThread},
		{name: "approve_account", connector: "slack", operationID: "postThreadReply", description: "Approve the account in Dex Cloud; the tool posts the outcome in the review thread.", schema: emptySchema, validate: validateEmpty, attempt: decisionTool("approve")},
		{name: "reject_account", connector: "slack", operationID: "postThreadReply", description: "Reject the account in Dex Cloud with the reviewer's reason; the tool posts the outcome in the review thread.", schema: rejectSchema, validate: validateReject, attempt: decisionTool("reject")},
		{name: "send_welcome_email", connector: "gmail", operationID: "sendMessage", description: "Send the latest posted draft to the applicant once the account is approved; the tool posts the outcome in the review thread.", schema: emptySchema, validate: validateEmpty, attempt: sendWelcomeEmail},
	}
}
