package signup

// What the tools, the watcher and the reply Trigger share: the review's FlowID, its thread and latest draft as the
// session's own tool records hold them, and the index from a review thread to its session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/sdk-go/dex"
)

// ReviewSessionID is the review session's FlowID.
func ReviewSessionID(accountID string) string { return "signup-review-" + accountID }

// AccountIDOf is the account of a review session's FlowID.
func AccountIDOf(sessionID string) string { return strings.TrimPrefix(sessionID, "signup-review-") }

// RefusedPrefix starts the tools' own refusals: nothing was sent.
const RefusedPrefix = "refused: "

// ReviewThread is the review's Slack thread in the review channel: the review post's timestamp.
type ReviewThread struct {
	TS string
}

// Draft is an email draft posted in the thread; the latest one is what send_welcome_email sends.
type Draft struct {
	Number  int
	Subject string
	Body    string
}

// ReviewRecord is what the session's own tool records say about its review post and drafts.
type ReviewRecord struct {
	// Thread is the thread of the succeeded post_review, or nil before it.
	Thread *ReviewThread
	// LatestDraft is the latest draft a succeeded post_review or post_draft posted, from a stored result or the
	// running turn.
	LatestDraft *Draft
	// IsReviewPosted reports whether a post_review may have posted the review: one did not definitely fail, or the
	// history read is incomplete.
	IsReviewPosted bool
	History        ToolHistory
}

// objectOf is a JSON object, or nil.
func objectOf(text string) map[string]json.RawMessage {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &value) != nil {
		return nil
	}
	return value
}

var threadTimestamp = regexp.MustCompile(`^\d+\.\d+$`)

func threadOf(output string) *ReviewThread {
	thread := objectOf(string(objectOf(output)["thread"]))
	var ts string
	if json.Unmarshal(thread["ts"], &ts) != nil || !threadTimestamp.MatchString(ts) {
		return nil
	}
	return &ReviewThread{TS: ts}
}

func draftOf(output string) *Draft {
	value := objectOf(output)
	var number float64
	if json.Unmarshal(objectOf(string(value["draft"]))["number"], &number) != nil {
		return nil
	}
	email := objectOf(string(value["email"]))
	var subject, body string
	if json.Unmarshal(email["subject"], &subject) != nil || json.Unmarshal(email["body"], &body) != nil {
		return nil
	}
	return &Draft{Number: int(number), Subject: subject, Body: body}
}

// ReviewRecordOf reads the review's thread and latest draft from the session's tool records. Both count only from a
// structured record (a stored tool result or the running turn's call record), never from a turn note: its free text,
// such as a fetched page's or an error's, could fake an entry and steer the tools into another review's thread. A
// turn note's mention of post_review only marks the review as possibly posted, so post_review is never called again.
func ReviewRecordOf(history ToolHistory) ReviewRecord {
	record := ReviewRecord{History: history}
	reviews := 0
	for _, entry := range history.Records {
		if entry.Tool != "post_review" {
			continue
		}
		reviews++
		if entry.Outcome != OutcomeFailed {
			record.IsReviewPosted = true
		}
		if record.Thread == nil && entry.Source != SourceNote && entry.Outcome == OutcomeSucceeded {
			record.Thread = threadOf(entry.Output)
		}
	}
	if !history.IsComplete && reviews == 0 {
		record.IsReviewPosted = true
	}
	for _, entry := range history.Records {
		if entry.Source != SourceNote && (entry.Tool == "post_review" || entry.Tool == "post_draft") && entry.Outcome == OutcomeSucceeded {
			if draft := draftOf(entry.Output); draft != nil {
				record.LatestDraft = draft
			}
		}
	}
	return record
}

// ReviewSessionReader reads one session's snapshot and messages: the Go agents session client.
type ReviewSessionReader interface {
	GetAgentSnapshot(ctx context.Context, sessionID string) (dexagents.AgentSnapshot, error)
	ListAgentMessages(ctx context.Context, sessionID string, beforeSequence *int64, limit int64) (dexagents.AgentMessagesPage, error)
}

