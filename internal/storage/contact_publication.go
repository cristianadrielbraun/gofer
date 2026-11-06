package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrContactPublication = errors.New("contact provider result no longer has a valid owned destination")

// InboundContact is a copied provider result. The repository supplies the owner,
// account, provider and profile identity; Contact.ID and SaveTargets are ignored.
type InboundContact struct {
	Contact                       models.Contact
	RemoteID, Etag, AddressBookID string
}

type InboundContactResult struct {
	ProfileID                 string
	Created, CanonicalChanged bool
	FanoutOperationID         string
}

// PublishInboundContacts merges a provider page, its source cards and durable
// excluded-source fanout in one transaction. The guard must compare the copied
// account/service snapshot inside this transaction, after acquiring the writer.
// No candidate identities are returned if any part of the page fails.
func (db *DB) PublishInboundContacts(ctx context.Context, owner, account, provider string, inputs []InboundContact, guard func(*sql.Tx) error) ([]InboundContactResult, error) {
	return db.publishInboundContacts(ctx, owner, account, provider, inputs, guard, nil)
}

func (db *DB) publishInboundContacts(ctx context.Context, owner, account, provider string, inputs []InboundContact, guard, checkpoint func(*sql.Tx) error) ([]InboundContactResult, error) {
	if owner == "" || account == "" || guard == nil {
		return nil, ErrContactPublication
	}
	if provider != "gmail" && provider != "outlook" && provider != "carddav" && provider != "jmap" {
		return nil, ErrContactPublication
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := guard(tx); err != nil {
		return nil, err
	}
	var valid int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM accounts a JOIN users u ON u.id=a.user_id
 LEFT JOIN account_contact_sync_configs c ON c.account_id=a.id AND c.user_id=a.user_id
 WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0
 AND u.status='active' AND u.deletion_pending=0
 AND ((a.provider IN ('gmail','outlook') AND a.provider=? AND COALESCE(c.enabled,1)=1)
 OR (a.provider NOT IN ('gmail','outlook') AND ? IN ('carddav','jmap') AND c.provider=? AND c.enabled=1)))`, account, owner, provider, provider, provider).Scan(&valid); err != nil {
		return nil, err
	}
	if valid != 1 {
		return nil, ErrContactPublication
	}
	results := make([]InboundContactResult, 0, len(inputs))
	var events []ContactActivityNotification
	for _, input := range inputs {
		input.RemoteID = strings.TrimSpace(input.RemoteID)
		input.AddressBookID = strings.TrimSpace(input.AddressBookID)
		if err := inboundContactBookTx(ctx, tx, owner, account, provider, input.AddressBookID); err != nil {
			return nil, err
		}
		preferred, err := inboundContactProfileTx(ctx, tx, owner, account, provider, input.RemoteID)
		if err != nil {
			return nil, err
		}
		// A contact in the Trash still has its address-book copy; leave it
		// alone rather than bring it back as a new contact.
		if trashed, err := contactInTrash(ctx, tx, owner, preferred); err != nil {
			return nil, err
		} else if trashed {
			results = append(results, InboundContactResult{})
			continue
		}
		id, created, changed, err := upsertSyncedContactTx(ctx, tx, owner, account, preferred, input.Contact)
		if err != nil {
			return nil, err
		}
		result := InboundContactResult{ProfileID: id, Created: created, CanonicalChanged: changed}
		if id != "" && input.RemoteID != "" {
			if err := upsertContactCardTx(ctx, tx, models.ContactCard{UserID: owner, ProfileID: id, Kind: "provider", Provider: provider,
				AccountID: account, AddressBookID: input.AddressBookID, RemoteID: input.RemoteID, Etag: input.Etag}); err != nil {
				return nil, err
			}
		}
		if changed {
			needed, err := inboundContactFanoutTx(ctx, tx, owner, id, account)
			if err != nil {
				return nil, err
			}
			if needed {
				// The outbound worker re-reads the current owned canonical profile
				// and enabled targets. This payload is a durable marker, not a
				// provider snapshot to replay over newer manual edits.
				contact := input.Contact
				contact.ID, contact.GoferSyncEnabled, contact.SaveTargets = id, true, nil
				result.FanoutOperationID, err = enqueueContactSyncOperation(ctx, tx, owner, contact, nil, account)
				if err != nil {
					return nil, err
				}
				event := ContactActivityNotification{UserID: owner, ContactID: id, EventType: "contact_sync_queued", Email: strings.TrimSpace(contact.Email), Status: "pending", Message: "Gofer Sync queued", Count: 1}
				if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, owner, event.EventType, event.Email, event.Message, event.Count); err != nil {
					return nil, err
				}
				events = append(events, event)
			}
		}
		results = append(results, result)
	}
	if checkpoint != nil {
		if err := checkpoint(tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, event := range events {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		db.notifyContactActivity(event)
	}
	return results, nil
}

// InboundContactBookPage is one complete DAV book response. Full means the
// server returned a full listing instead of a delta; SeenRemoteIDs includes
// resources without usable vCard/email data so they are not wrongly pruned.
type InboundContactBookPage struct {
	Contacts                        []InboundContact
	DeletedRemoteIDs, SeenRemoteIDs []string
	Full                            bool
	SyncToken                       string
}

// PublishInboundContactBook commits imports, remote deletions/full-list pruning
// and the durable book cursor together. The checkpoint callback copies the next
// service snapshot after those writes but before committing. It must do only
// local SQL; the caller returns that snapshot only after a successful commit.
func (db *DB) PublishInboundContactBook(ctx context.Context, owner, account string, book models.ContactAddressBook, page InboundContactBookPage, guard, checkpoint func(*sql.Tx) error) ([]InboundContactResult, error) {
	book.ID, book.URL = strings.TrimSpace(book.ID), strings.TrimSpace(book.URL)
	if book.URL == "" || checkpoint == nil {
		return nil, ErrContactPublication
	}
	prefix := strings.TrimRight(book.URL, "/") + "/"
	inScope := func(remote string) bool { return remote != "" && strings.HasPrefix(remote, prefix) }
	seen := make(map[string]bool, len(page.SeenRemoteIDs))
	deleted := make(map[string]bool, len(page.DeletedRemoteIDs))
	for _, remote := range page.SeenRemoteIDs {
		remote = strings.TrimSpace(remote)
		if !inScope(remote) {
			return nil, ErrContactPublication
		}
		seen[remote] = true
	}
	for _, remote := range page.DeletedRemoteIDs {
		remote = strings.TrimSpace(remote)
		if !inScope(remote) {
			return nil, ErrContactPublication
		}
		deleted[remote] = true
	}
	inputs := append([]InboundContact(nil), page.Contacts...)
	for i := range inputs {
		inputs[i].AddressBookID = book.ID
		remote := strings.TrimSpace(inputs[i].RemoteID)
		if remote != "" && (!inScope(remote) || deleted[remote]) {
			return nil, ErrContactPublication
		}
		if remote != "" {
			seen[remote] = true
		}
	}
	var unresolved []string
	results, err := db.publishInboundContacts(ctx, owner, account, "carddav", inputs, guard, func(tx *sql.Tx) error {
		if err := inboundContactBookTx(ctx, tx, owner, account, "carddav", book.ID); err != nil {
			return err
		}
		var configured int
		if book.ID == "" {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_contact_sync_configs WHERE user_id=? AND account_id=? AND addressbook_url=?)`, owner, account, book.URL).Scan(&configured); err != nil {
				return err
			}
		} else if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_contact_address_books WHERE user_id=? AND account_id=? AND id=? AND url=?)`, owner, account, book.ID, book.URL).Scan(&configured); err != nil {
			return err
		}
		if configured != 1 {
			return ErrContactPublication
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,remote_id,profile_id,address_book_id FROM contact_cards WHERE user_id=? AND account_id=? AND provider='carddav' AND kind='provider'
 AND (address_book_id=? OR address_book_id='')`, owner, account, book.ID)
		if err != nil {
			return err
		}
		var remove []string
		var lost []removedContactCard
		for rows.Next() {
			var id, remote, profile, book string
			if err := rows.Scan(&id, &remote, &profile, &book); err != nil {
				rows.Close()
				return err
			}
			if inScope(remote) && (deleted[remote] || (page.Full && !seen[remote])) {
				remove = append(remove, id)
				lost = append(lost, removedContactCard{profile: profile, account: account, book: book})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range remove {
			if _, err := tx.ExecContext(ctx, `DELETE FROM contact_cards WHERE id=? AND user_id=? AND account_id=?`, id, owner, account); err != nil {
				return err
			}
		}
		if _, unresolved, err = contactsLostCopiesTx(ctx, tx, owner, lost); err != nil {
			return err
		}
		if book.ID == "" {
			_, err = tx.ExecContext(ctx, `UPDATE account_contact_sync_configs SET last_sync_token=?,last_success_at=CURRENT_TIMESTAMP,last_error='',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND account_id=?`, strings.TrimSpace(page.SyncToken), owner, account)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE account_contact_address_books SET last_sync_token=?,last_success_at=CURRENT_TIMESTAMP,last_error='',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND account_id=? AND id=?`, strings.TrimSpace(page.SyncToken), owner, account, book.ID)
		}
		if err != nil {
			return err
		}
		return checkpoint(tx)
	})
	if err == nil {
		db.logContactsUnresolved(ctx, owner, unresolved)
	}
	return results, err
}

// InboundJMAPContactPage is one JMAP contacts pull. Each contact's
// AddressBookID is the Gofer id of its synced book. Full means the whole
// account was listed, so cards not in SeenRemoteIDs are gone; SyncToken is the
// JMAP state the next pull starts from.
type InboundJMAPContactPage struct {
	Contacts                        []InboundContact
	DeletedRemoteIDs, SeenRemoteIDs []string
	Full                            bool
	SyncToken                       string
}

// PublishInboundJMAPContacts commits a JMAP pull like PublishInboundContactBook
// does a DAV book: imports, removals and the account's JMAP state together.
func (db *DB) PublishInboundJMAPContacts(ctx context.Context, owner, account string, page InboundJMAPContactPage, guard, checkpoint func(*sql.Tx) error) ([]InboundContactResult, error) {
	if checkpoint == nil || strings.TrimSpace(page.SyncToken) == "" {
		return nil, ErrContactPublication
	}
	deleted := make(map[string]bool, len(page.DeletedRemoteIDs))
	for _, remote := range page.DeletedRemoteIDs {
		deleted[strings.TrimSpace(remote)] = true
	}
	seen := make(map[string]bool, len(page.SeenRemoteIDs)+len(page.Contacts))
	for _, remote := range page.SeenRemoteIDs {
		seen[strings.TrimSpace(remote)] = true
	}
	for _, input := range page.Contacts {
		remote := strings.TrimSpace(input.RemoteID)
		if remote == "" || deleted[remote] {
			return nil, ErrContactPublication
		}
		seen[remote] = true
	}
	var unresolved []string
	results, err := db.publishInboundContacts(ctx, owner, account, "jmap", page.Contacts, guard, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,remote_id,profile_id,address_book_id FROM contact_cards WHERE user_id=? AND account_id=? AND provider='jmap' AND kind='provider'`, owner, account)
		if err != nil {
			return err
		}
		var remove []string
		var lost []removedContactCard
		for rows.Next() {
			var id, remote, profile, book string
			if err := rows.Scan(&id, &remote, &profile, &book); err != nil {
				rows.Close()
				return err
			}
			if deleted[remote] || (page.Full && !seen[remote]) {
				remove = append(remove, id)
				lost = append(lost, removedContactCard{profile: profile, account: account, book: book})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range remove {
			if _, err := tx.ExecContext(ctx, `DELETE FROM contact_cards WHERE id=? AND user_id=? AND account_id=?`, id, owner, account); err != nil {
				return err
			}
		}
		if _, unresolved, err = contactsLostCopiesTx(ctx, tx, owner, lost); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE account_contact_sync_configs SET last_sync_token=?,last_success_at=CURRENT_TIMESTAMP,last_error='',updated_at=CURRENT_TIMESTAMP
 WHERE user_id=? AND account_id=? AND provider='jmap' AND enabled=1`, strings.TrimSpace(page.SyncToken), owner, account)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.Join(err, ErrContactPublication)
		}
		return checkpoint(tx)
	})
	if err == nil {
		db.logContactsUnresolved(ctx, owner, unresolved)
	}
	return results, err
}

func inboundContactBookTx(ctx context.Context, tx *sql.Tx, owner, account, provider, book string) error {
	if provider != "carddav" && provider != "jmap" {
		if book != "" {
			return ErrContactPublication
		}
		return nil
	}
	var valid int
	// Old single-book CardDAV configurations have no address-book row or
	// stable ID; JMAP ones always do.
	if book == "" && provider == "jmap" {
		return ErrContactPublication
	}
	if book == "" {
		if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM account_contact_address_books WHERE user_id=? AND account_id=?)`, owner, account).Scan(&valid); err != nil {
			return err
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_contact_address_books WHERE user_id=? AND account_id=? AND id=?)`, owner, account, book).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return ErrContactPublication
	}
	return nil
}

func inboundContactProfileTx(ctx context.Context, tx *sql.Tx, owner, account, provider, remote string) (string, error) {
	if remote == "" {
		return "", nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT profile_id FROM contact_cards
 WHERE user_id=? AND account_id=? AND provider=? AND remote_id=? AND kind='provider' AND is_deleted=0 LIMIT 2`, owner, account, provider, remote)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var preferred string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		if preferred != "" {
			return "", ErrContactPublication
		}
		preferred = id
	}
	if err := rows.Err(); err != nil || preferred != "" {
		return preferred, err
	}
	return contactOfRemovedCardTx(ctx, tx, owner, account, provider, remote)
}

