package handler

import (
	"context"
	"errors"
	"sync"
)

// localFoldersNoDelete answers a request to delete the Local folders account,
// which every user keeps. It can be hidden instead.
const localFoldersNoDelete = "Local folders can't be deleted. You can hide them instead."

// errLocalFoldersNoWriter refuses copying mail into Local folders, which
// needs its own writer.
var errLocalFoldersNoWriter = errors.New("mail can't be copied into Local folders yet")

// localFoldersReady holds the users whose Local folders account is known to
// exist, so the mailbox doesn't look for it on every page.
var localFoldersReady sync.Map

// ensureLocalFolders makes the user's Local folders account the first time
// they open their mail.
func (h *Handler) ensureLocalFolders(ctx context.Context, userID string) error {
	if _, ok := localFoldersReady.Load(h.localFoldersKey(userID)); ok {
		return nil
	}
	var err error
	if h.userStorage != nil {
		err = h.userStorage.EnsureLocalFolders(ctx, userID)
	} else if h.db != nil {
		_, err = h.db.EnsureLocalFoldersAccount(ctx, userID)
	}
	if err == nil {
		localFoldersReady.Store(h.localFoldersKey(userID), struct{}{})
	}
	return err
}

// localFoldersKey tells apart users of different handlers, such as tests
// that each open their own database.
func (h *Handler) localFoldersKey(userID string) any {
	if h.userStorage != nil {
		return [2]any{h.userStorage, userID}
	}
	return [2]any{h.db, userID}
}
