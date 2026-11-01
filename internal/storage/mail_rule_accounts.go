package storage

import (
	"context"
	"database/sql"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A rule runs on every account's mail, or only on the accounts in
// mail_rule_accounts when mail_rules.accounts_chosen is set. Deleting an
// account takes it out of its rules and leaves the rules; one left with no
// account does nothing until it's given one.
const mailRuleAccountsTableSQL = `CREATE TABLE IF NOT EXISTS mail_rule_accounts (
    rule_id TEXT NOT NULL REFERENCES mail_rules(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    PRIMARY KEY (rule_id, account_id)
);
CREATE INDEX IF NOT EXISTS idx_mail_rule_accounts_account ON mail_rule_accounts(account_id)`

// mail_rule_transfers holds messages a rule moves to a folder of another
// account. A move between accounts copies the message first, which a mail
// transfer does in the background; the rule only queues it here.
const mailRuleTransfersTableSQL = `CREATE TABLE IF NOT EXISTS mail_rule_transfers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    source_account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    source_folder_id TEXT NOT NULL,
    target_folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    for_good INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (message_id, target_folder_id)
);
CREATE INDEX IF NOT EXISTS idx_mail_rule_transfers_user ON mail_rule_transfers(user_id, id)`

// migrateV134ToV135 lets a rule run on several chosen accounts and move mail
// to other accounts. A rule that was for one account keeps that account,
// and no longer goes when the account does.
func migrateV134ToV135(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 135 {
		return nil
	}
	// The new tables refer to these; databases of tests made from very old
	// schemas can lack them.
	exists := true
	for _, table := range []string{"mail_rules", "accounts", "folders", "messages", "users"} {
		present, err := tableExistsTx(tx, table)
		if err != nil {
			return err
		}
		exists = exists && present
	}
	if exists {
		present, err := columnExistsTx(tx, "mail_rules", "accounts_chosen")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE mail_rules ADD COLUMN accounts_chosen INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(mailRuleAccountsTableSQL); err != nil {
			return err
		}
		if _, err := tx.Exec(mailRuleTransfersTableSQL); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO mail_rule_accounts (rule_id, account_id)
			SELECT id, account_id FROM mail_rules WHERE account_id IS NOT NULL`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE mail_rules SET accounts_chosen = 1, account_id = NULL WHERE account_id IS NOT NULL`); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 135); err != nil {
		return err
	}
	return tx.Commit()
}

// mailRuleWatchesSQL holds for the rule in the table named rule when it runs
// on the account named by the expression account.
func mailRuleWatchesSQL(rule, account string) string {
	return `(` + rule + `.accounts_chosen = 0 OR EXISTS (SELECT 1 FROM mail_rule_accounts ra WHERE ra.rule_id = ` + rule + `.id AND ra.account_id = ` + account + `))`
}

// saveMailRuleAccountsTx makes a rule's chosen accounts those given.
func saveMailRuleAccountsTx(ctx context.Context, tx *sql.Tx, ruleID string, accountIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_rule_accounts WHERE rule_id = ?`, ruleID); err != nil {
		return err
	}
	for _, id := range accountIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mail_rule_accounts (rule_id, account_id) VALUES (?, ?)`, ruleID, id); err != nil {
			return err
		}
	}
	return nil
}

func splitMailRuleAccounts(list string) []string {
	if list == "" {
		return nil
	}
	return strings.Split(list, ",")
}

// MailRuleTransfersQueued, when set, is told the owner whose rules queued
// moves to another account, for them to start.
var MailRuleTransfersQueued func(owner string)

// otherAccount reports whether a move's folder is in another account than
// the message's. A folder saved before rules could name other accounts'
// folders has no account and is in the message's.
func (r *mailRuleRunner) otherAccount(target models.MailRuleFolderTarget) bool {
	return target.FolderID != "" && target.AccountID != "" && target.AccountID != r.accountID
}

// transferNeedless is needless for a move to another account's folder.
func (r *mailRuleRunner) transferNeedless(m *mailRuleMessage, arrival string, target models.MailRuleFolderTarget) (string, bool) {
	folder, _, err := r.otherFolder(context.Background(), target)
	if err != nil {
		name := target.Path
		if name == "" {
			name = "chosen"
		}
		return "not moved: the folder “" + name + "” is gone", true
	}
	var queued bool
	if err := r.db.Read().QueryRow(`SELECT EXISTS(SELECT 1 FROM mail_rule_transfers WHERE message_id = ? AND target_folder_id = ?)`, m.id, folder.id).Scan(&queued); err == nil && queued {
		return "already on its way to “" + folder.name + "”", true
	}
	return "not moved: the message is in no folder", r.source(m, arrival) == ""
}

