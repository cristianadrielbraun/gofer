package storage

import (
	"context"
	"fmt"
)

// searchAttachmentNamesSQL is a message's attachment names as the search
// index keeps them: real attachments only. An inline image is part of the
// body, so its file name, such as a signature's image001.png, isn't one.
const searchAttachmentNamesSQL = `COALESCE((SELECT group_concat(att.filename, ' ') FROM attachments att WHERE att.message_id = message_search.rowid AND COALESCE(att.inline, 0) = 0), '')`

// EnsureSearchAttachmentNames corrects search index entries made when inline
// images' names were indexed as attachment names. It rewrites only entries
// that still differ, so once the index is right it changes nothing.
func (db *DB) EnsureSearchAttachmentNames(ctx context.Context) error {
	if _, err := db.Write().ExecContext(ctx, `UPDATE message_search SET attachment_names = `+searchAttachmentNamesSQL+`
		WHERE rowid IN (SELECT message_id FROM attachments WHERE COALESCE(inline, 0) = 1)
		AND attachment_names IS NOT `+searchAttachmentNamesSQL); err != nil {
		return fmt.Errorf("correct search attachment names: %w", err)
	}
	return nil
}

// EnsureUserSearchIndexForStartup runs EnsureSearchAttachmentNames on one
// owner's store during the startup sweep.
func (r *AccountRouting) EnsureUserSearchIndexForStartup(ctx context.Context, owner string) error {
	return r.withStartupStore(ctx, owner, func(db *DB) error {
		return db.EnsureSearchAttachmentNames(ctx)
	})
}
