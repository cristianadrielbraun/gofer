package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
)

// A rule's tags say what it is for, such as "finance", so rules can be found
// and changed together. They don't change when or how a rule runs. Each of
// an owner's tags has one spelling: a tag written with other capitals
// becomes the one the owner's rules already have.
const mailRuleTagsTableSQL = `CREATE TABLE IF NOT EXISTS mail_rule_tags (
    rule_id TEXT NOT NULL REFERENCES mail_rules(id) ON DELETE CASCADE,
    tag TEXT NOT NULL COLLATE NOCASE CHECK (tag != ''),
    PRIMARY KEY (rule_id, tag)
)`

// mailRuleTagSeparator joins a rule's tags when they are read with it. A
// tag can't hold it, as it holds no control characters.
const mailRuleTagSeparator = "\x1f"

// ErrTooManyMailRuleTags means a tag was to be added to a rule that has as
// many tags as one may.
var ErrTooManyMailRuleTags = fmt.Errorf("a rule can have at most %d tags", mailrules.MaxTagsPerRule)

// migrateV152ToV153 lets rules have tags. Rules have none at first.
func migrateV152ToV153(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 153 {
		return nil
	}
	// Databases of tests made from very old schemas can lack the rules the
	// tags belong to.
	rules, err := tableExistsTx(tx, "mail_rules")
	if err != nil {
		return err
	}
	if rules {
		if _, err := tx.Exec(mailRuleTagsTableSQL); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 153); err != nil {
		return err
	}
	return tx.Commit()
}

// splitMailRuleTags reads a rule's tags as mailRuleColumns joins them, in
// the order the list shows them.
func splitMailRuleTags(joined string) []string {
	if joined == "" {
		return nil
	}
	tags := strings.Split(joined, mailRuleTagSeparator)
	sortMailRuleTags(tags)
	return tags
}

// sortMailRuleTags puts tags in alphabetical order, whatever their capitals.
func sortMailRuleTags(tags []string) {
	slices.SortFunc(tags, func(a, b string) int {
		if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
}

// mailRuleTagSpellings maps each of the owner's tags, in lower case, to the
// spelling their rules have, leaving out the rule except (none when "").
func mailRuleTagSpellings(ctx context.Context, tx *sql.Tx, userID, except string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT t.tag FROM mail_rule_tags t JOIN mail_rules r ON r.id = t.rule_id
		WHERE r.user_id = ? AND r.id != ? ORDER BY t.tag`, userID, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	spellings := map[string]string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		if _, ok := spellings[strings.ToLower(tag)]; !ok {
			spellings[strings.ToLower(tag)] = tag
		}
	}
	return spellings, rows.Err()
}

// saveMailRuleTagsTx makes a rule's tags those given, which Validate has
// cleaned, each spelled as the owner's other rules spell it.
func saveMailRuleTagsTx(ctx context.Context, tx *sql.Tx, userID, ruleID string, tags []string) error {
	spellings, err := mailRuleTagSpellings(ctx, tx, userID, ruleID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mail_rule_tags WHERE rule_id = ?`, ruleID); err != nil {
		return err
	}
	for _, tag := range tags {
		if spelled, ok := spellings[strings.ToLower(tag)]; ok {
			tag = spelled
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mail_rule_tags (rule_id, tag) VALUES (?, ?)`, ruleID, tag); err != nil {
			return err
		}
	}
	return nil
}

// ListMailRuleTags returns the tags of the owner's rules, each once, in
// alphabetical order: the tags a rule's tags field suggests and the list
// can be filtered by.
func (db *DB) ListMailRuleTags(ctx context.Context, userID string) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT DISTINCT t.tag FROM mail_rule_tags t JOIN mail_rules r ON r.id = t.rule_id
		WHERE r.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		if key := strings.ToLower(tag); !seen[key] {
			seen[key] = true
			tags = append(tags, tag)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortMailRuleTags(tags)
	return tags, nil
}

// AddMailRulesTagGuarded gives a tag to the owner's rules named by ids, all
// in one change, and reports how many rules it was added to. The tag is
// cleaned as a rule's own tags are, and spelled as the owner's rules
// already spell it. A rule that has it already, or an ID that names none of
// the owner's rules, is skipped. If a rule already has as many tags as one
// may, nothing changes and it is ErrTooManyMailRuleTags.
func (db *DB) AddMailRulesTagGuarded(ctx context.Context, userID string, ids []string, tag string, guard func(*sql.Tx) error) (int, error) {
	tag, err := mailrules.CheckTag(tag)
	if err != nil {
		return 0, err
	}
	added := 0
	err = db.mailRuleWrite(ctx, guard, func(tx *sql.Tx) error {
		added = 0
		spellings, err := mailRuleTagSpellings(ctx, tx, userID, "")
		if err != nil {
			return err
		}
		if spelled, ok := spellings[strings.ToLower(tag)]; ok {
			tag = spelled
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			tags, owned, err := mailRuleTagsTx(ctx, tx, userID, id)
			if err != nil {
				return err
			}
			if !owned || slices.ContainsFunc(tags, func(t string) bool { return strings.EqualFold(t, tag) }) {
				continue
			}
			if len(tags) >= mailrules.MaxTagsPerRule {
				return ErrTooManyMailRuleTags
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO mail_rule_tags (rule_id, tag) VALUES (?, ?)`, id, tag); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE mail_rules SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
				return err
			}
			added++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// RemoveMailRulesTagGuarded takes a tag, whatever its capitals, off the
// owner's rules named by ids, all in one change, and reports how many rules
// had it. An ID that names none of the owner's rules is skipped.
func (db *DB) RemoveMailRulesTagGuarded(ctx context.Context, userID string, ids []string, tag string, guard func(*sql.Tx) error) (int, error) {
	tag = mailrules.CleanTag(tag)
	if tag == "" {
		return 0, nil
	}
	return db.mailRulesWrite(ctx, guard, ids, func(tx *sql.Tx, id string) (sql.Result, error) {
		tags, owned, err := mailRuleTagsTx(ctx, tx, userID, id)
		if err != nil {
			return nil, err
		}
		for _, t := range tags {
			if owned && strings.EqualFold(t, tag) {
				if _, err := tx.ExecContext(ctx, `UPDATE mail_rules SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
					return nil, err
				}
				return tx.ExecContext(ctx, `DELETE FROM mail_rule_tags WHERE rule_id = ? AND tag = ?`, id, t)
			}
		}
		return noRowsChanged{}, nil
	})
}

// mailRuleTagsTx returns a rule's tags, and whether the rule is the owner's.
func mailRuleTagsTx(ctx context.Context, tx *sql.Tx, userID, ruleID string) ([]string, bool, error) {
	var joined string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT group_concat(t.tag, char(31)) FROM mail_rule_tags t WHERE t.rule_id = r.id), '')
		FROM mail_rules r WHERE r.id = ? AND r.user_id = ?`, ruleID, userID).Scan(&joined)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return splitMailRuleTags(joined), true, nil
}

// noRowsChanged is the result of a change to a rule that had nothing to
// change, for mailRulesWrite to count none.
type noRowsChanged struct{}

func (noRowsChanged) LastInsertId() (int64, error) { return 0, nil }
func (noRowsChanged) RowsAffected() (int64, error) { return 0, nil }
