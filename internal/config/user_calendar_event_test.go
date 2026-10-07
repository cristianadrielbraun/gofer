package config

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarSourceSnapshotDerivesAccountPreservesDisplayAndEviction(t *testing.T) {
	system, routing, accounts, owners := newCalendarControlFixture(t)
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET is_hidden=1 WHERE id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
	if err != nil {
		t.Fatal(err)
	}
	source := snapshot.Source()
	if !source.IsHidden || source.Name != "alice primary" || source.AccountID != owners["alice"].ID || snapshot.Service().OwnerID() != "alice" {
		t.Fatal("source/account authority or copied display preference lost", source)
	}
	source.AccountID, source.RemoteID = owners["bob"].ID, "/forged/"
	if snapshot.Source().AccountID != owners["alice"].ID || snapshot.Source().RemoteID != "/primary/" {
		t.Fatal("source copy modified authority")
	}
	bob, err := accounts.SnapshotCalendarSource(t.Context(), "bob", "same-source")
	if err != nil || bob.Source().Name != "bob primary" {
		t.Fatal("colliding source ID crossed owner", err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET is_hidden=0 WHERE id='same-source'; UPDATE calendar_sync_state SET state='syncing',attempt_count=9 WHERE source_id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarSource(t.Context(), snapshot); err != nil {
		t.Fatal("eviction, visibility or progress invalidated source", err)
	}
	for _, id := range []string{"unknown", "second-source"} {
		if _, err := accounts.SnapshotCalendarSource(t.Context(), "alice", id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("missing/unselected source accepted", id, err)
		}
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*UserCalendarSourceSnapshot{nil, {}, snapshot} {
		if err := other.ValidateCalendarSource(t.Context(), invalid); !errors.Is(err, storage.ErrCalendarSourceChanged) {
			t.Fatal("unbound source accepted", err)
		}
	}
	for _, guards := range [][]func() error{{nil}, {func() error { return nil }, func() error { return nil }}} {
		if err := accounts.ValidateCalendarSource(t.Context(), snapshot, guards...); !errors.Is(err, storage.ErrCalendarSourceChanged) {
			t.Fatal("invalid source guard accepted", err)
		}
	}
	changed := errors.New("write grant replaced")
	if err := accounts.ValidateCalendarSource(t.Context(), snapshot, func() error { return changed }); !errors.Is(err, changed) {
		t.Fatal("exact central grant guard lost", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := accounts.ValidateCalendarSource(ctx, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled source validation", err)
	}
	var n int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_sources`).Scan(&n); err != nil || n != 0 {
		t.Fatal("source snapshot used central content", n, err)
	}
}

func TestUserCalendarSourceSnapshotRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"deleted", "deselected", "remote", "access-role", "local-account", "dav-config", "central-account", "owner"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newCalendarControlFixture(t)
			snapshot, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var err error
				switch change {
				case "deleted":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET is_deleted=1 WHERE id='same-source'`)
				case "deselected":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='same-source'`)
				case "remote":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET remote_id='/other/' WHERE id='same-source'`)
				case "access-role":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET access_role='reader' WHERE id='same-source'`)
				case "local-account":
					_, err = db.Write().Exec(`UPDATE accounts SET email_address='replaced@example.com' WHERE id=?`, owners["alice"].ID)
				case "dav-config":
					_, err = db.Write().Exec(`UPDATE account_caldav_configs SET base_url='https://replaced.test/' WHERE account_id=?`, owners["alice"].ID)
				case "central-account":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
				case "owner":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want := storage.ErrCalendarSourceChanged
			switch change {
			case "local-account", "dav-config":
				want = ErrAccountServicesChanged
			case "central-account":
				want = storage.ErrAccountRoute
			case "owner":
				want = storage.ErrUserStoreOwner
			}
			if err := accounts.ValidateCalendarSource(t.Context(), snapshot); !errors.Is(err, want) {
				t.Fatal("changed source authority accepted/misclassified", change, err, want)
			}
		})
	}
}

func TestUserCalendarEventSnapshotOwnershipCopiesEvictionAndProgress(t *testing.T) {
	system, routing, accounts, owners := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Event().Summary != "alice old" || snapshot.Source().AccountID != owners["alice"].ID || snapshot.Service().OwnerID() != "alice" {
		t.Fatal("wrong snapshot identity")
	}
	event, source := snapshot.Event(), snapshot.Source()
	original := *event.StartAt
	*event.StartAt = original.Add(time.Hour)
	event.ID, source.AccountID = "forged", owners["bob"].ID
	if !snapshot.Event().StartAt.Equal(original) || snapshot.Event().ID != "same-event" || snapshot.Source().AccountID != owners["alice"].ID {
		t.Fatal("mutable snapshot")
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET is_hidden=1 WHERE id='same-source'; UPDATE calendar_sync_state SET state='syncing',attempt_count=7 WHERE source_id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// MaxOpen=1. Claims keep no local store or lease alive across eviction.
	bob, err := accounts.SnapshotCalendarEvent(t.Context(), "bob", "same-event")
	if err != nil || bob.Event().Summary != "bob old" {
		t.Fatal("colliding event identity", err)
	}
	if err := accounts.ValidateCalendarEvent(t.Context(), snapshot); err != nil {
		t.Fatal("eviction/display/progress invalidated event", err)
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*UserCalendarEventSnapshot{nil, {}, snapshot} {
		if err := other.ValidateCalendarEvent(t.Context(), invalid); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("unbound snapshot accepted", err)
		}
	}
	if _, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "unknown"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unknown event", err)
	}
	for _, guards := range [][]func() error{{nil}, {func() error { return nil }, func() error { return nil }}} {
		if err := accounts.ValidateCalendarEvent(t.Context(), snapshot, guards...); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("invalid guard accepted", err)
		}
	}
	reconnected := errors.New("grant revision replaced")
	if err := accounts.ValidateCalendarEvent(t.Context(), snapshot, func() error { return reconnected }); !errors.Is(err, reconnected) {
		t.Fatal("exact central grant check lost", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := accounts.ValidateCalendarEvent(ctx, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled validation", err)
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count); err != nil || count != 0 {
		t.Fatal("central fallback", count, err)
	}
}

func TestUserCalendarEventSnapshotRejectsLateChanges(t *testing.T) {
	for _, change := range []string{"content-same-version", "time", "tombstone", "deselected", "remote-source", "dav-config", "account-email", "central-account", "owner-disabled"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var err error
				switch change {
				case "content-same-version":
					_, err = db.Write().Exec(`UPDATE calendar_events SET attendees_json='[{"email":"changed@example.com"}]' WHERE id='same-event'`)
				case "time":
					_, err = db.Write().Exec(`UPDATE calendar_events SET end_at=datetime(end_at,'+1 minute') WHERE id='same-event'`)
				case "tombstone":
					_, err = db.Write().Exec(`UPDATE calendar_events SET is_deleted=1 WHERE id='same-event'`)
				case "deselected":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='same-source'`)
				case "remote-source":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET remote_id='/other/' WHERE id='same-source'`)
				case "dav-config":
					_, err = db.Write().Exec(`UPDATE account_caldav_configs SET base_url='https://changed.test/' WHERE account_id=?`, owners["alice"].ID)
				case "account-email":
					_, err = db.Write().Exec(`UPDATE accounts SET email_address='changed@example.com' WHERE id=?`, owners["alice"].ID)
				case "central-account":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
				case "owner-disabled":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			err = accounts.ValidateCalendarEvent(t.Context(), snapshot)
			want := storage.ErrCalendarEventChanged
			switch change {
			case "dav-config", "account-email":
				want = ErrAccountServicesChanged
			case "central-account":
				want = storage.ErrAccountRoute
			case "owner-disabled":
				want = storage.ErrUserStoreOwner
			}
			if !errors.Is(err, want) {
				t.Fatal("late change not classified", change, err, want)
			}
		})
	}
}
