package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

var ErrContactEditChanged = errors.New("contact or sync locations changed during editing")
var ErrContactEditInvalid = errors.New("email is required")

// ContactEditSnapshot contains copied editor state, never a database handle.
// Its private identity/state prevents browser fields from authorizing publication.
type ContactEditSnapshot struct {
	owner, id, lookupEmail string
	setup                  bool
	request                models.Contact
	profile                *models.ContactProfile
	previous               *models.Contact
	state                  [32]byte
}

func cloneContactEditValue(c models.Contact) models.Contact {
	c.AdditionalEmails = append([]string(nil), c.AdditionalEmails...)
	c.AdditionalEmailLabels = append([]string(nil), c.AdditionalEmailLabels...)
	c.AdditionalPhones = append([]string(nil), c.AdditionalPhones...)
	c.AdditionalPhoneLabels = append([]string(nil), c.AdditionalPhoneLabels...)
	c.SaveTargets = append([]string(nil), c.SaveTargets...)
	c.SourceBooks = append([]models.ContactAddressBook(nil), c.SourceBooks...)
	return c
}

func (s *ContactEditSnapshot) OwnerID() string         { return s.owner }
func (s *ContactEditSnapshot) Contact() models.Contact { return cloneContactEditValue(s.request) }
func (s *ContactEditSnapshot) Previous() *models.Contact {
	if s.previous == nil {
		return nil
	}
	c := cloneContactEditValue(*s.previous)
	return &c
}

func contactEditState(profile *models.ContactProfile) ([32]byte, error) {
	if profile != nil {
		copy := *profile
		copy.Insights = nil // Computed insights have map-dependent ordering.
		copy.Cards = append([]models.ContactCard(nil), copy.Cards...)
		copy.Fields = append([]models.ContactField(nil), copy.Fields...)
		copy.SyncMemberships = append([]models.ContactSyncMembership(nil), copy.SyncMemberships...)
		sort.Slice(copy.Cards, func(i, j int) bool { return copy.Cards[i].ID < copy.Cards[j].ID })
		sort.Slice(copy.Fields, func(i, j int) bool { return copy.Fields[i].ID < copy.Fields[j].ID })
		sort.Slice(copy.SyncMemberships, func(i, j int) bool { return copy.SyncMemberships[i].ID < copy.SyncMemberships[j].ID })
		profile = &copy
	}
	data, err := json.Marshal(profile)
	return sha256.Sum256(data), err
}

func contactEditorProjection(ctx context.Context, profile models.ContactProfile) models.Contact {
	c := contactValuesFromProfile(ctx, profile)
	c.AvatarHash = avatarresolver.GravatarHash(c.Email)
	c.Initials = initials(contactDisplayName(c.Name, c.Email))
	for _, card := range profile.Cards {
		if card.Kind == "local" && !card.IsDeleted {
			c.SaveTargets = append(c.SaveTargets, "local")
			break
		}
	}
	for _, m := range profile.SyncMemberships {
		if !m.Enabled {
			continue
		}
		if m.AddressBookID != "" {
			c.SaveTargets = append(c.SaveTargets, "book:"+m.AddressBookID)
		} else if m.AccountID != "" {
			c.SaveTargets = append(c.SaveTargets, "account:"+m.AccountID)
		}
	}
	return c
}

