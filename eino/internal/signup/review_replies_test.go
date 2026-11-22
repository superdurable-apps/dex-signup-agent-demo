package signup_test

// The reply target's routing, with a recording RPC invoker and a scripted thread index: which replies reach which
// session, which are dropped, and which wait for a retry. Port of test/review-replies.test.ts.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/sdk-go/dex"
	"google.golang.org/grpc/codes"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
)

func nowTS() string { return fmt.Sprintf("%d.000100", time.Now().Unix()) }

var disputedThread = fmt.Sprintf("%d.000900", time.Now().Unix())

func reply(threadTS string, extra func(*slack.MessageEvent)) sdkgo.TriggerEvent[slack.MessageEvent] {
	payload := slack.MessageEvent{TeamID: "T1", ChannelID: "CREVIEWS01", Timestamp: "1710000200.000200", ThreadTimestamp: threadTS, UserID: "UREVIEWER1", Text: "  approve \n"}
	if extra != nil {
		extra(&payload)
	}
	return sdkgo.TriggerEvent[slack.MessageEvent]{ID: "Ev-" + threadTS + "-" + payload.UserID, OccurredAt: time.Now(), Payload: payload}
}

type invoked struct {
	flowID    string
	input     any
	requestID string
}

type recordingInvoker struct {
	calls []invoked
	err   error
}

// InvokeRPCWithOptions records the call; the target delivers each event with a request ID derived from its event ID.
func (invoker *recordingInvoker) InvokeRPCWithOptions(_ context.Context, flowID string, _ any, input any, _ any, options dex.RPCInvokeOptions) error {
	if options.RequestID == nil || *options.RequestID == "" {
		return errors.New("no request ID")
	}
	invoker.calls = append(invoker.calls, invoked{flowID: flowID, input: input, requestID: *options.RequestID})
	return invoker.err
}

type scriptedIndex struct {
	threads map[string]string
	lookups []string
}

func (index *scriptedIndex) AccountOfThread(_ context.Context, threadTS string, _ time.Time) (string, error) {
	index.lookups = append(index.lookups, threadTS)
	if threadTS == disputedThread {
		return "", nil
	}
	return index.threads[threadTS], nil
}

func (index *scriptedIndex) IsThreadDisputed(threadTS string) bool { return threadTS == disputedThread }

type replyHarness struct {
	target  sdkgo.TriggerTarget[slack.MessageEvent]
	invoker *recordingInvoker
	index   *scriptedIndex
	logs    *signuptest.RecordingLogger
}

func newReplyHarness(threads map[string]string) replyHarness {
	harness := replyHarness{invoker: &recordingInvoker{}, index: &scriptedIndex{threads: threads}, logs: signuptest.NewRecordingLogger()}
	harness.target = signup.NewReviewReplyTarget(signup.ReviewReplyParts{
		Client: harness.invoker, SendUserMessage: "send_user_message", Sessions: harness.index,
		Channel: signup.ReviewChannel{ChannelID: "CREVIEWS01", ReviewerIDs: map[string]bool{"UREVIEWER1": true}},
		Logger:  harness.logs.Logger(),
	})
	return harness
}

func (harness replyHarness) count(message string) int {
	count := 0
	for _, record := range harness.logs.Find(message, "target", "rpc") {
		_ = record
		count++
	}
	return count
}

