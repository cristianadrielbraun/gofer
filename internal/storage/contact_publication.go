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
	if provider != "gmail" && provider != "outlook" && provider != "carddav" {
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
 OR (a.provider NOT IN ('gmail','outlook') AND ?='carddav' AND c.provider='carddav' AND c.enabled=1)))`, account, owner, provider, provider).Scan(&valid); err != nil {
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
	return db.publishInboundContacts(ctx, owner, account, "carddav", inputs, guard, func(tx *sql.Tx) error {
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
		rows, err := tx.QueryContext(ctx, `SELECT id,remote_id FROM contact_cards WHERE user_id=? AND account_id=? AND provider='carddav' AND kind='provider'
 AND (address_book_id=? OR address_book_id='')`, owner, account, book.ID)
		if err != nil {
			return err
		}
		var remove []string
		for rows.Next() {
			var id, remote string
			if err := rows.Scan(&id, &remote); err != nil {
				rows.Close()
				return err
			}
			if inScope(remote) && (deleted[remote] || (page.Full && !seen[remote])) {
				remove = append(remove, id)
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
}

func inboundContactBookTx(ctx context.Context, tx *sql.Tx, owner, account, provider, book string) error {
	if provider != "carddav" {
		if book != "" {
			return ErrContactPublication
		}
		return nil
	}
	var valid int
	// Old single-book configurations have no address-book row or stable ID.
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
	return preferred, rows.Err()
}

func inboundContactFanoutTx(ctx context.Context, tx *sql.Tx, owner, profile, excluded string) (bool, error) {
	var needed int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM contact_sync_memberships m JOIN accounts a ON a.id=m.account_id AND a.user_id=m.user_id
 LEFT JOIN account_contact_sync_configs c ON c.account_id=a.id AND c.user_id=a.user_id
 WHERE m.user_id=? AND m.profile_id=? AND m.enabled=1 AND a.id!=? AND COALESCE(a.is_deleting,0)=0
 AND ((a.provider IN ('gmail','outlook') AND COALESCE(c.enabled,1)=1)
 OR (a.provider NOT IN ('gmail','outlook') AND c.provider='carddav' AND c.enabled=1))
 AND (m.address_book_id='' OR EXISTS(SELECT 1 FROM account_contact_address_books b
 WHERE b.id=m.address_book_id AND b.account_id=a.id AND b.user_id=m.user_id)))`, owner, profile, excluded).Scan(&needed)
	return needed == 1, err
}