// otherFolder finds a folder of another of the owner's accounts, by ID or by
// the name it had when the rule was saved, and names its account.
func (r *mailRuleRunner) otherFolder(ctx context.Context, target models.MailRuleFolderTarget) (mailRuleFolder, string, error) {
	var folder mailRuleFolder
	var account string
	err := r.db.Read().QueryRowContext(ctx, `
		SELECT f.id, f.name, f.role, COALESCE(f.remote_id, ''),
		       CASE WHEN a.email_address != '' THEN a.email_address ELSE a.display_name END
		FROM folders f JOIN accounts a ON a.id = f.account_id
		WHERE a.id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		  AND COALESCE(f.discovery_state, 'active') = 'active' AND f.selectable = 1
		  AND (f.id = ? OR (? != '' AND f.remote_id = ?))
		ORDER BY f.id = ? DESC LIMIT 1`,
		target.AccountID, r.owner, target.FolderID, target.Path, target.Path, target.FolderID).
		Scan(&folder.id, &folder.name, &folder.role, &folder.remoteID, &account)
	return folder, account, err
}

// queueTransfer sets a message aside to be moved to another account's
// folder, which copies it there and then deletes it here.
func (r *mailRuleRunner) queueTransfer(ctx context.Context, m *mailRuleMessage, arrival string, action models.MailRuleAction, target models.MailRuleFolderTarget) (string, error) {
	folder, account, err := r.otherFolder(ctx, target)
	if err != nil {
		return "move", err
	}
	if _, err := r.db.Write().ExecContext(ctx, `
		INSERT OR IGNORE INTO mail_rule_transfers (user_id, message_id, source_account_id, source_folder_id, target_folder_id, for_good)
		VALUES (?, ?, ?, ?, ?, ?)`, r.owner, m.id, r.accountID, r.source(m, arrival), folder.id, action.ForGood); err != nil {
		return "move", err
	}
	r.transfers = true
	return "moving to “" + folder.name + "” in " + account, nil
}

// MailRuleTransfer is a message a rule moves to another account's folder.
type MailRuleTransfer struct {
	ID              int64
	MessageID       int64
	SourceAccountID string
	SourceFolderID  string
	TargetFolderID  string
	ForGood         bool
}

// ListMailRuleTransfers returns the owner's queued moves to another
// account, oldest first.
func (db *DB) ListMailRuleTransfers(ctx context.Context, owner string, limit int) ([]MailRuleTransfer, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, message_id, source_account_id, source_folder_id, target_folder_id, for_good
		FROM mail_rule_transfers WHERE user_id = ? ORDER BY id LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var transfers []MailRuleTransfer
	for rows.Next() {
		var t MailRuleTransfer
		if err := rows.Scan(&t.ID, &t.MessageID, &t.SourceAccountID, &t.SourceFolderID, &t.TargetFolderID, &t.ForGood); err != nil {
			return nil, err
		}
		transfers = append(transfers, t)
	}
	return transfers, rows.Err()
}

// DeleteMailRuleTransfers takes started or dropped moves off the queue.
func (db *DB) DeleteMailRuleTransfers(ctx context.Context, owner string, ids []int64) error {
	for _, id := range ids {
		if _, err := db.Write().ExecContext(ctx, `DELETE FROM mail_rule_transfers WHERE id = ? AND user_id = ?`, id, owner); err != nil {
			return err
		}
	}
	return nil
}

// MailRuleTransferOwners returns the users with queued moves to another
// account.
func (db *DB) MailRuleTransferOwners(ctx context.Context) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT DISTINCT user_id FROM mail_rule_transfers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

func announceMailRuleTransfers(owner string) {
	if MailRuleTransfersQueued != nil {
		MailRuleTransfersQueued(owner)
	}
}

// MailRuleTransferOwners returns the users whose own stores hold queued moves
// to another account.
func (r *AccountRouting) MailRuleTransferOwners(ctx context.Context) ([]string, error) {
	rows, err := r.System().Read().QueryContext(ctx, `
		SELECT DISTINCT d.user_id FROM gofer_account_directory d JOIN users u ON u.id = d.user_id
		WHERE d.state = 'active' AND u.status = 'active' AND u.deletion_pending = 0 ORDER BY d.user_id`)
	if err != nil {
		return nil, err
	}
	var users []string
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			rows.Close()
			return nil, err
		}
		users = append(users, user)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var owners []string
	for _, user := range users {
		var queued bool
		err := r.WithExistingUser(ctx, user, func(db *DB) error {
			return db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mail_rule_transfers WHERE user_id = ?)`, user).Scan(&queued)
		})
		if err != nil {
			continue
		}
		if queued {
			owners = append(owners, user)
		}
	}
	return owners, nil
}
