package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ContactDeleteSnapshot holds copied state only. A provider acknowledgement
// returns a new snapshot based on its own committed change, never on a fresh
// manual edit. This keeps subsequent remote deletes bound to the same request.
type ContactDeleteSnapshot struct {
	owner, id       string
	profile         *models.ContactProfile
	state           [32]byte
	preventRecreate bool
	sources         []models.ContactCard
}

func (s *ContactDeleteSnapshot) OwnerID() string { return s.owner }
func (s *ContactDeleteSnapshot) Contact() models.Contact {
	return contactEditorProjection(context.Background(), *s.profile)
}
func (s *ContactDeleteSnapshot) Sources() []models.ContactCard {
	return append([]models.ContactCard(nil), s.sources...)
}

func contactDeletePolicyTx(ctx context.Context, tx *sql.Tx, owner string) (bool, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id=? AND key='ui_settings'`, owner).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var settings map[string]string
	// Match the existing preference reader's default for absent/invalid JSON.
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &settings)
	}
	return boolSetting(settings["contacts_prevent_recreate_deleted"], true), nil
}
func (db *DB) SnapshotUserContactDelete(ctx context.Context, owner, id string, guard func(*sql.Tx) error, inspect func(*sql.Tx, []models.ContactCard) error) (*ContactDeleteSnapshot, error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return nil, err
	}
	profile, err := contactProfileStateQuery(ctx, tx, owner, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if profile == nil || profile.IsDeleted {
		return nil, sql.ErrNoRows
	}
	s := &ContactDeleteSnapshot{owner: owner, id: profile.ID, profile: profile}
	s.state, err = contactEditState(profile)
	if err != nil {
		return nil, err
	}
	s.preventRecreate, err = contactDeletePolicyTx(ctx, tx, owner)
	if err != nil {
		return nil, err
	}
	for _, card := range profile.Cards {
		if card.Kind != "provider" || card.IsDeleted {
			continue
		}
		switch card.Provider {
		case "gmail", "outlook", "carddav":
			s.sources = append(s.sources, card)
		}
	}
	rank := map[string]int{"gmail": 0, "outlook": 1, "carddav": 2}
	sort.Slice(s.sources, func(i, j int) bool {
		a, b := s.sources[i], s.sources[j]
		if a.Provider != b.Provider {
			return rank[a.Provider] < rank[b.Provider]
		}
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.AddressBookID != b.AddressBookID {
			return a.AddressBookID < b.AddressBookID
		}
		if a.RemoteID != b.RemoteID {
			return a.RemoteID < b.RemoteID
		}
		return a.ID < b.ID
	})
	if inspect != nil {
		if err := inspect(tx, s.Sources()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}
func checkContactDeleteTx(ctx context.Context, tx *sql.Tx, s *ContactDeleteSnapshot) error {
	current, err := contactProfileStateQuery(ctx, tx, s.owner, s.id)
	if err != nil {
		return err
	}
	state, err := contactEditState(current)
	if err != nil {
		return err
	}
	if state != s.state {
		return ErrContactEditChanged
	}
	prevent, err := contactDeletePolicyTx(ctx, tx, s.owner)
	if err != nil {
		return err
	}
	if prevent != s.preventRecreate {
		return ErrContactEditChanged
	}
	return nil
}
func (db *DB) ValidateUserContactDelete(ctx context.Context, s *ContactDeleteSnapshot, guard func(*sql.Tx) error) error {
	if s == nil || s.profile == nil {
		return ErrContactEditChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, s.owner, guard); err != nil {
		return err
	}
	if err := checkContactDeleteTx(ctx, tx, s); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) AcknowledgeUserContactDeleteSource(ctx context.Context, s *ContactDeleteSnapshot, cardID string, guard func(*sql.Tx) error) (*ContactDeleteSnapshot, error) {
	if s == nil || s.profile == nil {
		return nil, ErrContactEditChanged
	}
	index := -1
	for i, card := range s.sources {
		if card.ID == cardID {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, ErrContactEditChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, s.owner, guard); err != nil {
		return nil, err
	}
	if err := checkContactDeleteTx(ctx, tx, s); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM contact_cards WHERE id=? AND user_id=? AND profile_id=? AND kind='provider' AND is_deleted=0`, cardID, s.owner, s.id)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrContactEditChanged
	}
	next := *s
	next.profile, err = contactProfileStateQuery(ctx, tx, s.owner, s.id)
	if err != nil {
		return nil, err
	}
	if next.profile == nil || next.profile.IsDeleted {
		return nil, ErrContactEditChanged
	}
	next.state, err = contactEditState(next.profile)
	if err != nil {
		return nil, err
	}
	next.sources = append([]models.ContactCard(nil), s.sources[:index]...)
	next.sources = append(next.sources, s.sources[index+1:]...)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}
func (db *DB) FinishUserContactDelete(ctx context.Context, s *ContactDeleteSnapshot, guard func(*sql.Tx) error) (string, error) {
	if s == nil || s.profile == nil || len(s.sources) != 0 {
		return "", ErrContactEditChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, s.owner, guard); err != nil {
		return "", err
	}
	if err := checkContactDeleteTx(ctx, tx, s); err != nil {
		return "", err
	}
	var manual int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_fields WHERE user_id=? AND profile_id=? AND source='manual'`, s.owner, s.id).Scan(&manual); err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET is_deleted=1,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=? AND is_deleted=0`, s.owner, s.id)
	if err != nil {
		return "", err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", ErrContactEditChanged
	}
	if manual == 0 && s.preventRecreate {
		if _, err := tx.ExecContext(ctx, `UPDATE contact_observations SET is_suppressed=1,suppress_auto_create=1,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND profile_id=?`, s.owner, s.id); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_sync_operations SET status='done',locked_at=NULL,last_error='',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND contact_id=? AND status IN ('pending','running')`, s.owner, s.id); err != nil {
		return "", err
	}
	event := ContactActivityNotification{UserID: s.owner, ContactID: s.id, EventType: "contact_deleted", Email: s.Contact().Email, Message: "Contact deleted", Count: 1}
	if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, event.UserID, event.EventType, event.Email, event.Message, event.Count); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	db.notifyContactActivity(event)
	return s.id, nil
}