// contactOfRemovedCardTx gives the contact a removed copy with this remote
// ID belonged to: a copy that comes back is the same contact, not a new one
// that happens to share its address.
func contactOfRemovedCardTx(ctx context.Context, query contactProfileQuery, owner, account, provider, remote string) (string, error) {
	var id string
	err := query.QueryRowContext(ctx, `SELECT c.profile_id FROM contact_cards c
 JOIN contact_profiles p ON p.user_id = c.user_id AND p.id = c.profile_id AND p.is_deleted = 0
 WHERE c.user_id=? AND c.account_id=? AND c.provider=? AND c.remote_id=? AND c.kind='provider' AND c.is_deleted=1
 ORDER BY c.updated_at DESC, c.id LIMIT 1`, owner, account, provider, remote).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func inboundContactFanoutTx(ctx context.Context, tx *sql.Tx, owner, profile, excluded string) (bool, error) {
	var needed int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM contact_sync_memberships m JOIN accounts a ON a.id=m.account_id AND a.user_id=m.user_id
 LEFT JOIN account_contact_sync_configs c ON c.account_id=a.id AND c.user_id=a.user_id
 WHERE m.user_id=? AND m.profile_id=? AND m.enabled=1 AND a.id!=? AND COALESCE(a.is_deleting,0)=0
 AND ((a.provider IN ('gmail','outlook') AND COALESCE(c.enabled,1)=1)
 OR (a.provider NOT IN ('gmail','outlook') AND c.provider IN ('carddav','jmap') AND c.enabled=1))
 AND (m.address_book_id='' OR EXISTS(SELECT 1 FROM account_contact_address_books b
 WHERE b.id=m.address_book_id AND b.account_id=a.id AND b.user_id=m.user_id)))`, owner, profile, excluded).Scan(&needed)
	return needed == 1, err
}

// ContactOfRemovedCard gives the contact a removed copy with this remote ID
// belonged to, or "".
func (db *DB) ContactOfRemovedCard(ctx context.Context, owner, provider, account, remote string) (string, error) {
	if strings.TrimSpace(remote) == "" {
		return "", nil
	}
	return contactOfRemovedCardTx(ctx, db.Read(), owner, account, provider, remote)
}

// InboundContactChangePage is one page of the changes Gmail or Outlook
// report for an account's contacts since its last sync: contacts added or
// changed, and the remote IDs of ones deleted. SyncToken, on the last page
// only, is where the next sync starts from: a Google sync token or an
// Outlook delta link.
type InboundContactChangePage struct {
	Contacts         []InboundContact
	DeletedRemoteIDs []string
	SyncToken        string
}

// PublishInboundContactChanges commits a page of Gmail or Outlook changes
// like PublishInboundContacts, settling the contacts deleted there as a
// CardDAV pull does, and keeps the page's sync token with them.
func (db *DB) PublishInboundContactChanges(ctx context.Context, owner, account, provider string, page InboundContactChangePage, guard, checkpoint func(*sql.Tx) error) ([]InboundContactResult, error) {
	if provider != "gmail" && provider != "outlook" {
		return nil, ErrContactPublication
	}
	deleted := make(map[string]bool, len(page.DeletedRemoteIDs))
	for _, remote := range page.DeletedRemoteIDs {
		if remote = strings.TrimSpace(remote); remote != "" {
			deleted[remote] = true
		}
	}
	for _, input := range page.Contacts {
		if deleted[strings.TrimSpace(input.RemoteID)] {
			return nil, ErrContactPublication
		}
	}
	var unresolved []string
	results, err := db.publishInboundContacts(ctx, owner, account, provider, page.Contacts, guard, func(tx *sql.Tx) error {
		trashed, waiting, err := contactsDeletedOnAccountTx(ctx, tx, owner, account, provider, deleted)
		if err != nil {
			return err
		}
		unresolved = waiting
		if trashed > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO contact_activity_events(user_id,event_type,email,message,event_count) VALUES(?,?,?,?,?)`, owner, "contacts_trashed", "", "Contacts moved to Trash", trashed); err != nil {
				return err
			}
		}
		if token := strings.TrimSpace(page.SyncToken); token != "" {
			if err := saveContactSyncTokenTx(ctx, tx, owner, account, provider, token); err != nil {
				return err
			}
		}
		if checkpoint != nil {
			return checkpoint(tx)
		}
		return nil
	})
	if err == nil {
		db.logContactsUnresolved(ctx, owner, unresolved)
	}
	return results, err
}

