package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// In the shared store, contacts waiting to be resolved are pushed to their
// owner, with no account to go through.
func TestSharedNotificationPushesContactsToResolve(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default')`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUISettings(ctx, "default", map[string]string{"desktop_notifications": "true", "notification_mode": "web_push"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebPushSubscription(ctx, storage.WebPushSubscription{Endpoint: "https://push.example/default", UserID: "default", P256DH: "synthetic-public", Auth: "synthetic-auth"}); err != nil {
		t.Fatal(err)
	}
	service := New(db, mail.NewEventBus(), "public-key", "private-key", "mailto:admin@example.com")
	var pushed []map[string]any
	service.sendNotification = func(_ context.Context, payload []byte, _ *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		var data map[string]any
		_ = json.Unmarshal(payload, &data)
		pushed = append(pushed, data)
		return pushResponse(http.StatusCreated), nil
	}
	service.handleContactsUnresolved(ctx, mail.Event{Type: mail.EventContactActivity, UserID: "default", Payload: map[string]any{"event_type": "contacts_unresolved", "event_count": 3}})
	if len(pushed) != 1 || pushed[0]["url"] != "/contacts?resolve=1" || pushed[0]["tag"] != "gofer-contacts-unresolved" || pushed[0]["title"] == "" {
		t.Fatalf("pushed %+v", pushed)
	}
	if err := db.SetUISettings(ctx, "default", map[string]string{"notification_mode": "browser_tab"}); err != nil {
		t.Fatal(err)
	}
	service.handleContactsUnresolved(ctx, mail.Event{Type: mail.EventContactActivity, UserID: "default", Payload: map[string]any{"event_type": "contacts_unresolved", "event_count": 1}})
	if len(pushed) != 1 {
		t.Fatal("pushed with browser-tab notifications", pushed)
	}
}
