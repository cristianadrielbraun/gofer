package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrContactSyncSuperseded = errors.New("contact sync work no longer matches its claim or current profile")
var ErrContactSyncUnavailable = errors.New("contact has no enabled sync locations")

// ContactSyncClaim contains no database handle. Attempt identity remains private
// and survives cache eviction; another claim cannot finalize this attempt.
type ContactSyncClaim struct {
	owner, id, profile, payload, excluded, locked string
	attempt                                       int
	expires                                       time.Time
}

func (c *ContactSyncClaim) OwnerID() string     { return c.owner }
func (c *ContactSyncClaim) OperationID() string { return c.id }
func (c *ContactSyncClaim) ContactID() string   { return c.profile }
func (c *ContactSyncClaim) AttemptCount() int   { return c.attempt }

type ContactSyncTarget struct {
	AccountID, AddressBookID string
}

// ContactSyncProfile is a copied canonical profile and membership set. A job
// uses this same snapshot for every destination, rather than treating a newer
// manual edit as permission to publish a response to an older provider request.
type ContactSyncProfile struct {
	claim   *ContactSyncClaim
	contact models.Contact
	targets []ContactSyncTarget
	state   [32]byte
}

func (p *ContactSyncProfile) Contact() models.Contact {
	c := p.contact
	c.AdditionalEmails = append([]string(nil), c.AdditionalEmails...)
	c.AdditionalEmailLabels = append([]string(nil), c.AdditionalEmailLabels...)
	c.AdditionalPhones = append([]string(nil), c.AdditionalPhones...)
	c.AdditionalPhoneLabels = append([]string(nil), c.AdditionalPhoneLabels...)
	c.SaveTargets = append([]string(nil), c.SaveTargets...)
	return c
}

func (p *ContactSyncProfile) Targets() []ContactSyncTarget {
	return append([]ContactSyncTarget(nil), p.targets...)
}

// ContactSyncDestination also freezes the provider card. Its acknowledgement
// cannot overwrite a card changed by a later pull or another queued job.
type ContactSyncDestination struct {
	profile                 *ContactSyncProfile
	account, provider, book string
	bookURL                 string
	card                    *models.ContactCard
}

func (d *ContactSyncDestination) Source() *ContactSource {
	if d.card == nil {
		return nil
	}
	c := d.card
	return &ContactSource{ContactID: c.ProfileID, UserID: c.UserID, Provider: c.Provider,
		AccountID: c.AccountID, AddressBookID: c.AddressBookID, RemoteID: c.RemoteID, Etag: c.Etag}
}

func contactSyncRemoteInBook(bookURL, remoteID string) bool {
	book, bookErr := url.Parse(bookURL)
	remote, remoteErr := url.Parse(remoteID)
	return bookErr == nil && remoteErr == nil && remote.IsAbs() && remote.Scheme == book.Scheme &&
		remote.Host == book.Host && remote.User == nil && remote.Fragment == "" && remote.RawQuery == "" &&
		strings.HasPrefix(remoteID, strings.TrimRight(bookURL, "/")+"/") &&
		strings.HasPrefix(path.Clean(remote.Path), strings.TrimRight(path.Clean(book.Path), "/")+"/")
}

// ContactRemoteInAddressBook checks a copied endpoint boundary. It does not
// authorize an owner/account; callers still need the captured service guards.
func ContactRemoteInAddressBook(bookURL, remoteID string) bool {
	return contactSyncRemoteInBook(bookURL, remoteID)
}

