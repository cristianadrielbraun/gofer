package notifications

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserJMAPHTTPRefreshAndSyncControls(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	f.useIMAPServer(t, newRoutedIMAPServer(t))
	server := newJMAPDraftServer(t)
	endpoint, err := jmap.ParseEndpoint(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPlaintextTransportException(t.Context(), "http", endpoint.Host, endpoint.Port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "http", endpoint.Host, endpoint.Port, "administrator"); err != nil {
		t.Fatal(err)
	}
	account, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "robin@example.test", JMAPSessionURL: server.URL + "/.well-known/jmap",
		Username: "robin@example.test", Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := f.events.Subscribe()
	defer f.events.Unsubscribe(events)
	for _, tc := range []struct {
		path  string
		total int
	}{{"/api/mail/sync/accounts/" + account.ID, 1}, {"/api/mail/sync", 2}} {
		response := f.request("alice", http.MethodPost, tc.path, "")
		runID := response.Header().Get("X-Gofer-Mail-Sync-Run-ID")
		if response.Code != http.StatusOK || runID == "" {
			t.Fatalf("JMAP refresh %s: %d %s", tc.path, response.Code, response.Body.String())
		}
		timer := time.NewTimer(10 * time.Second)
		complete := false
		for !complete {
			select {
			case event := <-events:
				if event.Type == mail.EventManualSyncComplete && event.Payload["run_id"] == runID {
					if event.UserID != "alice" || event.Payload["failures"] != 0 || event.Payload["skipped"] != 0 || event.Payload["accounts_done"] != tc.total || event.Payload["accounts_total"] != tc.total {
						t.Fatalf("JMAP refresh result: %+v", event)
					}
					complete = true
				}
			case <-timer.C:
				t.Fatal("JMAP refresh did not complete")
			}
		}
		timer.Stop()
		waitUserIMAP(t, func() bool {
			snapshot, err := f.imap.ManualSyncSnapshot(t.Context(), "alice")
			return err == nil && len(snapshot) == 0
		})
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		state, err := db.GetLabelSyncState(t.Context(), account.ID, storage.LabelProviderJMAP, "email")
		if err == nil && state.Cursor != "s1" {
			t.Fatalf("refresh did not save the JMAP cursor: %+v", state)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []string{"false", "true"} {
		if response := f.request("alice", http.MethodPost, "/api/accounts/"+account.ID+"/service", "service=email&enabled="+enabled); response.Code != http.StatusOK {
			t.Fatalf("JMAP email toggle %s: %d %s", enabled, response.Code, response.Body.String())
		}
		if enabled == "false" {
			if response := f.request("alice", http.MethodPost, "/api/mail/sync/accounts/"+account.ID, ""); response.Code != http.StatusNotFound {
				t.Fatalf("disabled JMAP email accepted manual refresh: %d", response.Code)
			}
		}
	}
	form := url.Values{"sync_interval_minutes": {"2"}, "account_ids": {account.ID}}
	if response := f.request("alice", http.MethodPost, "/api/settings/sync", form.Encode()); response.Code != http.StatusOK {
		t.Fatalf("JMAP sync settings: %d %s", response.Code, response.Body.String())
	}
	if response := f.request("bob", http.MethodPost, "/api/settings/sync", form.Encode()); response.Code != http.StatusNotFound {
		t.Fatalf("foreign JMAP sync settings accepted: %d", response.Code)
	}
	form.Set("idle_folders", account.ID+":"+storage.FolderIDForIdentity(account.ID, "jmap", "mb-in"))
	if response := f.request("alice", http.MethodPost, "/api/settings/sync", form.Encode()); response.Code != http.StatusNotFound {
		t.Fatalf("JMAP accepted per-folder IMAP IDLE: %d", response.Code)
	}
	for _, path := range []string{"/api/mail/sync/accounts/" + account.ID, "/api/accounts/" + account.ID + "/service"} {
		if response := f.request("bob", http.MethodPost, path, "service=email&enabled=true"); response.Code != http.StatusNotFound {
			t.Fatalf("foreign JMAP control %s: %d", path, response.Code)
		}
	}
}
