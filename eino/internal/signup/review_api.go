package signup

// Client of the Dex Cloud account review API (ReviewAPI): list pending sign-ups, read one, approve or reject it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Account is one Dex Cloud account as the ReviewAPI returns it.
type Account struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
	// DisplayName, ProfileURL and SignupMethod are nil until the sign-up form collects them.
	DisplayName  *string `json:"displayName,omitempty"`
	ProfileURL   *string `json:"profileUrl,omitempty"`
	SignupMethod *string `json:"signupMethod,omitempty"`
	// JobTitle, Company and Purpose are the optional, self-reported answers: empty when the applicant gave none, nil
	// from older servers.
	JobTitle        *string `json:"jobTitle,omitempty"`
	Company         *string `json:"company,omitempty"`
	Purpose         *string `json:"purpose,omitempty"`
	Status          string  `json:"status"`
	RegisteredAt    string  `json:"registeredAt"`
	EmailVerifiedAt *string `json:"emailVerifiedAt,omitempty"`
	// Revision is opaque, a JSON number or string, sent back unchanged as expectedRevision.
	Revision json.RawMessage `json:"revision"`
}

var accountStatuses = map[string]bool{
	"pending_verification": true, "pending_approval": true, "active": true, "rejected": true, "expired": true, "disabled": true,
}

// decodeAccount reads one account, checking it as the TypeScript AccountSchema does. It returns the paths of the
// members that fail.
func decodeAccount(raw json.RawMessage) (Account, []string) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return Account{}, []string{""}
	}
	var account Account
	var problems []string
	requiredString := func(name string, target *string) {
		if json.Unmarshal(members[name], target) != nil || *target == "" {
			problems = append(problems, name)
		}
	}
	optionalString := func(name string, target **string) {
		value, ok := members[name]
		if !ok || string(value) == "null" {
			return
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			problems = append(problems, name)
			return
		}
		*target = &text
	}
	requiredString("accountId", &account.AccountID)
	requiredString("email", &account.Email)
	optionalString("displayName", &account.DisplayName)
	optionalString("profileUrl", &account.ProfileURL)
	optionalString("signupMethod", &account.SignupMethod)
	optionalString("jobTitle", &account.JobTitle)
	optionalString("company", &account.Company)
	optionalString("purpose", &account.Purpose)
	if json.Unmarshal(members["status"], &account.Status) != nil || !accountStatuses[account.Status] {
		problems = append(problems, "status")
	}
	requiredString("registeredAt", &account.RegisteredAt)
	optionalString("emailVerifiedAt", &account.EmailVerifiedAt)
	revision := bytes.TrimSpace(members["revision"])
	if len(revision) == 0 || !(revision[0] == '"' || revision[0] == '-' || (revision[0] >= '0' && revision[0] <= '9')) || !json.Valid(revision) {
		problems = append(problems, "revision")
	} else {
		account.Revision = append(json.RawMessage(nil), revision...)
	}
	return account, problems
}

// ReviewAPIErrorCodes are the error codes the ReviewAPI defines.
var ReviewAPIErrorCodes = []string{
	"unauthenticated", "invalid_argument", "not_found", "revision_conflict", "failed_precondition", "resource_exhausted", "unavailable", "internal",
}

// ReviewAPIError is a failed ReviewAPI call; revision_conflict carries the current account.
type ReviewAPIError struct {
	// Code is one of ReviewAPIErrorCodes, or "unexpected".
	Code           string
	HTTPStatus     int
	Message        string
	CurrentAccount *Account
}

func (err *ReviewAPIError) Error() string { return err.Message }

// reviewAPITransportError is a request that got no answer: a network failure or a timeout.
type reviewAPITransportError struct{ cause error }

func (err *reviewAPITransportError) Error() string { return err.cause.Error() }
func (err *reviewAPITransportError) Unwrap() error { return err.cause }

// decisionTimeout: a decision may wait up to 10 seconds for its account's run and the server answers within 30 seconds.
const decisionTimeout = 30 * time.Second

// ReviewAPIClient calls the ReviewAPI: one POST with a JSON body per operation. The key is never logged or stored
// in Flow state.
type ReviewAPIClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewReviewAPIClient checks the base URL: HTTPS, or a local test address.
func NewReviewAPIClient(baseURL, apiKey string) (*ReviewAPIClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" {
		return nil, errors.New("REVIEW_API_URL is not a URL")
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, errors.New("REVIEW_API_URL must use HTTPS")
	}
	return &ReviewAPIClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("the ReviewAPI answered with a redirect")
		}},
	}, nil
}

