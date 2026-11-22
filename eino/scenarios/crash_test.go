package scenarios_test

// Crash tests: the Worker runs in a child process on the real Slack and Gmail Go connectors against local provider
// stand-ins, and the test SIGKILLs it during a RunModel, during post_review's Slack call, and during
// send_welcome_email's Gmail send. After a restart the review is posted once, the email is sent once, and the session
// goes on to its end.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/agents/go/core/agentstest"
	"github.com/superdurable/dex/sdk-go/dex"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
)

const (
	crashWorkerRole = "SIGNUP_EINO_TEST_WORKER"
	// The crash Worker's settings.
	crashAddress     = "SIGNUP_EINO_TEST_ADDRESS"
	crashReviewAPI   = "SIGNUP_EINO_TEST_REVIEW_API"
	crashReviewKey   = "SIGNUP_EINO_TEST_REVIEW_KEY"
	crashConnections = "SIGNUP_EINO_TEST_CONNECTIONS"
	crashDirectory   = "SIGNUP_EINO_TEST_DIRECTORY"
	crashGate        = "SIGNUP_EINO_TEST_GATE"
	// The gates: where the first such call of the child process stops, after it writes its marker.
	gateModel     = "model"
	gateSlackPost = "slack-post"
)

// marker is the file a gated call writes before it stops.
func marker(directory, gate string) string { return filepath.Join(directory, gate+".reached") }

// gatedChatModel stops its first model call at the gate, once per coordination directory.
type gatedChatModel struct {
	model.ToolCallingChatModel
	directory string
}

func (gated gatedChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	bound, err := gated.ToolCallingChatModel.WithTools(tools)
	return gatedChatModel{ToolCallingChatModel: bound, directory: gated.directory}, err
}

func (gated gatedChatModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	if reachGate(gated.directory, gateModel) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return gated.ToolCallingChatModel.Generate(ctx, input, options...)
}

func (gated gatedChatModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if reachGate(gated.directory, gateModel) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return gated.ToolCallingChatModel.Stream(ctx, input, options...)
}

// reachGate writes the gate's marker the first time and reports whether this call stops there.
func reachGate(directory, gate string) bool {
	file, err := os.OpenFile(marker(directory, gate), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

// gatedSlackOperations stop the first review post at the gate after Slack answered it: the post went out and the
// connector checkpointed its ts, and the tool Step has not stored its result yet.
type gatedSlackOperations struct {
	slack.Operations
	directory string
}

func (operations gatedSlackOperations) PostChannelMessage() sdkgo.Mutation[slack.PostChannelMessageInput, slack.PostMessageOutput] {
	return gatedPost{inner: operations.Operations.PostChannelMessage(), directory: operations.directory}
}

type gatedPost struct {
	inner     sdkgo.Mutation[slack.PostChannelMessageInput, slack.PostMessageOutput]
	directory string
}

func (post gatedPost) Definition() sdkgo.MutationDefinition { return post.inner.Definition() }

func (post gatedPost) IdempotencyKey(callID sdkgo.CallID, input slack.PostChannelMessageInput) sdkgo.IdempotencyKey {
	return post.inner.IdempotencyKey(callID, input)
}

func (post gatedPost) Invoke(call sdkgo.Call, input slack.PostChannelMessageInput) sdkgo.MutationAttempt[slack.PostMessageOutput] {
	attempt := post.inner.Invoke(call, input)
	if reachGate(post.directory, gateSlackPost) {
		<-call.Context.Done()
	}
	return attempt
}

// crashAgent is the session Flow both processes build, over the project's real connections.
func crashAgent(ctx context.Context, project *projectconfig.LoadedProject, services *signup.Services, chatModel model.ToolCallingChatModel, gate, directory string) (*dexagents.AgentSessionFlow, error) {
	slackConnection, err := slack.NewProjectConnection(project, signup.ReviewSlackConnection)
	if err != nil {
		return nil, err
	}
	if gate == gateSlackPost {
		if slackConnection, err = slack.NewConnectionWithOperations(gatedSlackOperations{Operations: slackConnection.Operations(), directory: directory}, slackConnection.Reference()); err != nil {
			return nil, err
		}
	}
	gmailConnection, err := gmail.NewProjectConnection(project, signup.WelcomeGmailConnection)
	if err != nil {
		return nil, err
	}
	return signup.NewEinoSignupAgent(services, slackConnection, gmailConnection, chatModel)
}

// runCrashWorker is the child process: it serves the session Flow until it is killed.
func runCrashWorker(gate string) int {
	ctx := context.Background()
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "crash worker:", err)
		return 1
	}
	directory := os.Getenv(crashDirectory)
	project, err := projectconfig.LoadDexWebConnectionsFile(ctx, os.Getenv(crashConnections))
	if err != nil {
		return fail(err)
	}
	reviewAPI, err := signup.NewReviewAPIClient(os.Getenv(crashReviewAPI), os.Getenv(crashReviewKey))
	if err != nil {
		return fail(err)
	}
	services := &signup.Services{Sessions: signup.NewReviewSessions(), ReviewAPI: reviewAPI, Profiles: signuptest.FixtureProfileSources{}, SlackChannelID: channelID, HarnessLabel: harnessName}
	var chatModel model.ToolCallingChatModel = signuptest.NewScriptedChatModel()
	if gate == gateModel {
		chatModel = gatedChatModel{ToolCallingChatModel: chatModel, directory: directory}
	}
	flow, err := crashAgent(ctx, project, services, chatModel, gate, directory)
	if err != nil {
		return fail(err)
	}
	registry, err := dex.NewRegistry([]dex.Flow{flow})
	if err != nil {
		return fail(err)
	}
	cache, err := agentstest.OpenTestBlobCache(filepath.Join(directory, fmt.Sprintf("client-cache-%d", os.Getpid())))
	if err != nil {
		return fail(err)
	}
	address := os.Getenv(crashAddress)
	client, err := dex.NewClient(registry, cache, dex.ClientOptions{FlowServiceAddress: address})
	if err != nil {
		return fail(err)
	}
	services.Sessions.Connect(client, flow.GetFlowType(), dexagents.NewAgentSessionClient(client, flow))
	if err := agentstest.ServeAgentWorker(ctx, []dex.Flow{flow}, address, filepath.Join(directory, fmt.Sprintf("worker-cache-%d", os.Getpid()))); err != nil {
		return fail(err)
	}
	return 0
}

