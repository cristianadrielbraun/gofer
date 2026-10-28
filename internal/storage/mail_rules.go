package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// ErrMailRuleOrderChanged means a new rule order didn't name each of the
// owner's rules once: one was added or removed meanwhile.
var ErrMailRuleOrderChanged = errors.New("the rules changed; reload to see them")

// ErrMailRuleFolder means a rule names a folder its account doesn't have.
var ErrMailRuleFolder = errors.New("that folder is not in the rule's account")

// ErrTooManyMailRules means the owner has as many rules as one may.
var ErrTooManyMailRules = fmt.Errorf("you can have at most %d rules", mailrules.MaxRulesPerOwner)

// mailRuleRunsKept is how many activity rows each owner keeps; older ones
// are dropped as new ones come.
const mailRuleRunsKept = 2000

const mailRulesTableSQL = `CREATE TABLE IF NOT EXISTS mail_rules (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (name != ''),
    enabled INTEGER NOT NULL DEFAULT 1,
    match_all INTEGER NOT NULL DEFAULT 1,
    stop_processing INTEGER NOT NULL DEFAULT 0,
    conditions_json TEXT NOT NULL DEFAULT '[]',
    actions_json TEXT NOT NULL DEFAULT '[]',
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_mail_rules_user ON mail_rules(user_id, sort_order)`

const mailRulePendingTableSQL = `CREATE TABLE IF NOT EXISTS mail_rule_pending (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder_id TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_mail_rule_pending_account ON mail_rule_pending(account_id, message_id)`

const mailRuleRunsTableSQL = `CREATE TABLE IF NOT EXISTS mail_rule_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    rule_id TEXT NOT NULL DEFAULT '',
    rule_name TEXT NOT NULL DEFAULT '',
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL DEFAULT 0,
    subject TEXT NOT NULL DEFAULT '',
    from_email TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL CHECK (source IN ('arrival', 'manual')),
    status TEXT NOT NULL CHECK (status IN ('applied', 'skipped', 'failed')),
    detail TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_mail_rule_runs_user ON mail_rule_runs(user_id, id)`

// migrateV131ToV132 adds mail rules, the arrivals waiting for them, the
// record of what they did, and the List-Id of messages, which rules can
// test. There are no rules at first, so mail arrives as before.
func migrateV131ToV132(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 132 {
		return nil
	}
	for _, statement := range []string{mailRulesTableSQL, mailRulePendingTableSQL, mailRuleRunsTableSQL} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	exists, err := tableExistsTx(tx, "messages")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "messages", "list_id")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE messages ADD COLUMN list_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 132); err != nil {
		return err
	}
	return tx.Commit()
}

const mailRuleColumns = `id, COALESCE(account_id, ''), name, enabled, match_all, stop_processing, conditions_json, actions_json, sort_order, created_at, updated_at`

func scanMailRule(scan func(...any) error) (models.MailRule, error) {
	var rule models.MailRule
	var conditions, actions string
	var created, updated sqliteNullTime
	if err := scan(&rule.ID, &rule.AccountID, &rule.Name, &rule.Enabled, &rule.MatchAll, &rule.StopProcessing,
		&conditions, &actions, &rule.SortOrder, &created, &updated); err != nil {
		return models.MailRule{}, err
	}
	if err := json.Unmarshal([]byte(conditions), &rule.Conditions); err != nil {
		return models.MailRule{}, fmt.Errorf("rule %s conditions: %w", rule.ID, err)
	}
	if err := json.Unmarshal([]byte(actions), &rule.Actions); err != nil {
		return models.MailRule{}, fmt.Errorf("rule %s actions: %w", rule.ID, err)
	}
	rule.CreatedAt, rule.UpdatedAt = created.Time, updated.Time
	mailrules.GroupConditions(&rule)
	return rule, nil
}

