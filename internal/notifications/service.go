package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type Service struct {
	workers          sync.WaitGroup
	db               *storage.DB
	userStorage      *storage.AccountRouting
	sendNotification func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error)
	events           *mail.EventBus
	vapidPublicKey   string
	vapidPrivateKey  string
	vapidSubject     string
}

// pushHTTPClient delivers notifications to the push service each browser
// named when it subscribed. Browsers supply that address, so delivery goes
// through netguard and never reaches a private network.
var pushHTTPClient = &http.Client{Timeout: 15 * time.Second, Transport: netguard.NewHTTPTransport(nil)}

func New(db *storage.DB, events *mail.EventBus, vapidPublicKey, vapidPrivateKey, vapidSubject string) *Service {
	return &Service{
		db:               db,
		events:           events,
		vapidPublicKey:   vapidPublicKey,
		vapidPrivateKey:  vapidPrivateKey,
		vapidSubject:     vapidSubject,
		sendNotification: webpush.SendNotificationWithContext,
	}
}

func NewWithUserStorage(routing *storage.AccountRouting, events *mail.EventBus, vapidPublicKey, vapidPrivateKey, vapidSubject string) (*Service, error) {
	if routing == nil {
		return nil, errors.New("user routing is required")
	}
	s := New(routing.System(), events, vapidPublicKey, vapidPrivateKey, vapidSubject)
	s.userStorage = routing
	return s, nil
}

func (s *Service) Start(ctx context.Context) {
	if s == nil || s.db == nil || s.events == nil {
		return
	}

	s.workers.Add(1)
	// Only new mail, event reminders and contact activity, which is mostly
	// sync progress and is dropped here unless contacts wait to be resolved,
	// so other events cannot fill the queue ahead of them.
	events := s.events.SubscribeFor(mail.SubscribeOptions{All: true, Types: []mail.EventType{mail.EventNewMail, mail.EventCalendarReminder, mail.EventContactActivity}})
	go func() {
		defer s.workers.Done()
		defer events.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-events.Ready():
				for _, event := range events.Take() {
					if event.Type == mail.EventCalendarReminder {
						s.handleCalendarReminder(ctx, event)
					} else if event.Type == mail.EventContactActivity {
						s.handleContactsUnresolved(ctx, event)
					} else {
						s.handleNewMail(ctx, event)
					}
				}
			}
		}
	}()
}

func (s *Service) handleNewMail(ctx context.Context, event mail.Event) {
	if event.AccountID == "" || event.FolderID == "" {
		return
	}
	if roleSuppressesNotification(event.FolderRole) {
		return
	}

	unreadCount := intPayload(event.Payload, "unread_count")
	if unreadCount <= 0 {
		return
	}

	var userID string
	var settings map[string]string
	var err error
	if s.userStorage != nil {
		err = s.userStorage.WithAccount(ctx, event.AccountID, func(db *storage.DB, owner string) error {
			if event.AdminOnly || (event.UserID != "" && event.UserID != owner) {
				return storage.ErrAccountRoute
			}
			var role string
			if err := db.Read().QueryRowContext(ctx, `SELECT role FROM folders WHERE id = ? AND account_id = ?`, event.FolderID, event.AccountID).Scan(&role); err != nil {
				return err
			}
			if roleSuppressesNotification(role) {
				return storage.ErrAccountRoute
			}
			userID, settings = owner, db.GetUISettings(ctx, owner)
			return nil
		})
	} else {
		userID, err = s.db.GetAccountUserID(ctx, event.AccountID)
		if err == nil {
			settings = s.db.GetUISettings(ctx, userID)
		}
	}
	if err != nil || userID == "" {
		if err != nil {
			log.Printf("notifications: account user lookup failed: %v", err)
		}
		return
	}
	// The push is written in the owner's language, with the folder's
	// translated name while it keeps the one sync gave it.
	userCtx := i18n.ForUser(ctx, userID)
	payload := event.Payload
	if name := stringPayload(payload, "folder_name"); name != "" {
		payload = make(map[string]any, len(event.Payload))
		for key, value := range event.Payload {
			payload[key] = value
		}
		payload["folder_name"] = i18n.FolderName(userCtx, event.FolderRole, name)
	}
	title, message, icon := newMailNotificationAppearance(userCtx, payload, unreadCount, settings)
	if settings["desktop_notifications"] != "true" {
		return
	}
	mode := notificationMode(settings["notification_mode"])
	if mode == "web_push" || mode == "auto" {
		payload := map[string]any{
			"title":     title,
			"body":      message,
			"tag":       "gofer-new-mail-" + event.AccountID + "-" + event.FolderID,
			"folder_id": event.FolderID,
			"url":       "/folder/" + event.FolderID,
		}
		payload["icon"] = icon
		if initials := newMailNotificationInitials(event.Payload, settings); initials != "" {
			payload["initials"] = initials
		}
		s.sendWebPush(ctx, userID, event.AccountID, payload, "gofer-new-mail", webpush.UrgencyNormal)
	}
}

