package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// outlookServiceTestTokens gives each Graph service its own token, so the
// fake can tell which one a call was made with.
type outlookServiceTestTokens struct{}

func (outlookServiceTestTokens) GetOAuthTokenForAccount(context.Context, string) (string, error) {
	return "", errors.New("not a Google account")
}

func (outlookServiceTestTokens) GetMicrosoftGraphMailTokenForAccount(context.Context, string) (string, error) {
	return "mail-token", nil
}
func (outlookServiceTestTokens) GetMicrosoftGraphCalendarTokenForAccount(context.Context, string) (string, error) {
	return "calendar-token", nil
}
func (outlookServiceTestTokens) GetMicrosoftGraphContactsTokenForAccount(context.Context, string) (string, error) {
	return "contacts-token", nil
}

// fakeOutlookServiceGraph keeps subscriptions as Graph would, recording each
// call with the token it came with.
type fakeOutlookServiceGraph struct {
	mu     sync.Mutex
	calls  []string
	listed []outlookGraphSubscription
	subs   map[string]outlookGraphSubscription
	next   int
}

func (g *fakeOutlookServiceGraph) request(_ context.Context, method, endpoint, token string, _ map[string]string, body, out any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := strings.TrimPrefix(endpoint, outlookGraphBaseURL)
	var response any
	switch {
	case method == http.MethodGet && path == "/subscriptions":
		g.calls = append(g.calls, token+" GET")
		response = outlookGraphSubscriptionsResponse{Value: g.listed}
	case method == http.MethodPost && path == "/subscriptions":
		created := body.(outlookGraphSubscription)
		g.next++
		created.ID = "sub-" + string(rune('0'+g.next))
		g.subs[created.ID] = created
		g.calls = append(g.calls, token+" POST "+created.Resource)
		response = created
	case method == http.MethodPatch:
		g.calls = append(g.calls, token+" PATCH "+strings.TrimPrefix(path, "/subscriptions/"))
		response = body
	case method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/subscriptions/")
		delete(g.subs, id)
		g.calls = append(g.calls, token+" DELETE "+id)
		return nil
	default:
		return errors.New("unexpected Graph call " + method + " " + path)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (g *fakeOutlookServiceGraph) takeCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls
	g.calls = nil
	return calls
}

func (g *fakeOutlookServiceGraph) subscription(resource string) outlookGraphSubscription {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, sub := range g.subs {
		if sub.Resource == resource {
			return sub
		}
	}
	return outlookGraphSubscription{}
}

func TestOutlookServiceClientStatesNameTheService(t *testing.T) {
	for _, service := range outlookPushServices {
		state, err := outlookServiceClientState(testOutlookWebhookKey, "acc.with.dots", service)
		if err != nil {
			t.Fatal(err)
		}
		account, got, ok := outlookClientStateTarget(testOutlookWebhookKey, state)
		if !ok || account != "acc.with.dots" || got != service {
			t.Fatalf("%q read back as %q %q %t", state, account, got, ok)
		}
	}
	mail, _ := outlookClientState(testOutlookWebhookKey, "acc-1")
	calendar, _ := outlookServiceClientState(testOutlookWebhookKey, "acc-1", OutlookPushCalendar)
	signature := func(state string) string { return state[strings.LastIndexByte(state, '.'):] }
	for _, forged := range []string{
		// A mail signature doesn't make a calendar state, nor the reverse.
		"acc-1.calendar" + signature(mail),
		"acc-1" + signature(calendar),
		// A calendar signature doesn't carry over to contacts or another account.
		"acc-1.contacts" + signature(calendar),
		"acc-2.calendar" + signature(calendar),
		// Only known services are read.
		"acc-1.files" + signature(calendar),
	} {
		if account, service, ok := outlookClientStateTarget(testOutlookWebhookKey, forged); ok {
			t.Fatalf("forged %q read as %q %q", forged, account, service)
		}
	}
}

// An Outlook account is subscribed to the calendars and contacts it syncs,
// each with its own token; the leftovers cleared for one service leave the
// others' alone; and a service the account stops syncing is unsubscribed.
func TestOutlookSubscriptionsFollowTheServicesAnAccountSyncs(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('out-1', 'default', ?, 'out-1@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(ctx, "default", "out-1", providers.ProviderOutlook, []storage.CalendarSource{{RemoteID: "cal-1", Name: "Calendar", IsPrimary: true, IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	graph := &fakeOutlookServiceGraph{subs: map[string]outlookGraphSubscription{}, listed: []outlookGraphSubscription{
		{ID: "old-mail", NotificationURL: testOutlookWebhookURL, Resource: "me/messages"},
		{ID: "old-events", NotificationURL: testOutlookWebhookURL, Resource: "me/events"},
	}}
	o := NewSyncOrchestrator(db, nil, nil, outlookServiceTestTokens{})
	o.EnableOutlookWebhooks(testOutlookWebhookURL, testOutlookWebhookKey)
	o.outlookGraphRequest = graph.request
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	o.sweepOutlookSubscriptions(ctx, now)
	equalCalls(t, "first sweep", graph.takeCalls(),
		"mail-token GET", "mail-token DELETE old-mail", "mail-token POST me/messages",
		"calendar-token GET", "calendar-token DELETE old-events", "calendar-token POST me/events",
		"contacts-token GET", "contacts-token POST me/contacts")
	for resource, service := range map[string]OutlookPushService{"me/events": OutlookPushCalendar, "me/contacts": OutlookPushContacts} {
		sub := graph.subscription(resource)
		if account, got, ok := outlookClientStateTarget(testOutlookWebhookKey, sub.ClientState); !ok || account != "out-1" || got != service {
			t.Fatalf("%s client state %q names %q %q", resource, sub.ClientState, account, got)
		}
	}

	o.sweepOutlookSubscriptions(ctx, now.Add(time.Hour))
	equalCalls(t, "sweep with fresh subscriptions", graph.takeCalls())

	// Contacts turned off and no calendar selected.
	if _, err := db.Write().Exec(`INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled) VALUES ('out-1', 'default', 'outlook', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected = 0 WHERE account_id = 'out-1'`); err != nil {
		t.Fatal(err)
	}
	o.sweepOutlookSubscriptions(ctx, now.Add(2*time.Hour))
	calls := graph.takeCalls()
	slices.Sort(calls)
	equalCalls(t, "sweep after calendars and contacts stopped", calls, "calendar-token DELETE sub-2", "contacts-token DELETE sub-3")
	if graph.subscription("me/messages").ID == "" {
		t.Fatal("the mail subscription went with the others")
	}
}

