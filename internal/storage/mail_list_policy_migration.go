package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The allowed mail lists moved into policies at schema 152
// (internal/policy): the servers, the address domains and the providers
// let in while servers are limited became the Default policy's
// mail.allowed_servers, mail.allowed_address_domains and
// mail.allowed_providers, and the people the lists didn't apply to are
// assigned a policy that sets all three to nothing, first in the order.
// The old tables keep their rows for one release, untouched, and nothing
// reads them any more.
//
// The move happens once: an admin_revisions row records it. A database
// split from an older shared one copies the old tables as they were, so
// opening it moves them then.
const (
	mailListServersKey   = "mail.allowed_servers"
	mailListDomainsKey   = "mail.allowed_address_domains"
	mailListProvidersKey = "mail.allowed_providers"
	// MailListExemptionPolicyID is the policy the people the lists didn't
	// apply to are assigned, with the name and description it's given.
	MailListExemptionPolicyID    = "mail-exemption"
	MailListExemptionName        = "Mail list exemption"
	MailListExemptionDescription = "The allowed mail servers and domains don't limit the people this policy is assigned to."
	// mailListsMoved is the admin_revisions row that records the move.
	mailListsMoved = "mail_lists_moved"
	// The Default policy's row, as internal/policy makes it.
	defaultPolicyID             = "default"
	defaultPolicyName           = "Default policy"
	defaultPolicyNameNormalized = "default policy"
)

// migrateV151ToV152 moves the allowed mail lists into policies.
func migrateV151ToV152(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 152 {
		return nil
	}
	if err := moveMailListsOnce(ctx, tx, time.Now().UTC()); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 152); err != nil {
		return err
	}
	return tx.Commit()
}