func queryMailRules(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]models.MailRule, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []models.MailRule{}
	for rows.Next() {
		rule, err := scanMailRule(rows.Scan)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// ListMailRules returns the owner's rules in the order they run.
func (db *DB) ListMailRules(ctx context.Context, userID string) ([]models.MailRule, error) {
	return queryMailRules(ctx, db.Read(), `
		SELECT `+mailRuleColumns+` FROM mail_rules
		WHERE user_id = ? ORDER BY sort_order, created_at, id`, userID)
}

// GetMailRule returns one of the owner's rules, or sql.ErrNoRows.
func (db *DB) GetMailRule(ctx context.Context, userID, ruleID string) (models.MailRule, error) {
	return scanMailRule(db.Read().QueryRowContext(ctx, `
		SELECT `+mailRuleColumns+` FROM mail_rules WHERE user_id = ? AND id = ?`, userID, ruleID).Scan)
}

// SaveMailRuleGuarded checks and stores one of the owner's rules, a new one
// last, and returns it as stored. A folder a rule names by ID must be in
// the rule's account; its current name is kept with it, to find it by
// should its ID change.
func (db *DB) SaveMailRuleGuarded(ctx context.Context, userID string, rule models.MailRule, guard func(*sql.Tx) error) (models.MailRule, error) {
	if err := mailrules.Validate(&rule); err != nil {
		return models.MailRule{}, err
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return models.MailRule{}, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return models.MailRule{}, err
		}
	}
	if rule.AccountID != "" {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0)`,
			rule.AccountID, userID).Scan(&owned); err != nil {
			return models.MailRule{}, err
		}
		if !owned {
			return models.MailRule{}, sql.ErrNoRows
		}
	}
	targets := make([]*models.MailRuleFolderTarget, 0, len(rule.Conditions)+len(rule.Actions))
	for i := range rule.Conditions {
		targets = append(targets, rule.Conditions[i].Folder)
	}
	for i := range rule.Actions {
		targets = append(targets, rule.Actions[i].Folder)
	}
	for _, target := range targets {
		if target == nil || target.FolderID == "" {
			continue
		}
		var path string
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(remote_id, '') FROM folders WHERE id = ? AND account_id = ?`, target.FolderID, rule.AccountID).Scan(&path)
		if errors.Is(err, sql.ErrNoRows) {
			return models.MailRule{}, ErrMailRuleFolder
		}
		if err != nil {
			return models.MailRule{}, err
		}
		target.Path = path
	}
	conditions, err := json.Marshal(rule.Conditions)
	if err != nil {
		return models.MailRule{}, err
	}
	actions, err := json.Marshal(rule.Actions)
	if err != nil {
		return models.MailRule{}, err
	}
	account := sql.NullString{String: rule.AccountID, Valid: rule.AccountID != ""}
	if rule.ID == "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_rules WHERE user_id = ?`, userID).Scan(&count); err != nil {
			return models.MailRule{}, err
		}
		if count >= mailrules.MaxRulesPerOwner {
			return models.MailRule{}, ErrTooManyMailRules
		}
		rule.ID = "rul_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mail_rules (id, user_id, account_id, name, enabled, match_all, stop_processing, conditions_json, actions_json, sort_order)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM mail_rules WHERE user_id = ?))`,
			rule.ID, userID, account, rule.Name, rule.Enabled, rule.MatchAll, rule.StopProcessing, string(conditions), string(actions), userID); err != nil {
			return models.MailRule{}, fmt.Errorf("save rule: %w", err)
		}
	} else {
		result, err := tx.ExecContext(ctx, `
			UPDATE mail_rules SET account_id = ?, name = ?, enabled = ?, match_all = ?, stop_processing = ?,
				conditions_json = ?, actions_json = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND user_id = ?`,
			account, rule.Name, rule.Enabled, rule.MatchAll, rule.StopProcessing, string(conditions), string(actions), rule.ID, userID)
		if err != nil {
			return models.MailRule{}, fmt.Errorf("save rule: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil {
			return models.MailRule{}, err
		} else if changed == 0 {
			return models.MailRule{}, sql.ErrNoRows
		}
	}
	saved, err := scanMailRule(tx.QueryRowContext(ctx, `SELECT `+mailRuleColumns+` FROM mail_rules WHERE id = ?`, rule.ID).Scan)
	if err != nil {
		return models.MailRule{}, err
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return models.MailRule{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return models.MailRule{}, err
	}
	return saved, nil
}

// SetMailRuleEnabledGuarded turns one of the owner's rules on or off.
func (db *DB) SetMailRuleEnabledGuarded(ctx context.Context, userID, ruleID string, enabled bool, guard func(*sql.Tx) error) error {
	return db.mailRuleWrite(ctx, guard, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE mail_rules SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, enabled, ruleID, userID)
		return requireOneRow(result, err)
	})
}

