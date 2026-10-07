package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedCalendarCreateResult() storage.CalendarEvent {
	return storage.CalendarEvent{UserID: "forged-owner", SourceID: "forged-source", ID: "forged-id", RemoteID: "created-resource", ICalUID: "created-uid", ETag: "confirmed", Summary: "Created", StartAt: calendarMutationTime(10), EndAt: calendarMutationTime(11), AttendeesJSON: `[{"email":"guest@example.com"}]`, OnlineMeetingJSON: `{"link":"confirmed"}`}
}

func TestUserCalendarCreateDurableOwnershipEvictionAndReplay(t *testing.T) {
	system, _, accounts, _ := newCalendarControlFixture(t)
	claims := map[string]*UserCalendarCreateClaim{}
	for _, owner := range []string{"alice", "bob"} {
		source, err := accounts.SnapshotCalendarSource(t.Context(), owner, "same-source")
		if err != nil {
			t.Fatal(err)
		}
		claim, err := accounts.BeginCalendarCreate(t.Context(), source, "same-request", "original-draft")
		if err != nil || claim.Result() != (storage.CalendarCreateRequest{}) {
			t.Fatal("reserve", err)
		}
		claims[owner] = claim
	}
	original := ownedCalendarCreateResult()
	result, err := accounts.PublishCalendarCreate(t.Context(), claims["alice"], original, false)
	if err != nil || result.EventID == "" || result.EventID == "forged-id" {
		t.Fatal("publication", result, err)
	}
	if err := accounts.ValidateCalendarCreate(t.Context(), claims["alice"]); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("consumed reservation remained current", err)
	}
	for _, owner := range []string{"bob", "alice"} {
		source, err := accounts.SnapshotCalendarSource(t.Context(), owner, "same-source")
		if err != nil {
			t.Fatal(err)
		}
		replay, err := accounts.BeginCalendarCreate(t.Context(), source, "same-request", "original-draft")
		if err != nil {
			t.Fatal(err)
		}
		if owner == "alice" && replay.Result() != result {
			t.Fatal("durable result lost across eviction", replay.Result())
		}
		if owner == "bob" && replay.Result() != (storage.CalendarCreateRequest{}) {
			t.Fatal("request crossed owners", replay.Result())
		}
		if _, err := accounts.BeginCalendarCreate(t.Context(), source, "same-request", "different-draft"); !errors.Is(err, storage.ErrCalendarCreateConflict) {
			t.Fatal("changed draft accepted", err)
		}
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		saved, err := db.GetCalendarEvent(t.Context(), "alice", result.EventID)
		if err != nil {
			return err
		}
		if saved.SourceID != "same-source" || saved.UserID != "alice" || saved.Summary != original.Summary || saved.AttendeesJSON != original.AttendeesJSON || saved.OnlineMeetingJSON != original.OnlineMeetingJSON {
			t.Fatal("owned confirmed content", saved)
		}
		var state string
		var success sql.NullString
		err = db.Read().QueryRow(`SELECT state,last_success_at FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &success)
		if err == nil && (state != "pending" || success.Valid) {
			t.Fatal("create fabricated full sync")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"calendar_create_requests", "calendar_events"} {
		var n int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("central fallback", table, n, err)
		}
	}
}

func TestUserCalendarCreatePrincipalRepairAndLegacyResults(t *testing.T) {
	for _, mode := range []string{"repair", "principal", "collection", "legacy-pending", "legacy-completed"} {
		t.Run(mode, func(t *testing.T) {
			_, _, accounts, _ := newCalendarControlFixture(t)
			source, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
			if err != nil {
				t.Fatal(err)
			}
			claim, err := accounts.BeginCalendarCreate(t.Context(), source, "request", "draft")
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var err error
				switch mode {
				case "repair":
					_, err = db.Write().Exec(`UPDATE accounts SET smtp_host='repaired-host'`)
				case "principal":
					_, err = db.Write().Exec(`UPDATE account_caldav_configs SET username='new-principal'`)
				case "collection":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET remote_id='/different/' WHERE id='same-source'`)
				case "legacy-pending":
					_, err = db.Write().Exec(`UPDATE calendar_create_requests SET request_hash='draft'`)
				case "legacy-completed":
					_, err = db.Write().Exec(`UPDATE calendar_create_requests SET request_hash='draft',remote_id='already-created',event_id='cached-event'`)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.ValidateCalendarCreate(t.Context(), claim); err == nil {
				t.Fatal("old service/reservation accepted")
			}
			fresh, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := accounts.BeginCalendarCreate(t.Context(), fresh, "request", "draft")
			allowed := mode == "repair" || mode == "legacy-completed"
			if (err == nil) != allowed {
				t.Fatal("retry changed original identity", mode, err)
			}
			if mode == "legacy-completed" && resumed.Result().RemoteID != "already-created" {
				t.Fatal("legacy result lost")
			}
		})
	}
}

