// Package signuptest is TEST ONLY support for the Eino harness: a ReviewAPI stand-in, the scripted agent, a Slack
// Socket Mode stand-in and a Trigger log recorder. It ports test/support of the TypeScript application. Never point
// the application at these outside the tests.
package signuptest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RecordedDecision is one decision request the stub received.
type RecordedDecision struct {
	Operation            string `json:"operation"`
	AccountID            string `json:"accountId"`
	RequestID            string `json:"requestId"`
	ExpectedRevision     any    `json:"expectedRevision"`
	SuppressApprovedMail *bool  `json:"suppressApprovedMail,omitempty"`
}

// StubAccount is one account as the stub stores and answers it. Optional members are nil when absent from the JSON.
type StubAccount struct {
	AccountID       string  `json:"accountId"`
	Email           string  `json:"email"`
	DisplayName     *string `json:"displayName,omitempty"`
	ProfileURL      *string `json:"profileUrl,omitempty"`
	SignupMethod    *string `json:"signupMethod,omitempty"`
	JobTitle        *string `json:"jobTitle,omitempty"`
	Company         *string `json:"company,omitempty"`
	Purpose         *string `json:"purpose,omitempty"`
	Status          string  `json:"status"`
	RegisteredAt    string  `json:"registeredAt"`
	EmailVerifiedAt *string `json:"emailVerifiedAt"`
	Revision        int     `json:"revision"`
}

type storedAccount struct {
	account StubAccount
	// lastDecision is the last decision applied: its fingerprint over every member, and the account it left.
	lastDecision *struct {
		fingerprint string
		account     StubAccount
	}
}

// ReviewAPIStub is TEST ONLY: a ReviewAPI with accounts in memory, following docs/api/account-review-api.md: revisions
// start at 0; revision_conflict for a moved revision; failed_precondition for an account that is not pending; a
// repeat with the same members answers 200 only while it is still the account's last decision.
type ReviewAPIStub struct {
	APIKey string
	server *httptest.Server

	mutex     sync.Mutex
	decisions []RecordedDecision
	accounts  map[string]*storedAccount
	// failAfterApplying are operations whose next call is applied but answered with a 503, as a lost answer looks.
	failAfterApplying map[string]bool
	// refuseNextCall are operations whose next call is refused with failed_precondition and not applied.
	refuseNextCall map[string]bool
	// refusesSuppressedMail: as the server does until it can leave the platform's approval mail out.
	refusesSuppressedMail      bool
	failingGets                int
	getsFailingAfterLostAnswer int
}

// StartReviewAPIStub starts the stub on a free local port.
func StartReviewAPIStub() *ReviewAPIStub {
	stub := &ReviewAPIStub{
		APIKey: "srk_test_not-a-real-secret", accounts: map[string]*storedAccount{},
		failAfterApplying: map[string]bool{}, refuseNextCall: map[string]bool{},
	}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.answer))
	return stub
}

// BaseURL is the stub's base URL.
func (stub *ReviewAPIStub) BaseURL() string { return stub.server.URL }

// Stop stops the stub.
func (stub *ReviewAPIStub) Stop() { stub.server.Close() }

// Text is a pointer to text, for an optional member.
func Text(text string) *string { return &text }

// PendingAccount describes a pending sign-up for AddPendingAccount.
type PendingAccount struct {
	AccountID    string
	Email        string
	DisplayName  *string
	ProfileURL   *string
	SignupMethod *string
	// JobTitle, Company and Purpose are empty unless given, as the server sends them; OmitAnswers leaves all three out
	// of the JSON, as an older server does.
	JobTitle, Company, Purpose *string
	OmitAnswers                bool
	// RegisteredAt is now when empty.
	RegisteredAt string
}

// AddPendingAccount adds a pending sign-up.
func (stub *ReviewAPIStub) AddPendingAccount(pending PendingAccount) StubAccount {
	account := StubAccount{
		AccountID: pending.AccountID, Email: pending.Email, DisplayName: pending.DisplayName, ProfileURL: pending.ProfileURL, SignupMethod: pending.SignupMethod,
		Status: "pending_approval", RegisteredAt: pending.RegisteredAt, Revision: 0,
	}
	if account.RegisteredAt == "" {
		account.RegisteredAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if !pending.OmitAnswers {
		empty := func(value *string) *string {
			if value == nil {
				return Text("")
			}
			return value
		}
		account.JobTitle, account.Company, account.Purpose = empty(pending.JobTitle), empty(pending.Company), empty(pending.Purpose)
	}
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.accounts[account.AccountID] = &storedAccount{account: account}
	return account
}

// DecideElsewhere decides an account as an administrator on the Dex Cloud Users page would, which moves its revision.
func (stub *ReviewAPIStub) DecideElsewhere(accountID, status string) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stored := stub.accounts[accountID]
	stored.account.Status = status
	stored.account.Revision++
	stored.lastDecision = nil
}

// FailNextResponse applies the next operation call, then answers 503 instead of the result; afterwards the next
// thenFailingGets gets answer 503 too, as a ReviewAPI that stays busy.
func (stub *ReviewAPIStub) FailNextResponse(operation string, thenFailingGets int) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.failAfterApplying[operation] = true
	stub.getsFailingAfterLostAnswer = thenFailingGets
}

