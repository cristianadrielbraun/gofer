package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const ownedMeetingDraftID = "b7931511-7c6a-4ce8-8d1a-5239ba410589"
const ownedMeetConference = `{"conferenceId":"abc-defg-hij","entryPoints":[{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}]}`
const ownedTeamsConference = `{"isOnlineMeeting":true,"joinUrl":"https://teams.microsoft.com/l/meetup-join/test/0"}`

func TestUserCalendarMeetingCleanupDiscoveryIsBoundedOwnedAndReadOnly(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			system, accounts, sources := ownedMeetingFixture(t, provider)
			for _, owner := range []string{"alice", "bob"} {
				extra, err := accounts.CreateAccount(t.Context(), owner, &models.CreateAccountRequest{Provider: "imap", EmailAddress: "other-" + owner + "@example.com", IMAPHost: "imap.test", SMTPHost: "smtp.test", Username: "other-" + owner, Password: "synthetic"})
				if err != nil {
					t.Fatal(err)
				}
				if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
					if _, err := db.Write().Exec(`UPDATE accounts SET provider=?,auth_method='oauth2',provider_account_id='other-subject' WHERE id=?`, provider, extra.ID); err != nil {
						return err
					}
					if err := db.ReplaceCalendarSources(t.Context(), owner, extra.ID, provider, []storage.CalendarSource{{ID: "other-account-source", RemoteID: "other-primary", IsSelected: true, AccessRole: "owner"}}); err != nil {
						return err
					}
					if provider == "gmail" {
						if _, err := db.Write().Exec(`INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id,created_at) VALUES(?,'other-account-source','draft-00','other-private',datetime('now','-3 hours'))`, owner); err != nil {
							return err
						}
					} else {
						if _, err := db.Write().Exec(`INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id,updated_at) VALUES(?,'other-account-source','draft-00',datetime('now','-3 hours'))`, owner); err != nil {
							return err
						}
					}
					// Same scheduling IDs in both stores; all rows are old enough
					// for cleanup, and discovery must leave active state intact.
					var query string
					if provider == "gmail" {
						query = `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<23)
 INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id,created_at)
 SELECT ?,'same-source',printf('draft-%02d',i),printf('private-%02d',i),datetime('now','-2 hours') FROM n`
					} else {
						query = `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<23)
 INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id,updated_at)
 SELECT ?,'same-source',printf('draft-%02d',i),datetime('now','-2 hours') FROM n`
					}
					if _, err := db.Write().Exec(query, owner); err != nil {
						return err
					}
					_, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='same-source'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, owner := range []string{"alice", "bob", "alice"} {
				id := sources[owner].Service().AccountID()
				for _, invalid := range []int{-1, 0, 17, 1000} {
					if _, err := accounts.ListCalendarMeetingCleanup(t.Context(), owner, id, invalid); !errors.Is(err, storage.ErrCalendarCreateConflict) {
						t.Fatal("unbounded discovery accepted", invalid, err)
					}
				}
				rows, err := accounts.ListCalendarMeetingCleanup(t.Context(), owner, id, 8)
				if err != nil || len(rows) != 8 {
					t.Fatal("bounded discovery", len(rows), err)
				}
				for i, row := range rows {
					if row.SourceID != "same-source" || row.DraftID != fmt.Sprintf("draft-%02d", i) || row.Provider != provider || row.Prune {
						t.Fatal("wrong owned work/order", row, i)
					}
				}
				if _, err := accounts.ListCalendarMeetingCleanup(t.Context(), owner, sources[map[string]string{"alice": "bob", "bob": "alice"}[owner]].Service().AccountID(), 8); err == nil {
					t.Fatal("foreign account discovery accepted")
				}
				if _, err := accounts.SnapshotCalendarSource(t.Context(), owner, "same-source"); err == nil {
					t.Fatal("cleanup discovery relaxed selected-source action authority")
				}
				if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
					query := `SELECT COUNT(*) FROM calendar_meet_drafts WHERE source_id='same-source' AND cleanup_pending=1 AND conference_json='' AND used_by=''`
					if provider == "outlook" {
						query = `SELECT COUNT(*) FROM calendar_teams_drafts WHERE source_id='same-source' AND state='active' AND used_by=''`
					}
					var count int
					if err := db.Read().QueryRow(query).Scan(&count); err != nil {
						return err
					}
					if count != 24 {
						return fmt.Errorf("discovery mutated or lost drafts: %d", count)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var central int
			if err := system.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM calendar_meet_drafts)+(SELECT COUNT(*) FROM calendar_teams_drafts)`).Scan(&central); err != nil || central != 0 {
				t.Fatal("discovery copied work centrally", central, err)
			}
		})
	}
}

func TestUserCalendarMeetingCleanupDiscoveryLifecycleEligibility(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			_, accounts, sources := ownedMeetingFixture(t, provider)
			id := sources["alice"].Service().AccountID()
			want := map[string]bool{}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(t.Context(), nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if provider == "gmail" {
					for _, row := range []struct {
						id, age, conference string
						pending             bool
					}{
						{"fresh-unprepared", "-1 minute", "", true},
						{"old-unprepared", "-2 hours", "", true},
						{"fresh-ready", "-1 minute", ownedMeetConference, true},
						{"cleaned-recent", "-1 minute", ownedMeetConference, false},
						{"cleaned-old", "-8 days", ownedMeetConference, false},
					} {
						if _, err := tx.Exec(`INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id,conference_json,cleanup_pending,created_at) VALUES('alice','same-source',?,'private',?,?,datetime('now',?))`, row.id, row.conference, row.pending, row.age); err != nil {
							return err
						}
					}
					want = map[string]bool{"old-unprepared": false, "fresh-ready": false, "cleaned-old": true}
				} else {
					for _, row := range []struct{ id, age, state string }{
						{"fresh-active", "-1 minute", "active"},
						{"old-active", "-2 hours", "active"},
						{"fresh-saving", "-1 minute", "saving"},
						{"old-saving", "-2 hours", "saving"},
						{"abandoned", "-1 minute", "abandoned"},
						{"saved-recent", "-1 minute", "saved"},
						{"saved-old", "-8 days", "saved"},
						{"cleaned-old", "-8 days", "cleaned"},
					} {
						if _, err := tx.Exec(`INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id,state,updated_at) VALUES('alice','same-source',?,?,datetime('now',?))`, row.id, row.state, row.age); err != nil {
							return err
						}
					}
					want = map[string]bool{"old-active": false, "old-saving": false, "abandoned": false, "saved-old": true, "cleaned-old": true}
				}
				return tx.Commit()
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.SetCalendarServiceEnabled(t.Context(), "alice", id, false); err != nil {
				t.Fatal(err)
			}
			rows, err := accounts.ListCalendarMeetingCleanup(t.Context(), "alice", id, 16)
			if err != nil || len(rows) != len(want) {
				t.Fatal("eligibility", rows, err)
			}
			for _, row := range rows {
				prune, exists := want[row.DraftID]
				if !exists || row.Prune != prune || row.Provider != provider || row.SourceID != "same-source" {
					t.Fatal("unexpected lifecycle work", row)
				}
				delete(want, row.DraftID)
			}
			if len(want) != 0 {
				t.Fatal("missing lifecycle work", want)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				calls := 0
				rows, err := db.ListAccountCalendarMeetingCleanup(t.Context(), "alice", id, 16, func(_ *sql.Tx, account string) error {
					calls++
					if account != id || calls == 2 {
						return storage.ErrAccountRoute
					}
					return nil
				})
				if !errors.Is(err, storage.ErrAccountRoute) || rows != nil || calls != 2 {
					return fmt.Errorf("late lifecycle change leaked work: rows=%v calls=%d err=%v", rows, calls, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func ownedMeetingFixture(t *testing.T, provider string) (*storage.DB, *UserAccountStore, map[string]*UserCalendarSourceSnapshot) {
	t.Helper()
	system, _, accounts, _ := newCalendarControlFixture(t)
	scopes := map[string]*UserCalendarSourceSnapshot{}
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET provider=?,auth_method='oauth2',provider_account_id='subject'; UPDATE calendar_sources SET provider=?,access_role='owner'`, provider, provider)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var err error
		scopes[owner], err = accounts.SnapshotCalendarSource(t.Context(), owner, "same-source")
		if err != nil {
			t.Fatal(err)
		}
	}
	return system, accounts, scopes
}
func meetingDraftConference(provider string) string {
	if provider == "gmail" {
		return ownedMeetConference
	}
	return ownedTeamsConference
}
func readyMeetingDraft(t *testing.T, accounts *UserAccountStore, source *UserCalendarSourceSnapshot) *UserCalendarMeetingDraftSnapshot {
	t.Helper()
	guard := func() error { return nil }
	d, err := accounts.BeginCalendarMeetingDraft(t.Context(), source, ownedMeetingDraftID, guard)
	if err != nil {
		t.Fatal(err)
	}
	if source.Source().Provider == "outlook" {
		d, err = accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftRemote, "private-native", guard)
		if err != nil {
			t.Fatal(err)
		}
	}
	d, err = accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftConference, meetingDraftConference(source.Source().Provider), guard)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestUserCalendarMeetingDraftOwnershipRecoveryAndTransitions(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			system, accounts, sources := ownedMeetingFixture(t, provider)
			guard := func() error { return nil }
			drafts := map[string]*UserCalendarMeetingDraftSnapshot{}
			for _, owner := range []string{"alice", "bob"} {
				drafts[owner] = readyMeetingDraft(t, accounts, sources[owner])
			}
			// Same source and draft IDs survive eviction independently at MaxOpen=1.
			for _, owner := range []string{"alice", "bob"} {
				fresh, err := accounts.SnapshotCalendarMeetingDraft(t.Context(), sources[owner], ownedMeetingDraftID, guard)
				if err != nil {
					t.Fatal(err)
				}
				if fresh.Google() != drafts[owner].Google() || fresh.Teams() != drafts[owner].Teams() {
					t.Fatal("draft crossed owners/eviction")
				}
			}
			d := drafts["alice"]
			if provider == "gmail" {
				var err error
				d, err = accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftCleaned, "", guard)
				if err != nil {
					t.Fatal(err)
				}
				replay, err := accounts.BeginCalendarMeetingDraft(t.Context(), sources["alice"], ownedMeetingDraftID, guard)
				if err != nil || replay.Google().ConferenceJSON != ownedMeetConference {
					t.Fatal("ready link lost after cleanup", err)
				}
			}
			claim, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "original", "submitted", guard)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := accounts.BindCalendarMeetingDraft(t.Context(), d, claim, nil, guard)
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.ValidateCalendarMeetingDraft(t.Context(), d, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
				t.Fatal("stale draft retained authority", err)
			}
			other, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "other", "submitted", guard)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.BindCalendarMeetingDraft(t.Context(), bound, other, nil, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
				t.Fatal("draft retargeted", err)
			}
			if provider == "outlook" {
				abandoned, err := accounts.PublishCalendarMeetingDraft(t.Context(), bound, storage.CalendarMeetingDraftAbandon, "", guard)
				if err != nil || abandoned.Teams().State != "saving" {
					t.Fatal("discard destroyed saving event", err)
				}
				saved, err := accounts.PublishCalendarMeetingDraft(t.Context(), abandoned, storage.CalendarMeetingDraftSaved, "", guard)
				if err != nil || saved.Teams().State != "saved" {
					t.Fatal(err)
				}
				if _, err := accounts.PublishCalendarMeetingDraft(t.Context(), saved, storage.CalendarMeetingDraftCleaned, "", guard); err == nil {
					t.Fatal("cleaned saved event")
				}
				tombstoneID := "d7931511-7c6a-4ce8-8d1a-5239ba410589"
				tomb, err := accounts.BeginCalendarMeetingDraft(t.Context(), sources["alice"], tombstoneID, guard)
				if err != nil {
					t.Fatal(err)
				}
				tomb, err = accounts.PublishCalendarMeetingDraft(t.Context(), tomb, storage.CalendarMeetingDraftAbandon, "", guard)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := accounts.PublishCalendarMeetingDraft(t.Context(), tomb, storage.CalendarMeetingDraftRemote, "late-allocation", guard); err == nil {
					t.Fatal("discard reopened preview")
				}
			}
			for _, table := range []string{"calendar_create_requests", "calendar_meet_drafts", "calendar_teams_drafts"} {
				var count int
				if err := system.Read().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("central fallback", table, count, err)
				}
			}
		})
	}
}