// moveCopiedMailLists moves lists a split copied from an older shared
// database, the first time the copy is opened.
func moveCopiedMailLists(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := moveMailListsOnce(ctx, tx, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// moveMailListsOnce moves the lists unless they were moved already. With
// nothing to move, nothing is recorded, so a copy made into this database
// later still moves.
func moveMailListsOnce(ctx context.Context, tx *sql.Tx, now time.Time) error {
	var moved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_revisions WHERE name = ?`, mailListsMoved).Scan(&moved); err != nil {
		return err
	}
	if moved > 0 {
		return nil
	}
	found, err := moveMailListsIntoPolicies(ctx, tx, now)
	if err != nil || !found {
		if err != nil {
			err = fmt.Errorf("move the allowed mail lists into policies: %w", err)
		}
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_revisions (name, revision) VALUES (?, 1)`, mailListsMoved)
	return err
}

// moveMailListsIntoPolicies copies the old tables' lists and exemptions
// into the policies, in tx, and says whether there was anything to copy.
func moveMailListsIntoPolicies(ctx context.Context, tx *sql.Tx, now time.Time) (bool, error) {
	lists := map[string][]string{mailListServersKey: {}, mailListDomainsKey: {}, mailListProvidersKey: {}}
	rows, err := tx.QueryContext(ctx, `SELECT kind, value FROM mail_allowlist_entries ORDER BY kind, value`)
	if err != nil {
		return false, fmt.Errorf("read the allowed mail lists: %w", err)
	}
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			rows.Close()
			return false, err
		}
		switch kind {
		case MailAllowlistServer:
			lists[mailListServersKey] = append(lists[mailListServersKey], value)
		case MailAllowlistDomain:
			lists[mailListDomainsKey] = append(lists[mailListDomainsKey], value)
		case MailAllowlistProvider:
			lists[mailListProvidersKey] = append(lists[mailListProvidersKey], value)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	// Only the people a policy can be assigned to: an administrator has no
	// mail of their own, and someone being deleted is going. Nobody is
	// looked up when nobody was exempted, as in databases that have no
	// people of their own.
	var exempt []string
	var anyExempt int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mail_allowlist_exempt_users)`).Scan(&anyExempt); err != nil {
		return false, fmt.Errorf("read the allowed mail list exemptions: %w", err)
	}
	if anyExempt == 0 {
		return moveMailLists(ctx, tx, lists, nil, now)
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT e.user_id FROM mail_allowlist_exempt_users e
		JOIN users u ON u.id = e.user_id
		WHERE u.user_type = 'webmail' AND u.deletion_pending = 0
		ORDER BY e.user_id`)
	if err != nil {
		return false, fmt.Errorf("read the allowed mail list exemptions: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		exempt = append(exempt, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	return moveMailLists(ctx, tx, lists, exempt, now)
}

// moveMailLists writes the lists to the Default policy and the exempt
// people's policy, and says whether there was anything to write.
func moveMailLists(ctx context.Context, tx *sql.Tx, lists map[string][]string, exempt []string, now time.Time) (bool, error) {
	limited := false
	for _, list := range lists {
		limited = limited || len(list) > 0
	}
	if !limited && len(exempt) == 0 {
		return false, nil
	}

	if limited {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM policies WHERE id = ?`, defaultPolicyID).Scan(&existing); err != nil {
			return false, err
		}
		if existing == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO policies (id, name, name_normalized, is_default, priority, created_at, created_by, updated_at, updated_by, version)
				VALUES (?, ?, ?, 1, NULL, ?, '', ?, '', 1)`,
				defaultPolicyID, defaultPolicyName, defaultPolicyNameNormalized, now, now); err != nil {
				return false, fmt.Errorf("save the Default policy: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE policies SET updated_at = ?, version = version + 1 WHERE id = ?`, now, defaultPolicyID); err != nil {
			return false, fmt.Errorf("update the Default policy: %w", err)
		}
		for _, key := range []string{mailListServersKey, mailListDomainsKey, mailListProvidersKey} {
			if len(lists[key]) == 0 {
				continue
			}
			if err := setMovedPolicyValue(ctx, tx, defaultPolicyID, key, lists[key], now); err != nil {
				return false, err
			}
		}
	}

	if len(exempt) > 0 {
		name, err := freeMovedPolicyName(ctx, tx, MailListExemptionName)
		if err != nil {
			return false, err
		}
		// First in the order, so no other policy's list reaches them, as
		// none did before. The others step down, out of each other's way.
		var named int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM policies WHERE is_default = 0`).Scan(&named); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE policies SET priority = priority + ? WHERE is_default = 0`, named+1); err != nil {
			return false, fmt.Errorf("make room for the exemption policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE policies SET priority = priority - ? WHERE is_default = 0`, named); err != nil {
			return false, fmt.Errorf("make room for the exemption policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO policies (id, name, name_normalized, description, is_default, priority, created_at, created_by, updated_at, updated_by, version)
			VALUES (?, ?, ?, ?, 0, 1, ?, '', ?, '', 1)`,
			MailListExemptionPolicyID, name, movedPolicyNameNormalized(name), MailListExemptionDescription, now, now); err != nil {
			return false, fmt.Errorf("create the exemption policy: %w", err)
		}
		for _, key := range []string{mailListServersKey, mailListDomainsKey, mailListProvidersKey} {
			if err := setMovedPolicyValue(ctx, tx, MailListExemptionPolicyID, key, []string{}, now); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at, created_by)
			SELECT ?, 'user', e.user_id, e.created_at, e.created_by FROM mail_allowlist_exempt_users e
			JOIN users u ON u.id = e.user_id
			WHERE u.user_type = 'webmail' AND u.deletion_pending = 0`,
			MailListExemptionPolicyID); err != nil {
			return false, fmt.Errorf("assign the exemption policy: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO admin_revisions (name, revision) VALUES ('policy', 1)
		ON CONFLICT (name) DO UPDATE SET revision = revision + 1`); err != nil {
		return false, fmt.Errorf("bump the policy revision: %w", err)
	}
	return true, nil
}

func setMovedPolicyValue(ctx context.Context, tx *sql.Tx, policyID, key string, list []string, now time.Time) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO policy_values (policy_id, setting_key, value_json, locked, enforced, updated_at, updated_by)
		VALUES (?, ?, ?, 0, 0, ?, '')
		ON CONFLICT (policy_id, setting_key) DO UPDATE SET
			value_json = excluded.value_json, locked = 0, enforced = 0, updated_at = excluded.updated_at, updated_by = ''`,
		policyID, key, string(raw), now); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return nil
}

// freeMovedPolicyName is name, or name and a number when a policy has it.
func freeMovedPolicyName(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	candidate := name
	for n := 2; ; n++ {
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM policies WHERE name_normalized = ?`, movedPolicyNameNormalized(candidate)).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return candidate, nil
		}
		candidate = name + " " + strconv.Itoa(n)
	}
}

// movedPolicyNameNormalized is directory.NormalizeName for the names the
// move gives, which are plain words with single spaces.
func movedPolicyNameNormalized(name string) string {
	return strings.ToLower(name)
}
