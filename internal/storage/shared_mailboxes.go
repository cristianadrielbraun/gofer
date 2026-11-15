package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
	"github.com/google/uuid"
)

// sharedIdentityColumns read, for an identity row "i", what
// shared_mailbox_identities says of it (models.Identity.SharedMailbox and
// the fields after it), empty for any other identity.
const sharedIdentityColumns = `COALESCE((SELECT s.mailbox_email FROM shared_mailbox_identities s WHERE s.identity_id = i.id AND s.state != 'deleted'), ''),
	COALESCE((SELECT CASE WHEN EXISTS (SELECT 1 FROM shared_mailboxes m WHERE m.account_id = s.account_id AND m.email = s.mailbox_email AND m.enabled = 0) THEN '` + SharedIdentityMailboxOff + `' ELSE s.state END
		FROM shared_mailbox_identities s WHERE s.identity_id = i.id AND s.state != 'deleted'), ''),
	COALESCE((SELECT s.sender_check FROM shared_mailbox_identities s WHERE s.identity_id = i.id AND s.state != 'deleted'), ''),
	COALESCE((SELECT s.sender_check_message FROM shared_mailbox_identities s WHERE s.identity_id = i.id AND s.state != 'deleted'), '')`

// Shared mailbox states.
const (
	SharedMailboxActive  = "active"
	SharedMailboxRemoved = "removed"
)

// Shared mailbox identity states.
const (
	SharedIdentityActive   = "active"
	SharedIdentityInactive = "inactive" // its mailbox is no longer shared
	SharedIdentityDisabled = "disabled" // the user turned it off
	SharedIdentityDeleted  = "deleted"  // the user deleted the identity
	// SharedIdentityMailboxOff is how an identity reads, never how it is
	// stored, while the user has its shared mailbox turned off.
	SharedIdentityMailboxOff = "mailbox_off"
)

// Sender check results.
const (
	SenderCheckAccepted = "accepted"
	SenderCheckRejected = "rejected"
	SenderCheckUnknown  = "unknown"
)

// SharedMailbox is a mailbox an account can open besides its own.
type SharedMailbox struct {
	AccountID string
	Email     string
	State     string
	// Enabled is false while the user has the mailbox turned off.
	Enabled bool
	// ServerName is the server's name for the mailbox, CustomName the
	// user's; Name what Gofer calls it (sharedmailbox.MailboxName).
	ServerName   string
	CustomName   string
	Name         string
	DiscoveredAt time.Time
	LastSeenAt   time.Time
}

// SharedMailboxChanges says what a folder sync changed in an account's
// shared mailboxes, each list sorted.
type SharedMailboxChanges struct {
	Added    []string
	Restored []string
	Removed  []string
}

// Changed reports whether anything changed.
func (c SharedMailboxChanges) Changed() bool {
	return len(c.Added)+len(c.Restored)+len(c.Removed) > 0
}

