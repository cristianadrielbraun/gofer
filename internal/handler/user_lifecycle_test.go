package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedStatusPost(t *testing.T, f *ownedAdminDiagnosticFixture, owner, status string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	path := adminUserStatusPath(owner)
	form := url.Values{"status": {status}}
	if csrf {
		form.Set(auth.CSRFFormFieldName, csrfProofForSession(t, f.manager, f.session.Token, path))
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, r)
	return w
}

func verifiedOwnedStatusFixture(t *testing.T) *ownedAdminDiagnosticFixture {
	t.Helper()
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE sessions SET step_up_method='totp',step_up_at=? WHERE id=?`, time.Now().UTC(), f.session.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOwnedUserStatusHTTPStopsWorkAndResumesSchedules(t *testing.T) {
	f := verifiedOwnedStatusFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'; INSERT INTO password_credentials(user_id,password_hash) VALUES('alice','retained-login-secret')`); err != nil {
		t.Fatal(err)
	}
	aliceSession, err := f.manager.CreateAuthenticatedSession(t.Context(), "alice", "owner browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	for _, owner := range []string{"alice", "bob"} {
		id := f.accounts[owner].ID
		for _, query := range []string{
			`INSERT OR REPLACE INTO gofer_account_poll_schedule(account_id,next_due_ms,revision) VALUES(?,?,2)`,
			`INSERT OR REPLACE INTO gofer_account_active_poll(account_id,next_due_ms,revision) VALUES(?,?,2)`,
		} {
			if _, err := f.system.Write().Exec(query, id, future); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.system.Write().Exec(`INSERT OR REPLACE INTO gofer_account_service_schedule(account_id,service,next_due_ms,revision) VALUES(?,'contacts',?,2),(?,'calendar',?,2)`, id, future, id, future); err != nil {
			t.Fatal(err)
		}
		if _, err := f.system.Write().Exec(`INSERT OR REPLACE INTO gofer_contact_queue_schedule(user_id,next_due_ms,revision) VALUES(?,?,2)`, owner, future); err != nil {
			t.Fatal(err)
		}
	}
	aliceStarted, bobStarted := make(chan struct{}), make(chan struct{})
	aliceDone, bobDone := make(chan error, 1), make(chan error, 1)
	work := func(owner string, started chan struct{}, done chan error) {
		done <- f.h.userIMAP.RunUserServiceWork(t.Context(), owner, func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() })
	}
	go work("alice", aliceStarted, aliceDone)
	go work("bob", bobStarted, bobDone)
	for _, started := range []chan struct{}{aliceStarted, bobStarted} {
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("owner work did not start")
		}
	}
	t.Cleanup(func() { f.cancel(); f.h.userIMAP.Wait() })
	if w := ownedStatusPost(t, f, "alice", "disabled", false); w.Code != http.StatusForbidden {
		t.Fatal("status without CSRF", w.Code, w.Body.String())
	}
	select {
	case err := <-aliceDone:
		t.Fatal("rejected status request stopped work", err)
	default:
	}
	if w := ownedStatusPost(t, f, "alice", "disabled", true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case err := <-aliceDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("disable did not cancel whole-user service")
	}
	select {
	case err := <-bobDone:
		t.Fatal("other owner canceled", err)
	default:
	}
	if session, err := f.manager.GetSessionByToken(t.Context(), aliceSession.Token); err != nil || session != nil {
		t.Fatal("disabled session retained", session, err)
	}
	data, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "alice", storage.UserDiagnosticsContacts)
	if err != nil || data.Contacts.Total != 1 {
		t.Fatal("disable erased private data", data, err)
	}
	var state string
	if err := f.system.Read().QueryRow(`SELECT state FROM gofer_user_store_directory WHERE user_id='alice'`).Scan(&state); err != nil || state != "present" {
		t.Fatal("disable changed store lifecycle", state, err)
	}
	if w := ownedStatusPost(t, f, "alice", "active", true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, query := range []string{
		`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`,
		`SELECT next_due_ms FROM gofer_account_active_poll WHERE account_id=?`,
		`SELECT next_due_ms FROM gofer_account_service_schedule WHERE account_id=? AND service='contacts'`,
		`SELECT next_due_ms FROM gofer_account_service_schedule WHERE account_id=? AND service='calendar'`,
	} {
		var next int64
		if err := f.system.Read().QueryRow(query, f.accounts["alice"].ID).Scan(&next); err != nil || next != 0 {
			t.Fatal("enabled service did not become due", query, next, err)
		}
		if err := f.system.Read().QueryRow(query, f.accounts["bob"].ID).Scan(&next); err != nil || next != future {
			t.Fatal("foreign service schedule changed", query, next, err)
		}
	}
	var next int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_contact_queue_schedule WHERE user_id='alice'`).Scan(&next); err != nil || next != 0 {
		t.Fatal(next, err)
	}
	if err := f.h.userIMAP.RunUserServiceWork(t.Context(), "alice", func(context.Context) error { return nil }); err != nil {
		t.Fatal("enabled new work rejected", err)
	}
	if session, err := f.manager.GetSessionByToken(t.Context(), aliceSession.Token); err != nil || session != nil {
		t.Fatal("enable revived old session", session, err)
	}
	var password string
	if err := f.system.Read().QueryRow(`SELECT password_hash FROM password_credentials WHERE user_id='alice'`).Scan(&password); err != nil || password != "retained-login-secret" {
		t.Fatal("credential changed", password, err)
	}
	f.cancel()
	f.h.userIMAP.Wait()
	select {
	case err := <-bobDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runtime did not join remaining user")
	}
}

func TestOwnedUserStatusRetriesFailedPostcommitWake(t *testing.T) {
	f := verifiedOwnedStatusFixture(t)
	id := f.accounts["bob"].ID
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_status_wake BEFORE INSERT ON gofer_account_poll_schedule WHEN NEW.account_id='` + id + `' BEGIN SELECT RAISE(ABORT,'wake unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if w := ownedStatusPost(t, f, "bob", "active", true); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "User access changed") {
		t.Fatal(w.Code, w.Body.String())
	}
	user, err := f.manager.GetUserByID(t.Context(), "bob")
	if err != nil || user.Status != auth.UserStatusActive {
		t.Fatal("postcommit error rolled back access", user, err)
	}
	if _, err := f.system.Write().Exec(`DROP TRIGGER reject_status_wake`); err != nil {
		t.Fatal(err)
	}
	if w := ownedStatusPost(t, f, "bob", "active", true); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type='user_enabled' AND subject_user_id='bob'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("retry duplicated auth transition", count, err)
	}
	var due int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`, id).Scan(&due); err != nil || due != 0 {
		t.Fatal("unchanged status retry did not repair wake", due, err)
	}
}
