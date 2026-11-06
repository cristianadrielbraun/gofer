package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// A contact's mail count is the number of the owner's messages that have its
// address as the sender or a recipient, each message once. It is counted from
// the mail itself, never added up: syncs see the same message again and
// again, and mail gets deleted. Saving an observed contact counts its address
// there and then, and the writer's hook (contact_changes.go) recounts the
// addresses on any message or recipient row that changes, after the commit.

// contactMessageCountSQL counts the messages of the owner userExpr with the
// normalized address emailExpr. The indexes on lower(trim(...)) of schema 140
// serve both halves. The address is compared without its column's affinity
// (the unary +): with it, SQLite converts the indexed expression and can only
// scan the index, which on a large mailbox takes minutes instead of a moment.
func contactMessageCountSQL(userExpr, emailExpr string) string {
	emailExpr = "+" + emailExpr
	return `(SELECT COUNT(*) FROM (
		SELECT m.id FROM messages m JOIN accounts a ON a.id = m.account_id
		WHERE lower(trim(m.from_email)) = ` + emailExpr + ` AND a.user_id = ` + userExpr + `
		UNION
		SELECT mr.message_id FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		JOIN accounts a ON a.id = m.account_id
		WHERE lower(trim(mr.email)) = ` + emailExpr + ` AND a.user_id = ` + userExpr + `))`
}

// recountContactObservationTx sets one observation's count from the mail.
func recountContactObservationTx(ctx context.Context, tx *sql.Tx, userID, normalized string) error {
	_, err := tx.ExecContext(ctx, `UPDATE contact_observations
		SET message_count = `+contactMessageCountSQL("?1", "?2")+`
		WHERE user_id = ?1 AND normalized_email = ?2`, userID, normalized)
	return err
}

// contactRecountBatch bounds the addresses one recount statement takes.
const contactRecountBatch = 500

// recountContactAddresses recounts every observation of these normalized
// addresses, whoever owns it, a bounded batch per transaction.
func (db *DB) recountContactAddresses(ctx context.Context, addresses []string) error {
	for start := 0; start < len(addresses); start += contactRecountBatch {
		batch := addresses[start:min(start+contactRecountBatch, len(addresses))]
		args := make([]any, len(batch))
		for i, address := range batch {
			args[i] = address
		}
		if _, err := db.Write().ExecContext(ctx, `UPDATE contact_observations
			SET message_count = `+contactMessageCountSQL("contact_observations.user_id", "contact_observations.normalized_email")+`
			WHERE normalized_email IN (`+sqlPlaceholders(len(batch))+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// recountChangedContactAddresses runs after the commits that changed mail
// with these addresses. A failure only leaves counts until the next change.
func (db *DB) recountChangedContactAddresses(addresses []string) {
	if err := db.recountContactAddresses(context.Background(), addresses); err != nil && !strings.Contains(err.Error(), "database is closed") {
		log.Printf("storage: recount contact mail: %v", err)
	}
}

// migrateV139ToV140 indexes mail by normalized address and recounts every
// contact, whose counts had been added up and grown far past the mail.
func migrateV139ToV140(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 140 {
		return nil
	}
	log.Printf("storage: schema 140: recounting contacts' mail")
	// Very old databases tested for upgrades lack some of these tables: only
	// the ones there are get indexed, and counts need all of them.
	complete := true
	for _, index := range []struct{ table, column, statement string }{
		{"messages", "from_email", `CREATE INDEX IF NOT EXISTS idx_messages_contact_email ON messages(lower(trim(from_email)), account_id)`},
		{"message_recipients", "email", `CREATE INDEX IF NOT EXISTS idx_recipients_contact_email ON message_recipients(lower(trim(email)), message_id)`},
		{"contact_observations", "message_count", ``},
		{"accounts", "user_id", ``},
	} {
		ok, err := columnExistsTx(tx, index.table, index.column)
		if err != nil {
			return err
		}
		if !ok {
			complete = false
			continue
		}
		if index.statement == "" {
			continue
		}
		started := time.Now()
		log.Printf("storage: schema 140: indexing %s by address", index.table)
		if _, err := tx.Exec(index.statement); err != nil {
			return fmt.Errorf("index contact mail: %w", err)
		}
		log.Printf("storage: schema 140: indexed %s in %s", index.table, time.Since(started).Round(time.Millisecond))
	}
	if complete {
		if err := recountAllContactObservationsTx(tx); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 140); err != nil {
		return err
	}
	return tx.Commit()
}

// contactMigrationRecountBatch is how many contacts the migration recounts
// between progress lines.
const contactMigrationRecountBatch = 1000

// recountAllContactObservationsTx recounts every contact, logging progress.
func recountAllContactObservationsTx(tx *sql.Tx) error {
	var total int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM contact_observations`).Scan(&total); err != nil {
		return err
	}
	started := time.Now()
	done, last := 0, int64(0)
	for {
		var upTo sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(rowid) FROM (SELECT rowid FROM contact_observations WHERE rowid > ? ORDER BY rowid LIMIT ?)`, last, contactMigrationRecountBatch).Scan(&upTo); err != nil {
			return err
		}
		if !upTo.Valid {
			break
		}
		result, err := tx.Exec(`UPDATE contact_observations SET message_count = `+contactMessageCountSQL("contact_observations.user_id", "contact_observations.normalized_email")+`
			WHERE rowid > ? AND rowid <= ?`, last, upTo.Int64)
		if err != nil {
			return fmt.Errorf("recount contact mail: %w", err)
		}
		changed, _ := result.RowsAffected()
		done += int(changed)
		last = upTo.Int64
		log.Printf("storage: schema 140: recounted %d/%d contacts", done, total)
	}
	log.Printf("storage: schema 140: recounted %d contacts in %s", done, time.Since(started).Round(time.Millisecond))
	return nil
}
