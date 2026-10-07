package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrContactSetupInvalid = errors.New("invalid contact setup choices")

// SnapshotUserContactSetup derives values and locations from the stored contact
// in one read transaction. Browser fields never choose a profile's destinations.
func (db *DB) SnapshotUserContactSetup(ctx context.Context, owner, id string, guard func(*sql.Tx) error, inspect func(*sql.Tx, []string) error) (*ContactEditSnapshot, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, sql.ErrNoRows
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return nil, err
	}
	snapshot, accounts, err := contactSetupSnapshotTx(ctx, tx, owner, id)
	if err != nil {
		return nil, err
	}
	if inspect != nil {
		if err := inspect(tx, accounts); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func contactSetupSnapshotTx(ctx context.Context, tx *sql.Tx, owner, id string) (*ContactEditSnapshot, []string, error) {
	profile, err := contactProfileStateQuery(ctx, tx, owner, id)
	if err != nil {
		return nil, nil, err
	}
	if profile == nil || profile.IsDeleted {
		return nil, nil, sql.ErrNoRows
	}
	contact := contactEditorProjection(ctx, *profile)
	accepted, accounts, err := contactEditTargetsQuery(ctx, tx, owner, contact.SaveTargets)
	if err != nil {
		return nil, nil, err
	}
	if !reflect.DeepEqual(accepted, normalizeContactSaveTargets(contact.SaveTargets)) {
		return nil, nil, ErrContactEditChanged
	}
	contact.SaveTargets = accepted
	state, err := contactEditState(profile)
	if err != nil {
		return nil, nil, err
	}
	previous := cloneContactEditValue(contact)
	return &ContactEditSnapshot{owner: owner, id: id, setup: true, request: contact, profile: profile, previous: &previous, state: state}, accounts, nil
}

// ConfirmUserContactSetup changes canonical fields, enables synchronization and
// records durable work/activity in the same transaction. Provider cards and the
// original field choices remain intact. Publication rechecks the captured state
// after acquiring the writer; no successful identity escapes a failed commit.
func (db *DB) ConfirmUserContactSetup(ctx context.Context, snapshot *ContactEditSnapshot, selected map[string]string, guard func(*sql.Tx) error) (ContactEditResult, error) {
	if snapshot == nil || !snapshot.setup || snapshot.owner == "" || snapshot.id == "" || snapshot.profile == nil {
		return ContactEditResult{}, ErrContactEditChanged
	}
	canonical, err := contactSetupCanonicalValues(*snapshot.profile, selected)
	if err != nil {
		return ContactEditResult{}, fmt.Errorf("%w: %v", ErrContactSetupInvalid, err)
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return ContactEditResult{}, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, snapshot.owner, guard); err != nil {
		return ContactEditResult{}, err
	}
	if err := checkContactEditTx(ctx, tx, snapshot); err != nil {
		return ContactEditResult{}, err
	}
	if err := replaceCanonicalContactFieldsTx(ctx, tx, snapshot.owner, snapshot.id, canonical); err != nil {
		return ContactEditResult{}, err
	}
	changed, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET sync_enabled=1,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=? AND is_deleted=0`, snapshot.owner, snapshot.id)
	if err != nil {
		return ContactEditResult{}, err
	}
	if count, err := changed.RowsAffected(); err != nil {
		return ContactEditResult{}, err
	} else if count != 1 {
		return ContactEditResult{}, ErrContactEditChanged
	}
	var result ContactEditResult
	var event *ContactActivityNotification
	current, err := contactSyncProfileStateTx(ctx, tx, &ContactSyncClaim{owner: snapshot.owner, profile: snapshot.id})
	if err != nil {
		return ContactEditResult{}, err
	}
	if current != nil && len(current.targets) > 0 {
		result.OperationID, err = enqueueContactSyncOperation(ctx, tx, snapshot.owner, current.contact, nil, "")
		if err != nil {
			return ContactEditResult{}, err
		}
		event = &ContactActivityNotification{UserID: snapshot.owner, ContactID: snapshot.id, Email: current.contact.Email,
			EventType: "contact_sync_queued", Status: "pending", Message: "Gofer Sync queued", Count: 1}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, event.UserID, event.EventType, event.Email, event.Message, event.Count); err != nil {
			return ContactEditResult{}, err
		}
	}
	saved, err := contactProfileStateQuery(ctx, tx, snapshot.owner, snapshot.id)
	if err != nil {
		return ContactEditResult{}, err
	}
	if saved == nil {
		return ContactEditResult{}, ErrContactEditChanged
	}
	result.Contact = contactEditorProjection(ctx, *saved)
	if err := tx.Commit(); err != nil {
		return ContactEditResult{}, err
	}
	if event != nil {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		db.notifyContactActivity(*event)
	}
	return result, nil
}

func (s *ContactEditSnapshot) SetupFields() []models.ContactField {
	if s == nil || !s.setup || s.profile == nil {
		return nil
	}
	return append([]models.ContactField(nil), s.profile.Fields...)
}

func (db *DB) ValidateUserContactSetup(ctx context.Context, snapshot *ContactEditSnapshot, guard func(*sql.Tx) error) error {
	if snapshot == nil || !snapshot.setup {
		return ErrContactEditChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, snapshot.owner, guard); err != nil {
		return err
	}
	if err := checkContactEditTx(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}
