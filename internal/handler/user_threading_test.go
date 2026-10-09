package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func awaitOwnedThreading(t *testing.T, h *Handler) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return h.AwaitUserThreading(ctx)
}

func TestOwnedStartupThreadingStatusHTTPAndCacheEviction(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	f.h.syncer = f.admin.syncer
	f.system.SetThreadingState(storage.ThreadingState{InProgress: true, Processed: 99, Total: 99})
	events := f.admin.syncer.Events().Subscribe()
	defer f.admin.syncer.Events().Unsubscribe(events)
	if err := f.admin.AwaitUserThreading(t.Context()); err == nil {
		t.Fatal("wait accepted absent worker")
	}
	before := f.request("/api/system/processing")
	var state UserThreadingStatus
	if before.Code != 200 || json.Unmarshal(before.Body.Bytes(), &state) != nil || state != (UserThreadingStatus{}) {
		t.Fatal("central status leaked", before.Code, before.Body.String())
	}
	if err := f.admin.StartUserThreading(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.admin.WaitUserThreading)
	if err := awaitOwnedThreading(t, f.admin); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.StartUserThreading(t.Context()); err == nil {
		t.Fatal("duplicate startup worker admitted")
	}
	for _, owner := range []string{"alice", "bob", "alice"} {
		if _, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, owner, storage.UserDiagnosticsMail); err != nil {
			t.Fatal(err)
		}
	}
	response := f.request("/api/system/processing")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.InProgress || state.Processed != 2 || state.Total != 2 || state.LastError != "" || state.FailedUsers != 0 {
		t.Fatal(response.Code, response.Body.String())
	}
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
		var normalized string
		if err := db.Read().QueryRow(`SELECT message_id_normalized FROM messages WHERE id=1`).Scan(&normalized); err != nil {
			return err
		}
		if normalized != "same@mail.test" || db.GetThreadingState() != (storage.ThreadingState{}) {
			t.Error("repair / cache-local state", normalized, db.GetThreadingState())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finalEvent := false
drain:
	for {
		select {
		case event := <-events:
			if event.Type == mail.EventProcessingStatus {
				if !event.AdminOnly || event.UserID != "" || len(event.UserIDs) != 0 {
					t.Fatal("global progress published as private mail", event)
				}
				if event.Payload["processed"] == 2 && event.Payload["in_progress"] == false {
					finalEvent = true
				}
			}
		default:
			break drain
		}
	}
	if !finalEvent {
		t.Fatal("missing completed runtime progress event")
	}
	// The legacy branch retains the shared processing contract.
	f.admin.ownedMailbox = nil
	legacy := f.request("/api/system/processing")
	var shared storage.ThreadingState
	if legacy.Code != 200 || json.Unmarshal(legacy.Body.Bytes(), &shared) != nil || shared != f.system.GetThreadingState() {
		t.Fatal("legacy status", legacy.Code, legacy.Body.String())
	}
	f.admin.ownedMailbox = f.h
}

func TestOwnedStartupThreadingRuntimeCancellationJoinsWriterWait(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
			tx, err := db.Write().BeginTx(t.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			close(entered)
			<-release
			return tx.Rollback()
		})
	}()
	<-entered
	released := false
	defer func() {
		if !released {
			close(release)
		}
		if err := <-held; err != nil {
			t.Error(err)
		}
	}()
	if err := f.admin.StartUserThreading(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.admin.WaitUserThreading)
	waitUserContactWorkers(t, func() bool { return f.h.getUserThreadingStatus().Total == 1 })
	response := f.request("/api/system/processing")
	var state UserThreadingStatus
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil || !state.InProgress || state.Processed != 0 || state.Total != 1 {
		t.Fatal("read waited for local writer", response.Code, response.Body.String())
	}
	f.cancel()
	joined := make(chan struct{})
	go func() { f.h.userIMAP.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		t.Fatal("runtime shutdown waited for another caller's writer")
	}
	if err := awaitOwnedThreading(t, f.admin); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	state = f.h.getUserThreadingStatus()
	if state.InProgress || state.Processed != 0 || state.Total != 1 || state.LastError == "" {
		t.Fatal("canceled status", state)
	}
	close(release)
	released = true
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
		var normalized string
		err := db.Read().QueryRow(`SELECT message_id_normalized FROM messages WHERE id=1`).Scan(&normalized)
		if normalized != "" {
			t.Error("canceled repair wrote message", normalized)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedStartupThreadingFailureDoesNotReplaceStoreOrHideOtherUsers(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	var path string
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsContacts); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.StartUserThreading(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.admin.WaitUserThreading)
	// One user's failure is reported, but does not stop startup for everyone.
	if err := awaitOwnedThreading(t, f.admin); err != nil {
		t.Fatal("one user's failure blocked startup", err)
	}
	state := f.h.getUserThreadingStatus()
	if state.InProgress || state.Processed != 1 || state.Total != 1 || state.FailedUsers != 1 || !strings.Contains(state.LastError, "repair threading for user alice") {
		t.Fatal("failure hid healthy owner", state)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing owner store recreated", err)
	}
	response := f.request("/api/system/processing")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"failed_users":1`) {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestOwnedProcessingStatusRejectsStaleAuthorityAndCanceledRoot(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	for _, actor := range []*auth.User{{ID: "administrator", AuthVersion: 0}, {ID: "alice", AuthVersion: 1}} {
		r := httptest.NewRequest(http.MethodGet, "/api/system/processing", nil)
		r = r.WithContext(auth.ContextWithUser(r.Context(), actor))
		w := httptest.NewRecorder()
		f.admin.handleProcessingStatus(w, r)
		if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "in_progress") {
			t.Fatal("stale/private authority", w.Code, w.Body.String())
		}
	}
	root, cancel := context.WithCancel(t.Context())
	f.h.userStorageContext = root
	cancel()
	if response := f.request("/api/system/processing"); response.Code != http.StatusServiceUnavailable {
		t.Fatal("canceled runtime exposed processing state", response.Code, response.Body.String())
	}
}

func TestOwnedStartupThreadingStandaloneLifecycle(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	// The no-IMAP registration is also used by narrow router fixtures.
	f.h.userIMAP = nil
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.admin.StartUserThreading(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.admin.StartUserThreading(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := awaitOwnedThreading(t, f.admin); err != nil {
		t.Fatal(err)
	}
	f.admin.WaitUserThreading()
	if state := f.h.getUserThreadingStatus(); state.InProgress || state.Processed != 2 {
		t.Fatal(state)
	}
}
