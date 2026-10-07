package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

var ErrContactBackfillChanged = errors.New("contact backfill settings changed")

// ContactBackfillSourceKey identifies the configured participant sources.
func ContactBackfillSourceKey(s ContactSettings) string {
	if !s.AutoCreateObserved {
		return ""
	}
	var parts []string
	if s.ObserveSenders {
		parts = append(parts, "senders")
	}
	if s.ObserveRecipients {
		parts = append(parts, "recipients")
	}
	return strings.Join(parts, ",")
}

func contactBackfillSettingsTx(ctx context.Context, tx *sql.Tx, owner string) (ContactSettings, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id=? AND key='ui_settings'`, owner).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return ContactSettings{}, err
	}
	values := map[string]string{}
	if json.Unmarshal([]byte(raw), &values) != nil {
		values = map[string]string{}
	}
	sources := uiSettingCSV(values["contacts_observed_sources"], "senders,recipients")
	return ContactSettings{AutoCreateObserved: boolSetting(values["contacts_auto_create_observed"], true), PreventRecreateDeleted: boolSetting(values["contacts_prevent_recreate_deleted"], true), ObserveSenders: sources["senders"], ObserveRecipients: sources["recipients"]}, nil
}

// withContactBackfillStore keeps administrative maintenance distinct from an
// owner's private request. Disabled retained owners are allowed only to admins.
func (r *AccountRouting) withContactBackfillStore(ctx context.Context, owner string, actor *DiagnosticsActor, fn func(*DB) error) error {
	if actor != nil {
		return r.withDiagnosticsStore(ctx, *actor, owner, fn)
	}
	return r.WithExistingUser(ctx, owner, fn)
}

func (r *AccountRouting) contactBackfillGuard(ctx context.Context, owner string, actor *DiagnosticsActor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if actor != nil {
		return r.ValidateDiagnosticsAccess(ctx, *actor, owner)
	}
	return r.ValidateUser(ctx, owner)
}

func contactBackfillParticipants(owner string, settings ContactSettings, ceiling int64) (parts []string, args []any) {

	if settings.ObserveSenders {
		parts = append(parts, `SELECT m.from_name name,m.from_email email,COALESCE(m.date_received,m.date_sent,m.created_at) seen_at FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND m.from_email!='' AND COALESCE(a.is_deleting,0)=0 AND m.id<=?`)
		args = append(args, owner, ceiling)
	}
	if settings.ObserveRecipients {
		parts = append(parts, `SELECT mr.name,mr.email,COALESCE(m.date_received,m.date_sent,m.created_at) seen_at FROM message_recipients mr JOIN messages m ON m.id=mr.message_id JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND mr.email!='' AND COALESCE(a.is_deleting,0)=0 AND m.id<=?`)
		args = append(args, owner, ceiling)
	}
	return
}

func (r *AccountRouting) CountUserContactBackfill(ctx context.Context, owner string, actor *DiagnosticsActor) (total int, err error) {
	err = r.withContactBackfillStore(ctx, owner, actor, func(db *DB) error {
		settings := db.GetContactSettings(ctx, owner)
		if ContactBackfillSourceKey(settings) == "" {
			return nil
		}
		parts, args := contactBackfillParticipants(owner, settings, math.MaxInt64)
		return db.Read().QueryRowContext(ctx, `SELECT COUNT(DISTINCT lower(trim(email))) FROM (`+strings.Join(parts, " UNION ALL ")+`) WHERE lower(trim(email))!=''`, args...).Scan(&total)
	})
	return
}

// BackfillUserContacts copies at most 128 identities and commits one bounded
// batch at a time. Progress and activity hooks run after releasing the store;
// a one-entry cache can serve another owner between batches. Each batch checks
// current central authority and local settings after waiting for the writer.
// Only an automatic job (nonempty sourceKey) writes its completion marker.
func (r *AccountRouting) BackfillUserContacts(ctx context.Context, owner string, actor *DiagnosticsActor, sourceKey string, progress func(int)) error {
	var settings ContactSettings
	var ceiling int64
	if err := r.withContactBackfillStore(ctx, owner, actor, func(db *DB) error {
		settings = db.GetContactSettings(ctx, owner)
		return db.Read().QueryRowContext(ctx, `SELECT COALESCE(MAX(m.id),0) FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0`, owner).Scan(&ceiling)
	}); err != nil {
		return err
	}
	if sourceKey != "" && sourceKey != ContactBackfillSourceKey(settings) {
		return ErrContactBackfillChanged
	}
	if ContactBackfillSourceKey(settings) == "" {
		return nil
	}
	processed, cursor := 0, ""
	for {
		var count int
		var hook func(ContactActivityNotification)
		var events []ContactActivityNotification
		err := r.withContactBackfillStore(ctx, owner, actor, func(db *DB) error {
			tx, err := db.Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := r.contactBackfillGuard(ctx, owner, actor); err != nil {
				return err
			}
			current, err := contactBackfillSettingsTx(ctx, tx, owner)
			if err != nil {
				return err
			}
			if current != settings {
				return ErrContactBackfillChanged
			}
			parts, args := contactBackfillParticipants(owner, settings, ceiling)
			args = append(args, cursor, 128)
			rows, err := tx.QueryContext(ctx, `WITH participants AS (`+strings.Join(parts, " UNION ALL ")+`),ranked AS (
 SELECT lower(trim(email)) normalized_email,email,name,
 ROW_NUMBER() OVER(PARTITION BY lower(trim(email)) ORDER BY seen_at DESC) rn,
 COUNT(*) OVER(PARTITION BY lower(trim(email))) message_count,MAX(seen_at) OVER(PARTITION BY lower(trim(email))) last_seen
 FROM participants WHERE lower(trim(email))>?)
 SELECT normalized_email,name,email,message_count,last_seen FROM ranked WHERE rn=1 ORDER BY normalized_email LIMIT ?`, args...)
			if err != nil {
				return err
			}
			type candidate struct {
				key, name, email, seen string
				n                      int
			}
			var batch []candidate
			for rows.Next() {
				var c candidate
				if err := rows.Scan(&c.key, &c.name, &c.email, &c.n, &c.seen); err != nil {
					rows.Close()
					return err
				}
				batch = append(batch, c)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			addEvent := func(kind, email, message string, n int) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES (?,?,?,?,?)`, owner, kind, email, message, n); err != nil {
					return err
				}
				events = append(events, ContactActivityNotification{UserID: owner, EventType: kind, Email: email, Message: message, Count: n, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
				return nil
			}
			if cursor == "" {
				if actor != nil {
					if err := addEvent("backfill_forced", "", "Webmail account contact backfill requested", 0); err != nil {
						return err
					}
				}
				if err := addEvent("backfill_started", "", "Observed contact backfill started", 0); err != nil {
					return err
				}
			}
			for _, c := range batch {
				seen := time.Now().UTC()
				if parsed, ok := parseSQLiteDateTime(c.seen); ok {
					seen = parsed
				}
				created, err := db.upsertObservedContactTx(ctx, tx, owner, c.name, c.email, seen, c.n, settings)
				if err != nil {
					return err
				}
				if created {
					events = append(events, ContactActivityNotification{UserID: owner, EventType: "observed_contact_added", Email: strings.TrimSpace(c.email), Message: "Observed contact added", Count: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
				}
			}
			count = len(batch)
			if count < 128 {
				if err := addEvent("backfill_completed", "", "Observed contact backfill completed", processed+count); err != nil {
					return err
				}
				if sourceKey != "" {
					if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO app_settings(user_id,key,value,updated_at) VALUES (?,'contacts_observed_backfilled_v1',?,CURRENT_TIMESTAMP)`, owner, sourceKey); err != nil {
						return err
					}
				}
			}
			if err := r.contactBackfillGuard(ctx, owner, actor); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			if count > 0 {
				cursor = batch[count-1].key
			}
			db.contactHookMu.RLock()
			hook = db.contactHook
			db.contactHookMu.RUnlock()
			return nil
		})
		if err != nil {
			return err
		}
		if hook == nil {
			r.System().contactHookMu.RLock()
			hook = r.System().contactHook
			r.System().contactHookMu.RUnlock()
		}
		if hook != nil {
			for _, event := range events {
				hook(event)
			}
		}
		processed += count
		if progress != nil {
			progress(processed)
		}
		if count < 128 {
			return nil
		}
	}
}
