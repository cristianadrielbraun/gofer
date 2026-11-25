package storage

import (
	"database/sql"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
)

// forgetStatusKeywordLabels deletes the keyword labels that keywords.IsStatus
// now counts as status, like $HasNoAttachment, which some servers add to every
// new message. Sync already skips them, but labels saved before a keyword was
// added to that list stayed on their messages and in the sidebar. It runs at
// every start rather than as a schema step, so a keyword added to the list
// later is cleared the same way. Only the local copy goes: the keywords stay
// on the server.
func forgetStatusKeywordLabels(db *sql.DB) error {
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'labels'`).Scan(&tables); err != nil {
		return fmt.Errorf("look for the labels table: %w", err)
	}
	if tables == 0 {
		return nil
	}
	rows, err := db.Query(`SELECT id, name, provider_id FROM labels WHERE provider_type = ?`, LabelProviderKeyword)
	if err != nil {
		return fmt.Errorf("list keyword labels: %w", err)
	}
	var ids []string
	names := map[string]bool{}
	for rows.Next() {
		var id, name, providerID string
		if err := rows.Scan(&id, &name, &providerID); err != nil {
			rows.Close()
			return fmt.Errorf("read keyword label: %w", err)
		}
		if keywords.IsStatus(name) || keywords.IsStatus(providerID) {
			ids = append(ids, id)
			names[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tagged int64
	for _, id := range ids {
		var count int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM message_labels WHERE label_id = ?`, id).Scan(&count); err != nil {
			return fmt.Errorf("count messages with status keyword label: %w", err)
		}
		tagged += count
		// message_labels and message_label_copies go with the label.
		if _, err := tx.Exec(`DELETE FROM labels WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete status keyword label: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("storage: removed %d status keyword label(s) (%s) from %d message label(s)", len(ids), strings.Join(slices.Sorted(maps.Keys(names)), ", "), tagged)
	return nil
}
