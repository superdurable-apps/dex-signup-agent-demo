package signuptest

// A scripted model that follows the agent's procedure, shared by the harness tests, and fixed profiles: the port of
// test/support/scripted-signup-agent.ts.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

// ScriptedCall is one tool call of a scripted response.
type ScriptedCall struct {
	Name      string
	Arguments map[string]any
}

// ScriptedStep is one scripted model response: an answer, tool calls (several in one response when asked), or a
// failed call.
type ScriptedStep struct {
	// Kind is "answer", "tools" or "fail".
	Kind  string
	Text  string
	Calls []ScriptedCall
}

// SeenResult is a tool result the model has seen since the last user message.
type SeenResult struct {
	Name   string
	Output string
}

// The applicant words that steer the scripted model, by the applicant's email.
const (
	// RejectedApplicantWord gets a reject recommendation.
	RejectedApplicantWord = "spammy"
	// PassiveApplicantWord makes the model only note Slack replies, never act on them.
	PassiveApplicantWord = "passive"
	// MultiCallApplicantWord makes the model post a draft and a note in one response.
	MultiCallApplicantWord = "multi"
	// FailAfterApproveWord makes the model call right after the first approve_account fail.
	FailAfterApproveWord = "failafterapprove"
	// FailAfterSendWord makes the model call right after the first send_welcome_email fail.
	FailAfterSendWord = "failaftersend"
	// StayAfterSendWord makes the model only send on "send", and keep the review open.
	StayAfterSendWord = "stayaftersend"
)

var rejectedReason = regexp.MustCompile(`send_welcome_email failed and nothing was sent: (.*)$`)

// SendOutcomeNote is the model's note of a send_welcome_email result, or "" for a refusal.
func SendOutcomeNote(output, email string) string {
	if strings.Contains(output, "refused: ") {
		return ""
	}
	if strings.Contains(output, `"messageId"`) {
		return "Sent the welcome email to " + email + "."
	}
	if match := rejectedReason.FindStringSubmatch(output); match != nil {
		return "Sending the welcome email failed: " + match[1] + ". Nothing was sent."
	}
	return "The welcome email's outcome is unknown: it may have gone out. Check Sent Mail before sending it by hand."
}

// follow runs each response of plan in turn, skipping the calls already made since the user message, then answers.
func follow(plan []ScriptedStep, seen []SeenResult) ScriptedStep {
	made := 0
	for _, step := range plan {
		if made >= len(seen) {
			return step
		}
		if step.Kind == "tools" {
			made += len(step.Calls)
		} else {
			made++
		}
	}
	return ScriptedStep{Kind: "answer", Text: "Done."}
}

func tool(name string, arguments map[string]any) ScriptedStep {
	if arguments == nil {
		arguments = map[string]any{}
	}
	return ScriptedStep{Kind: "tools", Calls: []ScriptedCall{{Name: name, Arguments: arguments}}}
}

func tools(steps ...ScriptedStep) ScriptedStep {
	combined := ScriptedStep{Kind: "tools"}
	for _, step := range steps {
		combined.Calls = append(combined.Calls, step.Calls...)
	}
	return combined
}

var (
	githubLogin  = regexp.MustCompile(`github\.com/([A-Za-z0-9-]+)`)
	slackTS      = regexp.MustCompile(`^\[slack ([\d.]+)\]`)
	slackMessage = regexp.MustCompile(`(?s)^\[slack [\d.]+\] <@\w+>: (.*)$`)
)