func TestUserCalendarCreateSeriesAndExistingCachePreserved(t *testing.T) {
	for _, mode := range []string{"series", "existing-cache"} {
		t.Run(mode, func(t *testing.T) {
			_, _, accounts, _ := newCalendarControlFixture(t)
			if mode == "existing-cache" {
				seedCalendarSyncEvents(t, accounts)
			}
			source, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
			if err != nil {
				t.Fatal(err)
			}
			claim, err := accounts.BeginCalendarCreate(t.Context(), source, "request", "draft")
			if err != nil {
				t.Fatal(err)
			}
			event := ownedCalendarCreateResult()
			if mode == "series" {
				event.RecurrenceJSON = `["RRULE:FREQ=DAILY"]`
			} else {
				event.RemoteID = "kept"
			}
			result, err := accounts.PublishCalendarCreate(t.Context(), claim, event, mode == "series")
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if mode == "series" {
					if result.EventID != "" || result.RemoteID != event.RemoteID {
						t.Fatal("series not durable", result)
					}
					err := db.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&n)
					if err == nil && n != 0 {
						t.Fatal("cached a master appointment")
					}
					return err
				}
				existing, err := db.GetCalendarEvent(t.Context(), "alice", result.EventID)
				if err == nil && (existing.ID != "same-event" || existing.Summary != "alice old" || existing.ETag != "old-version") {
					t.Fatal("overwrote independently synced cache", existing)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarCreateRequiresBoundWritableAuthority(t *testing.T) {
	_, routing, accounts, _ := newCalendarControlFixture(t)
	source, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.BeginCalendarCreate(t.Context(), source, "request", "draft"); !errors.Is(err, storage.ErrCalendarSourceChanged) {
		t.Fatal("foreign repository", err)
	}
	for _, bad := range []*UserCalendarCreateClaim{nil, {}, {repository: accounts, source: source}} {
		if err := accounts.ValidateCalendarCreate(t.Context(), bad); !errors.Is(err, storage.ErrCalendarCreateConflict) {
			t.Fatal("unbound claim", err)
		}
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider='gmail',auth_method='oauth2',provider_account_id='subject'; UPDATE calendar_sources SET provider='gmail',access_role='owner'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source, err = accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.BeginCalendarCreate(t.Context(), source, "request", "draft"); !errors.Is(err, storage.ErrCalendarSourceChanged) {
		t.Fatal("OAuth create without write guard", err)
	}
	changed := errors.New("grant revision replaced")
	if _, err := accounts.BeginCalendarCreate(t.Context(), source, "request", "draft", func() error { return changed }); !errors.Is(err, changed) {
		t.Fatal("grant guard ignored", err)
	}
	if _, err := accounts.BeginCalendarCreate(t.Context(), source, "request", "draft", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarCreateWriterWaitAndAtomicRollback(t *testing.T) {
	for _, operation := range []string{"begin", "publish"} {
		modes := []string{"source", "settings", "owner", "account", "grant", "trigger-source", "trigger-request", "trigger-ignore", "input-copy"}
		if operation == "publish" {
			modes = append(modes, "replacement-nonce", "trigger-event")
		}
		for _, mode := range modes {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				system, _, accounts, owners := newCalendarControlFixture(t)
				source, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
				if err != nil {
					t.Fatal(err)
				}
				var claim *UserCalendarCreateClaim
				if operation == "publish" {
					claim, err = accounts.BeginCalendarCreate(t.Context(), source, "request", "draft")
					if err != nil {
						t.Fatal(err)
					}
				}
				event := ownedCalendarCreateResult()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
					tx, err := db.Write().BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					before := db.Write().Stats().WaitCount
					done := make(chan error, 1)
					var grantChanged atomic.Bool
					guard := func() error {
						if grantChanged.Load() {
							return errors.New("grant replaced")
						}
						return nil
					}
					go func() {
						var err error
						if operation == "begin" {
							_, err = accounts.BeginCalendarCreate(ctx, source, "request", "draft", guard)
						} else {
							_, err = accounts.PublishCalendarCreate(ctx, claim, event, false, guard)
						}
						done <- err
					}()
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for db.Write().Stats().WaitCount == before {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-ticker.C:
						}
					}
					switch mode {
					case "source":
						_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='/replaced/' WHERE id='same-source'`)
					case "settings":
						_, err = tx.Exec(`UPDATE account_caldav_configs SET username='replaced'`)
					case "owner":
						_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					case "account":
						_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
					case "grant":
						grantChanged.Store(true)
					case "replacement-nonce":
						var saved map[string]any
						var savedHash string
						if err = tx.QueryRow(`SELECT request_hash FROM calendar_create_requests`).Scan(&savedHash); err != nil {
							return err
						}
						if err = json.Unmarshal([]byte(savedHash), &saved); err != nil {
							return err
						}
						saved["Nonce"] = "replacement"
						wire, _ := json.Marshal(saved)
						_, err = tx.Exec(`UPDATE calendar_create_requests SET request_hash=?`, string(wire))
					case "trigger-source", "trigger-request", "trigger-ignore":
						mutation := "UPDATE calendar_sources SET remote_id='/replaced/' WHERE id='same-source';"
						if mode == "trigger-request" {
							mutation = "UPDATE calendar_create_requests SET remote_id='forged';"
						}
						timing := "AFTER"
						if mode == "trigger-ignore" {
							timing = "BEFORE"
							mutation = "SELECT RAISE(IGNORE);"
						}
						action := "INSERT"
						if operation == "publish" {
							action = "UPDATE"
						}
						_, err = tx.Exec("CREATE TRIGGER corrupt_create " + timing + " " + action + " ON calendar_create_requests BEGIN " + mutation + " END")
					case "trigger-event":
						_, err = tx.Exec(`CREATE TRIGGER corrupt_event AFTER INSERT ON calendar_events BEGIN UPDATE calendar_events SET summary='corrupt' WHERE id=NEW.id; END`)
					case "input-copy":
						*event.StartAt = event.StartAt.Add(time.Hour)
						sourceCopy := source.Source()
						sourceCopy.RemoteID = "/forged/"
					}
					if err != nil {
						return err
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					select {
					case err := <-done:
						if mode == "input-copy" {
							if err != nil {
								t.Fatal("provider input not copied before writer wait", err)
							}
						} else if err == nil {
							t.Fatal("stale/partial create accepted", operation, mode)
						}
					case <-ctx.Done():
						return ctx.Err()
					}
					if strings.HasPrefix(mode, "trigger-") {
						var n int
						if err := db.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&n); err != nil {
							return err
						}
						if n != 0 {
							t.Fatal("partial event escaped rollback")
						}
						var remote string
						err := db.Read().QueryRow(`SELECT remote_id FROM calendar_create_requests`).Scan(&remote)
						if operation == "begin" {
							if !errors.Is(err, sql.ErrNoRows) {
								t.Fatal("partial reservation escaped", err)
							}
						} else if err != nil || remote != "" {
							t.Fatal("partial request result escaped", remote, err)
						}
						var collection string
						if err := db.Read().QueryRow(`SELECT remote_id FROM calendar_sources WHERE id='same-source'`).Scan(&collection); err != nil {
							return err
						}
						if collection != "/primary/" {
							t.Fatal("source trigger escaped rollback")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