func TestAReviewersReplyInAReviewThreadGoesToThatReviewsSession(t *testing.T) {
	harness := newReplyHarness(map[string]string{"1710000100.000100": "acct1"})
	event := reply("1710000100.000100", nil)
	if err := harness.target.HandleTrigger(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	want := []invoked{{flowID: "signup-review-acct1", input: dexagents.AgentUserMessage{Text: "[slack 1710000200.000200] <@UREVIEWER1>: approve"}, requestID: signup.TriggerRPCRequestID("send_user_message", event.ID)}}
	if !reflect.DeepEqual(harness.invoker.calls, want) {
		t.Fatalf("invoked = %+v", harness.invoker.calls)
	}
	if got := signup.ReviewReplyText(event); got != "[slack 1710000200.000200] <@UREVIEWER1>: approve" {
		t.Fatalf("text = %q", got)
	}
}

func TestTheRequestIDIsTheTypeScriptConnectorSDKs(t *testing.T) {
	// The example of triggerRPCRequestId's documentation in @superdurable/connectors-sdk.
	if got := signup.TriggerRPCRequestID("receiveReply", "Ev1"); got != "trigger-rpc-kDFFz9I4ELDtzXkmvRTzlA" {
		t.Fatalf("request ID = %s", got)
	}
}

func TestAReplyOfSomeoneElseInAnotherChannelOrAtTheTopLevelIsFilteredWithoutALookup(t *testing.T) {
	harness := newReplyHarness(map[string]string{"1710000100.000100": "acct1"})
	for _, event := range []sdkgo.TriggerEvent[slack.MessageEvent]{
		reply("1710000100.000100", func(payload *slack.MessageEvent) { payload.UserID = "UBYSTANDER" }),
		reply("1710000100.000100", func(payload *slack.MessageEvent) { payload.ChannelID = "COTHER0001" }),
		reply("1710000200.000200", nil),
	} {
		if err := harness.target.HandleTrigger(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if len(harness.invoker.calls) != 0 || len(harness.index.lookups) != 0 {
		t.Fatalf("invoked %v, looked up %v", harness.invoker.calls, harness.index.lookups)
	}
	if got := harness.count("trigger event skipped: filtered"); got != 3 {
		t.Fatalf("filtered = %d", got)
	}
}

func TestAReplyInAnOldThreadThatIsNoRunningReviewsIsDroppedInARecentOneItWaitsForARetry(t *testing.T) {
	harness := newReplyHarness(map[string]string{})
	if err := harness.target.HandleTrigger(context.Background(), reply("1600000000.000100", nil)); err != nil {
		t.Fatal(err)
	}
	if got := harness.count("trigger event skipped: not a running review's thread"); got != 1 {
		t.Fatalf("skipped = %d", got)
	}
	// A review post younger than two minutes may not be in its session's records yet: the event stays in the inbox.
	err := harness.target.HandleTrigger(context.Background(), reply(nowTS(), nil))
	if err == nil || !strings.Contains(err.Error(), "is not a known review thread yet") || sdkgo.IsTriggerUndeliverable(err) {
		t.Fatalf("err = %v", err)
	}
	if len(harness.invoker.calls) != 0 {
		t.Fatalf("invoked = %v", harness.invoker.calls)
	}
}

func TestAReplyInAThreadTwoReviewsClaimIsDroppedAtOnceEvenWhenTheThreadIsRecent(t *testing.T) {
	harness := newReplyHarness(map[string]string{})
	if err := harness.target.HandleTrigger(context.Background(), reply(disputedThread, nil)); err != nil {
		t.Fatal(err)
	}
	if len(harness.invoker.calls) != 0 {
		t.Fatalf("invoked = %v", harness.invoker.calls)
	}
	if got := harness.count("trigger event skipped: two reviews claim the thread"); got != 1 {
		t.Fatalf("skipped = %d", got)
	}
}

func TestAReplyForAClosedSessionIsUndeliverableAndOtherDexErrorsAreRetried(t *testing.T) {
	harness := newReplyHarness(map[string]string{"1710000100.000100": "acct1"})
	harness.invoker.err = &dex.FlowNotActiveOrNotFoundError{ServiceError: &dex.ServiceError{Op: "InvokeRPC", Code: codes.NotFound}}
	if err := harness.target.HandleTrigger(context.Background(), reply("1710000100.000100", nil)); !sdkgo.IsTriggerUndeliverable(err) {
		t.Fatalf("closed session: err = %v", err)
	}
	harness.invoker.err = &dex.ServiceError{Op: "InvokeRPC", Code: codes.Unavailable, Detail: "no Worker"}
	if err := harness.target.HandleTrigger(context.Background(), reply("1710000100.000100", nil)); err == nil || sdkgo.IsTriggerUndeliverable(err) {
		t.Fatalf("unavailable: err = %v", err)
	}
	// The same event mapped to a different request is dropped with an error record.
	event := reply("1710000100.000100", nil)
	requestID := signup.TriggerRPCRequestID("send_user_message", event.ID)
	harness.invoker.err = &dex.ServiceError{Op: "InvokeRPC", Code: codes.InvalidArgument, Detail: "request ID " + requestID + " was already used for a different request"}
	if err := harness.target.HandleTrigger(context.Background(), event); !sdkgo.IsTriggerUndeliverable(err) {
		t.Fatalf("conflict: err = %v", err)
	}
	if got := harness.count("trigger rpc request conflict; event dropped"); got != 1 {
		t.Fatalf("conflict records = %d", got)
	}
}