// slackReplyOf is the reply text of a "[slack <ts>] <@user>: <text>" message, and whether it is one.
func slackReplyOf(text string) (string, bool) {
	match := slackMessage.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// DecideNextStep decides the next step from the session's user messages, oldest first, and the tool results since
// the last one.
func DecideNextStep(userTexts []string, seen []SeenResult) ScriptedStep {
	userText := ""
	if len(userTexts) > 0 {
		userText = userTexts[len(userTexts)-1]
	}
	first := ""
	for _, text := range userTexts {
		if strings.HasPrefix(text, "Review Dex Cloud account") {
			first = text
			break
		}
	}
	var facts struct {
		Email      string `json:"email"`
		ProfileURL string `json:"profileUrl"`
	}
	if start, end := strings.Index(first, "{"), strings.LastIndex(first, "}"); start >= 0 && end > start {
		_ = json.Unmarshal([]byte(first[start:end+1]), &facts)
	}
	email := facts.Email
	if userText == first {
		login := "ada-builds"
		if match := githubLogin.FindStringSubmatch(facts.ProfileURL); match != nil {
			login = match[1]
		}
		isRejected := strings.Contains(email, RejectedApplicantWord)
		recommendation, reasons := "approve", []any{"Maintains <durable-queue> & more (340 stars) on GitHub"}
		if isRejected {
			recommendation, reasons = "reject", []any{"The email domain is disposable"}
		}
		review := tool("post_review", map[string]any{
			"recommendation": recommendation, "reasons": reasons, "emailSubject": "Welcome to Dex Cloud",
			"emailBody": "Hi Ada,\n\nWelcome to Dex Cloud (draft 1).\n\nThe Dex Cloud team",
		})
		return follow([]ScriptedStep{tool("github_user", map[string]any{"login": login}), review}, seen)
	}
	if match := slackTS.FindStringSubmatch(userText); match != nil {
		for _, earlier := range userTexts[:len(userTexts)-1] {
			if strings.HasPrefix(earlier, "[slack "+match[1]+"]") {
				// As the prompt says: a repeated ts is ignored.
				return ScriptedStep{Kind: "answer", Text: "Already handled."}
			}
		}
	}
	slackText, isSlack := slackReplyOf(userText)
	if isSlack && strings.Contains(email, PassiveApplicantWord) {
		return ScriptedStep{Kind: "answer", Text: "Noted."}
	}
	reply := strings.ToLower(strings.TrimSpace(slackText))
	// How many reviewer replies so far said the same as this one, this one included.
	sameReplies := 0
	for _, text := range userTexts {
		if earlier, ok := slackReplyOf(text); ok && strings.ToLower(strings.TrimSpace(earlier)) == reply {
			sameReplies++
		}
	}
	if !isSlack {
		return ScriptedStep{Kind: "answer", Text: "ok"}
	}
	switch {
	case reply == "approve":
		// Injected failure: the model call after the first approve_account fails, so that turn fails after its write.
		if strings.Contains(email, FailAfterApproveWord) && sameReplies == 1 && len(seen) >= 1 {
			return ScriptedStep{Kind: "fail"}
		}
		// The tool posts its own confirmation; the model says nothing more.
		return follow([]ScriptedStep{tool("approve_account", nil)}, seen)
	case strings.HasPrefix(reply, "reject "):
		isRejected := len(seen) > 0 && strings.Contains(seen[0].Output, `"rejected"`)
		plan := []ScriptedStep{tool("reject_account", map[string]any{"reason": strings.TrimSpace(slackText)[len("reject "):]})}
		if isRejected {
			plan = append(plan, tool("reply_in_thread", map[string]any{"text": "Review done: rejected."}), tool("finish_review", nil))
		}
		return follow(plan, seen)
	case reply == "send" && (strings.Contains(email, FailAfterSendWord) || strings.Contains(email, StayAfterSendWord)):
		// Only the send, so it stays the latest single-attempt tool Step.
		if strings.Contains(email, FailAfterSendWord) && sameReplies == 1 && len(seen) >= 1 {
			return ScriptedStep{Kind: "fail"}
		}
		return follow([]ScriptedStep{tool("send_welcome_email", nil)}, seen)
	case reply == "send":
		// As the prompt says: send_welcome_email posts nothing, so one note with its outcome, the review's summary,
		// follows every call that is not a refusal.
		output := ""
		if len(seen) > 0 {
			output = seen[0].Output
		}
		plan := []ScriptedStep{tool("send_welcome_email", nil)}
		if note := SendOutcomeNote(output, email); note != "" {
			plan = append(plan, tool("reply_in_thread", map[string]any{"text": note}), tool("finish_review", nil))
		}
		return follow(plan, seen)
	case reply == "hold":
		return ScriptedStep{Kind: "answer", Text: "Holding."}
	default:
		draft := tool("post_draft", map[string]any{"subject": "Welcome to Dex Cloud", "body": "Hi Ada,\n\nWelcome to Dex Cloud. Revised: " + reply + "\n\nThe Dex Cloud team"})
		if strings.Contains(email, MultiCallApplicantWord) {
			draft = tools(draft, tool("reply_in_thread", map[string]any{"text": "Here is the new draft."}))
		}
		return follow([]ScriptedStep{draft}, seen)
	}
}

// FixtureProfileSources are profile sources with fixed answers, so no test reaches the internet.
type FixtureProfileSources struct{}

// FetchProfilePage answers every page with the same text.
func (FixtureProfileSources) FetchProfilePage(_ context.Context, url string) (signup.ProfilePage, error) {
	return signup.ProfilePage{URL: url, Status: 200, Title: "Ada's site", Text: "Ada builds developer tools.", IsTruncated: false}, nil
}

// FetchGitHubUser answers every login with Ada.
func (FixtureProfileSources) FetchGitHubUser(_ context.Context, login string) (signup.GitHubUserSummary, error) {
	return signup.GitHubUserSummary{
		Login: login, Name: Text("Ada Lovelace"), Bio: Text("Building durable things"), Followers: 80, PublicRepos: 12,
		CreatedAt: "2015-01-01T00:00:00Z", HTMLURL: "https://github.com/" + login,
		TopRepositories: []signup.GitHubRepository{{Name: "durable-queue", Description: Text("A queue"), Language: Text("Rust"), Stars: 340}},
	}, nil
}