// SyncSharedMailboxes records the shared mailboxes a folder listing showed.
// complete says the listing can be trusted to show every shared mailbox (the
// server answered NAMESPACE and LIST in full); only then is a mailbox it left
// out marked removed. A removed mailbox listed again is active again.
func (db *DB) SyncSharedMailboxes(ctx context.Context, accountID string, seen []string, complete bool, at time.Time) (SharedMailboxChanges, error) {
	var changes SharedMailboxChanges
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return changes, err
	}
	defer tx.Rollback()

	current := map[string]string{}
	// Rows recorded ahead of discovery (PresetSharedMailboxes, server
	// names) have not been seen yet; found now, they are added.
	pending := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT email, state, last_seen_at = ? FROM shared_mailboxes WHERE account_id = ?`, sharedMailboxNotYetSeen, accountID)
	if err != nil {
		return changes, fmt.Errorf("read shared mailboxes: %w", err)
	}
	for rows.Next() {
		var email, state string
		var notYetSeen bool
		if err := rows.Scan(&email, &state, &notYetSeen); err != nil {
			rows.Close()
			return changes, err
		}
		current[email] = state
		pending[email] = notYetSeen
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return changes, err
	}

	at = at.UTC()
	listed := map[string]bool{}
	for _, email := range seen {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" || listed[email] {
			continue
		}
		listed[email] = true
		state, known := current[email]
		switch {
		case !known || pending[email]:
			changes.Added = append(changes.Added, email)
		case state == SharedMailboxRemoved:
			changes.Restored = append(changes.Restored, email)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shared_mailboxes (account_id, email, state, discovered_at, last_seen_at)
			VALUES (?, ?, 'active', ?, ?)
			ON CONFLICT(account_id, email) DO UPDATE SET state = 'active', last_seen_at = excluded.last_seen_at, removed_at = NULL,
				discovered_at = CASE WHEN shared_mailboxes.last_seen_at = ? THEN excluded.discovered_at ELSE shared_mailboxes.discovered_at END`,
			accountID, email, at, at, sharedMailboxNotYetSeen); err != nil {
			return changes, fmt.Errorf("save shared mailbox %s: %w", email, err)
		}
	}
	if complete {
		for email, state := range current {
			if listed[email] || state == SharedMailboxRemoved || pending[email] {
				continue
			}
			changes.Removed = append(changes.Removed, email)
			if _, err := tx.ExecContext(ctx, `UPDATE shared_mailboxes SET state = 'removed', removed_at = ? WHERE account_id = ? AND email = ?`, at, accountID, email); err != nil {
				return changes, fmt.Errorf("remove shared mailbox %s: %w", email, err)
			}
		}
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Restored)
	sort.Strings(changes.Removed)
	return changes, tx.Commit()
}

