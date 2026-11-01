package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// localImportWriter saves imported or transferred mail straight into Local
// folders, as sync saves a message it fetched: its raw message, bodies,
// attachments, recipients and thread. Folders are named by their ID.
type localImportWriter struct {
	h         *Handler
	owner     string
	accountID string
}

func (w *localImportWriter) close() {}

// ensureFolder finds, or makes, the folder at path, a name with "/" between
// a folder and the one inside it, and returns its ID.
func (w *localImportWriter) ensureFolder(ctx context.Context, path string) (string, error) {
	parentID := ""
	for _, part := range strings.Split(path, "/") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		var id string
		err := w.h.withProfileAccount(ctx, w.owner, w.accountID, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			id, err = db.FolderIDByName(ctx, w.accountID, parentID, name)
			return err
		})
		if err != nil {
			return "", err
		}
		if id == "" {
			change, err := w.h.createFolder(ctx, w.owner, w.accountID, parentID, name)
			if err != nil {
				return "", err
			}
			id = change.ID
		}
		parentID = id
	}
	if parentID == "" {
		return "", errors.New("the folder has no name")
	}
	return parentID, nil
}

// upload saves msg in the Local folder folderID. A failure takes away what
// it had saved.
func (w *localImportWriter) upload(ctx context.Context, folderID string, msg mailImportMessage) error {
	blobs := w.h.blobStore
	if blobs == nil {
		return errors.New("no message storage")
	}
	copyID := uuid.NewString()
	var msgID int64
	err := w.h.withProfileAccount(ctx, w.owner, w.accountID, func(_ *config.AccountStore, db *storage.DB) error {
		ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
			AccountID: w.accountID, FolderID: folderID, ProviderMessageID: copyID,
			InternetMessageID: msg.MessageID, Subject: msg.Subject,
			DateSent: msg.Date, DateReceived: msg.Date,
			IsRead: msg.Read, IsStarred: msg.Flagged, IsDraft: msg.Draft,
			SizeBytes: int64(len(msg.Raw)),
		}})
		if err != nil {
			return err
		}
		msgID = ids[copyID]
		return nil
	})
	if err != nil {
		return err
	}
	if msgID == 0 {
		return errors.New("the message could not be saved")
	}
	saved := false
	defer func() {
		if !saved {
			w.discard(context.WithoutCancel(ctx), msgID, folderID)
		}
	}()

	candidate, err := blobs.NewMessageVersion()
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = candidate.DeleteMessage(w.accountID, msgID)
		}
	}()
	parsed, err := message.ParseMessage(ctx, bytes.NewReader(msg.Raw), candidate, w.accountID, msgID)
	if err != nil {
		return fmt.Errorf("read the message: %w", err)
	}
	cache, err := mail.PrepareMessageBody(ctx, candidate, w.accountID, msgID, parsed)
	if err != nil {
		return err
	}
	cache.ProviderMessageID, cache.ProviderType = copyID, storage.MessageMutationProviderLocal
	err = w.h.withProfileAccount(ctx, w.owner, w.accountID, func(_ *config.AccountStore, db *storage.DB) error {
		return db.SaveMessageBodyCache(ctx, msgID, w.accountID, cache)
	})
	if err != nil {
		return err
	}
	published, saved = true, true
	return nil
}

// discard takes away the copy a failed upload left in folderID.
func (w *localImportWriter) discard(ctx context.Context, msgID int64, folderID string) {
	_ = w.h.withProfileAccount(ctx, w.owner, w.accountID, func(_ *config.AccountStore, db *storage.DB) error {
		return db.PermanentlyDeleteMessagesAndQueueForUser(ctx, []int64{msgID}, folderID, w.owner)
	})
}