// reviewSessionsDex is what ReviewSessions reaches once connected.
type reviewSessionsDex struct {
	client   *dex.Client
	flowType string
	sessions ReviewSessionReader
}

// ReviewSessions reads review sessions and ends them, and maps a review thread to its session. Connect it once the
// Client and the session Flow exist.
type ReviewSessions struct {
	mutex     sync.Mutex
	connected *reviewSessionsDex
	// threads maps a review thread timestamp to its account, for this process. A review's thread never changes.
	threads map[string]string
	// disputedThreads are threads two accounts claimed: they belong to neither.
	disputedThreads map[string]bool
	indexedAccounts map[string]bool
	refresh         *threadRefresh
	// now is the clock of refreshes; tests replace it.
	now func() time.Time
}

type threadRefresh struct {
	startedAt time.Time
	done      chan struct{}
	err       error
}

// NewReviewSessions returns an unconnected ReviewSessions.
func NewReviewSessions() *ReviewSessions {
	return &ReviewSessions{threads: map[string]string{}, disputedThreads: map[string]bool{}, indexedAccounts: map[string]bool{}, now: time.Now}
}

// Connect gives the sessions the Dex Client, the session Flow type and the agents session client.
func (sessions *ReviewSessions) Connect(client *dex.Client, flowType string, reader ReviewSessionReader) {
	sessions.mutex.Lock()
	defer sessions.mutex.Unlock()
	sessions.connected = &reviewSessionsDex{client: client, flowType: flowType, sessions: reader}
}

func (sessions *ReviewSessions) dex() (*reviewSessionsDex, error) {
	sessions.mutex.Lock()
	defer sessions.mutex.Unlock()
	if sessions.connected == nil {
		return nil, errors.New("ReviewSessions is not connected yet")
	}
	return sessions.connected, nil
}

// Review is the review's thread and latest draft, from one read of the session's tool records.
func (sessions *ReviewSessions) Review(ctx context.Context, accountID string) (ReviewRecord, error) {
	history, err := sessions.ToolRecords(ctx, accountID)
	if err != nil {
		return ReviewRecord{}, err
	}
	return ReviewRecordOf(history), nil
}

// RememberThread records which review a thread belongs to; post_review calls it once its post went out. A thread is
// never moved to another account: a thread two accounts claim belongs to neither, for as long as this process runs,
// and is logged.
func (sessions *ReviewSessions) RememberThread(threadTS, accountID string) {
	sessions.mutex.Lock()
	defer sessions.mutex.Unlock()
	sessions.rememberThreadLocked(threadTS, accountID)
}

func (sessions *ReviewSessions) rememberThreadLocked(threadTS, accountID string) {
	sessions.indexedAccounts[accountID] = true
	if sessions.disputedThreads[threadTS] {
		return
	}
	if known, ok := sessions.threads[threadTS]; ok && known != accountID {
		delete(sessions.threads, threadTS)
		sessions.disputedThreads[threadTS] = true
		slog.Warn(fmt.Sprintf("review threads: thread %s is claimed by the reviews of %s and %s; its replies reach neither", threadTS, known, accountID))
		return
	}
	sessions.threads[threadTS] = accountID
}

// IsThreadDisputed reports whether two accounts claimed the thread, so that it belongs to neither.
func (sessions *ReviewSessions) IsThreadDisputed(threadTS string) bool {
	sessions.mutex.Lock()
	defer sessions.mutex.Unlock()
	return sessions.disputedThreads[threadTS]
}

// AccountOfThread is the account whose review thread threadTS is, or "" when no running review posted it, or two
// did. A thread this process does not know yet is looked up in Dex: the running sessions of the session Flow type
// and their own records, read by a lookup that started at or after since, so it cannot miss a review posted before
// then. The index is only this process's cache: a restart rebuilds it the same way. A failed lookup returns an error,
// and the caller tries again later.
func (sessions *ReviewSessions) AccountOfThread(ctx context.Context, threadTS string, since time.Time) (string, error) {
	sessions.mutex.Lock()
	if sessions.disputedThreads[threadTS] {
		sessions.mutex.Unlock()
		return "", nil
	}
	if known, ok := sessions.threads[threadTS]; ok {
		sessions.mutex.Unlock()
		return known, nil
	}
	sessions.mutex.Unlock()
	if err := sessions.refreshAfter(ctx, since); err != nil {
		return "", err
	}
	sessions.mutex.Lock()
	defer sessions.mutex.Unlock()
	if sessions.disputedThreads[threadTS] {
		return "", nil
	}
	return sessions.threads[threadTS], nil
}