// contactsDeletedOnAccountTx forgets the account's copies with these remote
// IDs and settles their contacts: to the Trash, or to be resolved.
func contactsDeletedOnAccountTx(ctx context.Context, tx *sql.Tx, owner, account, provider string, deleted map[string]bool) (int, []string, error) {
	if len(deleted) == 0 {
		return 0, nil, nil
	}
	var lost []removedContactCard
	for remote := range deleted {
		rows, err := tx.QueryContext(ctx, `SELECT id, profile_id, address_book_id FROM contact_cards WHERE user_id=? AND account_id=? AND provider=? AND kind='provider' AND remote_id=?`, owner, account, provider, remote)
		if err != nil {
			return 0, nil, err
		}
		var ids []string
		for rows.Next() {
			var id string
			card := removedContactCard{account: account}
			if err := rows.Scan(&id, &card.profile, &card.book); err != nil {
				rows.Close()
				return 0, nil, err
			}
			ids = append(ids, id)
			lost = append(lost, card)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, nil, err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM contact_cards WHERE id=? AND user_id=? AND account_id=?`, id, owner, account); err != nil {
				return 0, nil, err
			}
		}
	}
	return contactsLostCopiesTx(ctx, tx, owner, lost)
}

// saveContactSyncTokenTx keeps where an account's next contacts sync
// starts from.
func saveContactSyncTokenTx(ctx context.Context, tx *sql.Tx, owner, account, provider, token string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled, last_sync_token)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(account_id) DO UPDATE SET last_sync_token = excluded.last_sync_token, updated_at = CURRENT_TIMESTAMP
		WHERE account_contact_sync_configs.user_id = excluded.user_id`, account, owner, provider, token)
	return err
}

// SaveContactSyncToken keeps where an account's next contacts sync starts
// from, for the shared store's Gmail and Outlook pulls.
func (db *DB) SaveContactSyncToken(ctx context.Context, owner, account, provider, token string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveContactSyncTokenTx(ctx, tx, owner, account, provider, strings.TrimSpace(token)); err != nil {
		return err
	}
	return tx.Commit()
}

// ContactSyncToken gives where an account's next contacts sync starts from,
// for the shared store's Gmail and Outlook pulls.
func (db *DB) ContactSyncToken(ctx context.Context, owner, account string) (string, error) {
	var token string
	err := db.Read().QueryRowContext(ctx, `SELECT last_sync_token FROM account_contact_sync_configs WHERE account_id=? AND user_id=?`, account, owner).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return token, err
}