// crashStack is one crash test's engine, stand-ins, reply runner and Worker child.
type crashStack struct {
	t          *testing.T
	ctx        context.Context
	engine     *agentstest.PrivateDexEngine
	reviewAPI  *signuptest.ReviewAPIStub
	events     *signuptest.SlackEvents
	gmailAPI   *signuptest.GmailAPI
	directory  string
	connection string
	flow       *dexagents.AgentSessionFlow
	client     *dex.Client
	sessions   *dexagents.AgentSessionClient
	worker     *agentstest.KillableProcess
	workers    int
	logs       *signuptest.RecordingLogger
	threads    map[string]*signuptest.ReviewThread
	started    []string
}

// thread is the review thread rooted at root, so each reply gets a later ts than the ones before it.
func (stack *crashStack) thread(root string) *signuptest.ReviewThread {
	if stack.threads[root] == nil {
		stack.threads[root] = signuptest.NewReviewThread(stack.events, root)
	}
	return stack.threads[root]
}

func startCrashStack(t *testing.T) *crashStack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stack := &crashStack{t: t, ctx: ctx, directory: t.TempDir(), logs: signuptest.NewRecordingLogger(), threads: map[string]*signuptest.ReviewThread{}}
	engine, err := agentstest.NewPrivateDexEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start the private Dex engine: %v", err)
	}
	stack.engine = engine
	t.Cleanup(engine.Kill)
	stack.reviewAPI = signuptest.StartReviewAPIStub()
	t.Cleanup(stack.reviewAPI.Stop)
	stack.events = signuptest.StartSlackEvents()
	t.Cleanup(stack.events.Stop)
	stack.gmailAPI = signuptest.StartGmailAPI()
	t.Cleanup(stack.gmailAPI.Stop)
	if stack.connection, err = signuptest.ConnectionsFile(stack.directory, stack.events.Endpoint(), stack.gmailAPI.Endpoint()); err != nil {
		t.Fatal(err)
	}
	project, err := projectconfig.LoadDexWebConnectionsFile(ctx, stack.connection)
	if err != nil {
		t.Fatal(err)
	}
	reviewAPI, err := signup.NewReviewAPIClient(stack.reviewAPI.BaseURL(), stack.reviewAPI.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	// The test process runs the watcher and the reply Trigger; its Flow only types the calls.
	services := &signup.Services{Sessions: signup.NewReviewSessions(), ReviewAPI: reviewAPI, Profiles: signuptest.FixtureProfileSources{}, SlackChannelID: channelID, HarnessLabel: harnessName}
	if stack.flow, err = crashAgent(ctx, project, services, signuptest.NewScriptedChatModel(), "", stack.directory); err != nil {
		t.Fatal(err)
	}
	registry, err := dex.NewRegistry([]dex.Flow{stack.flow})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := agentstest.OpenTestBlobCache(filepath.Join(stack.directory, "test-cache"))
	if err != nil {
		t.Fatal(err)
	}
	if stack.client, err = dex.NewClient(registry, cache, dex.ClientOptions{FlowServiceAddress: engine.Address()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stack.client.Close() })
	stack.sessions = dexagents.NewAgentSessionClient(stack.client, stack.flow)
	services.Sessions.Connect(stack.client, stack.flow.GetFlowType(), stack.sessions)
	target := signup.NewReviewReplyTarget(signup.ReviewReplyParts{Client: stack.client, SendUserMessage: stack.flow.SendUserMessage, Sessions: services.Sessions, Channel: channel, Logger: stack.logs.Logger()})
	runner, err := signup.NewReviewReplyRunner(project, channel, target, stack.logs.Logger())
	if err != nil {
		t.Fatal(err)
	}
	runnerContext, stopRunner := context.WithCancel(ctx)
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = runner.Run(runnerContext)
	}()
	t.Cleanup(func() {
		stopRunner()
		<-runnerDone
		if stack.worker != nil {
			stack.worker.Kill()
		}
		if t.Failed() {
			stack.dumpLogs()
		}
	})
	return stack
}

