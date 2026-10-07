package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// A selection names an approved account and either a provider remote ID or an
// owned stored profile. The constructor binds it to actual local source state.
type ContactSetupSelection struct {
	AccountID, Provider, RemoteID, StoredProfileID string
}

type contactSetupBinding struct {
	selection ContactSetupSelection
	card      *models.ContactCard
	donor     *models.ContactProfile
	state     [32]byte
	stored    *models.Contact
}

type ContactPreviewSnapshot struct {
	setup    *ContactEditSnapshot
	bindings []contactSetupBinding
}

// SetupSnapshot is opaque copied state for the facade's combined lifecycle guard.
func (s *ContactPreviewSnapshot) SetupSnapshot() *ContactEditSnapshot { return s.setup }

func (s *ContactPreviewSnapshot) OwnerID() string         { return s.setup.OwnerID() }
func (s *ContactPreviewSnapshot) Contact() models.Contact { return s.setup.Contact() }
func (s *ContactPreviewSnapshot) Selections() []ContactSetupSelection {
	out := make([]ContactSetupSelection, len(s.bindings))
	for i := range s.bindings {
		out[i] = s.bindings[i].selection
	}
	return out
}
func (s *ContactPreviewSnapshot) StoredContact(account string) *models.Contact {
	for _, b := range s.bindings {
		if b.selection.AccountID == account && b.stored != nil {
			copy := cloneContactEditValue(*b.stored)
			return &copy
		}
	}
	return nil
}

