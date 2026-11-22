package signup

// The reviewers' replies: the Slack connector's threadReplyCreated Trigger delivers each reply of a listed reviewer in
// a review thread to that review's agent session, through the session Flow's send_user_message RPC. No other Flow.

import (
	"context"
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

// ThreadIndex maps a review thread to its account; ReviewSessions is the real one.
type ThreadIndex interface {
	AccountOfThread(ctx context.Context, threadTS string, since time.Time) (string, error)
	IsThreadDisputed(threadTS string) bool
}

// ReviewReplyParts is what the reply target reaches.
type ReviewReplyParts struct {
	Client *dex.Client
	// SendUserMessage is the session Flow's send_user_message RPC, flow.SendUserMessage.
	SendUserMessage dex.RPC[dexagents.AgentUserMessage, bool]
	Sessions        ThreadIndex
	Channel         ReviewChannel
	Logger          *slog.Logger
	// RecentThread: a reply in an unknown thread whose root is younger than this is retried rather than dropped, since
	// its review post may have gone out before post_review's result was stored. 2 minutes when zero.
	RecentThread time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// sessionTarget is the Connector SDK's Dex RPC target bound to one session: it sends the reply to send_user_message.
// Every delivery attempt of one event, such as a durable inbox replay or a Slack retry, carries the same request ID,
// sdkgo.TriggerRPCRequestID of the RPC's wire name and the event ID, as the TypeScript harnesses' target sends it, so
// Dex applies the reply at most once while the session keeps that request ID. send_user_message is transactional,
// so Dex answers a repeat with the first reply without running the RPC again. The target consumes an event for a
// closed or never-started session as undeliverable, and returns every other Dex error for a retry.
func sessionTarget(parts ReviewReplyParts, logger *slog.Logger, isReviewerReply sdkgo.TriggerFilter[slack.MessageEvent], sessionID string) sdkgo.TriggerTarget[slack.MessageEvent] {
	return sdkgo.NewDexRPCTriggerTarget(parts.Client, parts.SendUserMessage, isReviewerReply,
		func(sdkgo.TriggerEvent[slack.MessageEvent]) string { return sessionID },
		func(event sdkgo.TriggerEvent[slack.MessageEvent]) dexagents.AgentUserMessage {
			return dexagents.AgentUserMessage{Text: ReviewReplyText(event)}
		},
		sdkgo.WithTriggerRPCName(dexagents.SendUserMessageRPCName), sdkgo.WithTriggerLogger(logger))
}

// NewReviewReplyTarget is the target of the reply binding. A reply of a listed reviewer, in the review channel, in a
// thread of a running review, goes to that review's session; anything else is consumed without a delivery. The Slack
// connector already drops bot messages (any app's, this one's included), edits and other subtypes.
//
// The thread decides the session, and Trigger routing is synchronous, so this target looks the thread up first (see
// ReviewSessions.AccountOfThread) and then hands the event to a Dex RPC target bound to that session. A failed lookup
// returns an error and the event stays in the inbox for a retry; an event for a closed session is undeliverable and
// consumed by the RPC target; a thread two reviews claim is dropped.
func NewReviewReplyTarget(parts ReviewReplyParts) sdkgo.TriggerTarget[slack.MessageEvent] {
	return newReviewReplyTarget(parts, nil)
}

// newReviewReplyTarget is NewReviewReplyTarget, which delivers through deliverTo(sessionID) when it is not nil: a test
// sees the routing without a Dex server.
func newReviewReplyTarget(parts ReviewReplyParts, deliverTo func(sessionID string) sdkgo.TriggerTarget[slack.MessageEvent]) sdkgo.TriggerTarget[slack.MessageEvent] {
	logger := parts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// The RPC target adds target=rpc to its own records; these are the records of the routing in front of it.
	routingLogger := logger.With("target", "rpc")
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
	if deliverTo == nil {
		deliverTo = func(sessionID string) sdkgo.TriggerTarget[slack.MessageEvent] {
			return sessionTarget(parts, logger, isReviewerReply, sessionID)
		}
	}
	return sdkgo.TriggerTargetFunc[slack.MessageEvent](func(ctx context.Context, event sdkgo.TriggerEvent[slack.MessageEvent]) error {
		if !isReviewerReply(event) {
			routingLogger.InfoContext(ctx, "trigger event skipped: filtered", "event_id", event.ID)
			return nil
		}
		threadTS := event.Payload.ThreadTimestamp
		accountID, err := parts.Sessions.AccountOfThread(ctx, threadTS, now())
		if err != nil {
			return err
		}
		if accountID == "" {
			if parts.Sessions.IsThreadDisputed(threadTS) {
				routingLogger.WarnContext(ctx, "trigger event skipped: two reviews claim the thread", "event_id", event.ID, "thread_ts", threadTS)
				return nil
			}
			seconds, _ := strconv.ParseInt(strings.Split(threadTS, ".")[0], 10, 64)
			if now().Sub(time.Unix(seconds, 0)) < recentThread {
				return fmt.Errorf("thread %s is not a known review thread yet", threadTS)
			}
			// A warning: it may be a real reply in a review whose post_review result was lost.
			routingLogger.WarnContext(ctx, "trigger event skipped: not a running review's thread", "event_id", event.ID, "thread_ts", threadTS)
			return nil
		}
		return deliverTo(ReviewSessionID(accountID)).HandleTrigger(ctx, event)
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