func (stack *crashStack) dumpLogs() {
	stack.t.Logf("Trigger records:\n%s", stack.logs.Text())
	for _, sessionID := range stack.started {
		dumpSession(stack.t, stack.ctx, stack.sessions, stack.engine, sessionID)
	}
	for index := 1; index <= stack.workers; index++ {
		if contents, err := os.ReadFile(filepath.Join(stack.directory, fmt.Sprintf("worker-%d.log", index))); err == nil {
			stack.t.Logf("worker %d log:\n%s", index, contents)
		}
	}
}

// startWorker starts the Worker child with gate, or none.
func (stack *crashStack) startWorker(gate string) {
	stack.t.Helper()
	stack.workers++
	worker := agentstest.NewKillableProcess(fmt.Sprintf("worker-%d", stack.workers), filepath.Join(stack.directory, fmt.Sprintf("worker-%d.log", stack.workers)))
	role := gate
	if role == "" {
		role = "plain"
	}
	environment := append(os.Environ(),
		crashWorkerRole+"="+role, crashAddress+"="+stack.engine.Address(), crashReviewAPI+"="+stack.reviewAPI.BaseURL(), crashReviewKey+"="+stack.reviewAPI.APIKey,
		crashConnections+"="+stack.connection, crashDirectory+"="+stack.directory, crashGate+"="+gate)
	if err := worker.Start(stack.ctx, []string{os.Args[0], "-test.run=^$"}, environment, agentstest.WorkerReadyLine, time.Minute); err != nil {
		stack.dumpLogs()
		stack.t.Fatalf("start the Worker: %v", err)
	}
	stack.worker = worker
}

func (stack *crashStack) killWorker() {
	stack.worker.Kill()
	stack.worker = nil
}