func contactSetupBindingTx(ctx context.Context, tx *sql.Tx, owner string, selection ContactSetupSelection) (contactSetupBinding, error) {
	b := contactSetupBinding{selection: selection}
	query := `SELECT id,profile_id FROM contact_cards WHERE user_id=? AND kind='provider' AND account_id=? AND provider=? AND is_deleted=0`
	args := []any{owner, selection.AccountID, selection.Provider}
	limit := ` LIMIT 2`
	if selection.StoredProfileID != "" {
		query += ` AND profile_id=?`
		args = append(args, selection.StoredProfileID)
		// Stored keys name a profile, which may have multiple books on this
		// account. Match the legacy latest-source choice with a stable tie break.
		limit = ` ORDER BY updated_at DESC,id LIMIT 1`
	} else {
		query += ` AND remote_id=?`
		args = append(args, selection.RemoteID)
	}
	rows, err := tx.QueryContext(ctx, query+limit, args...)
	if err != nil {
		return b, err
	}
	var cardID, donorID string
	for rows.Next() {
		if cardID != "" {
			rows.Close()
			return b, ErrContactEditChanged
		}
		if err := rows.Scan(&cardID, &donorID); err != nil {
			rows.Close()
			return b, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	if cardID == "" {
		if selection.StoredProfileID != "" {
			return b, ErrContactSetupInvalid
		}
		return b, nil
	}
	b.donor, err = contactProfileStateQuery(ctx, tx, owner, donorID)
	if err != nil {
		return b, err
	}
	if b.donor == nil || b.donor.IsDeleted {
		return b, ErrContactEditChanged
	}
	for _, card := range b.donor.Cards {
		if card.ID == cardID {
			copy := card
			b.card = &copy
			break
		}
	}
	if b.card == nil {
		return b, ErrContactEditChanged
	}
	if selection.StoredProfileID != "" {
		b.selection.RemoteID = b.card.RemoteID
		stored := contactEditorProjection(ctx, *b.donor)
		b.stored = &stored
	}
	b.state, err = contactEditState(b.donor)
	return b, err
}

// build copies service state and resolves browser keys inside the same read
// transaction; it must not retain the transaction or call a provider.
func (db *DB) SnapshotUserContactPreview(ctx context.Context, owner, id string, guard func(*sql.Tx) error, build func(*sql.Tx, []string) ([]ContactSetupSelection, error)) (*ContactPreviewSnapshot, error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return nil, err
	}
	setup, accounts, err := contactSetupSnapshotTx(ctx, tx, owner, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	approved := map[string]bool{}
	for _, account := range accounts {
		approved[account] = true
	}
	var selections []ContactSetupSelection
	if build != nil {
		selections, err = build(tx, accounts)
		if err != nil {
			return nil, err
		}
	}
	snapshot := &ContactPreviewSnapshot{setup: setup}
	seen := map[string]bool{}
	for _, selection := range selections {
		if !approved[selection.AccountID] || seen[selection.AccountID] || (selection.RemoteID == "" && selection.StoredProfileID == "") {
			return nil, ErrContactSetupInvalid
		}
		seen[selection.AccountID] = true
		var provider string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(cfg.provider,''),a.provider) FROM accounts a LEFT JOIN account_contact_sync_configs cfg ON cfg.account_id=a.id AND cfg.user_id=a.user_id WHERE a.user_id=? AND a.id=?`, owner, selection.AccountID).Scan(&provider); err != nil {
			return nil, err
		}
		if provider != selection.Provider || (provider != "gmail" && provider != "outlook" && provider != "carddav") {
			return nil, ErrContactSetupInvalid
		}
		binding, err := contactSetupBindingTx(ctx, tx, owner, selection)
		if err != nil {
			return nil, err
		}
		snapshot.bindings = append(snapshot.bindings, binding)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

type ContactSetupCandidateValue struct {
	AccountID, RemoteID, Etag string
	Contact                   models.Contact
}

type ContactPreviewResult struct {
	Contact models.Contact
	Fields  []models.ContactField
}

type StoredContactSetupCandidate struct {
	Contact  models.Contact
	RemoteID string
}

// ReadUserContactSetupCandidates copies one bounded page from an approved
// account. All profiles and their selected cards are read in the same snapshot;
// the caller releases its lease before rendering or contacting a provider.
func (db *DB) ReadUserContactSetupCandidates(ctx context.Context, setup *ContactEditSnapshot, account, provider, after string, guard func(*sql.Tx) error) ([]StoredContactSetupCandidate, error) {
	if setup == nil || !setup.setup {
		return nil, ErrContactEditChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, setup.owner, guard); err != nil {
		return nil, err
	}
	if err := checkContactEditTx(ctx, tx, setup); err != nil {
		return nil, err
	}
	_, accounts, err := contactEditTargetsQuery(ctx, tx, setup.owner, setup.request.SaveTargets)
	if err != nil {
		return nil, err
	}
	approved := false
	for _, id := range accounts {
		approved = approved || id == account
	}
	if !approved {
		return nil, ErrContactSetupInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT p.id FROM contact_profiles p JOIN contact_cards c ON c.profile_id=p.id AND c.user_id=p.user_id WHERE p.user_id=? AND p.is_deleted=0 AND p.id>? AND c.kind='provider' AND c.account_id=? AND c.provider=? AND c.is_deleted=0 ORDER BY p.id LIMIT 200`, setup.owner, after, account, provider)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]StoredContactSetupCandidate, 0, len(ids))
	for _, id := range ids {
		binding, err := contactSetupBindingTx(ctx, tx, setup.owner, ContactSetupSelection{AccountID: account, Provider: provider, StoredProfileID: id})
		if err != nil {
			return nil, err
		}
		out = append(out, StoredContactSetupCandidate{Contact: *binding.stored, RemoteID: binding.selection.RemoteID})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (db *DB) PublishUserContactPreview(ctx context.Context, snapshot *ContactPreviewSnapshot, values []ContactSetupCandidateValue, guard func(*sql.Tx) error) (ContactPreviewResult, error) {
	if snapshot == nil || snapshot.setup == nil || !snapshot.setup.setup || len(values) != len(snapshot.bindings) {
		return ContactPreviewResult{}, ErrContactEditChanged
	}
	byAccount := map[string]ContactSetupCandidateValue{}
	for _, value := range values {
		if _, exists := byAccount[value.AccountID]; exists {
			return ContactPreviewResult{}, ErrContactSetupInvalid
		}
		byAccount[value.AccountID] = value
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return ContactPreviewResult{}, err
	}
	defer tx.Rollback()
	owner, id := snapshot.setup.owner, snapshot.setup.id
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return ContactPreviewResult{}, err
	}
	if err := checkContactEditTx(ctx, tx, snapshot.setup); err != nil {
		return ContactPreviewResult{}, err
	}
	for _, b := range snapshot.bindings {
		value, ok := byAccount[b.selection.AccountID]
		if !ok || value.RemoteID != b.selection.RemoteID {
			return ContactPreviewResult{}, ErrContactSetupInvalid
		}
	}
	if err := checkContactPreviewBindingsTx(ctx, tx, snapshot); err != nil {
		return ContactPreviewResult{}, err
	}

	for _, b := range snapshot.bindings {
		value := byAccount[b.selection.AccountID]
		if b.stored != nil {
			value.Contact = cloneContactEditValue(*b.stored)
			value.Etag = b.card.Etag
		}
		if err := replaceSyncedContactFieldsTx(ctx, tx, owner, id, "synced:"+b.selection.AccountID, value.Contact); err != nil {
			return ContactPreviewResult{}, err
		}
		if b.card != nil {
			if b.card.ProfileID != id {
				if _, err := tx.ExecContext(ctx, `UPDATE contact_fields SET card_id=NULL WHERE user_id=? AND profile_id=? AND card_id=?`, owner, b.card.ProfileID, b.card.ID); err != nil {
					return ContactPreviewResult{}, err
				}
			}
			changed, err := tx.ExecContext(ctx, `UPDATE contact_cards SET profile_id=?,etag=?,is_deleted=0,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=?`, id, value.Etag, owner, b.card.ID)
			if err != nil {
				return ContactPreviewResult{}, err
			}
			if count, err := changed.RowsAffected(); err != nil {
				return ContactPreviewResult{}, err
			} else if count != 1 {
				return ContactPreviewResult{}, ErrContactEditChanged
			}
		} else {
			changed, err := tx.ExecContext(ctx, `INSERT INTO contact_cards(id,user_id,profile_id,kind,provider,account_id,remote_id,etag) VALUES(?,?,?,'provider',?,?,?,?)`, uuid.NewString(), owner, id, b.selection.Provider, b.selection.AccountID, b.selection.RemoteID, value.Etag)
			if err != nil {
				return ContactPreviewResult{}, err
			}
			if count, err := changed.RowsAffected(); err != nil {
				return ContactPreviewResult{}, err
			} else if count != 1 {
				return ContactPreviewResult{}, ErrContactEditChanged
			}
		}
	}
	profile, err := contactProfileStateQuery(ctx, tx, owner, id)
	if err != nil {
		return ContactPreviewResult{}, err
	}
	if profile == nil {
		return ContactPreviewResult{}, ErrContactEditChanged
	}
	result := ContactPreviewResult{Contact: contactEditorProjection(ctx, *profile), Fields: append([]models.ContactField(nil), profile.Fields...)}
	if err := tx.Commit(); err != nil {
		return ContactPreviewResult{}, err
	}
	return result, nil
}

// Validate the whole set before any mutation: candidates may share a donor,
// which publication's first write would otherwise change for its own next check.
func checkContactPreviewBindingsTx(ctx context.Context, tx *sql.Tx, snapshot *ContactPreviewSnapshot) error {
	for _, b := range snapshot.bindings {
		current, err := contactSetupBindingTx(ctx, tx, snapshot.setup.owner, b.selection)
		if err != nil {
			if errors.Is(err, ErrContactSetupInvalid) {
				return ErrContactEditChanged
			}
			return err
		}
		if !reflect.DeepEqual(b.card, current.card) || b.state != current.state {
			return ErrContactEditChanged
		}
	}
	return nil
}

func (db *DB) ValidateUserContactPreview(ctx context.Context, snapshot *ContactPreviewSnapshot, guard func(*sql.Tx) error) error {
	if snapshot == nil || snapshot.setup == nil || !snapshot.setup.setup {
		return ErrContactEditChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, snapshot.setup.owner, guard); err != nil {
		return err
	}
	if err := checkContactEditTx(ctx, tx, snapshot.setup); err != nil {
		return err
	}
	if err := checkContactPreviewBindingsTx(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}