func TestUserCalendarMeetingDraftPrincipalLegacyAndCopiedPayload(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, mode := range []string{"principal", "collection", "legacy", "copied", "invalid-conference", "missing-guard", "missing-snapshot"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				_, accounts, sources := ownedMeetingFixture(t, provider)
				source := sources["alice"]
				guard := func() error { return nil }
				d := readyMeetingDraft(t, accounts, source)
				if mode == "invalid-conference" {
					if _, err := accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftConference, `{"joinUrl":"https://example.com/unsafe"}`, guard); err == nil {
						t.Fatal("foreign conference accepted")
					}
					return
				}
				if mode == "missing-guard" {
					if _, err := accounts.BeginCalendarMeetingDraft(t.Context(), source, ownedMeetingDraftID); err == nil {
						t.Fatal("read scope authorized write")
					}
					return
				}
				if mode == "missing-snapshot" {
					id := "d7931511-7c6a-4ce8-8d1a-5239ba410589"
					if _, err := accounts.SnapshotCalendarMeetingDraft(t.Context(), source, id, guard); !errors.Is(err, sql.ErrNoRows) {
						t.Fatal(err)
					}
					return
				}
				if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
					switch mode {
					case "principal":
						_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='new-subject'`)
						return err
					case "collection":
						_, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id='replacement' WHERE id='same-source'`)
						return err
					case "legacy":
						_, err := db.Write().Exec(`DELETE FROM calendar_create_requests`)
						return err
					case "copied":
						g, m := d.Google(), d.Teams()
						g.RemoteID = "foreign"
						m.RemoteID = "foreign"
						return nil
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if mode == "copied" {
					if err := accounts.ValidateCalendarMeetingDraft(t.Context(), d, guard); err != nil {
						t.Fatal("copy mutated private authority", err)
					}
					return
				}
				fresh, err := accounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := accounts.BeginCalendarMeetingDraft(t.Context(), fresh, ownedMeetingDraftID, guard); err == nil {
					t.Fatal("adopted legacy/replaced principal")
				}
			})
		}
	}
}