func contactSyncOwnerTx(ctx context.Context, tx *sql.Tx, owner string, guard func(*sql.Tx) error) error {
	if owner == "" || guard == nil {
		return ErrContactSyncSuperseded
	}
	if err := guard(tx); err != nil {
		return err
	}
	var valid int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND status='active' AND deletion_pending=0)`, owner).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return ErrContactSyncSuperseded
	}
	return nil
}

// ClaimUserContactSyncOperations is explicitly owner-scoped, including retry
// recovery. A central lifecycle guard runs after waiting for the local writer.
// No claims escape a failed commit (including malformed queued payloads).
func (db *DB) ClaimUserContactSyncOperations(ctx context.Context, owner string, limit int, lockTimeout time.Duration, guard func(*sql.Tx) error) ([]*ContactSyncClaim, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	if lockTimeout <= 0 {
		lockTimeout = 5 * time.Minute
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-lockTimeout).Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM contact_sync_operations
 WHERE user_id=? AND (status='pending' OR (status='running' AND julianday(replace(locked_at,' +0000 UTC',''))<=julianday(?)))
 AND julianday(replace(next_attempt_at,' +0000 UTC',''))<=julianday('now')
 ORDER BY created_at,id LIMIT ?`, owner, cutoff, limit)
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
	claims := make([]*ContactSyncClaim, 0, len(ids))
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE contact_sync_operations SET status='running',locked_at=?,attempt_count=attempt_count+1,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=?`, now.Format(time.RFC3339Nano), owner, id); err != nil {
			return nil, err
		}
		c := &ContactSyncClaim{owner: owner, id: id, locked: now.Format(time.RFC3339Nano), expires: now.Add(lockTimeout)}
		if err := tx.QueryRowContext(ctx, `SELECT contact_id,payload_json,attempt_count FROM contact_sync_operations WHERE user_id=? AND id=?`, owner, id).Scan(&c.profile, &c.payload, &c.attempt); err != nil {
			return nil, err
		}
		var payload ContactSyncOperationPayload
		if err := json.Unmarshal([]byte(c.payload), &payload); err != nil {
			return nil, err
		}
		// Only the durable exclusion is used. Profile values and targets in the
		// queued payload are intentionally never replayed over the current data.
		c.excluded = strings.TrimSpace(payload.ExcludedAccountID)
		claims = append(claims, c)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claims, nil
}

func checkContactSyncClaimTx(ctx context.Context, tx *sql.Tx, c *ContactSyncClaim) error {
	if c == nil || c.owner == "" || c.id == "" || c.attempt < 1 || !time.Now().Before(c.expires) {
		return ErrContactSyncSuperseded
	}
	var valid int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_sync_operations WHERE user_id=? AND id=? AND contact_id=? AND payload_json=? AND status='running' AND attempt_count=? AND locked_at=?)`, c.owner, c.id, c.profile, c.payload, c.attempt, c.locked).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return ErrContactSyncSuperseded
	}
	return nil
}

func contactSyncProfileTx(ctx context.Context, tx *sql.Tx, c *ContactSyncClaim) (*ContactSyncProfile, error) {
	if err := checkContactSyncClaimTx(ctx, tx, c); err != nil {
		return nil, err
	}
	return contactSyncProfileStateTx(ctx, tx, c)
}

func contactSyncProfileStateTx(ctx context.Context, tx *sql.Tx, c *ContactSyncClaim) (*ContactSyncProfile, error) {
	profile, err := contactProfileRow(ctx, tx, c.owner, c.profile)
	if err != nil || profile == nil {
		return nil, err
	}
	if profile.IsDeleted || !profile.SyncEnabled {
		return nil, nil
	}
	profile.Fields, err = contactFieldsQuery(ctx, tx, c.owner, c.profile)
	if err != nil {
		return nil, err
	}
	// Match the UI's canonical projection. Provider field acknowledgements do
	// not invalidate the canonical snapshot while this fanout runs.
	var canonical []models.ContactField
	for _, field := range profile.Fields {
		if field.Source == "canonical" {
			canonical = append(canonical, field)
		}
	}
	if len(canonical) > 0 {
		profile.Fields = canonical
	}
	p := &ContactSyncProfile{claim: c, contact: contactValuesFromProfile(ctx, *profile)}
	if normalizeContactEmail(p.contact.Email) == "" {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id,address_book_id FROM contact_sync_memberships WHERE user_id=? AND profile_id=? AND enabled=1 ORDER BY account_id,address_book_id`, c.owner, c.profile)
	if err != nil {
		return nil, err
	}
	var all []ContactSyncTarget
	for rows.Next() {
		var target ContactSyncTarget
		if err := rows.Scan(&target.AccountID, &target.AddressBookID); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, target)
		if target.AddressBookID != "" {
			p.contact.SaveTargets = append(p.contact.SaveTargets, "book:"+target.AddressBookID)
		} else {
			p.contact.SaveTargets = append(p.contact.SaveTargets, "account:"+target.AccountID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, target := range all {
		if target.AccountID == c.excluded {
			continue
		}
		var eligible int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts a
 LEFT JOIN account_contact_sync_configs cfg ON cfg.account_id=a.id AND cfg.user_id=a.user_id
 WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND
 ((a.provider IN ('gmail','outlook') AND COALESCE(cfg.enabled,1)=1 AND ?='') OR
 (a.provider NOT IN ('gmail','outlook') AND cfg.provider='carddav' AND cfg.enabled=1
 AND (?='' OR EXISTS(SELECT 1 FROM account_contact_address_books b WHERE b.user_id=a.user_id AND b.account_id=a.id AND b.id=?)))))`, target.AccountID, c.owner, target.AddressBookID, target.AddressBookID, target.AddressBookID).Scan(&eligible); err != nil {
			return nil, err
		}
		if eligible == 1 {
			p.targets = append(p.targets, target)
		}
	}
	// Compare provider-writable values rather than second-resolution timestamps.
	// The projected values also support older sync-enabled profiles with no
	// canonical rows: acknowledging identical provider values is not a new edit.
	var values [][]any
	for _, field := range contactFieldSnapshotValues(p.contact) {
		values = append(values, []any{field.kind, field.label, field.value, field.isPrimary})
	}
	data, err := json.Marshal([]any{profile.ID, profile.UserID, profile.DisplayName, profile.PrimaryEmail,
		profile.AvatarURL, profile.Notes, profile.SyncEnabled, profile.IsDeleted, values, all})
	if err != nil {
		return nil, err
	}
	p.state = sha256.Sum256(data)
	return p, nil
}