func (stack *crashStack) waitFor(description string, condition func() bool) {
	stack.t.Helper()
	deadline := time.Now().Add(2 * waitTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			stack.dumpLogs()
			stack.t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (stack *crashStack) addReview(emailWord string) (string, string) {
	stack.t.Helper()
	accountID := fmt.Sprintf("crash%d", time.Now().UnixNano())
	stack.reviewAPI.AddPendingAccount(signuptest.PendingAccount{
		AccountID: accountID, Email: emailWord + "@example.com", DisplayName: signuptest.Text("Ada Lovelace"), ProfileURL: signuptest.Text("https://github.com/ada-builds"),
	})
	reviewAPI, _ := signup.NewReviewAPIClient(stack.reviewAPI.BaseURL(), stack.reviewAPI.APIKey)
	if failed, err := signup.StartPendingReviews(stack.ctx, signup.SignupWatcherParts{Sessions: stack.sessions, ReviewAPI: reviewAPI, ReviewSince: time.Now().Add(-time.Hour)}); err != nil || len(failed) != 0 {
		stack.t.Fatalf("start the review: %v %v", failed, err)
	}
	stack.started = append(stack.started, signup.ReviewSessionID(accountID))
	return accountID, signup.ReviewSessionID(accountID)
}

// rootPosts are the review posts in the review channel; replies are the thread replies of root.
func (stack *crashStack) rootPosts() []signuptest.SlackPost {
	var posts []signuptest.SlackPost
	for _, post := range stack.events.Posts() {
		if post.ThreadTS == "" {
			posts = append(posts, post)
		}
	}
	return posts
}

func (stack *crashStack) replies(root string) []string {
	var texts []string
	for _, post := range stack.events.Posts() {
		if post.ThreadTS == root {
			texts = append(texts, post.Text)
		}
	}
	return texts
}

func (stack *crashStack) snapshot(sessionID string) dexagents.AgentSnapshot {
	snapshot, err := stack.sessions.GetAgentSnapshot(stack.ctx, sessionID)
	if err != nil {
		stack.t.Fatalf("snapshot: %v", err)
	}
	return snapshot
}

func (stack *crashStack) isWaiting(sessionID string, afterTurns int64) bool {
	snapshot, err := stack.sessions.GetAgentSnapshot(stack.ctx, sessionID)
	return err == nil && snapshot.State != nil && snapshot.State.Status == dexagents.AgentSessionStatusWaitingForUserMessage && snapshot.QueuedMessageCount == 0 && snapshot.State.TurnCount > afterTurns
}

func (stack *crashStack) reply(sessionID, root, replyText string) {
	stack.t.Helper()
	turns := int64(0)
	if state := stack.snapshot(sessionID).State; state != nil {
		turns = state.TurnCount
	}
	reply := stack.thread(root).Reply(reviewerID, replyText, nil)
	stack.waitFor("the reply "+reply.TS, func() bool {
		for _, message := range stack.snapshot(sessionID).Messages {
			if message.Role == dexagents.MessageRoleUser && strings.HasPrefix(text(message.Text), "[slack "+reply.TS+"]") {
				return true
			}
		}
		return false
	})
	stack.waitFor(sessionID+" handles "+replyText, func() bool { return stack.isWaiting(sessionID, turns) || !stack.isRunning(sessionID) })
}

func (stack *crashStack) isRunning(sessionID string) bool {
	info, err := stack.client.DescribeFlow(stack.ctx, sessionID)
	return err == nil && info.Status == dex.FlowRunning
}

func (stack *crashStack) finalAttempts(sessionID, stepType string) []int {
	stack.t.Helper()
	history, err := agentstest.ReadStepHistory(stack.ctx, stack.engine.Dexcli(), stack.engine.Address(), sessionID)
	if err != nil {
		stack.t.Fatalf("read the Step history: %v", err)
	}
	var attempts []int
	for _, entry := range history {
		if entry.EventType == agentstest.ExecuteCompletedEvent && entry.StepType == stepType {
			attempts = append(attempts, entry.FinalAttempt)
		}
	}
	return attempts
}

// approveAndSend finishes a posted review: approve, send, and the session's end.
func (stack *crashStack) approveAndSend(accountID, sessionID, root string) {
	stack.t.Helper()
	stack.reply(sessionID, root, "approve")
	if !contains(stack.replies(root), fmt.Sprintf("Approved Ada Lovelace (%s) on Dex Cloud. Reply `send` to email the welcome message.", stack.reviewAPI.AccountOf(accountID).Email)) {
		stack.t.Fatalf("replies = %q", stack.replies(root))
	}
	stack.reply(sessionID, root, "send")
	stack.waitFor(sessionID+" ended", func() bool {
		requireFinishAccepted(stack.t, stack.snapshot(sessionID))
		return !stack.isRunning(sessionID)
	})
	info, _ := stack.client.DescribeFlow(stack.ctx, sessionID)
	equal(stack.t, info.Status, dex.FlowCompleted)
}

func TestAWorkerKilledDuringARunModelRerunsOnlyThatCallAndTheReviewIsPostedOnce(t *testing.T) {
	stack := startCrashStack(t)
	stack.startWorker(gateModel)
	accountID, sessionID := stack.addReview("crashmodel")
	stack.waitFor("the first model call", func() bool { _, err := os.Stat(marker(stack.directory, gateModel)); return err == nil })
	stack.killWorker()
	equal(t, len(stack.rootPosts()), 0)
	stack.startWorker("")
	stack.waitFor("the review post", func() bool { return len(stack.rootPosts()) == 1 && stack.isWaiting(sessionID, 0) })
	root := stack.rootPosts()[0].TS
	// The killed RunModel ran again on the new Worker, as its second attempt.
	if attempts := stack.finalAttempts(sessionID, "RunModel"); len(attempts) == 0 || attempts[0] != 2 {
		t.Fatalf("RunModel attempts = %v", attempts)
	}
	stack.approveAndSend(accountID, sessionID, root)
	equal(t, len(stack.rootPosts()), 1)
	equal(t, len(stack.gmailAPI.Sends()), 1)
}

func TestAWorkerKilledDuringPostReviewsSlackCallPostsTheReviewOnce(t *testing.T) {
	stack := startCrashStack(t)
	stack.startWorker(gateSlackPost)
	accountID, sessionID := stack.addReview("crashpost")
	stack.waitFor("the review post's answer", func() bool { _, err := os.Stat(marker(stack.directory, gateSlackPost)); return err == nil })
	equal(t, len(stack.rootPosts()), 1)
	stack.killWorker()
	stack.startWorker("")
	stack.waitFor("the review", func() bool { return stack.isWaiting(sessionID, 0) })
	// The retried tool Step read the post back by its checkpointed ts and never posted it again.
	posts := stack.rootPosts()
	equal(t, len(posts), 1)
	if attempts := stack.finalAttempts(sessionID, "CallSingleAttemptTool"); len(attempts) == 0 || attempts[0] != 2 {
		t.Fatalf("CallSingleAttemptTool attempts = %v", attempts)
	}
	var results []string
	for _, message := range stack.snapshot(sessionID).Messages {
		if message.Role == dexagents.MessageRoleTool && text(message.ToolName) == "post_review" {
			results = append(results, text(message.ToolOutput))
		}
	}
	equal(t, len(results), 1)
	match(t, `"thread":\{"ts":"`+strings.ReplaceAll(posts[0].TS, ".", `\.`)+`"\}`, results[0])
	stack.approveAndSend(accountID, sessionID, posts[0].TS)
	equal(t, len(stack.rootPosts()), 1)
	equal(t, len(stack.gmailAPI.Sends()), 1)
}

func TestAWorkerKilledDuringSendWelcomeEmailSendsTheEmailOnce(t *testing.T) {
	stack := startCrashStack(t)
	stack.startWorker("")
	accountID, sessionID := stack.addReview("crashsend")
	stack.waitFor("the review post", func() bool { return len(stack.rootPosts()) == 1 && stack.isWaiting(sessionID, 0) })
	root := stack.rootPosts()[0].TS
	stack.reply(sessionID, root, "approve")
	equal(t, stack.reviewAPI.AccountOf(accountID).Status, "active")
	// Gmail gets the message and records it in Sent Mail, then its answer never comes: the Worker is killed.
	stack.gmailAPI.HoldNextSend()
	stack.thread(root).Reply(reviewerID, "send", nil)
	select {
	case <-stack.gmailAPI.Held():
	case <-time.After(2 * waitTimeout):
		stack.dumpLogs()
		t.Fatal("the send never reached Gmail")
	}
	stack.killWorker()
	stack.startWorker("")
	// The retried tool Step listed Sent Mail, found the copy by its X-Dex-Message-ID (Gmail replaced its Message-ID),
	// and never sent it again.
	stack.waitFor(sessionID+" ended", func() bool {
		requireFinishAccepted(t, stack.snapshot(sessionID))
		return !stack.isRunning(sessionID)
	})
	info, _ := stack.client.DescribeFlow(stack.ctx, sessionID)
	equal(t, info.Status, dex.FlowCompleted)
	sends := stack.gmailAPI.Sends()
	equal(t, len(sends), 1)
	equal(t, strings.Trim(sends[0].To, "<>"), stack.reviewAPI.AccountOf(accountID).Email)
	equal(t, sends[0].DexMessageID, sends[0].MessageID)
	if sends[0].SentMessageID == sends[0].MessageID || !contains(stack.gmailAPI.HeaderReads(), sends[0].GmailID) {
		t.Fatalf("the send was not read back by its X-Dex-Message-ID: %+v, header reads %q", sends[0], stack.gmailAPI.HeaderReads())
	}
	if !contains(stack.replies(root), fmt.Sprintf("Sent the welcome email to %s.", stack.reviewAPI.AccountOf(accountID).Email)) {
		t.Fatalf("replies = %q", stack.replies(root))
	}
	equal(t, len(stack.rootPosts()), 1)
	var sendAttempts []int
	history, err := agentstest.ReadStepHistory(stack.ctx, stack.engine.Dexcli(), stack.engine.Address(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range history {
		if entry.EventType == agentstest.ExecuteCompletedEvent && entry.StepType == "CallSingleAttemptTool" {
			sendAttempts = append(sendAttempts, entry.FinalAttempt)
		}
	}
	if !containsInt(sendAttempts, 2) {
		t.Fatalf("no tool Step ran a second attempt: %v", sendAttempts)
	}
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
