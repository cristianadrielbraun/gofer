package mail

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providerquota"
	"github.com/cristianadrielbraun/gofer/internal/providers"
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

// UserActive reports whether the user has Gofer open.
func (o *SyncOrchestrator) UserActive(userID string) bool {
	if o == nil {
		return false
	}
	if userID = strings.TrimSpace(userID); userID == "" {
		userID = "default"
	}
	o.gmailPollMu.Lock()
	defer o.gmailPollMu.Unlock()
	return o.activeUsers[userID] > 0
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
	var accounts []activePollAccount
	for _, userID := range userIDs {
		for _, provider := range []string{providers.ProviderGmail, providers.ProviderOutlook} {
			if ctx.Err() != nil {
				return
			}
			ids, err := o.db.GetProviderBackgroundEmailSyncAccountIDs(ctx, userID, provider)
			if err != nil {
				log.Printf("%s poll user=%s: list accounts: %v", provider, userID, err)
				continue
			}
			for _, accountID := range ids {
				accountID = strings.TrimSpace(accountID)
				if accountID == "" || seen[accountID] {
					continue
				}
				// Graph announces the mailbox changes of subscribed accounts.
				if provider == providers.ProviderOutlook && o.outlookSubs.subscribed(accountID, time.Now()) {
					continue
				}
				seen[accountID] = true
				accounts = append(accounts, activePollAccount{id: accountID, provider: provider})
			}
		}
	}
	if len(accounts) == 0 {
		return
	}

	parallelism := accountSyncParallelism(len(accounts), gmailAPIPollMaxParallelAccounts)
	jobs := make(chan activePollAccount)
	var wg sync.WaitGroup
	for worker := 0; worker < parallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for account := range jobs {
				if ctx.Err() != nil {
					return
				}
				o.pollAccountIfDue(ctx, account, force)
			}
		}()
	}
	for _, account := range accounts {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- account:
		}
	}
	close(jobs)
	wg.Wait()
}

// activePollAccount is a Gmail or Outlook account checked for changes while
// its owner has Gofer open.
type activePollAccount struct {
	id, provider string
}

func (o *SyncOrchestrator) pollAccountIfDue(ctx context.Context, account activePollAccount, force bool) {
	accountID := account.id
	if !o.gmailAPIPollReserve(accountID, force) {
		return
	}
	if o.IsAccountSyncRunning(accountID) {
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, gmailAPIPollRequestTimeout)
	defer cancel()
	var changed bool
	var err error
	if account.provider == providers.ProviderOutlook {
		changed, err = o.checkOutlookInbox(pollCtx, accountID)
	} else {
		changed, _, err = o.checkGmailAPIProfile(pollCtx, accountID)
	}
	o.gmailAPIPollComplete(accountID, err)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("%s poll account=%s: %v", account.provider, accountID, err)
		}
		return
	}
	if changed {
		if o.SyncAccount(context.Background(), accountID) {
			log.Printf("%s poll account=%s: changed, starting sync", account.provider, accountID)
		}
	}
}

func (o *SyncOrchestrator) checkOutlookInbox(ctx context.Context, accountID string) (bool, error) {
	ctx = providerquota.Background(ctx)
	graphTokens, ok := o.tokenProvider.(graphMailTokenProvider)
	if !ok {
		return false, fmt.Errorf("outlook graph mail token provider is unavailable")
	}
	token, err := graphTokens.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	return o.outlookInboxChanged(ctx, accountID, token)
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
	ctx = providerquota.Background(ctx)
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