// ListSharedMailboxes returns an account's shared mailboxes, active and
// removed, by address.
func (db *DB) ListSharedMailboxes(ctx context.Context, accountID string) ([]SharedMailbox, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT account_id, email, state, enabled, server_name, custom_name, discovered_at, last_seen_at
		FROM shared_mailboxes WHERE account_id = ? ORDER BY email`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedMailbox
	for rows.Next() {
		var m SharedMailbox
		if err := rows.Scan(&m.AccountID, &m.Email, &m.State, &m.Enabled, &m.ServerName, &m.CustomName, &m.DiscoveredAt, &m.LastSeenAt); err != nil {
			return nil, err
		}
		m.Name = sharedmailbox.MailboxName(m.Email, m.ServerName, m.CustomName)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SharedMailboxFolder returns the folder that has role inside an account's
// shared mailbox, such as its Sent folder, found by the role the server gave
// it (SPECIAL-USE) or its name, never by a fixed name. ok is false when the
// mailbox has no such folder.
func (db *DB) SharedMailboxFolder(ctx context.Context, accountID, mailbox, role string) (folderID, remoteID string, ok bool, err error) {
	err = db.Read().QueryRowContext(ctx, `
		SELECT id, COALESCE(remote_id, '') FROM folders
		WHERE account_id = ? AND shared_mailbox = ? AND shared_role = ?
		  AND COALESCE(discovery_state, 'active') = 'active' AND COALESCE(selectable, 1) = 1
		ORDER BY sort_order, id LIMIT 1`, accountID, strings.ToLower(strings.TrimSpace(mailbox)), role).Scan(&folderID, &remoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return folderID, remoteID, true, nil
}

// FolderSharedMailbox returns the shared mailbox a folder is in, "" for one
// of the account's own folders.
func (db *DB) FolderSharedMailbox(ctx context.Context, folderID string) (string, error) {
	var mailbox string
	err := db.Read().QueryRowContext(ctx, `SELECT shared_mailbox FROM folders WHERE id = ?`, folderID).Scan(&mailbox)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return mailbox, err
}

// SharedIdentityOptions are what SyncSharedMailboxIdentities follows.
type SharedIdentityOptions struct {
	// Create makes an identity for a shared mailbox that has none.
	Create bool
	// DisableRemoved puts aside the identity of a removed mailbox.
	DisableRemoved bool
	// Name gives a new identity's name.
	Name func(address string) string
}

// SharedIdentityChanges says what SyncSharedMailboxIdentities changed, by
// mailbox address.
type SharedIdentityChanges struct {
	Created     []string
	Renamed     []string
	Reactivated []string
	Deactivated []string
	// Unchecked are the created or reactivated identities the server has
	// not yet been asked to accept as senders.
	Unchecked []SharedMailboxIdentity
}

// Changed reports whether anything changed.
func (c SharedIdentityChanges) Changed() bool {
	return len(c.Created)+len(c.Renamed)+len(c.Reactivated)+len(c.Deactivated) > 0
}

// SharedMailboxIdentity is the identity Gofer made for a shared mailbox.
type SharedMailboxIdentity struct {
	IdentityID         string
	AccountID          string
	MailboxEmail       string
	GeneratedName      string
	State              string
	SenderCheck        string
	SenderCheckMessage string
}

// SyncSharedMailboxIdentities brings an account's sender identities in line
// with its shared mailboxes. It runs any number of times with the same
// result: a mailbox gets one identity at most. An identity the user made
// for the address is left alone and none is made beside it; a name the user
// changed is kept; an identity the user deleted is not made again; and an
// identity is never deleted, only put aside while its mailbox is removed.
func (db *DB) SyncSharedMailboxIdentities(ctx context.Context, accountID string, opts SharedIdentityOptions) (SharedIdentityChanges, error) {
	var changes SharedIdentityChanges
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return changes, err
	}
	defer tx.Rollback()

	mailboxes := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT email, state FROM shared_mailboxes WHERE account_id = ?`, accountID)
	if err != nil {
		return changes, err
	}
	for rows.Next() {
		var email, state string
		if err := rows.Scan(&email, &state); err != nil {
			rows.Close()
			return changes, err
		}
		mailboxes[email] = state
	}
	rows.Close()

	links, err := querySharedMailboxIdentities(ctx, tx, `WHERE s.account_id = ?`, accountID)
	if err != nil {
		return changes, err
	}
	linked := map[string]SharedMailboxIdentity{}
	for _, link := range links {
		linked[link.MailboxEmail] = link
	}

	identities, err := queryAccountIdentities(ctx, tx, `SELECT `+accountIdentityColumns+` FROM account_identities i WHERE i.account_id = ?`, accountID)
	if err != nil {
		return changes, err
	}
	byID := map[string]models.Identity{}
	byEmail := map[string]models.Identity{}
	maxOrder := -1
	// Identities the provider reports, as a JMAP server lists the shared
	// mailboxes' addresses for the login, give way to the shared mailbox's
	// own: those send from the mailbox and keep its Sent and Drafts.
	providerByEmail := map[string][]string{}
	for _, identity := range identities {
		byID[identity.ID] = identity
		if identity.Source == models.IdentitySourceProvider {
			key := strings.ToLower(identity.Email)
			providerByEmail[key] = append(providerByEmail[key], identity.ID)
		} else if _, taken := byEmail[strings.ToLower(identity.Email)]; !taken {
			byEmail[strings.ToLower(identity.Email)] = identity
		}
		maxOrder = max(maxOrder, identity.SortOrder)
	}

	setState := func(link SharedMailboxIdentity, state string) error {
		_, err := tx.ExecContext(ctx, `UPDATE shared_mailbox_identities SET state = ?, updated_at = CURRENT_TIMESTAMP WHERE identity_id = ?`, state, link.IdentityID)
		return err
	}

	emails := make([]string, 0, len(mailboxes))
	for email := range mailboxes {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	for _, email := range emails {
		state := mailboxes[email]
		link, hasLink := linked[email]
		identity, identityExists := byID[link.IdentityID]
		if hasLink && link.State != SharedIdentityDeleted && !identityExists {
			// The user deleted the identity: it stays deleted.
			if err := setState(link, SharedIdentityDeleted); err != nil {
				return changes, err
			}
			continue
		}
		if hasLink && (link.State == SharedIdentityDeleted || link.State == SharedIdentityDisabled) {
			continue
		}

		if state == SharedMailboxRemoved {
			if hasLink && link.State == SharedIdentityActive && opts.DisableRemoved {
				if err := setState(link, SharedIdentityInactive); err != nil {
					return changes, err
				}
				changes.Deactivated = append(changes.Deactivated, email)
			}
			continue
		}

		if hasLink {
			if link.State == SharedIdentityInactive {
				if err := setState(link, SharedIdentityActive); err != nil {
					return changes, err
				}
				changes.Reactivated = append(changes.Reactivated, email)
				link.State = SharedIdentityActive
				if link.SenderCheck != SenderCheckAccepted {
					changes.Unchecked = append(changes.Unchecked, link)
				}
			}
			// The name follows the naming rules only while the user has
			// not changed it.
			if opts.Name != nil && identity.Name == link.GeneratedName {
				if name := opts.Name(email); name != "" && name != identity.Name {
					if _, err := tx.ExecContext(ctx, `UPDATE account_identities SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, name, identity.ID); err != nil {
						return changes, err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE shared_mailbox_identities SET generated_name = ?, updated_at = CURRENT_TIMESTAMP WHERE identity_id = ?`, name, identity.ID); err != nil {
						return changes, err
					}
					changes.Renamed = append(changes.Renamed, email)
				}
			}
			continue
		}

		if !opts.Create || !strings.Contains(email, "@") {
			// A mailbox the server names without an address (a user name
			// in its "other users" namespace) gives no address to send as.
			continue
		}
		if _, exists := byEmail[email]; exists {
			// The user already has an identity for this address.
			continue
		}
		name := email
		if opts.Name != nil {
			if generated := opts.Name(email); generated != "" {
				name = generated
			}
		}
		id := uuid.NewString()
		maxOrder++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_identities (id, account_id, name, email, source, sort_order)
			VALUES (?, ?, ?, ?, 'manual', ?)`, id, accountID, name, email, maxOrder); err != nil {
			return changes, fmt.Errorf("create identity for %s: %w", email, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shared_mailbox_identities (identity_id, account_id, mailbox_email, generated_name, state)
			VALUES (?, ?, ?, ?, 'active')`, id, accountID, email, name); err != nil {
			return changes, fmt.Errorf("record identity for %s: %w", email, err)
		}
		for _, providerID := range providerByEmail[email] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM account_identities WHERE id = ? AND source = 'provider'`, providerID); err != nil {
				return changes, fmt.Errorf("replace the provider identity for %s: %w", email, err)
			}
		}
		changes.Created = append(changes.Created, email)
		changes.Unchecked = append(changes.Unchecked, SharedMailboxIdentity{IdentityID: id, AccountID: accountID, MailboxEmail: email, GeneratedName: name, State: SharedIdentityActive})
	}
	return changes, tx.Commit()
}

func querySharedMailboxIdentities(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, where string, args ...any) ([]SharedMailboxIdentity, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.identity_id, s.account_id, s.mailbox_email, s.generated_name, s.state, s.sender_check, s.sender_check_message
		FROM shared_mailbox_identities s `+where+` ORDER BY s.mailbox_email`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedMailboxIdentity
	for rows.Next() {
		var s SharedMailboxIdentity
		if err := rows.Scan(&s.IdentityID, &s.AccountID, &s.MailboxEmail, &s.GeneratedName, &s.State, &s.SenderCheck, &s.SenderCheckMessage); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSharedMailboxIdentities returns the identities Gofer made for an
// account's shared mailboxes.
func (db *DB) ListSharedMailboxIdentities(ctx context.Context, accountID string) ([]SharedMailboxIdentity, error) {
	return querySharedMailboxIdentities(ctx, db.Read(), `WHERE s.account_id = ?`, accountID)
}

// ListUserSharedMailboxIdentities returns the identities Gofer made for the
// shared mailboxes of all the owner's accounts.
func (db *DB) ListUserSharedMailboxIdentities(ctx context.Context, userID string) ([]SharedMailboxIdentity, error) {
	return querySharedMailboxIdentities(ctx, db.Read(), `JOIN accounts a ON a.id = s.account_id WHERE a.user_id = ?`, userID)
}

// SetSharedIdentitySenderCheck records whether the server accepted a shared
// mailbox identity's address as a sender, and what it said.
func (db *DB) SetSharedIdentitySenderCheck(ctx context.Context, identityID, result, message string, at time.Time) error {
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE shared_mailbox_identities
		SET sender_check = ?, sender_check_message = ?, sender_checked_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE identity_id = ?`, result, message, at.UTC(), identityID)
	return err
}

// SetSharedIdentityActive turns a shared mailbox identity on or off by
// hand, for one of the owner's accounts. Turned off, it stays off whatever
// the folder sync finds.
func (db *DB) SetSharedIdentityActive(ctx context.Context, userID, accountID, identityID string, active bool) error {
	state := SharedIdentityDisabled
	if active {
		state = SharedIdentityActive
	}
	result, err := db.Write().ExecContext(ctx, `
		UPDATE shared_mailbox_identities SET state = ?, updated_at = CURRENT_TIMESTAMP
		WHERE identity_id = ? AND account_id = ? AND state != 'deleted'
		  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`, state, identityID, accountID, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SharedMailboxSettings returns the shared mailbox choices of the owner of
// an account, kept with the user's other interface settings.
func (db *DB) SharedMailboxSettings(ctx context.Context, accountID string) (sharedmailbox.Settings, error) {
	var userID string
	err := db.Read().QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id = ?`, accountID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return sharedmailbox.DefaultSettings(), nil
	}
	if err != nil {
		return sharedmailbox.DefaultSettings(), err
	}
	return sharedmailbox.SettingsFrom(db.getStoredUISettings(ctx, userID)), nil
}

// ApplySharedMailboxDiscovery records the shared mailboxes a folder sync
// found (SyncSharedMailboxes) and then brings the account's sender
// identities in line with them under the owner's settings
// (SyncSharedMailboxIdentities).
func (db *DB) ApplySharedMailboxDiscovery(ctx context.Context, accountID string, seen []string, complete bool, at time.Time) (SharedMailboxChanges, SharedIdentityChanges, error) {
	mailboxes, err := db.SyncSharedMailboxes(ctx, accountID, seen, complete, at)
	if err != nil {
		return mailboxes, SharedIdentityChanges{}, err
	}
	settings, err := db.SharedMailboxSettings(ctx, accountID)
	if err != nil {
		return mailboxes, SharedIdentityChanges{}, err
	}
	serverNames := map[string]string{}
	if known, err := db.ListSharedMailboxes(ctx, accountID); err == nil {
		for _, m := range known {
			serverNames[m.Email] = m.ServerName
		}
	}
	identities, err := db.SyncSharedMailboxIdentities(ctx, accountID, SharedIdentityOptions{
		Create:         settings.AutoIdentities,
		DisableRemoved: settings.DisableRemoved,
		Name:           func(address string) string { return settings.IdentityNameFrom(address, serverNames[address]) },
	})
	if !settings.VerifySender {
		identities.Unchecked = nil
	}
	if err == nil {
		// Mailboxes or none, the account was looked at. (A partial listing
		// only keeps mailboxes from being marked removed.)
		err = db.MarkLabelSyncSuccess(ctx, accountID, sharedMailboxCheckProvider, sharedMailboxCheckScope, at.UTC().Format(time.RFC3339Nano), true)
	}
	return mailboxes, identities, err
}

// Where SharedMailboxesCheckedAt keeps the time of an account's last full
// look for shared mailboxes.
const (
	sharedMailboxCheckProvider = "shared_mailboxes"
	sharedMailboxCheckScope    = "checked"
)

// SharedMailboxesCheckedAt returns when a sync last fully answered which
// shared mailboxes an account can open; ok is false when none has yet, as
// for an account just added.
func (db *DB) SharedMailboxesCheckedAt(ctx context.Context, accountID string) (time.Time, bool, error) {
	state, err := db.GetLabelSyncState(ctx, accountID, sharedMailboxCheckProvider, sharedMailboxCheckScope)
	if err != nil || state.Cursor == "" {
		return time.Time{}, false, err
	}
	at, err := time.Parse(time.RFC3339Nano, state.Cursor)
	if err != nil {
		return time.Time{}, false, nil
	}
	return at, true, nil
}

// SentFolderFor returns the folder a message sent from address is kept in:
// the Sent folder of the account's shared mailbox at that address while the
// mailbox is shared with the account, so that the mailbox's other members
// see it, or else the account's own Sent folder (GetFolderIDByRole).
func (db *DB) SentFolderFor(ctx context.Context, accountID, address string) (folderID, remoteID string, err error) {
	return db.sharedOrOwnFolder(ctx, accountID, address, "sent")
}

// DraftsFolderFor is SentFolderFor for drafts.
func (db *DB) DraftsFolderFor(ctx context.Context, accountID, address string) (folderID, remoteID string, err error) {
	return db.sharedOrOwnFolder(ctx, accountID, address, "drafts")
}

func (db *DB) sharedOrOwnFolder(ctx context.Context, accountID, address, role string) (string, string, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address != "" {
		var active int
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM shared_mailboxes WHERE account_id = ? AND email = ? AND state = 'active'`, accountID, address).Scan(&active); err != nil {
			return "", "", err
		}
		if active > 0 {
			folderID, remoteID, ok, err := db.SharedMailboxFolder(ctx, accountID, address, role)
			if err != nil {
				return "", "", err
			}
			if ok {
				return folderID, remoteID, nil
			}
		}
	}
	return db.GetFolderIDByRole(ctx, accountID, role)
}

// RemoveDraftFromFolder takes a draft out of a Drafts folder it was moved
// from, as when its From changes from a shared mailbox to the account's own
// address; the message stays in the folder it was saved to.
func (db *DB) RemoveDraftFromFolder(ctx context.Context, messageID int64, folderID string) error {
	if _, err := db.Write().ExecContext(ctx, `DELETE FROM message_folder_state WHERE message_id = ? AND folder_id = ? AND is_draft = 1`, messageID, folderID); err != nil {
		return err
	}
	db.RefreshFolderUnreadCount(ctx, folderID)
	return nil
}

// RecordSharedSenderResult records what the server said when the account
// sent mail from address, if that is one of its shared mailboxes' identities:
// accepted, or refused with message. Nothing else is changed, and an identity
// the server refuses is kept.
func (db *DB) RecordSharedSenderResult(ctx context.Context, accountID, address string, accepted bool, message string) error {
	result := SenderCheckRejected
	if accepted {
		result, message = SenderCheckAccepted, ""
	}
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE shared_mailbox_identities
		SET sender_check = ?, sender_check_message = ?, sender_checked_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE account_id = ? AND mailbox_email = ? AND state != 'deleted'`,
		result, message, accountID, strings.ToLower(strings.TrimSpace(address)))
	return err
}

// effectiveFolderRoleSQL is a folder row's role as it acts: a shared
// mailbox's folder has its role inside that mailbox, so that Trash there is
// treated as Trash, while its role column stays "custom".
func effectiveFolderRoleSQL(alias string) string {
	return `CASE WHEN ` + alias + `.shared_role IN ('inbox', 'sent', 'drafts', 'junk', 'trash', 'archive') THEN ` + alias + `.shared_role ELSE ` + alias + `.role END`
}

// FolderIDByRoleNear returns the folder with role that goes with nearFolderID:
// in the shared mailbox nearFolderID is in, the mailbox's own folder, such as
// its Trash for a message deleted there, and otherwise, or when that mailbox
// has none, the account's (GetFolderIDByRole).
func (db *DB) FolderIDByRoleNear(ctx context.Context, accountID, nearFolderID, role string) (string, string, error) {
	if nearFolderID = strings.TrimSpace(nearFolderID); nearFolderID != "" {
		var mailbox string
		err := db.Read().QueryRowContext(ctx, `SELECT shared_mailbox FROM folders WHERE id = ? AND account_id = ?`, nearFolderID, accountID).Scan(&mailbox)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		if mailbox != "" {
			folderID, remoteID, ok, err := db.SharedMailboxFolder(ctx, accountID, mailbox, role)
			if err != nil {
				return "", "", err
			}
			if ok {
				return folderID, remoteID, nil
			}
		}
	}
	return db.GetFolderIDByRole(ctx, accountID, role)
}

// sharedMailboxNotYetSeen marks a shared mailbox recorded ahead of the
// discovery that finds it, as its server name or the user's choice is.
var sharedMailboxNotYetSeen = time.Unix(0, 0).UTC()

// ErrSharedMailboxNotFound is a shared mailbox the account cannot open.
var ErrSharedMailboxNotFound = errors.New("shared mailbox not found")

// SetSharedMailboxEnabled turns one of an account's shared mailboxes on or
// off for the user. One turned off keeps its folders and identity, hidden,
// so turning it back on brings them back as they were.
func (db *DB) SetSharedMailboxEnabled(ctx context.Context, accountID, email string, enabled bool) error {
	result, err := db.Write().ExecContext(ctx, `UPDATE shared_mailboxes SET enabled = ? WHERE account_id = ? AND email = ?`,
		boolInt(enabled), accountID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrSharedMailboxNotFound
	}
	return nil
}

// PresetSharedMailboxes records, for an account being added, the shared
// mailboxes its connection test found, each on or off as the user chose,
// before the first sync finds them.
func (db *DB) PresetSharedMailboxes(ctx context.Context, accountID string, enabled map[string]bool) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for email, on := range enabled {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shared_mailboxes (account_id, email, enabled, discovered_at, last_seen_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(account_id, email) DO UPDATE SET enabled = excluded.enabled`, accountID, email, boolInt(on), sharedMailboxNotYetSeen, sharedMailboxNotYetSeen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DisabledSharedMailboxes returns the account's shared mailboxes the user
// turned off, by address.
func (db *DB) DisabledSharedMailboxes(ctx context.Context, accountID string) (map[string]bool, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT email FROM shared_mailboxes WHERE account_id = ? AND enabled = 0`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	off := map[string]bool{}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		off[email] = true
	}
	return off, rows.Err()
}

// sharedMailboxOnSQL is a condition true for a folder (alias) unless it is
// in a shared mailbox the user turned off.
func sharedMailboxOnSQL(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM shared_mailboxes off_mailbox WHERE off_mailbox.account_id = ` + alias + `.account_id AND off_mailbox.email = ` + alias + `.shared_mailbox AND ` + alias + `.shared_mailbox != '' AND off_mailbox.enabled = 0)`
}

// SetSharedMailboxServerNames records the names the server gives an
// account's shared mailboxes, by address, before the discovery that records
// the mailboxes themselves, so the identities it makes can take them.
func (db *DB) SetSharedMailboxServerNames(ctx context.Context, accountID string, names map[string]string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for email, name := range names {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO shared_mailboxes (account_id, email, server_name, discovered_at, last_seen_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(account_id, email) DO UPDATE SET server_name = excluded.server_name`, accountID, email, strings.TrimSpace(name), sharedMailboxNotYetSeen, sharedMailboxNotYetSeen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetSharedMailboxName gives one of an account's shared mailboxes the name
// the user chose for it, or, empty, goes back to the server's.
func (db *DB) SetSharedMailboxName(ctx context.Context, accountID, email, name string) error {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	result, err := db.Write().ExecContext(ctx, `UPDATE shared_mailboxes SET custom_name = ? WHERE account_id = ? AND email = ?`,
		name, accountID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrSharedMailboxNotFound
	}
	return nil
}

// sharedMailboxNameSQL reads, for a folder (alias), the name of the shared
// mailbox it is in as the user or server gave it, "" when neither did.
func sharedMailboxNameSQL(alias string) string {
	return `COALESCE((SELECT COALESCE(NULLIF(named.custom_name, ''), named.server_name) FROM shared_mailboxes named
		WHERE named.account_id = ` + alias + `.account_id AND named.email = ` + alias + `.shared_mailbox), '')`
}
