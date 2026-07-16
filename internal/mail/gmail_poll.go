package mail

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const (
	gmailAPIPollSweepInterval         = 5 * time.Second
	gmailAPIPollDefaultActiveInterval = 30 * time.Second
	gmailAPIPollMinActiveInterval     = 10 * time.Second
	gmailAPIPollMaxBackoff            = 5 * time.Minute
	gmailAPIPollRequestTimeout        = 20 * time.Second
	gmailAPIPollMaxParallelAccounts   = 10
)

type gmailPollRuntimeState struct {
	nextAt   time.Time
	failures int
}

func gmailAPIPollEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOFER_GMAIL_API_POLL"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return gmailAPIMailEnabled()
	}
}

func gmailAPIActivePollInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GOFER_GMAIL_ACTIVE_POLL_INTERVAL_SECONDS"))
	if raw == "" {
		return gmailAPIPollDefaultActiveInterval
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return gmailAPIPollDefaultActiveInterval
	}
	interval := time.Duration(seconds) * time.Second
	if interval < gmailAPIPollMinActiveInterval {
		return gmailAPIPollMinActiveInterval
	}
	return interval
}

func (o *SyncOrchestrator) BeginActiveUserSession(userID string) func() {
	if o == nil {
		return func() {}
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		userID = "default"
	}

	o.gmailPollMu.Lock()
	if o.activeUsers == nil {
		o.activeUsers = map[string]int{}
	}
	firstSession := o.activeUsers[userID] == 0
	o.activeUsers[userID]++
	o.gmailPollMu.Unlock()

	if firstSession {
		go o.pollGmailAPIForUsers(context.Background(), []string{userID}, true)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			o.gmailPollMu.Lock()
			defer o.gmailPollMu.Unlock()
			if o.activeUsers == nil {
				return
			}
			if o.activeUsers[userID] <= 1 {
				delete(o.activeUsers, userID)
				return
			}
			o.activeUsers[userID]--
		})
	}
}

func (o *SyncOrchestrator) activeUserIDs() []string {
	if o == nil {
		return nil
	}
	o.gmailPollMu.Lock()
	defer o.gmailPollMu.Unlock()
	ids := make([]string, 0, len(o.activeUsers))
	for userID, sessions := range o.activeUsers {
		if sessions > 0 {
			ids = append(ids, userID)
		}
	}
	return ids
}

func (o *SyncOrchestrator) runGmailAPIPoller(ctx context.Context) {
	if o == nil || o.db == nil || o.tokenProvider == nil || !gmailAPIPollEnabled() {
		return
	}
	ticker := time.NewTicker(gmailAPIPollSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.pollGmailAPIForUsers(ctx, o.activeUserIDs(), false)
		}
	}
}

func (o *SyncOrchestrator) pollGmailAPIForUsers(ctx context.Context, userIDs []string, force bool) {
	if o == nil || o.db == nil || o.tokenProvider == nil || len(userIDs) == 0 || !gmailAPIPollEnabled() {
		return
	}
	seen := map[string]bool{}
	var accountIDs []string
	for _, userID := range userIDs {
		if ctx.Err() != nil {
			return
		}
		ids, err := o.db.GetGmailEmailSyncAccountIDs(ctx, userID)
		if err != nil {
			log.Printf("gmail api poll user=%s: list accounts: %v", userID, err)
			continue
		}
		for _, accountID := range ids {
			accountID = strings.TrimSpace(accountID)
			if accountID == "" || seen[accountID] {
				continue
			}
			seen[accountID] = true
			accountIDs = append(accountIDs, accountID)
		}
	}
	if len(accountIDs) == 0 {
		return
	}

	parallelism := accountSyncParallelism(len(accountIDs), gmailAPIPollMaxParallelAccounts)
	jobs := make(chan string)
	var wg sync.WaitGroup
	for worker := 0; worker < parallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for accountID := range jobs {
				if ctx.Err() != nil {
					return
				}
				o.pollGmailAPIAccountIfDue(ctx, accountID, force)
			}
		}()
	}
	for _, accountID := range accountIDs {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- accountID:
		}
	}
	close(jobs)
	wg.Wait()
}

