package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func migrateV155ToV156(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 156 {
		return nil
	}
	exists, err := tableExistsTx(tx, "message_mutations")
	if err != nil {
		return err
	}
	if exists {
		for _, column := range []struct{ name, definition string }{
			{"target_flag_color", "INTEGER NOT NULL DEFAULT 0 CHECK (target_flag_color BETWEEN 0 AND 7)"},
			{"target_flag_copy", "TEXT NOT NULL DEFAULT ''"},
		} {
			exists, err := columnExistsTx(tx, "message_mutations", column.name)
			if err != nil {
				return err
			}
			if !exists {
				if _, err := tx.Exec(`ALTER TABLE message_mutations ADD COLUMN ` + column.name + " " + column.definition); err != nil {
					return fmt.Errorf("add message mutation %s: %w", column.name, err)
				}
			}
		}
	}
	if err := markSchemaVersion(tx, 156); err != nil {
		return err
	}
	return tx.Commit()
}

// SupportsFlagColors reports whether a mail account speaks a protocol with
// RFC 9979 keyword support.
func SupportsFlagColors(provider string) bool {
	return provider == MessageMutationProviderIMAP || provider == MessageMutationProviderJMAP
}

// MessageMutationResetFlagColor clears the bits while setting a plain star.
// Zero leaves the bits alone when setting an already active star.
const MessageMutationResetFlagColor keywords.FlagColor = 7

// FlagBitsTarget returns the exact provider bit mask when this starred
// mutation needs to change color. It also handles unstars queued by older versions.
func (m MessageMutation) FlagBitsTarget() (int, bool) {
	if m.Kind != MessageMutationStarred || !SupportsFlagColors(m.ProviderType) {
		return 0, false
	}
	if !m.TargetValue || m.TargetFlagColor == MessageMutationResetFlagColor {
		return 0, true
	}
	return int(m.TargetFlagColor), m.TargetFlagColor.Valid()
}

// IsScopedFlagColor distinguishes an edit on the represented copy from the
// existing star action, which reaches all copies of a message.
func (m MessageMutation) IsScopedFlagColor() bool {
	return m.Kind == MessageMutationStarred &&
		(m.TargetFlagColor.Valid() || (!m.TargetValue && m.TargetFlagColor == MessageMutationResetFlagColor))
}

// flagColorSQL reads the visible color for the folder row used by the caller.
// Only a fetched snapshot for that row or its exact JMAP Email ID qualifies.
// Pending edits take precedence without modifying observations from the server.
func flagColorSQL(state, provider string) string {
	copyID := providerCopyIDSQL(state)
	return `CASE WHEN ` + state + `.is_starred = 1 AND ` + provider + ` IN ('imap', 'jmap') THEN
		COALESCE((SELECT CASE q.target_flag_color WHEN 7 THEN 0 ELSE q.target_flag_color END
			FROM message_mutations q
			WHERE q.message_id = ` + state + `.message_id AND q.kind = 'starred' AND q.target_flag_color != 0
			  AND ((q.target_flag_copy != '' AND q.target_flag_copy = ` + copyID + `)
			    OR (q.target_flag_copy = '' AND (q.folder_id = ` + state + `.folder_id OR (` + provider + ` = 'jmap' AND EXISTS (
				SELECT 1 FROM message_folder_state qs WHERE qs.message_id = q.message_id AND qs.folder_id = q.folder_id
				  AND ` + providerCopyIDSQL("qs") + ` = ` + copyID + `)))))
			ORDER BY q.updated_at DESC, q.id DESC LIMIT 1),
			(SELECT CASE WHEN bits BETWEEN 1 AND 6 THEN bits ELSE 0 END FROM (
				SELECT COALESCE(MAX(lower(value) = '$mailflagbit0'), 0)
				     + COALESCE(MAX(lower(value) = '$mailflagbit1'), 0) * 2
				     + COALESCE(MAX(lower(value) = '$mailflagbit2'), 0) * 4 AS bits
				FROM json_each(CASE ` + provider + ` WHEN 'imap' THEN ` + state + `.remote_keywords ELSE (
					SELECT k.keywords FROM message_provider_keywords k WHERE
						k.message_id = ` + state + `.message_id AND k.remote_message_id = ` + copyID + ` AND k.fetched = 1
				) END)
			)), 0)
		ELSE 0 END`
}