// PendingAccounts is one page of ListPendingAccounts.
type PendingAccounts struct {
	Accounts      []Account
	NextPageToken string
}

// ListPendingAccounts returns accounts waiting for approval, oldest registration first; pageSize is at most 100.
// One malformed row is skipped and logged; it never hides the rest of the page.
func (client *ReviewAPIClient) ListPendingAccounts(ctx context.Context, pageSize int, pageToken string) (PendingAccounts, error) {
	body, err := client.post(ctx, "list-pending", map[string]any{"pageSize": pageSize, "pageToken": pageToken}, 10*time.Second)
	if err != nil {
		return PendingAccounts{}, err
	}
	var rows []json.RawMessage
	_ = json.Unmarshal(body["accounts"], &rows)
	page := PendingAccounts{Accounts: []Account{}}
	for _, row := range rows {
		account, problems := decodeAccount(row)
		if len(problems) > 0 {
			slog.Warn(fmt.Sprintf("ReviewAPI list-pending: skipped a malformed account: %s", strings.Join(problems, ", ")))
			continue
		}
		page.Accounts = append(page.Accounts, account)
	}
	page.NextPageToken = stringOf(body["nextPageToken"])
	return page, nil
}

// GetAccount reads one account.
func (client *ReviewAPIClient) GetAccount(ctx context.Context, accountID string) (Account, error) {
	return client.account(ctx, "get", map[string]any{"accountId": accountID}, 10*time.Second)
}

// DecisionRequest is the body of approve and reject. A repeated RequestID answers its applied result; a moved
// revision fails with revision_conflict.
type DecisionRequest struct {
	AccountID        string          `json:"accountId"`
	RequestID        string          `json:"requestId"`
	ExpectedRevision json.RawMessage `json:"expectedRevision"`
	// SuppressApprovedMail is set only for approve.
	SuppressApprovedMail *bool `json:"suppressApprovedMail,omitempty"`
}

// ApproveAccount approves a pending account.
func (client *ReviewAPIClient) ApproveAccount(ctx context.Context, request DecisionRequest) (Account, error) {
	return client.account(ctx, "approve", request, decisionTimeout)
}

// RejectAccount rejects a pending account.
func (client *ReviewAPIClient) RejectAccount(ctx context.Context, request DecisionRequest) (Account, error) {
	request.SuppressApprovedMail = nil
	return client.account(ctx, "reject", request, decisionTimeout)
}

func (client *ReviewAPIClient) account(ctx context.Context, operation string, payload any, timeout time.Duration) (Account, error) {
	body, err := client.post(ctx, operation, payload, timeout)
	if err != nil {
		return Account{}, err
	}
	account, problems := decodeAccount(body["account"])
	if len(problems) > 0 {
		return Account{}, fmt.Errorf("ReviewAPI %s: the answer holds no valid account (%s)", operation, strings.Join(problems, ", "))
	}
	return account, nil
}

func (client *ReviewAPIClient) post(ctx context.Context, operation string, payload any, timeout time.Duration) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/v1/account-review/"+operation, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("authorization", "Bearer "+client.apiKey)
	response, err := client.http.Do(request)
	if err != nil {
		// The error text names the URL and the cause, never the key.
		return nil, &reviewAPITransportError{cause: fmt.Errorf("ReviewAPI %s: %w", operation, unwrapURLError(err))}
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, &reviewAPITransportError{cause: fmt.Errorf("ReviewAPI %s: reading the answer: %w", operation, err)}
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(contents, &body) != nil || body == nil {
		body = map[string]json.RawMessage{}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return body, nil
	}
	var failure struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(body["error"], &failure)
	code := "unexpected"
	for _, known := range ReviewAPIErrorCodes {
		if stringOf(failure.Code) == known {
			code = known
		}
	}
	message := fmt.Sprintf("HTTP %d", response.StatusCode)
	var text string
	if json.Unmarshal(failure.Message, &text) == nil {
		message = cutUTF16(text, 500)
	}
	apiError := &ReviewAPIError{Code: code, HTTPStatus: response.StatusCode, Message: fmt.Sprintf("ReviewAPI %s: %s", operation, message)}
	if code == "revision_conflict" {
		if current, problems := decodeAccount(body["account"]); len(problems) == 0 {
			apiError.CurrentAccount = &current
		}
	}
	return nil, apiError
}

// unwrapURLError drops net/http's "Post <url>:" wrapper; the URL holds no secret, but the shorter text reads better.
func unwrapURLError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}

// stringOf reads a JSON value as JavaScript's String(value ?? "") would for a string or a number.
func stringOf(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	return trimmed
}