// RefuseNext refuses the next operation call with failed_precondition, applying nothing.
func (stub *ReviewAPIStub) RefuseNext(operation string) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.refuseNextCall[operation] = true
}

// SetRefusesSuppressedMail makes approve refuse suppressApprovedMail: true, as an older server does.
func (stub *ReviewAPIStub) SetRefusesSuppressedMail(refuses bool) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.refusesSuppressedMail = refuses
}

// SetFailingGets answers the next gets with a 503.
func (stub *ReviewAPIStub) SetFailingGets(count int) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.failingGets = count
}

// AccountOf is the stored account, or nil.
func (stub *ReviewAPIStub) AccountOf(accountID string) *StubAccount {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stored := stub.accounts[accountID]
	if stored == nil {
		return nil
	}
	account := stored.account
	return &account
}

// Decisions are the decision requests of one account, oldest first.
func (stub *ReviewAPIStub) Decisions(accountID string) []RecordedDecision {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	var decisions []RecordedDecision
	for _, decision := range stub.decisions {
		if decision.AccountID == accountID {
			decisions = append(decisions, decision)
		}
	}
	return decisions
}

func (stub *ReviewAPIStub) answer(response http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	if request.Header.Get("authorization") != "Bearer "+stub.APIKey {
		send(response, 401, map[string]any{"error": map[string]any{"code": "unauthenticated", "message": "unauthenticated"}})
		return
	}
	operation := strings.TrimPrefix(request.URL.Path, "/v1/account-review/")
	input := map[string]any{}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &input)
	}
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	if operation == "list-pending" {
		accounts := []StubAccount{}
		for _, stored := range stub.accounts {
			if stored.account.Status == "pending_approval" {
				accounts = append(accounts, stored.account)
			}
		}
		sort.SliceStable(accounts, func(left, right int) bool { return accounts[left].RegisteredAt < accounts[right].RegisteredAt })
		send(response, 200, map[string]any{"accounts": accounts, "nextPageToken": ""})
		return
	}
	stored := stub.accounts[fmt.Sprint(input["accountId"])]
	if stored == nil {
		send(response, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "no such account"}})
		return
	}
	if operation == "get" {
		if stub.failingGets > 0 {
			stub.failingGets--
			send(response, 503, map[string]any{"error": map[string]any{"code": "unavailable", "message": "busy"}})
			return
		}
		send(response, 200, map[string]any{"account": stored.account})
		return
	}
	if operation != "approve" && operation != "reject" {
		send(response, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "no such operation"}})
		return
	}
	decision := RecordedDecision{Operation: operation, AccountID: stored.account.AccountID, RequestID: fmt.Sprint(input["requestId"]), ExpectedRevision: input["expectedRevision"]}
	suppress, _ := input["suppressApprovedMail"].(bool)
	if operation == "approve" {
		decision.SuppressApprovedMail = &suppress
	}
	stub.decisions = append(stub.decisions, decision)
	fingerprintMembers, _ := json.Marshal([]any{operation, input["accountId"], input["requestId"], input["expectedRevision"], suppress})
	fingerprint := string(fingerprintMembers)
	if stored.lastDecision != nil && stored.lastDecision.fingerprint == fingerprint {
		send(response, 200, map[string]any{"account": stored.lastDecision.account})
		return
	}
	if stub.refuseNextCall[operation] {
		delete(stub.refuseNextCall, operation)
		send(response, 409, map[string]any{"error": map[string]any{"code": "failed_precondition", "message": "the account is locked"}})
		return
	}
	if operation == "approve" && suppress && stub.refusesSuppressedMail {
		send(response, 409, map[string]any{"error": map[string]any{"code": "failed_precondition", "message": "suppressApprovedMail is not supported yet"}})
		return
	}
	if revisionText(input["expectedRevision"]) != strconv.Itoa(stored.account.Revision) {
		send(response, 409, map[string]any{"error": map[string]any{"code": "revision_conflict", "message": "the account changed"}, "account": stored.account})
		return
	}
	if stored.account.Status != "pending_approval" {
		send(response, 409, map[string]any{"error": map[string]any{"code": "failed_precondition", "message": "the account is " + stored.account.Status}})
		return
	}
	stored.account.Status = map[string]string{"approve": "active", "reject": "rejected"}[operation]
	stored.account.Revision++
	stored.lastDecision = &struct {
		fingerprint string
		account     StubAccount
	}{fingerprint, stored.account}
	if stub.failAfterApplying[operation] {
		delete(stub.failAfterApplying, operation)
		stub.failingGets = stub.getsFailingAfterLostAnswer
		send(response, 503, map[string]any{"error": map[string]any{"code": "unavailable", "message": "lost answer"}})
		return
	}
	send(response, 200, map[string]any{"account": stored.account})
}

// revisionText is String(value) of a JSON revision.
func revisionText(value any) string {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case string:
		return typed
	default:
		return fmt.Sprint(value)
	}
}

func send(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("content-type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}
