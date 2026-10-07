package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newOwnedAdminBackfillFixture(t *testing.T) *ownedAdminDiagnosticFixture {
	t.Helper()
	f := newOwnedAdminDiagnosticFixture(t)
	f.h.userBackfillQueue = make(chan userContactBackfillJob, 32)
	f.h.userBackfills = make(map[string]struct{})
	if err := f.h.startUserContactBackfills(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.h.WaitUserContactBackfills)
	return f
}

func TestOwnedAdminManualContactBackfillHTTP(t *testing.T) {
	f := newOwnedAdminBackfillFixture(t)
	path := "/admin/contacts/backfill"

	request := func() *httptest.ResponseRecorder {
		body := ""
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", "application/json")
		r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		return w
	}

	if w := request(); w.Code != http.StatusOK {
		t.Fatal("manual admission", w.Code, w.Body.String())
	}
	waitUserContactWorkers(t, func() bool { return !f.h.getContactBackfillState().InProgress })
	state := f.h.getContactBackfillState()
	if state.LastError != "" || state.Total != 2 || state.Processed != 2 {
		t.Fatal(state)
	}
	// The disabled owner's retained data is maintained under administrator
	// authority; the ordinary private route must still reject it.
	for _, owner := range []string{"alice", "bob"} {
		d, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, owner, storage.UserDiagnosticsContacts)
		if err != nil || d.Contacts.Observed != 1 {
			t.Fatal(owner, d.Contacts, err)
		}
	}
	var n int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM contact_observations`).Scan(&n); err != nil || n != 0 {
		t.Fatal("central fallback", n, err)
	}
	response := f.request("/api/admin/contacts/status")
	var data models.ContactAdminStatus
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Backfill.Processed != 2 || data.Observed != 2 {
		t.Fatal(response.Code, response.Body.String())
	}
	f.h.WaitUserContactBackfills()
	if w := request(); w.Code != http.StatusConflict {
		t.Fatal("admitted after close", w.Code, w.Body.String())
	}
}

func TestOwnedAutomaticContactBackfillAndRuntimeDrain(t *testing.T) {
	f := newOwnedAdminBackfillFixture(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "alice"})
	f.h.ensureUserContactsBackfilled(ctx)
	waitUserContactWorkers(t, func() bool {
		done := ""
		if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
			var err error
			done, err = db.GetSetting(t.Context(), "alice", "contacts_observed_backfilled_v1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return done == "senders,recipients"
	})
	f.cancel()
	f.h.userIMAP.Wait()
	select {
	case <-f.h.userBackfillDone:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not join backfill")
	}
	f.h.ensureUserContactsBackfilled(ctx)
	f.h.contactBackfillMu.RLock()
	pending := len(f.h.userBackfills)
	f.h.contactBackfillMu.RUnlock()
	if pending != 0 {
		t.Fatal("closed dispatcher admitted work", pending)
	}
}

func TestOwnedManualBackfillRejectsOldAdministrator(t *testing.T) {
	f := newOwnedAdminBackfillFixture(t)
	scope, err := f.admin.adminWebmailScope(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	stale := auth.ContextWithUser(context.Background(), &auth.User{ID: "administrator", AuthVersion: 0})
	if f.admin.startOwnedContactBackfill(stale, scope) {
		t.Fatal("stale actor admitted")
	}
	if f.h.getContactBackfillState().InProgress {
		t.Fatal("denied request created job")
	}
}

func TestOwnedContactBackfillShutdownCancelsCacheWait(t *testing.T) {
	f := newOwnedAdminBackfillFixture(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.h.withUserDB(t.Context(), "alice", func(*storage.DB) error { close(entered); <-release; return nil })
	}()
	<-entered
	unlocked := false
	defer func() {
		if !unlocked {
			close(release)
		}
		<-done
	}()
	scope, err := f.admin.adminWebmailScope(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "administrator", AuthVersion: 1})
	if !f.admin.startOwnedContactBackfill(ctx, scope) {
		t.Fatal("job not admitted")
	}
	joined := make(chan struct{})
	go func() { f.h.WaitUserContactBackfills(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for unrelated cached store")
	}
	state := f.h.getContactBackfillState()
	if state.InProgress || !strings.Contains(state.LastError, context.Canceled.Error()) {
		t.Fatal("canceled status", state)
	}
	close(release)
	unlocked = true
	// Retained data was not modified by the canceled administrative job.
	d, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsContacts)
	if err != nil || d.Contacts.Observed != 0 {
		t.Fatal(d.Contacts, err)
	}
	if err := f.h.startUserContactBackfills(t.Context()); err == nil {
		t.Fatal("duplicate dispatcher allowed")
	}
	if _, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "alice", AuthVersion: 1}, "bob", storage.UserDiagnosticsContacts); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal(err)
	}
}