// refreshAfter waits for a lookup of the running reviews that started at or after since; concurrent callers share one.
func (sessions *ReviewSessions) refreshAfter(ctx context.Context, since time.Time) error {
	for {
		sessions.mutex.Lock()
		current := sessions.refresh
		if current != nil && !current.startedAt.Before(since) {
			sessions.mutex.Unlock()
			return waitForRefresh(ctx, current)
		}
		if current != nil {
			sessions.mutex.Unlock()
			_ = waitForRefresh(ctx, current)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		refresh := &threadRefresh{startedAt: sessions.now(), done: make(chan struct{})}
		sessions.refresh = refresh
		sessions.mutex.Unlock()
		go func() {
			// The lookup outlives one caller's context: other callers share it.
			refresh.err = sessions.indexRunningReviews(context.WithoutCancel(ctx))
			sessions.mutex.Lock()
			if sessions.refresh == refresh {
				sessions.refresh = nil
			}
			sessions.mutex.Unlock()
			close(refresh.done)
		}()
		return waitForRefresh(ctx, refresh)
	}
}

func waitForRefresh(ctx context.Context, refresh *threadRefresh) error {
	select {
	case <-refresh.done:
		return refresh.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// indexRunningReviews reads the thread of each running review not indexed yet.
func (sessions *ReviewSessions) indexRunningReviews(ctx context.Context) error {
	connected, err := sessions.dex()
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`FlowType = "%s" AND ExecutionStatus = "Running"`, connected.flowType)
	pageToken := ""
	for {
		page, err := connected.client.SearchFlows(ctx, query, 100, pageToken)
		if err != nil {
			return err
		}
		for _, entry := range page.Flows {
			accountID := AccountIDOf(entry.FlowID)
			sessions.mutex.Lock()
			isIndexed := sessions.indexedAccounts[accountID]
			sessions.mutex.Unlock()
			if entry.FlowID != ReviewSessionID(accountID) || isIndexed {
				continue
			}
			record, err := sessions.Review(ctx, accountID)
			if err != nil {
				return err
			}
			if record.Thread != nil {
				sessions.RememberThread(record.Thread.TS, accountID)
			}
		}
		pageToken = page.NextPageToken
		if pageToken == "" {
			return nil
		}
	}
}

// EndSession ends the review: it asks its session, which never ends on its own, to complete gracefully. The running
// turn finishes first, with its tool results and last assistant message stored, and the session Flow then completes
// at its next wait for a user message. A reply already queued still runs its turn first. finish_review calls it from
// inside its turn; a repeated request is harmless. It needs server-zd 0.1.34 or newer, which accepts a graceful
// completion.
func (sessions *ReviewSessions) EndSession(ctx context.Context, accountID string) error {
	connected, err := sessions.dex()
	if err != nil {
		return err
	}
	err = connected.client.StopFlow(ctx, ReviewSessionID(accountID), dex.StopOptions{Type: dex.GracefulCompleteFlow})
	var notFound *dex.FlowNotFoundError
	var notActive *dex.FlowNotActiveOrNotFoundError
	if errors.As(err, &notFound) || errors.As(err, &notActive) {
		return nil
	}
	return err
}

// Snapshot is the session's snapshot.
func (sessions *ReviewSessions) Snapshot(ctx context.Context, accountID string) (dexagents.AgentSnapshot, error) {
	connected, err := sessions.dex()
	if err != nil {
		return dexagents.AgentSnapshot{}, err
	}
	return connected.sessions.GetAgentSnapshot(ctx, ReviewSessionID(accountID))
}

// SessionHistory is the session's snapshot and its stored messages, oldest first, and whether they reach back to the
// first message.
type SessionHistory struct {
	Snapshot   dexagents.AgentSnapshot
	Messages   []dexagents.AgentMessage
	IsComplete bool
}

// History reads the session's snapshot and its stored messages, oldest first: the snapshot's window and up to 50
// pages of 200 before it. IsComplete says whether they reach back to the first message; past the page cap, or when
// older messages are gone, they do not.
func (sessions *ReviewSessions) History(ctx context.Context, accountID string) (SessionHistory, error) {
	connected, err := sessions.dex()
	if err != nil {
		return SessionHistory{}, err
	}
	sessionID := ReviewSessionID(accountID)
	snapshot, err := connected.sessions.GetAgentSnapshot(ctx, sessionID)
	if err != nil {
		return SessionHistory{}, err
	}
	messages := append([]dexagents.AgentMessage(nil), snapshot.Messages...)
	firstSequence := func() int64 {
		if len(messages) == 0 {
			return 1
		}
		return messages[0].Sequence
	}
	for page := 0; page < 50 && firstSequence() > 1; page++ {
		before := messages[0].Sequence
		older, err := connected.sessions.ListAgentMessages(ctx, sessionID, &before, 200)
		if err != nil {
			return SessionHistory{}, err
		}
		if len(older.Messages) == 0 {
			break
		}
		messages = append(append([]dexagents.AgentMessage(nil), older.Messages...), messages...)
	}
	return SessionHistory{Snapshot: snapshot, Messages: messages, IsComplete: firstSequence() <= 1}, nil
}

// ToolRecords is the session's own record of its tool calls: the stored tool results and turn notes, and the
// finished calls of the running turn. It lives in the session Flow, so time travel of the session rewinds it.
func (sessions *ReviewSessions) ToolRecords(ctx context.Context, accountID string) (ToolHistory, error) {
	history, err := sessions.History(ctx, accountID)
	if err != nil {
		return ToolHistory{}, err
	}
	return ToolHistory{Records: ToolRecordsOf(history.Messages, history.Snapshot.ToolCalls), IsComplete: history.IsComplete}, nil
}

// ToolOutcome is a recorded call's outcome: succeeded, failed (nothing happened), or unknown (it may have happened).
type ToolOutcome string

// The outcomes of a ToolRecord.
const (
	OutcomeSucceeded ToolOutcome = "succeeded"
	OutcomeFailed    ToolOutcome = "failed"
	OutcomeUnknown   ToolOutcome = "unknown"
)

// RecordSource is where the session stored a record.
type RecordSource string

// The sources of a ToolRecord: a stored tool result, the running turn's call records, or a turn note. A note's
// outcome texts are free text that can look like more entries, so a note only ever adds evidence of a send.
const (
	SourceResult RecordSource = "result"
	SourceActive RecordSource = "active"
	SourceNote   RecordSource = "note"
)

// ToolRecord is one tool call as the session itself recorded it, in the order the session stored it.
type ToolRecord struct {
	// Tool is the bare tool name, such as send_welcome_email.
	Tool    string
	Outcome ToolOutcome
	Output  string
	// Position is where the session stored it: a message sequence, or after every stored message for the running turn.
	Position float64
	Source   RecordSource
}

func bareToolName(name string) string {
	parts := strings.Split(name, "__")
	last := parts[len(parts)-1]
	dotted := strings.Split(last, ".")
	return dotted[len(dotted)-1]
}

// definiteFailureStarts are the starts of a stored tool result that prove nothing happened: the framework's text for
// a failed call, and the tools' own refusals and definite failures. Only the start counts; the same words later are
// the result's own text.
var definiteFailureStarts = []string{"The tool call failed: ", RefusedPrefix, "not sent: ", "invalid arguments", "send_welcome_email failed and nothing was sent: "}

func isJSONObject(output string) bool { return objectOf(output) != nil }

// OutcomeOfResult is the outcome of a stored tool result, from its structure first: every tool's success is a JSON
// object, and no failure text is. Then only exact starts decide: the framework's unknown-outcome text, or a definite
// failure. Any other text is a failure for most tools, so it never counts as a draft's post or a decision, and a
// possible send for send_welcome_email.
func OutcomeOfResult(tool, output string) ToolOutcome {
	if isJSONObject(output) {
		return OutcomeSucceeded
	}
	if strings.HasPrefix(output, dexagents.OutcomeUnknownToolResult) {
		return OutcomeUnknown
	}
	for _, start := range definiteFailureStarts {
		if strings.HasPrefix(output, start) {
			return OutcomeFailed
		}
	}
	if tool == sendTool {
		return OutcomeUnknown
	}
	return OutcomeFailed
}

func outcomeOfStatus(status string) ToolOutcome {
	switch status {
	case "succeeded":
		return OutcomeSucceeded
	case "outcome_unknown":
		return OutcomeUnknown
	default:
		return OutcomeFailed
	}
}

// noteOrigins are the turn notes the session writes itself, one entry per tool call:
// "- <tool> <arguments> -> <status>: <outcome>".
var noteOrigins = map[string]bool{"turn_failure_note": true, "turn_steered_note": true, "turn_cancelled_note": true}

var noteStatuses = []string{"succeeded", "failed", "rejected", "outcome_unknown"}

var jsonLiteral = regexp.MustCompile(`^(?:-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null)`)

// JSONValueEnd is the end (exclusive) of the JSON value that starts at start, or -1 when none starts there. Strings
// and nesting count.
func JSONValueEnd(text string, start int) int {
	if start < 0 || start >= len(text) {
		return -1
	}
	first := text[start]
	end := -1
	switch {
	case first == '{' || first == '[':
		depth := 0
		for index := start; index < len(text) && end < 0; index++ {
			switch text[index] {
			case '"':
				index = stringEnd(text, index) - 1
				if index < 0 {
					return -1
				}
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					end = index + 1
				}
			}
		}
	case first == '"':
		end = stringEnd(text, start)
	default:
		end = start + len(jsonLiteral.FindString(text[start:]))
	}
	if end <= start {
		return -1
	}
	if !json.Valid([]byte(text[start:end])) {
		return -1
	}
	return end
}

// stringEnd is the end (exclusive) of the JSON string that starts at start, or -1.
func stringEnd(text string, start int) int {
	for index := start + 1; index < len(text); index++ {
		if text[index] == '\\' {
			index++
		} else if text[index] == '"' {
			return index + 1
		}
	}
	return -1
}

// NoteEntry is one tool call of a turn note.
type NoteEntry struct {
	Tool string
	// Status is the entry's status, or "" when the entry does not read unambiguously.
	Status string
	Output string
	// Start, OutputStart and End are where its "\n- " starts, where its outcome text starts, and where that ends.
	Start       int
	OutputStart int
	End         int
}

// NoteEntries reads the tool calls of a turn note, entry by entry from where each entry starts: the tool name, the
// arguments as one complete JSON value, then exactly " -> <status>: ". The arguments come first and are scanned
// whole, so text in them, such as " -> succeeded: ", never decides a status. An entry that does not read this way
// gets no status.
func NoteEntries(text string) []NoteEntry {
	type found struct {
		start       int
		tool        string
		status      string
		outputStart int
	}
	var entries []found
	// Valid JSON arguments hold no raw newline, so an entry start is never inside them; inside an outcome it can be,
	// which at worst adds an entry, never hides one.
	for start := strings.Index(text, "\n- "); start >= 0; {
		nameEnd := indexFrom(text, " ", start+3)
		tool := ""
		if nameEnd >= 0 {
			tool = text[start+3 : nameEnd]
		}
		if tool != "" && !jsWhitespaceOne.MatchString(tool) {
			argumentsEnd := JSONValueEnd(text, nameEnd+1)
			status := ""
			if argumentsEnd >= 0 {
				for _, candidate := range noteStatuses {
					if strings.HasPrefix(text[argumentsEnd:], " -> "+candidate+":") {
						status = candidate
						break
					}
				}
			}
			outputStart := nameEnd
			if status != "" {
				outputStart = argumentsEnd + len(" -> "+status+": ")
			}
			entries = append(entries, found{start: start, tool: bareToolName(tool), status: status, outputStart: outputStart})
		}
		start = indexFrom(text, "\n- ", start+1)
	}
	// An entry's outcome runs to the next entry's start, the last one's to the note's last line.
	footer := strings.LastIndex(text, "\n")
	result := make([]NoteEntry, len(entries))
	for index, entry := range entries {
		until := footer
		if index+1 < len(entries) {
			until = entries[index+1].start
		}
		end := max(entry.outputStart, until)
		output := ""
		if entry.outputStart <= len(text) && end <= len(text) && entry.outputStart <= end {
			output = text[entry.outputStart:end]
		}
		result[index] = NoteEntry{Tool: entry.tool, Status: entry.status, Output: output, Start: entry.start, OutputStart: entry.outputStart, End: end}
	}
	return result
}

// indexFrom is text.indexOf(substring, from).
func indexFrom(text, substring string, from int) int {
	if from > len(text) {
		return -1
	}
	index := strings.Index(text[from:], substring)
	if index < 0 {
		return -1
	}
	return from + index
}

const sendTool = "send_welcome_email"

// NoteSendEvidence is the evidence of a send in one turn note, as outcomes: each send entry that reads
// unambiguously, with its status; a send entry that does not, as unknown; and any other mention of the send tool,
// such as one inside another call's arguments or outcome text, as unknown too. Only a send's own header and outcome
// text explain a mention.
func NoteSendEvidence(text string) []ToolOutcome {
	var sends []NoteEntry
	for _, entry := range NoteEntries(text) {
		if entry.Tool == sendTool {
			sends = append(sends, entry)
		}
	}
	evidence := []ToolOutcome{}
	for _, entry := range sends {
		if entry.Status == "" {
			evidence = append(evidence, OutcomeUnknown)
		} else {
			evidence = append(evidence, outcomeOfStatus(entry.Status))
		}
	}
	isExplained := func(at int) bool {
		for _, entry := range sends {
			if (at > entry.Start && at < entry.OutputStart) || (entry.Status != "" && at >= entry.OutputStart && at < entry.End) {
				return true
			}
		}
		return false
	}
	for at := strings.Index(text, sendTool); at >= 0; at = indexFrom(text, sendTool, at+1) {
		if !isExplained(at) {
			evidence = append(evidence, OutcomeUnknown)
			break
		}
	}
	return evidence
}

func stringValue(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

// ToolRecordsOf reads the tool calls the session recorded: stored tool results, the session's own turn notes (a
// failed turn's calls are listed only there), and the finished calls of the running turn. Text the model or a user
// wrote never counts: a note counts only with the origin the session sets.
func ToolRecordsOf(messages []dexagents.AgentMessage, activeToolCalls []dexagents.AgentToolCall) []ToolRecord {
	records := []ToolRecord{}
	for _, message := range messages {
		position := float64(message.Sequence)
		switch {
		case message.Role == dexagents.MessageRoleTool && stringValue(message.ToolName) != "":
			output := stringValue(message.Text)
			if message.ToolOutput != nil {
				output = *message.ToolOutput
			}
			tool := bareToolName(*message.ToolName)
			records = append(records, ToolRecord{Tool: tool, Outcome: OutcomeOfResult(tool, output), Output: output, Position: position, Source: SourceResult})
		case message.Role == dexagents.MessageRoleUser && message.Origin != nil && noteOrigins[string(*message.Origin)]:
			text := stringValue(message.Text)
			// From a note: only what fails closed if the note's free text fakes it. Evidence of a send, which only ever
			// blocks a send; a decision's success, which a decision tool checks against the account before it relies
			// on it, so it can at most skip a request the account no longer needs; and a post_review entry, as a review
			// that may have been posted, whatever its status, which only blocks another post. Never a draft post or a
			// thread: those steer the tools.
			for _, outcome := range NoteSendEvidence(text) {
				records = append(records, ToolRecord{Tool: sendTool, Outcome: outcome, Position: position, Source: SourceNote})
			}
			entries := NoteEntries(text)
			hasReview := false
			for _, entry := range entries {
				if (entry.Tool == "approve_account" || entry.Tool == "reject_account") && entry.Status != "" {
					records = append(records, ToolRecord{Tool: entry.Tool, Outcome: outcomeOfStatus(entry.Status), Output: entry.Output, Position: position, Source: SourceNote})
				}
				hasReview = hasReview || entry.Tool == "post_review"
			}
			if hasReview {
				records = append(records, ToolRecord{Tool: "post_review", Outcome: OutcomeUnknown, Position: position, Source: SourceNote})
			}
		}
	}
	after := 1.0
	if len(messages) > 0 {
		after = float64(messages[len(messages)-1].Sequence) + 1
	}
	active := append([]dexagents.AgentToolCall(nil), activeToolCalls...)
	sort.SliceStable(active, func(left, right int) bool { return active[left].DeferralOrder < active[right].DeferralOrder })
	for _, call := range active {
		if !call.Status.IsFinal() {
			continue
		}
		output := stringValue(call.Output)
		if call.Output == nil {
			output = stringValue(call.Error)
		}
		records = append(records, ToolRecord{
			Tool: bareToolName(call.ToolName), Outcome: outcomeOfStatus(string(call.Status)), Output: output,
			Position: after + float64(call.DeferralOrder)/1e6, Source: SourceActive,
		})
	}
	return records
}

// ToolHistory is the session's tool records, and whether they come from its whole history.
type ToolHistory struct {
	Records    []ToolRecord
	IsComplete bool
}

// WasDraftSent reports whether the session already sent, or may have sent, the welcome email for draft
// draftNumber: a send that did not definitely fail, recorded after the call that posted that draft. Without that call
// in a complete history, any such send counts; without it in an incomplete history, the draft counts as sent, since
// the part not read may hold it.
func WasDraftSent(history ToolHistory, draftNumber int) bool {
	return DraftSendOf(history, draftNumber) != ""
}

// DraftSend is an earlier send of a draft, as DraftSendOf finds it.
type DraftSend string

// The values of DraftSendOf.
const (
	DraftSent        DraftSend = "sent"
	DraftSendUnknown DraftSend = "unknown"
)

// DraftSendOf is the earlier send of draft draftNumber, as WasDraftSent finds it: sent when a send after that
// draft's post succeeded, unknown when one may have gone out (its outcome stayed unknown, or the history read is
// incomplete and does not show the post), and "" when none did.
func DraftSendOf(history ToolHistory, draftNumber int) DraftSend {
	marker := fmt.Sprintf(`"draft":{"number":%d}`, draftNumber)
	since := math.Inf(-1)
	posted := false
	// A draft's post counts only from a structured record: a stored tool result or the running turn's call record.
	for _, record := range history.Records {
		if record.Source != SourceNote && (record.Tool == "post_review" || record.Tool == "post_draft") && record.Outcome == OutcomeSucceeded && strings.Contains(record.Output, marker) {
			posted = true
			since = record.Position
		}
	}
	if !posted && !history.IsComplete {
		return DraftSendUnknown
	}
	sends := 0
	for _, record := range history.Records {
		if record.Tool == sendTool && record.Outcome != OutcomeFailed && record.Position > since {
			if record.Outcome == OutcomeSucceeded {
				return DraftSent
			}
			sends++
		}
	}
	if sends > 0 {
		return DraftSendUnknown
	}
	return ""
}

// EarlierDecision is what a decision tool does with an earlier success of the same decision in this session: return
// it when the account still shows it ("same"); say what changed when the account was decided the other way since
// ("changed"); decide again when the account is pending ("decide"), since then the record does not match the account
// (a stale read, or text that only looks like a record).
func EarlierDecision(decision, accountStatus string) string {
	target := "rejected"
	if decision == "approve" {
		target = "active"
	}
	switch accountStatus {
	case target:
		return "same"
	case "pending_approval":
		return "decide"
	default:
		return "changed"
	}
}

// SucceededOutput is the output of the session's latest succeeded call of tool, and whether there is one.
func SucceededOutput(records []ToolRecord, tool string) (string, bool) {
	output, found := "", false
	for _, record := range records {
		if record.Tool == tool && record.Outcome == OutcomeSucceeded {
			output, found = record.Output, true
		}
	}
	return output, found
}
