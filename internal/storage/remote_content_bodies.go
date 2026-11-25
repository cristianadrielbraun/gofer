package storage

import (
	"database/sql"
	"log"
	"path/filepath"
	"time"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

// remoteContentBodyName is the body Gofer saves when the reader chooses to
// always load a message's remote content.
const remoteContentBodyName = "body_remote.html"

// remoteContentBodyBatch is how many saved bodies the schema 152 migration
// reads between progress lines.
const remoteContentBodyBatch = 200

// migrateV151ToV152 lets the remote images that older versions failed to
// save load again. Saving a message's remote content marked every image it
// could not download as removed, and nothing loaded those again, not even
// allowing remote content once more. Each saved body gets the ordinary
// remote-image marks back, so the reader loads those images through Gofer
// like any other. Only messages whose remote content was saved have such a
// body, so few files are read. A body that can't be read or rewritten is
// logged and left as it is, so one damaged file doesn't stop Gofer from
// starting.
func migrateV151ToV152(db *sql.DB) error {
	tx, err := db.Begin()
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
	exists, err := columnExistsTx(tx, "messages", "body_html_path")
	if err != nil {
		return err
	}
	if exists {
		if err := restoreUnsavedRemoteImagesTx(tx); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 152); err != nil {
		return err
	}
	return tx.Commit()
}

// restoreUnsavedRemoteImagesTx rewrites the saved remote-content bodies that
// still hold images marked as removed, logging progress.
func restoreUnsavedRemoteImagesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, body_html_path FROM messages WHERE body_html_path LIKE ?`, "%"+remoteContentBodyName)
	if err != nil {
		return err
	}
	type savedBody struct {
		messageID int64
		path      string
	}
	var bodies []savedBody
	for rows.Next() {
		var body savedBody
		if err := rows.Scan(&body.messageID, &body.path); err != nil {
			rows.Close()
			return err
		}
		// LIKE treats the underscore as any character.
		if filepath.Base(body.path) == remoteContentBodyName {
			bodies = append(bodies, body)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(bodies) == 0 {
		return nil
	}
	started := time.Now()
	restored, failed := 0, 0
	for i, body := range bodies {
		changed, err := store.RewriteMessageFile(body.path, mailmessage.RestoreUnsavedRemoteImages)
		switch {
		case err != nil:
			failed++
			log.Printf("storage: schema 152: restore remote images of message %d: %v", body.messageID, err)
		case changed:
			restored++
		}
		if done := i + 1; done%remoteContentBodyBatch == 0 && done < len(bodies) {
			log.Printf("storage: schema 152: checked %d/%d saved remote-content bodies", done, len(bodies))
		}
	}
	log.Printf("storage: schema 152: checked %d saved remote-content bodies in %s; restored images in %d, %d could not be read or rewritten",
		len(bodies), time.Since(started).Round(time.Millisecond), restored, failed)
	return nil
}
