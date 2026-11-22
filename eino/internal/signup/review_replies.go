package signup

// The reviewers' replies: the Slack connector's threadReplyCreated Trigger delivers each reply of a listed reviewer in
// a review thread to that review's agent session, through the session Flow's send_user_message RPC. No other Flow.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig/provider"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/sdk-go/dex"
	"google.golang.org/grpc/codes"
)

// ReviewSlackConnection is the Slack connection the review posts, replies and Trigger use.
const ReviewSlackConnection = "review-slack"

// ReviewReplyBinding is the threadReplyCreated binding of that connection.
const ReviewReplyBinding = "signup-review-reply"

// WelcomeGmailConnection is the Gmail connection the welcome email goes out from.
const WelcomeGmailConnection = "welcome-gmail"

// ReplyTriggerKey is the reply binding's durable inbox key.
var ReplyTriggerKey = projectconfig.TriggerInboxKey{
	ConnectorID: slack.ConnectorID, ConnectionName: ReviewSlackConnection,
	TriggerName: slack.ThreadReplyCreatedTriggerDefinition.Trigger.TriggerName, BindingName: ReviewReplyBinding,
}

// ReviewChannel is the review channel and the Slack members whose replies count.
type ReviewChannel struct {
	ChannelID   string
	ReviewerIDs map[string]bool
}

// ReviewReplyText is the message a reply becomes in the session: "[slack <ts>] <@user>: <text>".
func ReviewReplyText(event sdkgo.TriggerEvent[slack.MessageEvent]) string {
	return fmt.Sprintf("[slack %s] <@%s>: %s", event.Payload.Timestamp, event.Payload.UserID, jsTrim(event.Payload.Text))
}

// RPCInvoker invokes one RPC with options: the Dex Client.
type RPCInvoker interface {
	InvokeRPCWithOptions(ctx context.Context, flowID string, rpc any, input any, outputPtr any, options dex.RPCInvokeOptions) error
}

// ThreadIndex maps a review thread to its account; ReviewSessions is the real one.
type ThreadIndex interface {
	AccountOfThread(ctx context.Context, threadTS string, since time.Time) (string, error)
	IsThreadDisputed(threadTS string) bool
}