// EnqueueUserContactSync rereads values and enabled locations inside the same
// writer transaction as queue insertion and the queued activity event.
func (db *DB) EnqueueUserContactSync(ctx context.Context, owner, profile string, guard func(*sql.Tx) error) (string, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return "", err
	}
	p, err := contactSyncProfileStateTx(ctx, tx, &ContactSyncClaim{owner: owner, profile: profile})
	if err != nil {
		return "", err
	}
	if p == nil || len(p.targets) == 0 {
		return "", ErrContactSyncUnavailable
	}
	id, err := enqueueContactSyncOperation(ctx, tx, owner, p.contact, nil, "")
	if err != nil {
		return "", err
	}
	event := ContactActivityNotification{UserID: owner, ContactID: profile, Email: p.contact.Email,
		EventType: "contact_sync_queued", Status: "pending", Message: "Gofer Sync queued", Count: 1}
	if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, event.UserID, event.EventType, event.Email, event.Message, event.Count); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	db.notifyContactActivity(event)
	return id, nil
}

func checkContactSyncProfileTx(ctx context.Context, tx *sql.Tx, p *ContactSyncProfile) error {
	if p == nil {
		return ErrContactSyncSuperseded
	}
	current, err := contactSyncProfileTx(ctx, tx, p.claim)
	if err != nil {
		return err
	}
	if current == nil || current.state != p.state {
		return ErrContactSyncSuperseded
	}
	return nil
}