func contactEditIdentityQuery(ctx context.Context, query contactProfileQuery, owner, email string) (string, error) {
	var id string
	err := query.QueryRowContext(ctx, `SELECT ci.profile_id FROM contact_identities ci JOIN contact_profiles p ON p.id=ci.profile_id AND p.user_id=ci.user_id
 WHERE ci.user_id=? AND ci.kind='email' AND ci.normalized_value=? AND p.is_deleted=0`, owner, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Match the editor's allowlist: invalid or disabled destinations are omitted,
// and an empty selection becomes Local. The writer rechecks accepted locations.
func contactEditTargetsQuery(ctx context.Context, query contactProfileQuery, owner string, targets []string) ([]string, []string, error) {
	var accepted []string
	accounts := make(map[string]bool)
	for _, target := range normalizeContactSaveTargets(targets) {
		if target == "local" {
			accepted = append(accepted, target)
			continue
		}
		account, book := "", ""
		if id, ok := strings.CutPrefix(target, "account:"); ok {
			account = id
		} else if id, ok := strings.CutPrefix(target, "book:"); ok {
			book = id
		} else {
			continue
		}
		var id string
		err := query.QueryRowContext(ctx, `SELECT a.id FROM accounts a LEFT JOIN account_contact_sync_configs cfg ON cfg.account_id=a.id AND cfg.user_id=a.user_id
 WHERE a.user_id=? AND COALESCE(a.is_deleting,0)=0
 AND ((?<>'' AND a.id=?) OR (?<>'' AND EXISTS(SELECT 1 FROM account_contact_address_books b WHERE b.user_id=a.user_id AND b.account_id=a.id AND b.id=?)))
 AND ((a.provider IN ('gmail','outlook') AND COALESCE(cfg.enabled,1)=1) OR (a.provider NOT IN ('gmail','outlook') AND cfg.provider='carddav' AND cfg.enabled=1))`, owner, account, account, book, book).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		accepted = append(accepted, target)
		accounts[id] = true
	}
	accepted = normalizeContactSaveTargets(accepted)
	var ids []string
	for id := range accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return accepted, ids, nil
}

// SnapshotUserContactEdit captures a complete contact and its accepted target
// services in one read transaction. inspect is read-only, used by the account
// facade to copy service/credential state; it must not retain tx or do HTTP.
func (db *DB) SnapshotUserContactEdit(ctx context.Context, owner string, request models.Contact, guard func(*sql.Tx) error, inspect func(*sql.Tx, []string) error) (*ContactEditSnapshot, error) {
	request = cloneContactEditValue(request)
	request.Email, request.ID = strings.TrimSpace(request.Email), strings.TrimSpace(request.ID)
	if normalizeContactEmail(request.Email) == "" {
		return nil, ErrContactEditInvalid
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return nil, err
	}
	s := &ContactEditSnapshot{owner: owner, id: request.ID, request: request}
	if s.id == "" {
		s.lookupEmail = normalizeContactEmail(request.Email)
		s.id, err = contactEditIdentityQuery(ctx, tx, owner, s.lookupEmail)
		if err != nil {
			return nil, err
		}
	}
	if s.id != "" {
		s.profile, err = contactProfileStateQuery(ctx, tx, owner, s.id)
		if err != nil {
			return nil, err
		}
		if s.profile == nil || s.profile.IsDeleted {
			return nil, sql.ErrNoRows
		}
		previous := contactEditorProjection(ctx, *s.profile)
		s.previous = &previous
	} else {
		s.id = uuid.NewString()
	}
	s.request.ID = s.id
	s.state, err = contactEditState(s.profile)
	if err != nil {
		return nil, err
	}
	var accounts []string
	s.request.SaveTargets, accounts, err = contactEditTargetsQuery(ctx, tx, owner, request.SaveTargets)
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
	return s, nil
}

// Unification copies the current profile and destinations in one read snapshot;
// a caller cannot overwrite an intervening edit with an earlier UI projection.
func (db *DB) SnapshotUserContactUnify(ctx context.Context, owner, id string, guard func(*sql.Tx) error, inspect func(*sql.Tx, []string) error) (*ContactEditSnapshot, error) {
	snapshot, err := db.SnapshotUserContactSetup(ctx, owner, id, guard, inspect)
	if err != nil {
		return nil, err
	}
	if normalizeContactEmail(snapshot.request.Email) == "" {
		return nil, ErrContactEditInvalid
	}
	snapshot.setup = false
	return snapshot, nil
}

func checkContactEditTx(ctx context.Context, tx *sql.Tx, s *ContactEditSnapshot) error {
	if s.lookupEmail != "" {
		id, err := contactEditIdentityQuery(ctx, tx, s.owner, s.lookupEmail)
		if err != nil {
			return err
		}
		expected := ""
		if s.profile != nil {
			expected = s.id
		}
		if id != expected {
			return ErrContactEditChanged
		}
	}
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
	targets, _, err := contactEditTargetsQuery(ctx, tx, s.owner, s.request.SaveTargets)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(targets, s.request.SaveTargets) {
		return ErrContactEditChanged
	}
	return nil
}

type ContactEditResult struct {
	Contact       models.Contact
	OperationID   string
	SetupDeferred bool
}

// SaveUserContactEdit commits the editor's fields, cards, canonical values,
// memberships, queue marker and activity together. No candidate escapes rollback.
func (db *DB) SaveUserContactEdit(ctx context.Context, s *ContactEditSnapshot, deferSetup bool, guard func(*sql.Tx) error) (ContactEditResult, error) {
	if s == nil || s.setup || s.owner == "" || s.id == "" {
		return ContactEditResult{}, ErrContactEditChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return ContactEditResult{}, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, s.owner, guard); err != nil {
		return ContactEditResult{}, err
	}
	if err := checkContactEditTx(ctx, tx, s); err != nil {
		return ContactEditResult{}, err
	}
	request := s.Contact()
	if deferSetup {
		request.GoferSyncEnabled = false
	}
	profile, targets := contactProfileForSave(s.owner, request, s.profile)
	request.Name = profile.DisplayName
	if _, err := saveContactProfileTx(ctx, tx, s.owner, profile); err != nil {
		return ContactEditResult{}, err
	}
	if err := replaceContactSyncMembershipsTx(ctx, tx, s.owner, s.id, targets); err != nil {
		return ContactEditResult{}, err
	}
	if request.GoferSyncEnabled {
		if err := replaceCanonicalContactFieldsTx(ctx, tx, s.owner, s.id, request); err != nil {
			return ContactEditResult{}, err
		}
	}
	var events []ContactActivityNotification
	if s.profile == nil {
		events = append(events, ContactActivityNotification{UserID: s.owner, EventType: "manual_contact_added", Email: request.Email, Message: "Manual contact added", Count: 1})
	}
	result := ContactEditResult{SetupDeferred: deferSetup}
	if request.GoferSyncEnabled {
		p, err := contactSyncProfileStateTx(ctx, tx, &ContactSyncClaim{owner: s.owner, profile: s.id})
		if err != nil {
			return ContactEditResult{}, err
		}
		if p != nil && len(p.targets) > 0 {
			previous := s.Previous()
			if s.lookupEmail != "" {
				previous = nil
			}
			result.OperationID, err = enqueueContactSyncOperation(ctx, tx, s.owner, p.contact, previous, "")
			if err != nil {
				return ContactEditResult{}, err
			}
			events = append(events, ContactActivityNotification{UserID: s.owner, ContactID: s.id, EventType: "contact_sync_queued", Email: p.contact.Email, Status: "pending", Message: "Gofer Sync queued", Count: 1})
		}
	}
	for _, event := range events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, event.UserID, event.EventType, event.Email, event.Message, event.Count); err != nil {
			return ContactEditResult{}, err
		}
	}
	saved, err := contactProfileStateQuery(ctx, tx, s.owner, s.id)
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
	for _, event := range events {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		db.notifyContactActivity(event)
	}
	return result, nil
}