// ReviewReplyParts is what the reply target reaches.
type ReviewReplyParts struct {
	Client RPCInvoker
	// SendUserMessage is the session Flow's send_user_message RPC, flow.SendUserMessage.
	SendUserMessage any
	Sessions        ThreadIndex
	Channel         ReviewChannel
	Logger          *slog.Logger
	// RecentThread: a reply in an unknown thread whose root is younger than this is retried rather than dropped, since
	// its review post may have gone out before post_review's result was stored. 2 minutes when zero.
	RecentThread time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// SendUserMessageRPCName is the wire name of the session Flow's send_user_message RPC, which feeds request IDs.
const SendUserMessageRPCName = "send_user_message"

// TriggerRPCRequestIDPrefix starts every request ID the reply target derives.
const TriggerRPCRequestIDPrefix = "trigger-rpc-"

// TriggerRPCRequestID is the Dex request ID of every delivery attempt of one event, as the TypeScript connector SDK's
// triggerRPCRequestId derives it: the prefix and the first 16 bytes, unpadded base64url, of the SHA-256 digest of the
// RPC name, one 0x00 byte, and the event ID.
func TriggerRPCRequestID(rpcName, eventID string) string {
	digest := sha256.Sum256([]byte(rpcName + "\x00" + eventID))
	return TriggerRPCRequestIDPrefix + base64.RawURLEncoding.EncodeToString(digest[:16])
}

// undeliverableTriggerMessage starts the message of an sdkgo UndeliverableTriggerError, which a handler can return.
const undeliverableTriggerMessage = "Trigger event is undeliverable"

// classifyDexRPCError marks the Dex errors that no retry of the same event can fix, as sdkgo's Dex targets do: a
// closed or never-started Flow, an RPC handler's own undeliverable error, and an input that cannot be encoded.
func classifyDexRPCError(err error) error {
	if err == nil || sdkgo.IsTriggerUndeliverable(err) {
		return err
	}
	var notActiveOrNotFound *dex.FlowNotActiveOrNotFoundError
	var notFound *dex.FlowNotFoundError
	if errors.As(err, &notActiveOrNotFound) || errors.As(err, &notFound) {
		return sdkgo.MarkTriggerUndeliverable(err)
	}
	var workerFailure *dex.WorkerInvocationError
	if errors.As(err, &workerFailure) {
		if workerFailure.Worker != nil && workerFailure.Worker.Code == codes.FailedPrecondition && strings.HasPrefix(workerFailure.Worker.Detail, undeliverableTriggerMessage) {
			return sdkgo.MarkTriggerUndeliverable(err)
		}
		return err
	}
	var mappingFailure *dex.ValueMappingError
	if errors.As(err, &mappingFailure) && mappingFailure.Operation == "encode" {
		return sdkgo.MarkTriggerUndeliverable(err)
	}
	return err
}

func isRequestIDUsedForDifferentRequest(err error, requestID string) bool {
	var serviceError *dex.ServiceError
	return errors.As(err, &serviceError) && serviceError.Code == codes.InvalidArgument &&
		strings.Contains(serviceError.Detail, "request ID "+requestID+" was already used for a different request")
}

// sessionRPCTarget invokes send_user_message on the resolved session with a request ID derived from the event ID, so
// a redelivered event (a durable inbox replay, a Slack retry) applies at most once while the session keeps that
// request ID. It is the Go counterpart of the TypeScript connector SDK's newDexRPCTriggerTarget; sdkgo's Go target
// sends no request ID.
// WORKAROUND(dex-bugs.md#sdkgo-dex-rpc-trigger-target-sends-no-request-id)
func sessionRPCTarget(parts ReviewReplyParts, logger *slog.Logger, sessionID string) sdkgo.TriggerTarget[slack.MessageEvent] {
	return sdkgo.TriggerTargetFunc[slack.MessageEvent](func(ctx context.Context, event sdkgo.TriggerEvent[slack.MessageEvent]) error {
		if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(event.ID) == "" {
			return sdkgo.MarkTriggerUndeliverable(errors.New("RPC trigger requires Flow ID and event ID"))
		}
		requestID := TriggerRPCRequestID(SendUserMessageRPCName, event.ID)
		var isQueued bool
		err := parts.Client.InvokeRPCWithOptions(ctx, sessionID, parts.SendUserMessage, dexagents.AgentUserMessage{Text: ReviewReplyText(event)}, &isQueued,
			dex.RPCInvokeOptions{RequestID: &requestID})
		if err == nil {
			logger.DebugContext(ctx, "trigger event delivered", "event_id", event.ID, "flow_id", sessionID, "request_id", requestID)
			return nil
		}
		var mappingFailure *dex.ValueMappingError
		if errors.As(err, &mappingFailure) && mappingFailure.Operation == "decode" {
			logger.WarnContext(ctx, "trigger event delivered; rpc response undecodable", "event_id", event.ID, "flow_id", sessionID, "request_id", requestID, "error", err.Error())
			return nil
		}
		if isRequestIDUsedForDifferentRequest(err, requestID) {
			logger.ErrorContext(ctx, "trigger rpc request conflict; event dropped", "event_id", event.ID, "request_id", requestID, "rpc_name", SendUserMessageRPCName, "flow_id", sessionID)
			return sdkgo.MarkTriggerUndeliverable(err)
		}
		return classifyDexRPCError(err)
	})
}

// NewReviewReplyTarget is the target of the reply binding. A reply of a listed reviewer, in the review channel, in a
// thread of a running review, goes to that review's session; anything else is consumed without a delivery. The Slack
// connector already drops bot messages (any app's, this one's included), edits and other subtypes.
//
// The thread decides the session, and Trigger routing is synchronous, so this target looks the thread up first (see
// ReviewSessions.AccountOfThread) and then hands the event to an RPC target bound to that session. A failed lookup
// returns an error and the event stays in the inbox for a retry; an event for a closed session is undeliverable and
// consumed; a thread two reviews claim is dropped.
func NewReviewReplyTarget(parts ReviewReplyParts) sdkgo.TriggerTarget[slack.MessageEvent] {
	logger := parts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("target", "rpc")
	recentThread := parts.RecentThread
	if recentThread == 0 {
		recentThread = 2 * time.Minute
	}
	now := parts.Now
	if now == nil {
		now = time.Now
	}
	// Deterministic and side-effect free, as a Trigger filter must be: the binding's own matcher, checked again.
	isReviewerReply := func(event sdkgo.TriggerEvent[slack.MessageEvent]) bool {
		message := event.Payload
		return message.ChannelID == parts.Channel.ChannelID && parts.Channel.ReviewerIDs[message.UserID] && message.ThreadTimestamp != "" && message.ThreadTimestamp != message.Timestamp
	}
	return sdkgo.TriggerTargetFunc[slack.MessageEvent](func(ctx context.Context, event sdkgo.TriggerEvent[slack.MessageEvent]) error {
		if !isReviewerReply(event) {
			logger.InfoContext(ctx, "trigger event skipped: filtered", "event_id", event.ID)
			return nil
		}
		threadTS := event.Payload.ThreadTimestamp
		accountID, err := parts.Sessions.AccountOfThread(ctx, threadTS, now())
		if err != nil {
			return err
		}
		if accountID == "" {
			if parts.Sessions.IsThreadDisputed(threadTS) {
				logger.WarnContext(ctx, "trigger event skipped: two reviews claim the thread", "event_id", event.ID, "thread_ts", threadTS)
				return nil
			}
			seconds, _ := strconv.ParseInt(strings.Split(threadTS, ".")[0], 10, 64)
			if now().Sub(time.Unix(seconds, 0)) < recentThread {
				return fmt.Errorf("thread %s is not a known review thread yet", threadTS)
			}
			// A warning: it may be a real reply in a review whose post_review result was lost.
			logger.WarnContext(ctx, "trigger event skipped: not a running review's thread", "event_id", event.ID, "thread_ts", threadTS)
			return nil
		}
		return sessionRPCTarget(parts, logger, ReviewSessionID(accountID)).HandleTrigger(ctx, event)
	})
}

// NewReviewReplyRunner is the runner of the reply binding on the review-slack connection of a local connections.json,
// configured from the environment. The connection's credential carries an app_token, so the runner uses Slack Socket
// Mode. Its events wait in the binding's durable inbox, under trigger-inboxes/ beside connections.json, so an event
// Slack saw acknowledged is not lost in a crash.
func NewReviewReplyRunner(project *projectconfig.LoadedProject, channel ReviewChannel, target sdkgo.TriggerTarget[slack.MessageEvent], logger *slog.Logger, options ...slack.Option) (*slack.MessageTriggerRunner, error) {
	if logger == nil {
		logger = slog.Default()
	}
	inbox, err := project.TriggerInbox(ReplyTriggerKey)
	if err != nil {
		return nil, err
	}
	durable, err := provider.NewDurableTriggerTarget(inbox, ReplyTriggerKey, target, provider.WithTriggerLogger(logger))
	if err != nil {
		return nil, err
	}
	connection, err := slack.NewProjectConnection(project, ReviewSlackConnection, append([]slack.Option{slack.WithLogger(logger)}, options...)...)
	if err != nil {
		return nil, err
	}
	reviewers := make([]string, 0, len(channel.ReviewerIDs))
	for id := range channel.ReviewerIDs {
		reviewers = append(reviewers, id)
	}
	return slack.NewMessageTriggerRunner(slack.MessageTriggerRunnerConfig{
		Connection: connection,
		ThreadReplyCreatedRoutes: []slack.ThreadReplyCreatedTriggerRoute{{
			BindingName:   ReviewReplyBinding,
			Configuration: slack.ThreadReplyCreatedTriggerConfiguration{ChannelID: channel.ChannelID, ThreadReplyMatcher: slack.MessageMatcher{PosterUserIDs: reviewers}},
			Target:        durable,
		}},
	})
}