// Calendar and contact notifications refresh those services, not the mail;
// a lifecycle notification for one also runs the mail sync, which mends the
// account's subscriptions.
func TestOutlookServiceNotificationsRefreshTheirService(t *testing.T) {
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('out-1', 'default', ?, 'out-1@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatal(err)
	}
	o := NewSyncOrchestrator(db, nil, nil, outlookServiceTestTokens{})
	o.EnableOutlookWebhooks(testOutlookWebhookURL, testOutlookWebhookKey)
	mailSynced := make(chan string, 8)
	o.syncAccountOverride = func(_ context.Context, accountID string, _ bool) error {
		mailSynced <- accountID
		return nil
	}
	refreshed := make(chan string, 8)
	o.OnOutlookServiceChange(func(_ context.Context, service OutlookPushService, ids []string) {
		for _, id := range ids {
			refreshed <- string(service) + " " + id
		}
	})
	calendar, _ := outlookServiceClientState(testOutlookWebhookKey, "out-1", OutlookPushCalendar)
	contacts, _ := outlookServiceClientState(testOutlookWebhookKey, "out-1", OutlookPushContacts)

	o.ReceiveOutlookNotifications([]OutlookNotification{
		{SubscriptionID: "sub-2", ClientState: calendar},
		{SubscriptionID: "sub-2", ClientState: calendar},
		{SubscriptionID: "sub-3", ClientState: contacts},
	})
	var got []string
	for range 2 {
		select {
		case change := <-refreshed:
			got = append(got, change)
		case <-time.After(5 * time.Second):
			t.Fatalf("refreshed only %v", got)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"calendar out-1", "contacts out-1"}) {
		t.Fatalf("refreshed %v", got)
	}
	select {
	case account := <-mailSynced:
		t.Fatalf("calendar and contact changes synced the mail of %q", account)
	case change := <-refreshed:
		t.Fatalf("extra refresh %q", change)
	case <-time.After(100 * time.Millisecond):
	}

	o.ReceiveOutlookNotifications([]OutlookNotification{{SubscriptionID: "sub-2", ClientState: calendar, LifecycleEvent: "subscriptionRemoved"}})
	select {
	case <-mailSynced:
	case <-time.After(5 * time.Second):
		t.Fatal("a lifecycle notification didn't run the mail sync")
	}
}