func (o *SyncOrchestrator) pollGmailAPIAccountIfDue(ctx context.Context, accountID string, force bool) {
	if !o.gmailAPIPollReserve(accountID, force) {
		return
	}
	if o.IsAccountSyncRunning(accountID) {
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, gmailAPIPollRequestTimeout)
	defer cancel()
	changed, _, err := o.checkGmailAPIProfile(pollCtx, accountID)
	o.gmailAPIPollComplete(accountID, err)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("gmail api poll account=%s: %v", accountID, err)
		}
		return
	}
	if changed {
		if o.SyncAccount(context.Background(), accountID) {
			log.Printf("gmail api poll account=%s: history changed, starting sync", accountID)
		}
	}
}

func (o *SyncOrchestrator) gmailAPIPollReserve(accountID string, force bool) bool {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	now := time.Now().UTC()
	interval := gmailAPIActivePollInterval()
	o.gmailPollMu.Lock()
	defer o.gmailPollMu.Unlock()
	if o.gmailPollRuntime == nil {
		o.gmailPollRuntime = map[string]gmailPollRuntimeState{}
	}
	state := o.gmailPollRuntime[accountID]
	if !force && !state.nextAt.IsZero() && now.Before(state.nextAt) {
		return false
	}
	state.nextAt = now.Add(interval)
	o.gmailPollRuntime[accountID] = state
	return true
}

func (o *SyncOrchestrator) gmailAPIPollComplete(accountID string, pollErr error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	now := time.Now().UTC()
	interval := gmailAPIActivePollInterval()
	o.gmailPollMu.Lock()
	defer o.gmailPollMu.Unlock()
	if o.gmailPollRuntime == nil {
		o.gmailPollRuntime = map[string]gmailPollRuntimeState{}
	}
	state := o.gmailPollRuntime[accountID]
	if pollErr != nil {
		state.failures++
		delay := interval * time.Duration(1<<minInt(state.failures, 4))
		if delay > gmailAPIPollMaxBackoff {
			delay = gmailAPIPollMaxBackoff
		}
		state.nextAt = now.Add(delay)
		o.gmailPollRuntime[accountID] = state
		return
	}
	state.failures = 0
	state.nextAt = now.Add(interval)
	o.gmailPollRuntime[accountID] = state
}

func (o *SyncOrchestrator) checkGmailAPIProfile(ctx context.Context, accountID string) (bool, string, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || o == nil || o.db == nil || o.tokenProvider == nil {
		return false, "", nil
	}
	token, err := o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		o.markGmailPollCheck(ctx, accountID, "", false, err)
		return false, "", err
	}
	profileHistoryID, err := getGmailProfileHistoryID(ctx, token)
	if err != nil {
		o.markGmailPollCheck(ctx, accountID, "", false, err)
		return false, "", err
	}
	state, err := o.db.GetLabelSyncState(ctx, accountID, storage.LabelProviderGmail, "messages")
	if err != nil {
		o.markGmailPollCheck(ctx, accountID, profileHistoryID, false, err)
		return false, profileHistoryID, err
	}
	hasDueMessageFetch, err := o.db.HasDueGmailMessageFetch(ctx, accountID)
	if err != nil {
		o.markGmailPollCheck(ctx, accountID, profileHistoryID, false, err)
		return false, profileHistoryID, err
	}
	cursor := strings.TrimSpace(state.Cursor)
	changed := cursor == "" || !state.LastSuccessAt.Valid || newerGmailHistoryID(cursor, profileHistoryID) != cursor || hasDueMessageFetch
	o.markGmailPollCheck(ctx, accountID, profileHistoryID, changed, nil)
	return changed, profileHistoryID, nil
}

func (o *SyncOrchestrator) markGmailPollCheck(ctx context.Context, accountID, profileHistoryID string, changed bool, pollErr error) {
	if o == nil || o.db == nil {
		return
	}
	prev, _ := o.db.GetGmailPollState(ctx, accountID)
	now := time.Now().UTC()
	prev.AccountID = accountID
	prev.ProfileHistoryID = strings.TrimSpace(profileHistoryID)
	prev.LastCheckedAt = sqlNullTime(now)
	if changed {
		prev.LastChangedAt = sqlNullTime(now)
	}
	if err := o.db.MarkGmailPollCheck(ctx, prev, changed, pollErr); err != nil && ctx.Err() == nil {
		log.Printf("gmail api poll account=%s: store state: %v", accountID, err)
	}
}

func sqlNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
