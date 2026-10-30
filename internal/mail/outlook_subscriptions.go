package mail

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Outlook subscriptions ask Graph to post to Gofer's webhook when anything in
// the mailbox changes, in any folder, and, for accounts that sync them, when
// calendar events or contacts change. Graph keeps a subscription for at most
// a few days, so Gofer renews it well before it ends.
const (
	outlookSubscriptionResource = "me/messages"
	outlookSubscriptionLifetime = 70 * time.Hour
	outlookSubscriptionRenewal  = 24 * time.Hour
	outlookSubscriptionTimeout  = 30 * time.Second
	// Graph checks the webhook before it creates a subscription, so the
	// first attempt waits for the server to be listening.
	outlookSubscriptionFirstDelay = 15 * time.Second
	outlookSubscriptionSweep      = time.Minute
	outlookSubscriptionRetry      = 5 * time.Minute
	outlookSubscriptionMaxRetry   = time.Hour
	outlookClientStateMaxLength   = 128
)

// OutlookPushService is what an Outlook subscription announces changes to:
// the mailbox, which is the zero value, calendar events, or contacts.
type OutlookPushService string

const (
	OutlookPushMail     OutlookPushService = ""
	OutlookPushCalendar OutlookPushService = "calendar"
	OutlookPushContacts OutlookPushService = "contacts"
)

// outlookPushServices are the services an Outlook account can be subscribed
// to, mail first.
var outlookPushServices = []OutlookPushService{OutlookPushMail, OutlookPushCalendar, OutlookPushContacts}

// resource is what the service's subscription watches. Graph announces
// changes to events in every calendar of the mailbox, and to contacts in its
// default contacts folder.
func (s OutlookPushService) resource() string {
	switch s {
	case OutlookPushCalendar:
		return "me/events"
	case OutlookPushContacts:
		return "me/contacts"
	}
	return outlookSubscriptionResource
}

// outlookResourceService is the service a subscription Graph lists is for.
func outlookResourceService(resource string) OutlookPushService {
	resource = strings.ToLower(strings.TrimRight(strings.TrimSpace(resource), "/"))
	switch {
	case strings.HasSuffix(resource, "/events"):
		return OutlookPushCalendar
	case strings.HasSuffix(resource, "/contacts"):
		return OutlookPushContacts
	}
	return OutlookPushMail
}

// outlookClientState is the secret Graph repeats in every notification of
// an account's mail subscription. It names the account and signs that name
// with Gofer's key, so a notification proves which account it is for without
// Gofer storing a secret per subscription.
func outlookClientState(key []byte, accountID string) (string, error) {
	return outlookServiceClientState(key, accountID, OutlookPushMail)
}

// outlookServiceClientState is outlookClientState for any service. The
// calendar and contacts ones also name the service, signed with it.
func outlookServiceClientState(key []byte, accountID string, service OutlookPushService) (string, error) {
	state := accountID + "." + outlookClientStateSignature(key, accountID)
	if service != OutlookPushMail {
		state = accountID + "." + string(service) + "." + outlookServiceClientStateSignature(key, accountID, service)
	}
	if accountID == "" || len(state) > outlookClientStateMaxLength {
		return "", fmt.Errorf("account ID %q does not fit an Outlook subscription", accountID)
	}
	return state, nil
}

// outlookClientStateAccount returns the account a notification's client
// state was made for, if Gofer's key signed it.
func outlookClientStateAccount(key []byte, state string) (string, bool) {
	accountID, _, ok := outlookClientStateTarget(key, state)
	return accountID, ok
}

// outlookClientStateTarget returns the account and service a notification's
// client state was made for, if Gofer's key signed it.
func outlookClientStateTarget(key []byte, state string) (string, OutlookPushService, bool) {
	dot := strings.LastIndexByte(state, '.')
	if dot <= 0 || len(state) > outlookClientStateMaxLength {
		return "", "", false
	}
	signed, signature := state[:dot], []byte(state[dot+1:])
	if hmac.Equal(signature, []byte(outlookClientStateSignature(key, signed))) {
		return signed, OutlookPushMail, true
	}
	kind := strings.LastIndexByte(signed, '.')
	if kind <= 0 {
		return "", "", false
	}
	accountID, service := signed[:kind], OutlookPushService(signed[kind+1:])
	if service != OutlookPushCalendar && service != OutlookPushContacts {
		return "", "", false
	}
	if !hmac.Equal(signature, []byte(outlookServiceClientStateSignature(key, accountID, service))) {
		return "", "", false
	}
	return accountID, service, true
}

