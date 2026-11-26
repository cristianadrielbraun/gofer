package storage

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
)

// forgetStatusKeywordLabels retires technical labels copied verbatim by the
// offline shared-to-split storage migration before the copied store is opened.
func forgetStatusKeywordLabels(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := preserveStatusKeywordLabelsTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func preserveStatusKeywordLabelsTx(tx *sql.Tx) error {
	hasLabels, err := tableExistsTx(tx, "labels")
	if err != nil || !hasLabels {
		return err
	}
	rows, err := tx.Query(`SELECT id, name, provider_id FROM labels WHERE provider_type = ?`, LabelProviderKeyword)
	if err != nil {
		return fmt.Errorf("list keyword labels: %w", err)
	}
	type statusLabel struct{ id, keyword string }
	var labels []statusLabel
	for rows.Next() {
		var id, name, providerID string
		if err := rows.Scan(&id, &name, &providerID); err != nil {
			rows.Close()
			return fmt.Errorf("read keyword label: %w", err)
		}
		keyword := strings.TrimSpace(providerID)
		if keyword == "" {
			keyword = strings.TrimSpace(name)
		}
		if keywords.IsStatus(keyword) {
			labels = append(labels, statusLabel{id, keyword})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(labels) == 0 {
		return nil
	}

	started := time.Now()
	for i, label := range labels {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO retired_keyword_labels (id, account_id, name, provider_id)
			SELECT id, account_id, name, provider_id FROM labels WHERE id = ?`, label.id); err != nil {
			return fmt.Errorf("preserve status label identity: %w", err)
		}
		// A legacy association without copy data belongs to the logical
		// message. Assigning it to every folder or UID would invent state.
		if _, err := tx.Exec(`INSERT INTO message_provider_keywords (message_id, remote_message_id, keywords, fetched)
			SELECT message_id, remote_message_id, json_array(?), 0 FROM message_label_copies WHERE label_id = ?
			UNION ALL
			SELECT ml.message_id, '', json_array(?), 0 FROM message_labels ml WHERE ml.label_id = ?
			AND NOT EXISTS (SELECT 1 FROM message_label_copies c WHERE c.message_id = ml.message_id AND c.label_id = ml.label_id)
			ON CONFLICT (message_id, remote_message_id) DO UPDATE SET
				keywords = json_insert(message_provider_keywords.keywords, '$[#]', json_extract(excluded.keywords, '$[0]'))
			WHERE message_provider_keywords.fetched = 0`, label.keyword, label.id, label.keyword, label.id); err != nil {
			return fmt.Errorf("preserve status keyword label: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM labels WHERE id = ?`, label.id); err != nil {
			return fmt.Errorf("delete status keyword label: %w", err)
		}
		log.Printf("storage: preserved status keyword metadata for %d/%d labels", i+1, len(labels))
	}
	log.Printf("storage: status keyword metadata conversion done in %s", time.Since(started).Round(time.Millisecond))
	return nil
}
