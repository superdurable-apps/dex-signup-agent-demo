package signup_test

// The reply target's routing, with a recording session target and a scripted thread index: which replies reach which
// session, which are dropped, and which wait for a retry. Port of test/review-replies.test.ts. The session target is
// the Connector SDK's Dex RPC target; the scenario suite delivers through it to real sessions.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
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

type delivered struct {
	sessionID string
	eventID   string
}

// recordingSessions stands in for the session targets: it records each delivery and returns err.
type recordingSessions struct {
	deliveries []delivered
	err        error
}

func (sessions *recordingSessions) deliverTo(sessionID string) sdkgo.TriggerTarget[slack.MessageEvent] {
	return sdkgo.TriggerTargetFunc[slack.MessageEvent](func(_ context.Context, event sdkgo.TriggerEvent[slack.MessageEvent]) error {
		sessions.deliveries = append(sessions.deliveries, delivered{sessionID: sessionID, eventID: event.ID})
		return sessions.err
	})
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
	target   sdkgo.TriggerTarget[slack.MessageEvent]
	sessions *recordingSessions
	index    *scriptedIndex
	logs     *signuptest.RecordingLogger
}

func newReplyHarness(threads map[string]string) replyHarness {
	harness := replyHarness{sessions: &recordingSessions{}, index: &scriptedIndex{threads: threads}, logs: signuptest.NewRecordingLogger()}
	harness.target = signup.NewReviewReplyTargetDeliveringTo(signup.ReviewReplyParts{
		Sessions: harness.index,
		Channel:  signup.ReviewChannel{ChannelID: "CREVIEWS01", ReviewerIDs: map[string]bool{"UREVIEWER1": true}},
		Logger:   harness.logs.Logger(),
	}, harness.sessions.deliverTo)
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
	if want := []delivered{{sessionID: "signup-review-acct1", eventID: event.ID}}; !reflect.DeepEqual(harness.sessions.deliveries, want) {
		t.Fatalf("delivered = %+v", harness.sessions.deliveries)
	}
	if got := signup.ReviewReplyText(event); got != "[slack 1710000200.000200] <@UREVIEWER1>: approve" {
		t.Fatalf("text = %q", got)
	}
}

func TestTheRequestIDIsTheTypeScriptConnectorSDKs(t *testing.T) {
	// The session target sends sdkgo.TriggerRPCRequestID of send_user_message and the event ID, the request ID the
	// TypeScript harnesses' target sends. This is the example of triggerRPCRequestId's documentation in
	// @superdurable/connectors-sdk; the scenario suite checks the ID of each delivery.
	if got, err := sdkgo.TriggerRPCRequestID("receiveReply", "Ev1"); err != nil || got != "trigger-rpc-kDFFz9I4ELDtzXkmvRTzlA" {
		t.Fatalf("request ID = %s, %v", got, err)
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
	if len(harness.sessions.deliveries) != 0 || len(harness.index.lookups) != 0 {
		t.Fatalf("delivered %v, looked up %v", harness.sessions.deliveries, harness.index.lookups)
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
	if len(harness.sessions.deliveries) != 0 {
		t.Fatalf("delivered = %v", harness.sessions.deliveries)
	}
}

func TestAReplyInAThreadTwoReviewsClaimIsDroppedAtOnceEvenWhenTheThreadIsRecent(t *testing.T) {
	harness := newReplyHarness(map[string]string{})
	if err := harness.target.HandleTrigger(context.Background(), reply(disputedThread, nil)); err != nil {
		t.Fatal(err)
	}
	if len(harness.sessions.deliveries) != 0 {
		t.Fatalf("delivered = %v", harness.sessions.deliveries)
	}
	if got := harness.count("trigger event skipped: two reviews claim the thread"); got != 1 {
		t.Fatalf("skipped = %d", got)
	}
}

func TestAReplyForAClosedSessionIsUndeliverableAndOtherDexErrorsAreRetried(t *testing.T) {
	// The session target classifies the Dex error (an event for a closed session is undeliverable, every other error
	// is retried), and the reply target returns its result as it is, so the durable inbox consumes or keeps the event.
	harness := newReplyHarness(map[string]string{"1710000100.000100": "acct1"})
	harness.sessions.err = sdkgo.MarkTriggerUndeliverable(&dex.FlowNotActiveOrNotFoundError{ServiceError: &dex.ServiceError{Op: "InvokeRPC", Code: codes.NotFound}})
	if err := harness.target.HandleTrigger(context.Background(), reply("1710000100.000100", nil)); !sdkgo.IsTriggerUndeliverable(err) {
		t.Fatalf("closed session: err = %v", err)
	}
	harness.sessions.err = &dex.ServiceError{Op: "InvokeRPC", Code: codes.Unavailable, Detail: "no Worker"}
	if err := harness.target.HandleTrigger(context.Background(), reply("1710000100.000100", nil)); err == nil || sdkgo.IsTriggerUndeliverable(err) {
		t.Fatalf("unavailable: err = %v", err)
	}
	if got := len(harness.sessions.deliveries); got != 2 {
		t.Fatalf("delivered %d times", got)
	}
}