// handleCalendarReminder pushes a due event reminder, with the owner's
// notification settings for new mail.
func (s *Service) handleCalendarReminder(ctx context.Context, event mail.Event) {
	if event.UserID == "" || event.AccountID == "" {
		return
	}
	var settings map[string]string
	var err error
	if s.userStorage != nil {
		err = s.userStorage.WithAccount(ctx, event.AccountID, func(db *storage.DB, owner string) error {
			if owner != event.UserID {
				return storage.ErrAccountRoute
			}
			settings = db.GetUISettings(ctx, owner)
			return nil
		})
	} else {
		var owner string
		if owner, err = s.db.GetAccountUserID(ctx, event.AccountID); err == nil && owner != event.UserID {
			err = storage.ErrAccountRoute
		}
		if err == nil {
			settings = s.db.GetUISettings(ctx, event.UserID)
		}
	}
	if err != nil || settings["desktop_notifications"] != "true" {
		return
	}
	if mode := notificationMode(settings["notification_mode"]); mode != "web_push" && mode != "auto" {
		return
	}
	payload := map[string]any{"title": stringPayload(event.Payload, "title"), "body": stringPayload(event.Payload, "body"), "tag": stringPayload(event.Payload, "tag"), "url": stringPayload(event.Payload, "url")}
	s.sendWebPush(ctx, event.UserID, event.AccountID, payload, "gofer-calendar-reminder", webpush.UrgencyHigh)
}

// handleContactsUnresolved pushes that synced contacts were deleted in one
// of their places and wait to be resolved, with the owner's notification
// settings for new mail. Contacts belong to the user, not to an account.
func (s *Service) handleContactsUnresolved(ctx context.Context, event mail.Event) {
	if event.AdminOnly || event.UserID == "" || stringPayload(event.Payload, "event_type") != "contacts_unresolved" {
		return
	}
	var settings map[string]string
	if s.userStorage != nil {
		if err := s.userStorage.WithUser(ctx, event.UserID, func(db *storage.DB) error {
			settings = db.GetUISettings(ctx, event.UserID)
			return nil
		}); err != nil {
			return
		}
	} else {
		settings = s.db.GetUISettings(ctx, event.UserID)
	}
	if settings["desktop_notifications"] != "true" {
		return
	}
	if mode := notificationMode(settings["notification_mode"]); mode != "web_push" && mode != "auto" {
		return
	}
	userCtx := i18n.ForUser(ctx, event.UserID)
	count := intPayload(event.Payload, "event_count")
	if count < 1 {
		count = 1
	}
	// The same words as the toast an open page shows.
	title := i18n.N(userCtx, "js.contacts.unresolved.arrived", count)
	if name := stringPayload(event.Payload, "email"); count == 1 && name != "" {
		title = i18n.T(userCtx, "js.contacts.unresolved.arrived_one", "name", name)
	}
	payload := map[string]any{"title": title, "body": i18n.T(userCtx, "js.contacts.unresolved.arrived_detail"), "tag": "gofer-contacts-unresolved", "url": "/contacts?resolve=1"}
	s.sendWebPush(ctx, event.UserID, "", payload, "gofer-contacts-unresolved", webpush.UrgencyNormal)
}

func notificationMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "web_push", "browser_tab", "off":
		return mode
	default:
		return "auto"
	}
}

func (s *Service) sendWebPush(ctx context.Context, userID, accountID string, payloadData map[string]any, topic string, urgency webpush.Urgency) {
	if s.vapidPublicKey == "" || s.vapidPrivateKey == "" {
		return
	}
	subs, err := s.db.ListWebPushSubscriptions(ctx, userID)
	if err != nil {
		log.Printf("notifications: list web push subscriptions failed: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}

	payload, err := json.Marshal(payloadData)
	if err != nil {
		return
	}

	for _, sub := range subs {
		if s.userStorage != nil {
			if err := s.withOwner(ctx, userID, accountID, func(*storage.DB) error { return nil }); err != nil {
				return
			}
		}
		pushSub := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				Auth:   sub.Auth,
				P256dh: sub.P256DH,
			},
		}
		pushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := s.sendNotification(pushCtx, payload, pushSub, &webpush.Options{
			HTTPClient:      pushHTTPClient,
			Subscriber:      s.vapidSubject,
			VAPIDPublicKey:  s.vapidPublicKey,
			VAPIDPrivateKey: s.vapidPrivateKey,
			TTL:             60,
			Topic:           topic,
			Urgency:         urgency,
		})
		cancel()
		if resp != nil {
			resp.Body.Close()
		}
		if err != nil {
			log.Printf("notifications: web push failed: %v", err)
			_ = s.recordDelivery(ctx, userID, accountID, sub, false, err.Error())
			continue
		}
		if resp != nil && (resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound) {
			_ = s.recordDelivery(ctx, userID, accountID, sub, true, "")
		} else if resp != nil && resp.StatusCode >= 400 {
			_ = s.recordDelivery(ctx, userID, accountID, sub, false, resp.Status)
		}
	}
}

func (s *Service) recordDelivery(ctx context.Context, userID, accountID string, sub storage.WebPushSubscription, remove bool, message string) error {
	update := func(*storage.DB) error {
		if remove {
			return s.db.DeleteWebPushSubscriptionIfCurrent(ctx, sub)
		}
		return s.db.SetWebPushSubscriptionErrorIfCurrent(ctx, sub, message)
	}
	if s.userStorage != nil {
		return s.withOwner(ctx, userID, accountID, update)
	}
	return update(nil)
}

// withOwner checks the user still owns the account a push is about, or
// is still a user, for a push about no account.
func (s *Service) withOwner(ctx context.Context, userID, accountID string, fn func(*storage.DB) error) error {
	if accountID == "" {
		return s.userStorage.WithUser(ctx, userID, fn)
	}
	return s.userStorage.WithAccountForUser(ctx, userID, accountID, fn)
}

func roleSuppressesNotification(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "sent", "drafts", "trash", "junk", "archive":
		return true
	default:
		return false
	}
}

func newMailNotificationText(ctx context.Context, payload map[string]any, unreadCount int) (string, string) {
	sender := strings.TrimSpace(stringPayload(payload, "from_name"))
	if sender == "" {
		sender = strings.TrimSpace(stringPayload(payload, "from_email"))
	}
	if sender == "" {
		sender = i18n.T(ctx, "js.notify.new_mail")
	}

	subject := i18n.Subject(ctx, strings.TrimSpace(stringPayload(payload, "subject")))
	if subject == "" {
		subject = i18n.T(ctx, "js.mail.no_subject")
	}

	folder := strings.TrimSpace(stringPayload(payload, "folder_name"))
	if folder == "" {
		folder = i18n.T(ctx, "js.mail.inbox")
	}

	if unreadCount == 1 {
		return sender, subject
	}
	return i18n.N(ctx, "js.notify.new_messages", unreadCount), i18n.T(ctx, "js.notify.body_many", "sender", sender, "subject", subject, "folder", folder)
}

