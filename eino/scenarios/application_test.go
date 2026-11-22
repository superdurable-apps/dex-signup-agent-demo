package scenarios_test

// The local process as cmd/signup-eino runs it: LoadApplication with the configuration from the environment, the
// Worker, and Start's watcher and reply Trigger, on the real Slack and Gmail connectors against the provider
// stand-ins. The counterpart of the TypeScript app test TestReviewIsApprovedAndSentFromSlackReplies.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/superdurable/dex/agents/go/core/agentstest"
	"github.com/superdurable/dex/sdk-go/dex"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
)

func TestTheLocalProcessReviewsApprovesAndSendsFromSlackReplies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine, err := agentstest.NewPrivateDexEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start the private Dex engine: %v", err)
	}
	defer engine.Kill()
	reviewAPI, events, gmailAPI := signuptest.StartReviewAPIStub(), signuptest.StartSlackEvents(), signuptest.StartGmailAPI()
	defer reviewAPI.Stop()
	defer events.Stop()
	defer gmailAPI.Stop()
	directory := t.TempDir()
	connections, err := signuptest.ConnectionsFile(directory, events.Endpoint(), gmailAPI.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	logs := signuptest.NewRecordingLogger()
	application, err := signup.LoadApplication(ctx, signup.ApplicationOptions{
		Environment: map[string]string{
			"REVIEW_API_URL": reviewAPI.BaseURL(), "REVIEW_API_KEY": reviewAPI.APIKey, "SIGNUP_REVIEW_SINCE": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"SIGNUP_WATCH_INTERVAL_SECONDS": "1", "SLACK_REVIEW_CHANNEL": channelID, "SIGNUP_REVIEWER_SLACK_IDS": reviewerID,
			"DEX_CONNECTOR_CONFIG_FILE": connections, "DEX_FLOW_SERVICE_ADDRESS": engine.Address(), "SIGNUP_AGENT_HARNESS": "eino",
			// The configured endpoint is never called: the scripted model replaces it.
			"SIGNUP_AGENT_EINO_BASE_URL": "https://llm.example.invalid/v1", "SIGNUP_AGENT_EINO_MODEL": "scripted", "SIGNUP_AGENT_EINO_API_KEY": "sk-test-not-a-real-key",
		},
		Profiles: signuptest.FixtureProfileSources{}, ChatModel: signuptest.NewScriptedChatModel(), TriggerLogger: logs.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, application.SessionFlow.GetFlowType(), "SignupReviewEinoAgentFlow")
	registry, err := dex.NewRegistry([]dex.Flow{application.SessionFlow})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := agentstest.OpenTestBlobCache(filepath.Join(directory, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	client, err := dex.NewClient(registry, cache, dex.ClientOptions{FlowServiceAddress: application.Config.Dex.ServerAddress})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	application.Connect(client)
	worker, err := dex.NewWorker(registry, cache, dex.WorkerOptions{FlowServiceAddress: engine.Address(), Pull: &dex.PullWorkerOptions{MaxConcurrentTasks: application.Config.WorkerSlots, StopDrainTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Start() }()
	defer func() {
		stopping, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = worker.Stop(stopping)
		<-workerDone
	}()
	registered, stopWaiting := context.WithTimeout(ctx, 30*time.Second)
	defer stopWaiting()
	if err := worker.WaitPullRegistered(registered); err != nil {
		t.Fatal(err)
	}
	running, err := application.Start(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	defer running.Stop()

	accountID := fmt.Sprintf("app%d", time.Now().UnixNano())
	sessionID := signup.ReviewSessionID(accountID)
	reviewAPI.AddPendingAccount(signuptest.PendingAccount{AccountID: accountID, Email: "ada-" + accountID + "@example.com", DisplayName: signuptest.Text("Ada Lovelace"), ProfileURL: signuptest.Text("https://github.com/ada-builds")})
	eventually := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(waitTimeout)
		for !condition() {
			select {
			case err := <-running.Done:
				t.Fatalf("the application stopped: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s\n%s", description, logs.Text())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	var root string
	// The watcher starts the review within its interval.
	eventually("the review post", func() bool {
		for _, post := range events.Posts() {
			if post.ThreadTS == "" && strings.Contains(post.Text, sessionID) {
				root = post.TS
				return true
			}
		}
		return false
	})
	replies := func() []string {
		var texts []string
		for _, post := range events.Posts() {
			if post.ThreadTS == root {
				texts = append(texts, post.Text)
			}
		}
		return texts
	}
	thread := signuptest.NewReviewThread(events, root)
	thread.Reply(reviewerID, "Shorter", nil)
	eventually("draft 2", func() bool { return countPrefix(replies(), "*[draft 2]*") == 1 })
	thread.Reply(reviewerID, "approve", nil)
	eventually("the approval", func() bool { return countPrefix(replies(), "Approved ") == 1 })
	equal(t, reviewAPI.AccountOf(accountID).Status, "active")
	thread.Reply(reviewerID, "send", nil)
	// finish_review ends the session after the send: it completes once that turn ends.
	eventually("the end of the review", func() bool {
		info, err := client.DescribeFlow(ctx, sessionID)
		return err == nil && info.Status != dex.FlowRunning
	})
	info, _ := client.DescribeFlow(ctx, sessionID)
	equal(t, info.Status, dex.FlowCompleted)
	sends := gmailAPI.Sends()
	equal(t, len(sends), 1)
	if !strings.Contains(sends[0].Raw, "Revised: shorter") {
		t.Fatalf("the email is not the latest draft:\n%s", sends[0].Raw)
	}
	equal(t, contains(replies(), fmt.Sprintf("Sent the welcome email to %s.", reviewAPI.AccountOf(accountID).Email)), true)
	snapshot, err := application.Services.Sessions.Snapshot(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	final := snapshot.Messages[len(snapshot.Messages)-1]
	equal(t, text(final.Text), "Done.")
}
