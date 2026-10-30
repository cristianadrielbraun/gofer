package mail

import (
	"context"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// EnableOutlookWebhooks makes Start subscribe Outlook accounts to changes,
// announced at notificationURL. key signs each subscription's client state.
// It is called before Start.
func (o *SyncOrchestrator) EnableOutlookWebhooks(notificationURL string, key []byte) {
	o.outlookSubs = newOutlookSubscriptions(notificationURL, key)
}

func (o *SyncOrchestrator) outlookSubscriptionRequest() graphJSONRequest {
	if o.outlookGraphRequest != nil {
		return o.outlookGraphRequest
	}
	return providerJSON
}

// OnOutlookServiceChange sets what refreshes the calendars and contacts of
// the accounts Microsoft announces changes for. It is called before Start.
func (o *SyncOrchestrator) OnOutlookServiceChange(change OutlookServiceChange) {
	o.outlookServiceChange = change
}

func (o *SyncOrchestrator) outlookSubscriptionToken(accountID string) outlookSubscriptionToken {
	return outlookServiceToken(o.tokenProvider, accountID, OutlookPushMail)
}

func (o *SyncOrchestrator) runOutlookSubscriptions(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(outlookSubscriptionFirstDelay):
	}
	ticker := time.NewTicker(outlookSubscriptionSweep)
	defer ticker.Stop()
	for {
		o.sweepOutlookSubscriptions(ctx, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.outlookSubs.wake:
		}
	}
}

// sweepOutlookSubscriptions subscribes Outlook accounts that receive mail to
// their mailbox changes, and to those of the calendars and contacts they
// sync, renews subscriptions nearing their end, and drops those of accounts
// that were removed or stopped receiving mail.
func (o *SyncOrchestrator) sweepOutlookSubscriptions(ctx context.Context, now time.Time) {
	if o.outlookSubs == nil {
		return
	}
	ids, err := o.db.GetAllEmailSyncAccountIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("outlook webhook: list accounts: %v", err)
		}
		return
	}
	wanted := make(map[string]bool)
	for _, accountID := range ids {
		if provider, err := o.db.GetAccountProvider(ctx, accountID); err != nil || provider != providers.ProviderOutlook {
			continue
		}
		push, err := o.db.ProviderPushEnabled(ctx, accountID)
		if err != nil {
			// Leave the subscriptions as they are until the choice can be read.
			wanted[accountID] = true
			continue
		}
		if push {
			wanted[accountID] = true
			o.outlookSubs.keep(ctx, accountID, o.outlookSubscriptionToken(accountID), o.outlookSubscriptionRequest(), now)
			if calendar, contacts, err := o.db.OutlookPushServices(ctx, accountID); err == nil {
				o.outlookSubs.keepServices(ctx, accountID, calendar, contacts, o.tokenProvider, o.outlookSubscriptionRequest(), now)
			}
		}
	}
	for _, key := range o.outlookSubs.tracked() {
		if !wanted[key.account] {
			o.outlookSubs.dropService(ctx, key, outlookServiceToken(o.tokenProvider, key.account, key.service), o.outlookSubscriptionRequest())
		}
	}
}

// ReceiveOutlookNotifications syncs the accounts Graph announced changes
// for. Subscription problems it reports wake the subscription loop.
func (o *SyncOrchestrator) ReceiveOutlookNotifications(notifications []OutlookNotification) {
	changed := o.outlookSubs.receive(notifications)
	if len(changed) == 0 {
		return
	}
	ctx := o.accountLifecycleContext(context.Background())
	go func() {
		for _, accountID := range changed[OutlookPushMail] {
			if o.db.IsEmailSyncEnabled(ctx, accountID) {
				o.requestPushSync(ctx, accountID)
			}
		}
		if o.outlookServiceChange == nil {
			return
		}
		for _, service := range outlookPushServices[1:] {
			if ids := changed[service]; len(ids) > 0 {
				o.outlookServiceChange(ctx, service, ids)
			}
		}
	}()
}

// OutlookPushStatus reports the account's subscription for the settings page.
func (o *SyncOrchestrator) OutlookPushStatus(accountID string) OutlookPushStatus {
	return o.outlookSubs.status(accountID, time.Now().UTC())
}

// CheckOutlookPush makes a new subscription for the account now, which shows
// whether Microsoft can reach Gofer.
func (o *SyncOrchestrator) CheckOutlookPush(ctx context.Context, accountID string) error {
	if push, err := o.db.ProviderPushEnabled(ctx, accountID); err != nil {
		return err
	} else if !push {
		return errOutlookPushOff
	}
	return o.outlookSubs.check(ctx, accountID, o.outlookSubscriptionToken(accountID), o.outlookSubscriptionRequest(), time.Now().UTC())
}

// OutlookPushChanged applies an account's new choice between notifications
// and scheduled checks without waiting for the next sweep.
func (o *SyncOrchestrator) OutlookPushChanged() {
	o.outlookSubs.nudge()
}