func newMailNotificationIcon(payload map[string]any) string {
	if icon := strings.TrimSpace(stringPayload(payload, "avatar_url")); icon != "" {
		return icon
	}
	return strings.TrimSpace(stringPayload(payload, "icon"))
}

func stringPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	switch value := payload[key].(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}

func intPayload(payload map[string]any, key string) int {
	if payload == nil {
		return 0
	}
	switch value := payload[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case jsonNumber:
		n, _ := strconv.Atoi(value.String())
		return n
	default:
		return 0
	}
}

type jsonNumber interface {
	String() string
}

// Wait joins notification workers after their lifecycle contexts are canceled.
func (s *Service) Wait() { s.workers.Wait() }

// SendTestPush sends a test notification to one of the user's Web Push subscriptions,
// the one belonging to the browser that asked for it, through the same path as new-mail
// notifications.
func (s *Service) SendTestPush(ctx context.Context, userID, endpoint string) error {
	if s == nil || s.db == nil || s.vapidPublicKey == "" || s.vapidPrivateKey == "" {
		return storage.ErrNoWebPushSubscription
	}
	endpoint = strings.TrimSpace(endpoint)
	subs, err := s.db.ListWebPushSubscriptions(ctx, userID)
	if err != nil {
		return err
	}
	var target *storage.WebPushSubscription
	for i := range subs {
		if endpoint != "" && subs[i].Endpoint == endpoint {
			target = &subs[i]
			break
		}
	}
	if target == nil {
		return storage.ErrNoWebPushSubscription
	}

	var settings map[string]string
	if s.userStorage != nil {
		err = s.userStorage.WithUser(ctx, userID, func(db *storage.DB) error {
			settings = db.GetUISettings(ctx, userID)
			return nil
		})
	} else {
		settings = s.db.GetUISettings(ctx, userID)
	}
	if err != nil {
		return err
	}
	userCtx := i18n.ForUser(ctx, userID)
	example := notificationAppearanceExample(userCtx)
	title, body, icon := newMailNotificationAppearance(userCtx, example, 1, settings)
	payload, err := json.Marshal(map[string]any{
		"title":    title,
		"body":     body,
		"icon":     icon,
		"initials": newMailNotificationInitials(example, settings),
		"tag":   "gofer-test-notification",
		"url":   "/settings/advanced",
	})
	if err != nil {
		return err
	}
	pushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := s.sendNotification(pushCtx, payload, &webpush.Subscription{
		Endpoint: target.Endpoint,
		Keys:     webpush.Keys{Auth: target.Auth, P256dh: target.P256DH},
	}, &webpush.Options{
		HTTPClient:      pushHTTPClient,
		Subscriber:      s.vapidSubject,
		VAPIDPublicKey:  s.vapidPublicKey,
		VAPIDPrivateKey: s.vapidPrivateKey,
		TTL:             60,
		Topic:           "gofer-test",
		Urgency:         webpush.UrgencyHigh,
	})
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		_ = s.db.SetWebPushSubscriptionErrorIfCurrent(ctx, *target, err.Error())
		return err
	}
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		_ = s.db.DeleteWebPushSubscriptionIfCurrent(ctx, *target)
		return storage.ErrNoWebPushSubscription
	}
	if resp.StatusCode >= 400 {
		_ = s.db.SetWebPushSubscriptionErrorIfCurrent(ctx, *target, resp.Status)
		return errors.New("the push service rejected the notification: " + resp.Status)
	}
	return nil
}
