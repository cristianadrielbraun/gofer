package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const userOutlookHTTPScopes = "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/MailboxSettings.ReadWrite"

type userOutlookRouteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t userOutlookRouteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "graph.microsoft.com" {
		return t.base.RoundTrip(request)
	}
	copy := request.Clone(request.Context())
	address := *request.URL
	address.Scheme, address.Host = t.target.Scheme, t.target.Host
	address.Path = strings.TrimPrefix(address.Path, "/v1.0")
	copy.URL = &address
	return t.base.RoundTrip(copy)
}

func TestUserOutlookHTTPBodiesAttachmentsAndSyncControls(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		if owner != "alice" && owner != "bob" {
			http.Error(w, "invalid owner", 401)
			return
		}
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			http.Error(w, "missing immutable preference", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/mailFolders":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"id": "inbox-id", "displayName": "Inbox", "totalItemCount": 1, "unreadItemCount": 1}}})
		case "/me/mailFolders/inbox":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "inbox-id", "displayName": "Inbox"})
		case "/me/outlook/masterCategories":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
		case "/me/mailFolders/inbox-id/messages/delta":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"id": "m1", "internetMessageId": "<" + owner + "@outlook.test>", "subject": owner + " message", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@outlook.test"}}, "receivedDateTime": "2026-10-06T10:00:00Z", "hasAttachments": true}}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/mailFolders/inbox-id/messages/delta?%24deltatoken=100"})
		case "/me/messages/m1/$value":
			w.Header().Set("Content-Type", "message/rfc822")
			raw := "Message-ID: <" + owner + "@outlook.test>\r\nSubject: " + owner + " message\r\nFrom: sender@outlook.test\r\nTo: " + owner + "@outlook.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=test\r\n\r\n--test\r\nContent-Type: text/plain\r\n\r\n" + owner + " body\r\n--test\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=report.txt\r\n\r\n" + owner + " attachment\r\n--test--\r\n"
			_, _ = fmt.Fprint(w, raw)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: userOutlookRouteTransport{target: address, base: http.DefaultTransport}}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithCancel(t.Context())
	credentials, err := mailauth.NewUserCredentials(ctx, nil, f.routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { f.stopIMAP(); f.imap.Wait(); cancel(); credentials.Wait() })
	if err := f.imap.SetCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	accounts := map[string]string{}
	messageIDs, attachmentIDs := map[string]int64{}, map[string]int64{}
	for _, owner := range []string{"alice", "bob"} {
		account, err := f.accountStore.CreateAccount(t.Context(), owner, providers.OutlookAccountRequest(owner+"@outlook.test", owner, "same-subject"))
		if err != nil {
			t.Fatal(err)
		}
		accounts[owner] = account.ID
		expires := time.Now().Add(time.Hour)
		if err := credentials.UpsertForUser(t.Context(), owner, account.ID, "microsoft", "same-subject", owner+"-route-token", owner+"-refresh", "Bearer", &expires, userOutlookHTTPScopes); err != nil {
			t.Fatal(err)
		}
		if err := f.imap.Sync(t.Context(), owner, account.ID); err != nil {
			t.Fatal(err)
		}
		var messageID int64
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND remote_message_id='m1'`, account.ID).Scan(&messageID)
		}); err != nil {
			t.Fatal(err)
		}
		messageIDs[owner] = messageID
		body := f.request(owner, http.MethodGet, fmt.Sprintf("/email/%d/body", messageIDs[owner]), "")
		if body.Code != 200 || !strings.Contains(body.Body.String(), owner+" body") {
			t.Fatalf("owned Outlook HTTP body: %d %s", body.Code, body.Body.String())
		}
		var attachmentID int64
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT id FROM attachments WHERE message_id=?`, messageIDs[owner]).Scan(&attachmentID)
		}); err != nil {
			t.Fatal(err)
		}
		attachmentIDs[owner] = attachmentID
		attachment := f.request(owner, http.MethodGet, fmt.Sprintf("/api/attachments/%d/download", attachmentID), "")
		if attachment.Code != 200 || !strings.Contains(attachment.Body.String(), owner+" attachment") {
			t.Fatalf("owned Outlook HTTP attachment: %d %s", attachment.Code, attachment.Body.String())
		}
	}
	if messageIDs["alice"] != messageIDs["bob"] || attachmentIDs["alice"] != attachmentIDs["bob"] {
		t.Fatal("HTTP fixture did not exercise overlapping user-local IDs")
	}
	if response := f.request("alice", http.MethodPost, "/api/mail/sync/accounts/"+accounts["bob"], ""); response.Code != 404 {
		t.Fatalf("foreign Outlook manual sync: %d", response.Code)
	}
	if response := f.request("alice", http.MethodGet, "/email/999999/body", ""); response.Code != 404 {
		t.Fatalf("unknown Outlook message: %d", response.Code)
	}
	events := f.events.Subscribe()
	defer f.events.Unsubscribe(events)
	if response := f.request("alice", http.MethodPost, "/api/mail/sync/accounts/"+accounts["alice"], ""); response.Code != 200 {
		t.Fatalf("owned manual Outlook sync: %d %s", response.Code, response.Body.String())
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
wait:
	for {
		select {
		case event := <-events:
			if event.Type == mail.EventManualSyncComplete {
				if event.UserID != "alice" || event.Payload["failures"] != 0 {
					t.Fatalf("HTTP manual run: %+v", event)
				}
				break wait
			}
		case <-deadline.C:
			t.Fatal("HTTP manual Outlook sync did not complete")
		}
	}
	for _, enabled := range []string{"false", "true"} {
		form := url.Values{"service": {"email"}, "enabled": {enabled}}
		if response := f.request("alice", http.MethodPost, "/api/accounts/"+accounts["alice"]+"/service", form.Encode()); response.Code != 200 {
			t.Fatalf("Outlook email control %s: %d %s", enabled, response.Code, response.Body.String())
		}
	}
	form := url.Values{"sync_interval_minutes": {"2"}, "account_ids": {accounts["alice"]}}
	if response := f.request("alice", http.MethodPost, "/api/settings/sync", form.Encode()); response.Code != 200 {
		t.Fatalf("Outlook polling settings: %d %s", response.Code, response.Body.String())
	}
	form.Set("idle_folders", accounts["alice"]+":"+storage.FolderIDForIdentity(accounts["alice"], "gmail", "INBOX"))
	if response := f.request("alice", http.MethodPost, "/api/settings/sync", form.Encode()); response.Code < 400 {
		t.Fatal("Outlook accepted IMAP IDLE selection")
	}
	if response := f.request("alice", http.MethodPost, "/api/accounts/"+accounts["bob"]+"/service", "service=email&enabled=true"); response.Code != 404 {
		t.Fatalf("foreign Outlook service control: %d", response.Code)
	}
}