func (db *DB) SnapshotUserContactSyncProfile(ctx context.Context, c *ContactSyncClaim, guard func(*sql.Tx) error) (*ContactSyncProfile, error) {
	if c == nil {
		return nil, ErrContactSyncSuperseded
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, c.owner, guard); err != nil {
		return nil, err
	}
	p, err := contactSyncProfileTx(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

// StartUserContactSyncOperation publishes the legacy running activity only for
// the exact claimed, current profile. The event cannot escape a failed commit.
func (db *DB) StartUserContactSyncOperation(ctx context.Context, p *ContactSyncProfile, guard func(*sql.Tx) error) error {
	if p == nil || p.claim == nil {
		return ErrContactSyncSuperseded
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, p.claim.owner, guard); err != nil {
		return err
	}
	if err := checkContactSyncProfileTx(ctx, tx, p); err != nil {
		return err
	}
	event := ContactActivityNotification{UserID: p.claim.owner, ContactID: p.claim.profile, Email: p.contact.Email,
		EventType: "contact_sync_started", Status: "running", Message: "Gofer Sync started", Count: 1}
	if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, event.UserID, event.EventType, event.Email, event.Message, event.Count); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	db.notifyContactActivity(event)
	return nil
}

// CancelUserContactSyncOperation finishes a missing/disabled profile silently,
// matching the shared worker. It never reports a cancelled job as provider
// success, or cancels a profile that has been re-enabled since its snapshot.
func (db *DB) CancelUserContactSyncOperation(ctx context.Context, c *ContactSyncClaim, guard func(*sql.Tx) error) error {
	if c == nil {
		return ErrContactSyncSuperseded
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, c.owner, guard); err != nil {
		return err
	}
	p, err := contactSyncProfileTx(ctx, tx, c)
	if err != nil {
		return err
	}
	if p != nil {
		return ErrContactSyncSuperseded
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_sync_operations SET status='done',locked_at=NULL,last_error='',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=?`, c.owner, c.id); err != nil {
		return err
	}
	return tx.Commit()
}

func contactSyncDestinationTx(ctx context.Context, tx *sql.Tx, p *ContactSyncProfile, account, provider, book string) (*ContactSyncDestination, error) {
	if err := checkContactSyncProfileTx(ctx, tx, p); err != nil {
		return nil, err
	}
	if account == "" || account == p.claim.excluded || (provider != "gmail" && provider != "outlook" && provider != "carddav") || (provider != "carddav" && book != "") {
		return nil, ErrContactPublication
	}
	owner := p.claim.owner
	var valid int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts a
 LEFT JOIN account_contact_sync_configs c ON c.account_id=a.id AND c.user_id=a.user_id
 WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0
 AND ((a.provider IN ('gmail','outlook') AND a.provider=? AND COALESCE(c.enabled,1)=1)
 OR (a.provider NOT IN ('gmail','outlook') AND ?='carddav' AND c.provider='carddav' AND c.enabled=1))
 AND EXISTS(SELECT 1 FROM contact_sync_memberships m WHERE m.user_id=a.user_id AND m.profile_id=? AND m.account_id=a.id AND m.enabled=1 AND (m.address_book_id='' OR m.address_book_id=?)))`, account, owner, provider, provider, p.claim.profile, book).Scan(&valid); err != nil {
		return nil, err
	}
	if valid != 1 {
		return nil, ErrContactPublication
	}
	d := &ContactSyncDestination{profile: p, account: account, provider: provider, book: book}
	if provider == "carddav" {
		if book == "" {
			err := tx.QueryRowContext(ctx, `SELECT addressbook_url FROM account_contact_sync_configs WHERE user_id=? AND account_id=? AND NOT EXISTS(SELECT 1 FROM account_contact_address_books WHERE user_id=? AND account_id=?)`, owner, account, owner, account).Scan(&d.bookURL)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrContactPublication
			}
			if err != nil {
				return nil, err
			}
		} else if err := tx.QueryRowContext(ctx, `SELECT url FROM account_contact_address_books WHERE user_id=? AND account_id=? AND id=?`, owner, account, book).Scan(&d.bookURL); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrContactPublication
			}
			return nil, err
		}
		if strings.TrimSpace(d.bookURL) == "" {
			return nil, ErrContactPublication
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,user_id,profile_id,kind,provider,account_id,address_book_id,remote_id,etag,raw_payload,raw_payload_type,sync_status,last_error,is_deleted
 FROM contact_cards WHERE user_id=? AND profile_id=? AND kind='provider' AND provider=? AND account_id=? AND is_deleted=0
 AND (address_book_id=? OR (?='carddav' AND address_book_id='')) ORDER BY id`, owner, p.claim.profile, provider, account, book, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var card models.ContactCard
		var deleted int
		if err := rows.Scan(&card.ID, &card.UserID, &card.ProfileID, &card.Kind, &card.Provider, &card.AccountID, &card.AddressBookID, &card.RemoteID, &card.Etag, &card.RawPayload, &card.RawPayloadType, &card.SyncStatus, &card.LastError, &deleted); err != nil {
			return nil, err
		}
		if provider == "carddav" && card.AddressBookID == "" && !contactSyncRemoteInBook(d.bookURL, card.RemoteID) {
			continue
		}
		if provider == "carddav" && card.RemoteID != "" && !contactSyncRemoteInBook(d.bookURL, card.RemoteID) {
			return nil, ErrContactPublication
		}
		if d.card != nil {
			return nil, ErrContactPublication
		}
		card.IsDeleted = deleted == 1
		d.card = &card
	}
	return d, rows.Err()
}

func (db *DB) SnapshotUserContactSyncDestination(ctx context.Context, p *ContactSyncProfile, account, provider, book string, guard func(*sql.Tx) error) (*ContactSyncDestination, error) {
	if p == nil || p.claim == nil {
		return nil, ErrContactSyncSuperseded
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, p.claim.owner, guard); err != nil {
		return nil, err
	}
	d, err := contactSyncDestinationTx(ctx, tx, p, account, provider, book)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d, nil
}

func checkContactSyncDestinationTx(ctx context.Context, tx *sql.Tx, d *ContactSyncDestination) error {
	if d == nil {
		return ErrContactSyncSuperseded
	}
	current, err := contactSyncDestinationTx(ctx, tx, d.profile, d.account, d.provider, d.book)
	if err != nil {
		return err
	}
	if current.bookURL != d.bookURL || (current.card == nil) != (d.card == nil) || (current.card != nil && *current.card != *d.card) {
		return ErrContactSyncSuperseded
	}
	return nil
}

func (db *DB) ValidateUserContactSyncDestination(ctx context.Context, d *ContactSyncDestination, guard func(*sql.Tx) error) error {
	if d == nil || d.profile == nil || d.profile.claim == nil {
		return ErrContactSyncSuperseded
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, d.profile.claim.owner, guard); err != nil {
		return err
	}
	if err := checkContactSyncDestinationTx(ctx, tx, d); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishUserContactSyncResult acknowledges only the remote identity/version.
// Card and source-field snapshot publication is atomic and never overwrites the
// canonical/manual data, queues another fanout, or accepts caller profile IDs.
func (db *DB) PublishUserContactSyncResult(ctx context.Context, d *ContactSyncDestination, remoteID, etag string, guard func(*sql.Tx) error) error {
	if d == nil || d.profile == nil || d.profile.claim == nil || strings.TrimSpace(remoteID) == "" {
		return ErrContactPublication
	}
	remoteID, etag = strings.TrimSpace(remoteID), strings.TrimSpace(etag)
	if d.provider == "carddav" && !contactSyncRemoteInBook(d.bookURL, remoteID) {
		return ErrContactPublication
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owner, profile := d.profile.claim.owner, d.profile.claim.profile
	if err := contactSyncOwnerTx(ctx, tx, owner, guard); err != nil {
		return err
	}
	if err := checkContactSyncDestinationTx(ctx, tx, d); err != nil {
		return err
	}
	if err := checkContactSyncRemoteTx(ctx, tx, d, remoteID); err != nil {
		return err
	}
	card := models.ContactCard{UserID: owner, ProfileID: profile, Kind: "provider", Provider: d.provider,
		AccountID: d.account, AddressBookID: d.book, RemoteID: remoteID, Etag: etag, SyncStatus: "done"}
	if d.card != nil {
		card = *d.card
		card.AddressBookID, card.RemoteID, card.Etag, card.SyncStatus, card.LastError = d.book, remoteID, etag, "done", ""
	}
	if err := upsertContactCardTx(ctx, tx, card); err != nil {
		return err
	}
	if err := replaceSyncedContactFieldsTx(ctx, tx, owner, profile, "synced:"+d.account, d.profile.contact); err != nil {
		return err
	}
	return tx.Commit()
}

// ValidateUserContactSyncRemote checks a preflight alias before a provider write,
// not merely after the remote side has already accepted a change.
func (db *DB) ValidateUserContactSyncRemote(ctx context.Context, d *ContactSyncDestination, remoteID string, guard func(*sql.Tx) error) error {
	if d == nil || d.profile == nil || d.profile.claim == nil || strings.TrimSpace(remoteID) == "" {
		return ErrContactPublication
	}
	if d.provider == "carddav" && !contactSyncRemoteInBook(d.bookURL, remoteID) {
		return ErrContactPublication
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, d.profile.claim.owner, guard); err != nil {
		return err
	}
	if err := checkContactSyncDestinationTx(ctx, tx, d); err != nil {
		return err
	}
	if err := checkContactSyncRemoteTx(ctx, tx, d, remoteID); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishUserContactSyncVersion refreshes conflict metadata only. It must never
// claim that the remote side accepted the canonical values from a failed PUT.
func (db *DB) PublishUserContactSyncVersion(ctx context.Context, d *ContactSyncDestination, remoteID, etag string, guard func(*sql.Tx) error) error {
	if d == nil || d.profile == nil || d.profile.claim == nil || d.card == nil || d.card.RemoteID != remoteID || strings.TrimSpace(etag) == "" {
		return ErrContactPublication
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, d.profile.claim.owner, guard); err != nil {
		return err
	}
	if err := checkContactSyncDestinationTx(ctx, tx, d); err != nil {
		return err
	}
	if err := checkContactSyncRemoteTx(ctx, tx, d, remoteID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_cards SET etag=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=?`, strings.TrimSpace(etag), d.card.ID, d.profile.claim.owner); err != nil {
		return err
	}
	return tx.Commit()
}
func checkContactSyncRemoteTx(ctx context.Context, tx *sql.Tx, d *ContactSyncDestination, remoteID string) error {
	owner, profile := d.profile.claim.owner, d.profile.claim.profile
	linked, err := inboundContactProfileTx(ctx, tx, owner, d.account, d.provider, remoteID)
	if err != nil {
		return err
	}
	if linked != "" && linked != profile {
		return ErrContactPublication
	}
	// The generic card upsert can resurrect a deleted card by remote ID. That
	// is allowed only for this profile; outbound acknowledgement is not a merge
	// authorization for a different profile's retained provider history.
	var foreignHistory int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_cards WHERE user_id=? AND kind='provider' AND provider=? AND account_id=? AND remote_id=? AND profile_id<>?`, owner, d.provider, d.account, remoteID, profile).Scan(&foreignHistory); err != nil {
		return err
	}
	if foreignHistory != 0 {
		return ErrContactPublication
	}
	return nil
}

// FinishUserContactSyncOperation updates the exact claimed attempt and its
// activity event together. A failed/stale worker cannot finalize a replacement
// claim; cancellation never switches to a detached background context.
func (db *DB) FinishUserContactSyncOperation(ctx context.Context, c *ContactSyncClaim, status, message string, retryAt time.Time, guard func(*sql.Tx) error) error {
	if c == nil || (status != "done" && status != "pending" && status != "error") {
		return ErrContactSyncSuperseded
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := contactSyncOwnerTx(ctx, tx, c.owner, guard); err != nil {
		return err
	}
	if err := checkContactSyncClaimTx(ctx, tx, c); err != nil {
		return err
	}
	if retryAt.IsZero() {
		retryAt = time.Now()
		if status == "pending" {
			retryAt = retryAt.Add(2 * time.Minute)
		}
	}
	message = strings.TrimSpace(message)
	if status == "done" {
		message = ""
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_sync_operations SET status=?,locked_at=NULL,last_error=?,next_attempt_at=?,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND id=?`, status, message, retryAt.UTC().Format(time.RFC3339Nano), c.owner, c.id); err != nil {
		return err
	}
	event := ContactActivityNotification{UserID: c.owner, ContactID: c.profile, EventType: "contact_synced", Status: status, Message: "Gofer Sync complete", Count: 1}
	if status != "done" {
		event.EventType, event.Message, event.Error = "contact_sync_failed", "Gofer Sync failed: "+message, message
	}
	if err := tx.QueryRowContext(ctx, `SELECT email FROM contact_sync_operations WHERE user_id=? AND id=?`, c.owner, c.id).Scan(&event.Email); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, event.UserID, event.EventType, event.Email, event.Message, event.Count); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	db.notifyContactActivity(event)
	return nil
}

func (db *DB) NextUserContactSyncAttempt(ctx context.Context, owner string, lockTimeout time.Duration) (time.Time, error) {
	if owner == "" {
		return time.Time{}, ErrContactSyncSuperseded
	}
	if lockTimeout <= 0 {
		lockTimeout = 5 * time.Minute
	}
	var next, locked sqliteNullTime
	var status string
	err := db.Read().QueryRowContext(ctx, `SELECT next_attempt_at,locked_at,status FROM contact_sync_operations WHERE user_id=? AND status IN ('pending','running')
 ORDER BY CASE WHEN status='running' THEN max(julianday(replace(next_attempt_at,' +0000 UTC','')),julianday(replace(locked_at,' +0000 UTC',''))+?)
 ELSE julianday(replace(next_attempt_at,' +0000 UTC','')) END LIMIT 1`, owner, lockTimeout.Hours()/24).Scan(&next, &locked, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if status == "running" && locked.Time.Add(lockTimeout).After(next.Time) {
		next.Time = locked.Time.Add(lockTimeout)
	}
	return next.Time, err
}