func TestUserCalendarMeetingDraftWriterWaitAndTriggerRollback(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, operation := range []string{"begin", "publish"} {
			modes := []string{"owner", "account", "settings", "source", "grant", "draft", "nonce", "trigger-row", "trigger-source", "trigger-reservation"}
			if operation == "publish" {
				modes = append(modes, "target", "trigger-target")
			}
			for _, mode := range modes {
				t.Run(provider+"/"+operation+"/"+mode, func(t *testing.T) {
					system, accounts, sources := ownedMeetingFixture(t, provider)
					source := sources["alice"]
					var d *UserCalendarMeetingDraftSnapshot
					var claim *UserCalendarCreateClaim
					if operation == "publish" {
						d = readyMeetingDraft(t, accounts, source)
						var err error
						claim, err = accounts.BeginCalendarCreate(t.Context(), source, "original", "submitted", func() error { return nil })
						if err != nil {
							t.Fatal(err)
						}
					}
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
						tx, err := db.Write().BeginTx(ctx, nil)
						if err != nil {
							return err
						}
						defer tx.Rollback()
						before := db.Write().Stats().WaitCount
						var changed atomic.Bool
						done := make(chan error, 1)
						go func() {
							guard := func() error {
								if changed.Load() {
									return errors.New("grant changed")
								}
								return nil
							}
							var err error
							if operation == "begin" {
								_, err = accounts.BeginCalendarMeetingDraft(ctx, source, ownedMeetingDraftID, guard)
							} else {
								_, err = accounts.BindCalendarMeetingDraft(ctx, d, claim, nil, guard)
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
						table := "calendar_meet_drafts"
						if provider == "outlook" {
							table = "calendar_teams_drafts"
						}
						switch mode {
						case "owner":
							_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
						case "account":
							_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, source.Source().AccountID)
						case "settings":
							_, err = tx.Exec(`UPDATE accounts SET provider_account_id='replaced'`)
						case "target":
							_, err = tx.Exec(`UPDATE calendar_create_requests SET remote_id='consumed' WHERE request_id='original'`)
						case "source":
							_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='replaced' WHERE id='same-source'`)
						case "grant":
							changed.Store(true)
						case "draft":
							if operation == "begin" {
								_, err = tx.Exec("INSERT INTO "+table+"(user_id,source_id,draft_id,remote_id) VALUES('alice','same-source',?,'preexisting')", ownedMeetingDraftID)
							} else {
								_, err = tx.Exec("UPDATE " + table + " SET remote_id='replaced'")
							}
						case "nonce":
							if operation == "begin" {
								_, err = tx.Exec(`CREATE TRIGGER corrupt_nonce AFTER INSERT ON calendar_create_requests BEGIN UPDATE calendar_create_requests SET request_hash='replaced'; END`)
							} else {
								_, err = tx.Exec(`UPDATE calendar_create_requests SET request_hash='replaced'`)
							}
						default:
							mutation := "UPDATE " + table + " SET remote_id='corrupt';"
							if mode == "trigger-source" {
								mutation = "UPDATE calendar_sources SET remote_id='corrupt' WHERE id='same-source';"
							}
							if mode == "trigger-target" {
								mutation = "UPDATE calendar_create_requests SET remote_id='consumed' WHERE request_id='original';"
							}
							if mode == "trigger-reservation" {
								mutation = "UPDATE calendar_create_requests SET request_hash='corrupt';"
							}
							action := "INSERT"
							if operation == "publish" {
								action = "UPDATE"
							}
							_, err = tx.Exec("CREATE TRIGGER corrupt_meeting AFTER " + action + " ON " + table + " BEGIN " + mutation + " END")
						}
						if err != nil {
							return err
						}
						if err := tx.Commit(); err != nil {
							return err
						}
						select {
						case err := <-done:
							if err == nil {
								t.Fatal("stale draft transition accepted", mode)
							}
						case <-ctx.Done():
							return ctx.Err()
						}
						if strings.HasPrefix(mode, "trigger-") || (mode == "nonce" && operation == "begin") {
							var remote string
							err := db.Read().QueryRow("SELECT remote_id FROM " + table).Scan(&remote)
							if operation == "begin" {
								if !errors.Is(err, sql.ErrNoRows) {
									t.Fatal("partial draft escaped rollback", remote, err)
								}
							} else {
								want := d.Google().RemoteID
								if provider == "outlook" {
									want = d.Teams().RemoteID
								}
								if err != nil || remote != want {
									t.Fatal("trigger escaped rollback", remote, err)
								}
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
}

func TestUserCalendarMeetingDraftExpirationAndLateDiscard(t *testing.T) {
	for _, mode := range []string{"expired-bind", "recent-expire", "saving-close"} {
		t.Run(mode, func(t *testing.T) {
			_, accounts, sources := ownedMeetingFixture(t, "outlook")
			source := sources["alice"]
			guard := func() error { return nil }
			d := readyMeetingDraft(t, accounts, source)
			create, err := accounts.BeginCalendarCreate(t.Context(), source, "original", "submitted", guard)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "expired-bind" {
				d, err = accounts.BindCalendarMeetingDraft(t.Context(), d, create, nil, guard)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode != "recent-expire" {
				if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				d, err = accounts.SnapshotCalendarMeetingDraft(t.Context(), source, ownedMeetingDraftID, guard)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expired-bind" {
				if _, err := accounts.BindCalendarMeetingDraft(t.Context(), d, create, nil, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
					t.Fatal("expired preview bound", err)
				}
				return
			}
			if mode == "recent-expire" {
				if _, err := accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftExpire, "", guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
					t.Fatal("recent saving reservation expired", err)
				}
				return
			}
			d, err = accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftAbandon, "", guard)
			if err != nil || d.Teams().State != "saving" {
				t.Fatal("late discard changed saving resource", err)
			}
			d, err = accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftExpire, "", guard)
			if err != nil || d.Teams().State != "abandoned" {
				t.Fatal("late discard renewed cleanup deadline", err)
			}
		})
	}
}

func TestUserCalendarMeetingDraftPrivateEventBinding(t *testing.T) {
	for _, mode := range []string{"current", "foreign", "changed", "trigger", "missing", "string-target"} {
		t.Run(mode, func(t *testing.T) {
			_, accounts, sources := ownedMeetingFixture(t, "gmail")
			seedCalendarSyncEvents(t, accounts)
			source := sources["alice"]
			guard := func() error { return nil }
			d := readyMeetingDraft(t, accounts, source)
			owner := "alice"
			if mode == "foreign" {
				owner = "bob"
			}
			event, err := accounts.SnapshotCalendarEvent(t.Context(), owner, "same-event")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				if _, err := accounts.BindCalendarMeetingDraft(t.Context(), d, nil, nil, guard); err == nil {
					t.Fatal("missing target authority accepted")
				}
				return
			}
			if mode == "string-target" {
				if _, err := accounts.PublishCalendarMeetingDraft(t.Context(), d, storage.CalendarMeetingDraftBind, "event:same-event", guard); err == nil {
					t.Fatal("string used as target authority")
				}
				return
			}
			if mode == "changed" || mode == "trigger" {
				if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
					if mode == "changed" {
						_, err := db.Write().Exec(`UPDATE calendar_events SET summary='changed' WHERE id='same-event'`)
						return err
					}
					_, err := db.Write().Exec(`CREATE TRIGGER corrupt_target AFTER UPDATE ON calendar_meet_drafts BEGIN UPDATE calendar_events SET summary='changed' WHERE id='same-event'; END`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			next, err := accounts.BindCalendarMeetingDraft(t.Context(), d, nil, event, guard)
			if mode == "current" {
				if err != nil || next.Google().UsedBy != "event:same-event" {
					t.Fatal("owned event binding", err)
				}
				return
			}
			if err == nil {
				t.Fatal("foreign/stale event bound")
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var target string
				if err := db.Read().QueryRow(`SELECT used_by FROM calendar_meet_drafts`).Scan(&target); err != nil {
					return err
				}
				if target != "" {
					t.Fatal("partial target binding")
				}
				if mode == "trigger" {
					var title string
					if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&title); err != nil {
						return err
					}
					if title != "alice old" {
						t.Fatal("target trigger escaped rollback")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarMeetingDraftFinishRequiresCompletedOriginalCreate(t *testing.T) {
	_, accounts, sources := ownedMeetingFixture(t, "outlook")
	guard := func() error { return nil }
	d := readyMeetingDraft(t, accounts, sources["alice"])
	claim, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "original", "submitted", guard)
	if err != nil {
		t.Fatal(err)
	}
	d, err = accounts.BindCalendarMeetingDraft(t.Context(), d, claim, nil, guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarTeamsCreate(t.Context(), d, claim, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("pending create marked saved", err)
	}
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	result, err := accounts.PublishCalendarCreate(t.Context(), claim, storage.CalendarEvent{RemoteID: "private-native", ETag: `W/"saved-v1"`, Summary: "Accepted event", StartAt: &start, EndAt: &end}, false, guard)
	if err != nil || result.EventID == "" {
		t.Fatal("publish accepted event", err)
	}
	completed, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "original", "submitted", guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarTeamsCreate(t.Context(), d, claim, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("consumed pending claim marked saved", err)
	}
	foreign, err := accounts.BeginCalendarCreate(t.Context(), sources["bob"], "original", "submitted", guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarTeamsCreate(t.Context(), d, foreign, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("foreign create marked saved", err)
	}
	other, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "other", "submitted", guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarTeamsCreate(t.Context(), d, other, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("different request marked saved", err)
	}
	saved, err := accounts.FinishCalendarTeamsCreate(t.Context(), d, completed, guard)
	if err != nil || saved.Teams().State != "saved" {
		t.Fatal("completed original target not saved", err)
	}
	if _, err := accounts.FinishCalendarTeamsCreate(t.Context(), d, completed, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("stale meeting claim accepted", err)
	}
	replay, err := accounts.FinishCalendarTeamsCreate(t.Context(), saved, completed, guard)
	if err != nil || replay.Teams() != saved.Teams() {
		t.Fatal("saved replay changed target", err)
	}
}

func TestUserCalendarMeetingDraftFinishRechecksWriterWaitAndTriggers(t *testing.T) {
	for _, mode := range []string{"target", "draft", "source", "grant", "trigger-target", "trigger-draft"} {
		t.Run(mode, func(t *testing.T) {
			_, accounts, sources := ownedMeetingFixture(t, "outlook")
			guard := func() error { return nil }
			d := readyMeetingDraft(t, accounts, sources["alice"])
			claim, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "original", "submitted", guard)
			if err != nil {
				t.Fatal(err)
			}
			d, err = accounts.BindCalendarMeetingDraft(t.Context(), d, claim, nil, guard)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			_, err = accounts.PublishCalendarCreate(t.Context(), claim, storage.CalendarEvent{RemoteID: "private-native", ETag: `W/"saved-v1"`, Summary: "Accepted", StartAt: &start, EndAt: &end}, false, guard)
			if err != nil {
				t.Fatal(err)
			}
			completed, err := accounts.BeginCalendarCreate(t.Context(), sources["alice"], "original", "submitted", guard)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				var changed atomic.Bool
				done := make(chan error, 1)
				go func() {
					_, err := accounts.FinishCalendarTeamsCreate(ctx, d, completed, func() error {
						if changed.Load() {
							return errors.New("grant changed")
						}
						return nil
					})
					done <- err
				}()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				switch mode {
				case "target":
					_, err = tx.Exec(`UPDATE calendar_create_requests SET remote_id='replaced' WHERE request_id='original'`)
				case "draft":
					_, err = tx.Exec(`UPDATE calendar_teams_drafts SET used_by='create:other'`)
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='replaced' WHERE id='same-source'`)
				case "grant":
					changed.Store(true)
				case "trigger-target":
					_, err = tx.Exec(`CREATE TRIGGER corrupt_finish AFTER UPDATE ON calendar_teams_drafts BEGIN UPDATE calendar_create_requests SET remote_id='replaced' WHERE request_id='original'; END`)
				case "trigger-draft":
					_, err = tx.Exec(`CREATE TRIGGER corrupt_finish AFTER UPDATE ON calendar_teams_drafts BEGIN UPDATE calendar_teams_drafts SET used_by='create:other'; END`)
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("stale finish accepted")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var state string
				if err := db.Read().QueryRow(`SELECT state FROM calendar_teams_drafts`).Scan(&state); err != nil {
					return err
				}
				if state != "saving" {
					t.Fatal("partial saved state escaped", state)
				}
				if strings.HasPrefix(mode, "trigger-") {
					var target, remote string
					if err := db.Read().QueryRow(`SELECT used_by FROM calendar_teams_drafts`).Scan(&target); err != nil {
						return err
					}
					if err := db.Read().QueryRow(`SELECT remote_id FROM calendar_create_requests WHERE request_id='original'`).Scan(&remote); err != nil {
						return err
					}
					if target != "create:original" || remote != "private-native" {
						t.Fatal("trigger escaped rollback", target, remote)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