func outlookClientStateSignature(key []byte, accountID string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("gofer outlook webhook\x00" + accountID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func outlookServiceClientStateSignature(key []byte, accountID string, service OutlookPushService) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("gofer outlook webhook\x00" + accountID + "\x00" + string(service)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type outlookGraphSubscription struct {
	ID                 string `json:"id,omitempty"`
	ChangeType         string `json:"changeType,omitempty"`
	Resource           string `json:"resource,omitempty"`
	NotificationURL    string `json:"notificationUrl,omitempty"`
	LifecycleURL       string `json:"lifecycleNotificationUrl,omitempty"`
	ExpirationDateTime string `json:"expirationDateTime,omitempty"`
	ClientState        string `json:"clientState,omitempty"`
}

type outlookGraphSubscriptionsResponse struct {
	Value    []outlookGraphSubscription `json:"value"`
	NextLink string                     `json:"@odata.nextLink"`
}

func (s outlookGraphSubscription) expires() time.Time {
	at, err := time.Parse(time.RFC3339Nano, s.ExpirationDateTime)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// createOutlookSubscription subscribes to the service's changes in the
// account, such as its whole mailbox. Graph answers only after the webhook
// repeated its validation token.
func createOutlookSubscription(ctx context.Context, request graphJSONRequest, token, notificationURL, clientState string, service OutlookPushService, now time.Time) (outlookGraphSubscription, error) {
	body := outlookGraphSubscription{
		ChangeType:         "created,updated,deleted",
		Resource:           service.resource(),
		NotificationURL:    notificationURL,
		LifecycleURL:       notificationURL,
		ExpirationDateTime: now.Add(outlookSubscriptionLifetime).UTC().Format(time.RFC3339),
		ClientState:        clientState,
	}
	var created outlookGraphSubscription
	err := request(ctx, http.MethodPost, outlookGraphBaseURL+"/subscriptions", token, nil, body, &created)
	if err == nil && (strings.TrimSpace(created.ID) == "" || created.expires().IsZero()) {
		err = fmt.Errorf("Graph created a subscription without an ID or an end")
	}
	return created, err
}

// renewOutlookSubscription moves a subscription's end out again. Graph also
// takes a renewal as reauthorizing the subscription.
func renewOutlookSubscription(ctx context.Context, request graphJSONRequest, token, id string, now time.Time) (time.Time, error) {
	body := outlookGraphSubscription{ExpirationDateTime: now.Add(outlookSubscriptionLifetime).UTC().Format(time.RFC3339)}
	var renewed outlookGraphSubscription
	if err := request(ctx, http.MethodPatch, outlookGraphBaseURL+"/subscriptions/"+url.PathEscape(id), token, nil, body, &renewed); err != nil {
		return time.Time{}, err
	}
	if renewed.expires().IsZero() {
		return time.Time{}, fmt.Errorf("Graph renewed subscription %s without an end", id)
	}
	return renewed.expires(), nil
}

func deleteOutlookSubscription(ctx context.Context, request graphJSONRequest, token, id string) error {
	err := request(ctx, http.MethodDelete, outlookGraphBaseURL+"/subscriptions/"+url.PathEscape(id), token, nil, nil, nil)
	if status, ok := providerAPIStatus(err); ok && status == http.StatusNotFound {
		return nil
	}
	return err
}

// deleteStaleOutlookSubscriptions removes the account's subscriptions for the
// service to this webhook left from before Gofer restarted, which nothing
// renews. Each service clears only its own, so clearing one never removes
// another service's subscription in use.
func deleteStaleOutlookSubscriptions(ctx context.Context, request graphJSONRequest, token, notificationURL string, service OutlookPushService) error {
	endpoint := outlookGraphBaseURL + "/subscriptions"
	for page := 0; endpoint != "" && page < 10; page++ {
		var response outlookGraphSubscriptionsResponse
		if err := request(ctx, http.MethodGet, endpoint, token, nil, nil, &response); err != nil {
			return err
		}
		for _, subscription := range response.Value {
			if subscription.NotificationURL != notificationURL || strings.TrimSpace(subscription.ID) == "" || outlookResourceService(subscription.Resource) != service {
				continue
			}
			if err := deleteOutlookSubscription(ctx, request, token, subscription.ID); err != nil {
				return err
			}
		}
		endpoint = strings.TrimSpace(response.NextLink)
		if endpoint != "" {
			if err := validateUserGraphEndpoint(endpoint); err != nil {
				return err
			}
		}
	}
	return nil
}
