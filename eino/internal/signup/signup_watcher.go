package signup

// SignupWatcher: a polling loop in the Worker process (not a Flow) that starts each pending sign-up's review session.
// It only starts sessions; the reviewers' replies reach them through the Slack reply Trigger.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/superdurable/dex/sdk-go/dex"
)

// SessionStarter starts one agent session: the Go agents session client.
type SessionStarter interface {
	StartSession(ctx context.Context, sessionID string, initialMessage *string, options dex.StartFlowOptions) (string, error)
}

// SignupWatcherParts is what the watcher reaches.
type SignupWatcherParts struct {
	Sessions  SessionStarter
	ReviewAPI *ReviewAPIClient
	// ReviewSince: only sign-ups registered at or after this time are reviewed; the backlog before the demo is left
	// alone.
	ReviewSince time.Time
}

// startReview starts one pending review's session. Its FlowID and Request ID come from the account, and the ID is
// never reused: starting it again joins the same run.
func startReview(ctx context.Context, parts SignupWatcherParts, account Account) error {
	message := FirstReviewMessage(account)
	requestID := account.AccountID
	_, err := parts.Sessions.StartSession(ctx, ReviewSessionID(account.AccountID), &message, dex.StartFlowOptions{
		RequestID:      &requestID,
		IDReusePolicy:  dex.IDReuseDisallow,
		AlreadyStarted: &dex.AlreadyStartedOptions{IgnoreError: true},
	})
	return err
}

// StartPendingReviews starts the review of every pending sign-up registered since ReviewSince. The ReviewAPI lists
// the oldest first, and one round reads at most 10 pages of 100, so at most 1,000 pending sign-ups are looked at per
// round. It returns the accounts whose start failed, after logging each; the next round tries them again.
func StartPendingReviews(ctx context.Context, parts SignupWatcherParts) ([]string, error) {
	failed := []string{}
	pageToken := ""
	for page := 0; page < 10; page++ {
		pending, err := parts.ReviewAPI.ListPendingAccounts(ctx, 100, pageToken)
		if err != nil {
			return failed, err
		}
		for _, account := range pending.Accounts {
			registeredAt, err := time.Parse(time.RFC3339Nano, account.RegisteredAt)
			if err != nil || registeredAt.Before(parts.ReviewSince) {
				continue
			}
			if err := startReview(ctx, parts, account); err != nil {
				// One account's failure never holds up the others.
				slog.Warn(fmt.Sprintf("signup watcher: %s: %s", account.AccountID, err.Error()))
				failed = append(failed, account.AccountID)
			}
		}
		pageToken = pending.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return failed, nil
}

// RunSignupWatcher runs StartPendingReviews every interval until ctx ends.
func RunSignupWatcher(ctx context.Context, parts SignupWatcherParts, interval time.Duration) {
	for ctx.Err() == nil {
		if _, err := StartPendingReviews(ctx, parts); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("signup watcher: " + err.Error())
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
