package handler

import (
	"net/mail"
	"path/filepath"
	"testing"
	"time"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestSaveSentMessageKeepsMultipleMessagesWithoutRemoteUIDs(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID:         "victim-sent",
		AccountID:  "victim-account",
		RemoteID:   "Sent",
		Name:       "Sent",
		Role:       "sent",
		Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	for _, id := range []string{"<first@gofer>", "<second@gofer>"} {
		h.saveSentMessage(t.Context(), "victim-account", &message.OutgoingMessage{
			FromName:  "Owner",
			FromEmail: "owner@example.com",
			To:        []*mail.Address{{Address: "recipient@example.com"}},
			Subject:   id,
			TextBody:  "sent body",
			MessageID: id,
			Date:      time.Now().UTC(),
		})
	}

	var count, nullUIDs int
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), SUM(CASE WHEN remote_uid IS NULL THEN 1 ELSE 0 END)
		FROM message_folder_state
		WHERE folder_id = 'victim-sent'`).Scan(&count, &nullUIDs); err != nil {
		t.Fatalf("query sent memberships: %v", err)
	}
	if count != 2 || nullUIDs != 2 {
		t.Fatalf("sent memberships count=%d nullUIDs=%d, want 2 and 2", count, nullUIDs)
	}
}
