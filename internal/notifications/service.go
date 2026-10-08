package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/cristianadrielbraun/gofer/internal/mail"
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
	ch := s.events.Subscribe()
	go func() {
		defer s.workers.Done()
		defer s.events.Unsubscribe(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-ch:
				if event.Type == mail.EventNewMail {
					s.handleNewMail(ctx, event)
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
	title, message := newMailNotificationText(event.Payload, unreadCount)
	if settings["desktop_notifications"] != "true" {
		return
	}
	mode := notificationMode(settings["notification_mode"])
	if mode == "web_push" || mode == "auto" {
		s.sendWebPush(ctx, userID, event, title, message)
	}
}

func notificationMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "web_push", "browser_tab", "off":
		return mode
	default:
		return "auto"
	}
}

func (s *Service) sendWebPush(ctx context.Context, userID string, event mail.Event, title, message string) {
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

	payloadData := map[string]any{
		"title":     title,
		"body":      message,
		"tag":       "gofer-new-mail-" + event.AccountID + "-" + event.FolderID,
		"folder_id": event.FolderID,
		"url":       "/folder/" + event.FolderID,
	}
	if icon := newMailNotificationIcon(event.Payload); icon != "" {
		payloadData["icon"] = icon
	}

	payload, err := json.Marshal(payloadData)
	if err != nil {
		return
	}

	for _, sub := range subs {
		if s.userStorage != nil {
			if err := s.userStorage.WithAccountForUser(ctx, userID, event.AccountID, func(*storage.DB) error { return nil }); err != nil {
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
			Subscriber:      s.vapidSubject,
			VAPIDPublicKey:  s.vapidPublicKey,
			VAPIDPrivateKey: s.vapidPrivateKey,
			TTL:             60,
			Topic:           "gofer-new-mail",
			Urgency:         webpush.UrgencyNormal,
		})
		cancel()
		if resp != nil {
			resp.Body.Close()
		}
		if err != nil {
			log.Printf("notifications: web push failed: %v", err)
			_ = s.recordDelivery(ctx, userID, event.AccountID, sub, false, err.Error())
			continue
		}
		if resp != nil && (resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound) {
			_ = s.recordDelivery(ctx, userID, event.AccountID, sub, true, "")
		} else if resp != nil && resp.StatusCode >= 400 {
			_ = s.recordDelivery(ctx, userID, event.AccountID, sub, false, resp.Status)
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
		return s.userStorage.WithAccountForUser(ctx, userID, accountID, update)
	}
	return update(nil)
}

func roleSuppressesNotification(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "sent", "drafts", "trash", "junk", "archive":
		return true
	default:
		return false
	}
}

func newMailNotificationText(payload map[string]any, unreadCount int) (string, string) {
	sender := strings.TrimSpace(stringPayload(payload, "from_name"))
	if sender == "" {
		sender = strings.TrimSpace(stringPayload(payload, "from_email"))
	}
	if sender == "" {
		sender = "New mail"
	}

	subject := strings.TrimSpace(stringPayload(payload, "subject"))
	if subject == "" {
		subject = "(no subject)"
	}

	folder := strings.TrimSpace(stringPayload(payload, "folder_name"))
	if folder == "" {
		folder = "Inbox"
	}

	if unreadCount == 1 {
		return sender, subject
	}
	return strconv.Itoa(unreadCount) + " new messages", sender + ": " + subject + " - " + folder
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