// DeleteMailRuleGuarded removes one of the owner's rules. What it did stays
// in the activity.
func (db *DB) DeleteMailRuleGuarded(ctx context.Context, userID, ruleID string, guard func(*sql.Tx) error) error {
	return db.mailRuleWrite(ctx, guard, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM mail_rules WHERE id = ? AND user_id = ?`, ruleID, userID)
		return requireOneRow(result, err)
	})
}

// SetMailRuleOrderGuarded puts the owner's rules in the order of ids, which
// must name each of them once; otherwise it is ErrMailRuleOrderChanged.
func (db *DB) SetMailRuleOrderGuarded(ctx context.Context, userID string, ids []string, guard func(*sql.Tx) error) error {
	return db.mailRuleWrite(ctx, guard, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM mail_rules WHERE user_id = ?`, userID)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			known[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) != len(known) {
			return ErrMailRuleOrderChanged
		}
		for i, id := range ids {
			if !known[id] {
				return ErrMailRuleOrderChanged
			}
			delete(known, id)
			if _, err := tx.ExecContext(ctx, `UPDATE mail_rules SET sort_order = ? WHERE id = ? AND user_id = ?`, i, id, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) mailRuleWrite(ctx context.Context, guard func(*sql.Tx) error, write func(*sql.Tx) error) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return err
		}
	}
	if err := write(tx); err != nil {
		return err
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func requireOneRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListMailRuleRuns returns what the owner's rules did, newest first.
func (db *DB) ListMailRuleRuns(ctx context.Context, userID string, limit int) ([]models.MailRuleRun, error) {
	runs, _, err := db.ListMailRuleActivity(ctx, userID, MailRuleActivityFilter{Limit: limit})
	return runs, err
}

// MailRuleActivityFilter picks the entries of the rules' activity to show:
// one rule's only when RuleID is set, failures only, and at most Limit.
type MailRuleActivityFilter struct {
	RuleID     string
	FailedOnly bool
	Limit      int
}

// ListMailRuleActivity returns what the owner's rules did, newest first, as
// filtered, with whether there are more entries, and for each message a
// folder it shows in now.
func (db *DB) ListMailRuleActivity(ctx context.Context, userID string, filter MailRuleActivityFilter) ([]models.MailRuleRun, bool, error) {
	if filter.Limit <= 0 || filter.Limit > mailRuleRunsKept {
		filter.Limit = mailRuleRunsKept
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT r.id, r.rule_id, r.rule_name, r.account_id, r.message_id, r.subject, r.from_email, r.source, r.status, r.detail, r.created_at,
			COALESCE((SELECT s.folder_id FROM message_folder_state s JOIN messages m ON m.id = s.message_id
				WHERE s.message_id = r.message_id AND m.account_id = r.account_id AND s.is_deleted = 0 ORDER BY s.folder_id LIMIT 1), '')
		FROM mail_rule_runs r
		WHERE r.user_id = ? AND (? = '' OR r.rule_id = ?) AND (? = 0 OR r.status = ?)
		ORDER BY r.id DESC LIMIT ?`,
		userID, filter.RuleID, filter.RuleID, filter.FailedOnly, models.MailRuleRunFailed, filter.Limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	runs := []models.MailRuleRun{}
	for rows.Next() {
		var run models.MailRuleRun
		var created sqliteNullTime
		if err := rows.Scan(&run.ID, &run.RuleID, &run.RuleName, &run.AccountID, &run.MessageID, &run.Subject, &run.FromEmail,
			&run.Trigger, &run.Status, &run.Detail, &created, &run.FolderID); err != nil {
			return nil, false, err
		}
		run.CreatedAt = created.Time
		runs = append(runs, run)
	}
	more := len(runs) > filter.Limit
	if more {
		runs = runs[:filter.Limit]
	}
	return runs, more, rows.Err()
}

// ClearMailRuleRunsGuarded forgets what the owner's rules did.
func (db *DB) ClearMailRuleRunsGuarded(ctx context.Context, userID string, guard func(*sql.Tx) error) error {
	return db.mailRuleWrite(ctx, guard, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM mail_rule_runs WHERE user_id = ?`, userID)
		return err
	})
}

func recordMailRuleRun(ctx context.Context, db *sql.DB, userID string, run models.MailRuleRun) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO mail_rule_runs (user_id, rule_id, rule_name, account_id, message_id, subject, from_email, source, status, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, run.RuleID, run.RuleName, run.AccountID, run.MessageID, run.Subject, run.FromEmail, run.Trigger, run.Status, run.Detail)
	return err
}

func pruneMailRuleRuns(ctx context.Context, db *sql.DB, userID string) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM mail_rule_runs WHERE user_id = ? AND id <= (
			SELECT id FROM mail_rule_runs WHERE user_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		userID, userID, mailRuleRunsKept)
	return err
}

// markMailRuleArrivalTx sets a message that just arrived aside for its
// owner's rules, if any rule could act on it.
func markMailRuleArrivalTx(ctx context.Context, tx *sql.Tx, messageID int64, accountID, folderID string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO mail_rule_pending (message_id, account_id, folder_id)
		SELECT ?, ?, ? WHERE EXISTS (
			SELECT 1 FROM mail_rules r JOIN accounts a ON a.user_id = r.user_id
			WHERE a.id = ? AND r.enabled = 1 AND (r.account_id IS NULL OR r.account_id = a.id))`,
		messageID, accountID, folderID, accountID)
	return err
}

// ListMailRuleFolderChoices returns, for each of the owner's accounts, the
// folders a rule can name, by name.
func (db *DB) ListMailRuleFolderChoices(ctx context.Context, userID string) (map[string][]models.MailRuleFolderChoice, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT f.account_id, f.id, f.name, COALESCE(f.remote_id, ''), f.role FROM folders f JOIN accounts a ON a.id = f.account_id
		WHERE a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0 AND COALESCE(f.discovery_state, 'active') = 'active' AND f.selectable = 1
		ORDER BY lower(COALESCE(NULLIF(f.remote_id, ''), f.name)), f.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	choices := map[string][]models.MailRuleFolderChoice{}
	for rows.Next() {
		var accountID string
		var folder models.MailRuleFolderChoice
		if err := rows.Scan(&accountID, &folder.ID, &folder.Name, &folder.Path, &folder.Role); err != nil {
			return nil, err
		}
		choices[accountID] = append(choices[accountID], folder)
	}
	return choices, rows.Err()
}
