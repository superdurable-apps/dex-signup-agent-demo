package signup_test

// The provider stand-ins of the crash tests answer the real Go Slack and Gmail connectors: a post and its read-back,
// a send whose answer is lost and its Sent Mail search by Message-ID.

import (
	"context"
	"os"
	"testing"
	"time"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
	"github.com/superdurable/dex-connectors-library/sdkgo/singledispatchtest"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signuptest"
)

func TestTheProviderStandInsAnswerTheRealConnectors(t *testing.T) {
	events, gmailAPI := signuptest.StartSlackEvents(), signuptest.StartGmailAPI()
	defer events.Stop()
	defer gmailAPI.Stop()
	path, err := signuptest.ConnectionsFile(t.TempDir(), events.Endpoint(), gmailAPI.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	project, err := projectconfig.LoadDexWebConnectionsFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	slackConnection, err := slack.NewProjectConnection(project, signup.ReviewSlackConnection)
	if err != nil {
		t.Fatal(err)
	}
	gmailConnection, err := gmail.NewProjectConnection(project, signup.WelcomeGmailConnection)
	if err != nil {
		t.Fatal(err)
	}
	step := singledispatchtest.NewContext("signup-review-acct1", "step-1")
	posted, err := signup.RunConnectorAttempt(step, slackConnection.Operations().PostChannelMessage(), slackConnection.Reference(), slack.PostChannelMessageInput{ChannelID: "CREVIEWS01", Text: "review"}, 3)
	if err != nil || kindOf(posted) != "sent" {
		t.Fatalf("post = %+v, %v", posted, err)
	}
	if posts := events.Posts(); len(posts) != 1 || posts[0].ClientMsgID == "" {
		t.Fatalf("posts = %+v", posts)
	}
	send := singledispatchtest.NewContext("signup-review-acct1", "step-2")
	gmailAPI.HoldNextSend()
	done := make(chan signup.ConnectorOutcome, 1)
	cancel := send.SetDeadline(time.Now().Add(2 * time.Second))
	defer cancel()
	go func() {
		outcome, _ := signup.RunConnectorAttempt(send, gmailConnection.Operations().SendMessage(), gmailConnection.Reference(), gmail.SendMessageInput{To: []string{"ada@example.com"}, Subject: "Welcome", TextBody: "Hi"}, 3)
		done <- outcome
	}()
	var first signup.ConnectorOutcome
	select {
	case <-gmailAPI.Held():
		first = <-done
	case first = <-done:
		t.Fatalf("the send was not held: %+v %+v", first, first.Failure)
	}
	// The answer never came: the attempt searched Sent Mail by the message's Message-ID, found it, and never resent it.
	if kindOf(first) != "sent" {
		t.Fatalf("a held send = %+v %+v", first, first.Failure)
	}
	if sends := gmailAPI.Sends(); len(sends) != 1 || sends[0].MessageID == "" || sends[0].To == "" {
		t.Fatalf("sends = %+v", sends)
	}
}