func (db *DB) hydrateFlagColors(ctx context.Context, emails []models.Email) error {
	var values []string
	var args []any
	for i, email := range emails {
		if email.IsStarred {
			values = append(values, "(?, ?, ?, ?)")
			args = append(args, i, email.ID, email.FolderID, email.AccountID)
		}
	}
	if len(values) == 0 {
		return nil
	}
	// Keep page first so the query only looks up the copies displayed on the
	// page, including those from the materialized conversation list.
	rows, err := db.Read().QueryContext(ctx, `WITH page(pos, message_id, folder_id, account_id) AS (VALUES `+strings.Join(values, ",")+`)
		SELECT page.pos, `+flagColorSQL("mfs", "a.provider")+` FROM page CROSS JOIN message_folder_state mfs
		ON mfs.message_id = page.message_id AND mfs.folder_id = page.folder_id
		JOIN accounts a ON a.id = page.account_id`, args...)
	if err != nil {
		return fmt.Errorf("load mail list flag colors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pos int
		var color keywords.FlagColor
		if err := rows.Scan(&pos, &color); err != nil {
			return err
		}
		emails[pos].FlagColor = color
	}
	return rows.Err()
}

// RetiredFlagLabelReference identifies a legacy bit label, without considering
// an ordinary label that shares its display name to be retired.
func (db *DB) RetiredFlagLabelReference(ctx context.Context, owner, account, name, providerID, providerType string) (bool, error) {
	if owner == "" || (name == "" && providerID == "") || (providerType != "" && providerType != LabelProviderKeyword) {
		return false, nil
	}
	var live, retired, eligible bool
	err := db.Read().QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM labels l JOIN accounts a ON a.id=l.account_id
			WHERE a.user_id=?1 AND (?2='' OR a.id=?2) AND COALESCE(a.is_deleting,0)=0
			  AND ((?4='' AND (l.name=?3 COLLATE NOCASE OR EXISTS (
				SELECT 1 FROM label_aliases la WHERE la.account_id=l.account_id AND la.provider_type=l.provider_type
				  AND la.provider_id=l.provider_id AND la.display_name=?3 COLLATE NOCASE
			  ))) OR (?4!='' AND (l.id=?4 OR l.provider_id=?4 COLLATE NOCASE)))),
		(EXISTS (SELECT 1 FROM retired_keyword_labels l JOIN accounts a ON a.id=l.account_id
			WHERE a.user_id=?1 AND (?2='' OR a.id=?2) AND COALESCE(a.is_deleting,0)=0
			  AND lower(COALESCE(NULLIF(l.provider_id,''),l.name)) IN ('$mailflagbit0','$mailflagbit1','$mailflagbit2')
			  AND ((?4='' AND (l.name=?3 COLLATE NOCASE OR l.id=?3)) OR (?4!='' AND (l.id=?4 OR l.provider_id=?4 COLLATE NOCASE))))
		 OR (?4='' AND EXISTS (SELECT 1 FROM label_aliases la JOIN accounts a ON a.id=la.account_id
			WHERE a.user_id=?1 AND (?2='' OR a.id=?2) AND COALESCE(a.is_deleting,0)=0 AND la.provider_type=?5
			  AND lower(la.provider_id) IN ('$mailflagbit0','$mailflagbit1','$mailflagbit2') AND la.display_name=?3 COLLATE NOCASE))),
		EXISTS (SELECT 1 FROM accounts a WHERE a.user_id=?1 AND (?2='' OR a.id=?2) AND COALESCE(a.is_deleting,0)=0 AND a.provider IN ('imap','jmap'))`,
		owner, account, name, providerID, LabelProviderKeyword).Scan(&live, &retired, &eligible)
	// Old filter links can carry either the provider keyword or its internal id.
	raw := providerID
	if raw == "" {
		raw = name
	}
	return !live && (retired || (eligible && keywords.FlagBits([]string{raw}) != 0)), err
}

// SetMessageFlagColorForUser applies or removes one managed color on the
// selected copies. Removing a color that is no longer active leaves it alone.
func (db *DB) SetMessageFlagColorForUser(ctx context.Context, owner string, infos []ThreadMessageMutationInfo, color keywords.FlagColor, add bool) error {
	if owner == "" || len(infos) == 0 || !color.Valid() {
		return fmt.Errorf("invalid flag color action")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	affected := map[string]struct{}{}
	for _, info := range infos {
		if !SupportsFlagColors(info.AccountProvider) {
			return fmt.Errorf("flag colors require an IMAP or JMAP account")
		}
		var current keywords.FlagColor
		var epoch uint32
		var remoteID string
		if err := tx.QueryRowContext(ctx, `SELECT `+flagColorSQL("mfs", "a.provider")+`, COALESCE(f.uid_validity, 0), `+providerCopyIDSQL("mfs")+`
			FROM message_folder_state mfs
			JOIN folders f ON f.id = mfs.folder_id
			JOIN messages m ON m.id = mfs.message_id AND m.account_id = f.account_id
			JOIN accounts a ON a.id = m.account_id
			JOIN users u ON u.id = a.user_id
			WHERE mfs.message_id = ? AND mfs.folder_id = ? AND mfs.is_deleted = 0
			  AND a.id = ? AND a.provider = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0 AND u.status = 'active'`,
			info.MessageID, info.FolderID, info.AccountID, info.AccountProvider, owner).Scan(&current, &epoch, &remoteID); err != nil {
			return fmt.Errorf("load flag color target: %w", err)
		}
		if (add && current == color) || (!add && current != color) {
			continue
		}
		target := color
		if !add {
			target = MessageMutationResetFlagColor
		}
		folderID := info.FolderID
		targetCopy := ""
		if info.AccountProvider == MessageMutationProviderJMAP {
			if remoteID == "" {
				return fmt.Errorf("flag color target has no JMAP email identity")
			}
			targetCopy = remoteID
			// Use an existing queued scope for this email, including a flag set
			// in another mailbox, so two intents cannot compete on replay.
			err := tx.QueryRowContext(ctx, `SELECT q.folder_id FROM message_mutations q
				JOIN message_folder_state s ON s.message_id = q.message_id AND s.folder_id = q.folder_id
				WHERE q.message_id = ? AND q.kind = 'starred' AND s.is_deleted = 0
				  AND COALESCE(NULLIF(q.target_flag_copy, ''), `+providerCopyIDSQL("s")+`) = ?
				ORDER BY q.updated_at DESC, q.id DESC LIMIT 1`, info.MessageID, remoteID).Scan(&folderID)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
		}
		if err := queueMessageStateTx(ctx, tx, info.AccountID, info.MessageID, folderID, info.AccountProvider, MessageMutationStarred, add, target, targetCopy, epoch); err != nil {
			return err
		}
		query := `UPDATE message_folder_state SET is_starred = ? WHERE message_id = ? AND is_deleted = 0 AND `
		args := []any{add, info.MessageID, info.FolderID}
		if info.AccountProvider == MessageMutationProviderJMAP {
			query += providerCopyIDSQL("message_folder_state") + ` = ?`
			args[2] = remoteID
		} else {
			query += `folder_id = ?`
		}
		rows, err := tx.QueryContext(ctx, query+` RETURNING folder_id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var folder string
			if err := rows.Scan(&folder); err != nil {
				rows.Close()
				return err
			}
			affected[folder] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for folderID := range affected {
		_ = db.RefreshFolderThreadState(ctx, folderID)
	}
	return nil
}
