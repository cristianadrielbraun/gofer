package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A mail transfer copies the messages of chosen folders from one account
// into folders of another. A move then deletes each message from the first
// account once its copy is saved, which puts it in that account's Trash.
const (
	mailTransferBatch = 100
	// Deleted messages are sent to the delete action in groups this size.
	mailTransferDeleteBatch = 200
)

type mailTransferFolder struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
	Role     string `json:"role"`
	RemoteID string `json:"remote_id"`
	Total    int    `json:"total"`

	// Target, NewName, TargetRemote, TargetFolderID and TargetName say where
	// the messages go, as for an import.
	// MessageIDs, when set, are the only messages of the folder to transfer,
	// as chosen in the message list.
	MessageIDs []int64 `json:"message_ids,omitempty"`

	Target         string `json:"target"`
	NewName        string `json:"new_name,omitempty"`
	TargetRemote   string `json:"target_remote,omitempty"`
	TargetFolderID string `json:"target_folder_id,omitempty"`
	TargetName     string `json:"target_name"`

	LastMessageID int64 `json:"last_message_id"`
	Done          int   `json:"done"`
	Copied        int   `json:"copied"`
	Duplicates    int   `json:"duplicates"`
	Failed        int   `json:"failed"`
	Deleted       int   `json:"deleted"`
	Finished      bool  `json:"finished"`
}

type mailTransferDelete struct {
	ID     int64  `json:"id"`
	Folder string `json:"folder"`
}

type mailTransferJob struct {
	mailJobInfo
	SourceAccountID string                `json:"source_account_id"`
	SourceEmail     string                `json:"source_email"`
	TargetAccountID string                `json:"target_account_id"`
	TargetEmail     string                `json:"target_email"`
	TargetProvider  string                `json:"target_provider"`
	Move            bool                  `json:"move"`
	Folders         []*mailTransferFolder `json:"folders"`
	// PendingDelete holds moved messages whose copies are saved but which
	// are not yet deleted from the first account.
	PendingDelete []mailTransferDelete `json:"pending_delete,omitempty"`
	Skipped       []mailImportSkip     `json:"skipped,omitempty"`
}

func (j *mailTransferJob) totals() (done, total, copied, duplicates, failed, deleted int) {
	for _, folder := range j.Folders {
		done += folder.Done
		total += folder.Total
		copied += folder.Copied
		duplicates += folder.Duplicates
		failed += folder.Failed
		deleted += folder.Deleted
	}
	if done > total {
		total = done
	}
	return
}

func (j *mailTransferJob) current() *mailTransferFolder {
	for _, folder := range j.Folders {
		if !folder.Finished {
			return folder
		}
	}
	return nil
}

// StartMailTransfers continues transfers that were running when Gofer
// stopped and lets new ones start.
func (h *Handler) StartMailTransfers(ctx context.Context) {
	if h.db == nil || h.mailTransfers != nil {
		return
	}
	h.mailTransfers = newMailJobManager(ctx, "mail transfer", mailJobDirectory(h.db, "mail-transfers"),
		func() *mailTransferJob { return &mailTransferJob{} }, h.runMailTransfer)
}

var (
	errMailTransferSameAccount = errors.New("same account")
	errMailTransferNoFolders   = errors.New("no folders")
)

// createMailTransfer records which folders go where and starts copying.
func (h *Handler) createMailTransfer(ctx context.Context, owner string, source, target models.Account, move bool, form url.Values) (*mailTransferJob, error) {
	if source.ID == target.ID {
		return nil, errMailTransferSameAccount
	}
	sourceFolders, err := h.mailExportFolders(ctx, owner, source.ID)
	if err != nil {
		return nil, err
	}
	targetFolders, err := h.mailExportFolders(ctx, owner, target.ID)
	if err != nil {
		return nil, err
	}
	targets := map[string]storage.MailExportFolder{}
	for _, folder := range mailImportTargets(target.Provider, targetFolders) {
		targets[folder.ID] = folder
	}
	chosen := map[string]bool{}
	for _, id := range form["folder"] {
		chosen[id] = true
	}
	now := time.Now().UTC()
	job := &mailTransferJob{
		mailJobInfo:     mailJobInfo{ID: newMailJobID(), Owner: owner, State: mailJobRunning, CreatedAt: now, UpdatedAt: now},
		SourceAccountID: source.ID, SourceEmail: source.Email,
		TargetAccountID: target.ID, TargetEmail: target.Email, TargetProvider: target.Provider, Move: move,
	}
	for _, folder := range sourceFolders {
		if !chosen[folder.ID] {
			continue
		}
		entry := &mailTransferFolder{FolderID: folder.ID, Path: folder.Path, Role: folder.Role, RemoteID: folder.RemoteID, Total: folder.Messages}
		entry.Target = form.Get("target:" + folder.ID)
		switch entry.Target {
		case mailImportTargetNew:
			name := strings.Trim(strings.TrimSpace(form.Get("new:"+folder.ID)), "/")
			if name == "" {
				return nil, errMailImportNoFolderName
			}
			entry.NewName, entry.TargetName = name, name
		default:
			destination, found := targets[entry.Target]
			if !found {
				return nil, errMailImportNoTarget
			}
			entry.TargetFolderID, entry.TargetName, entry.TargetRemote = destination.ID, destination.Path, mailImportFolderRemote(target.Provider, destination)
		}
		job.Folders = append(job.Folders, entry)
	}
	if len(job.Folders) == 0 {
		return nil, errMailTransferNoFolders
	}
	if err := h.mailTransfers.save(job); err != nil {
		return nil, err
	}
	h.mailTransfers.start(job)
	return job, nil
}

func (h *Handler) runMailTransfer(ctx context.Context, job *mailTransferJob) {
	err := h.transferMail(ctx, job)
	if ctx.Err() == nil {
		h.syncMailAccount(h.mailTransfers.ctx, job.Owner, job.TargetAccountID)
		if job.Move {
			h.syncMailAccount(h.mailTransfers.ctx, job.Owner, job.SourceAccountID)
		}
	}
	h.mailTransfers.finish(ctx, job, err, mailTransferErrorText)
}

var errMailTransferDelete = errors.New("delete moved messages")

func mailTransferErrorText(err error) string {
	switch {
	case errors.Is(err, storage.ErrAccountRoute), errors.Is(err, os.ErrNotExist):
		return "One of the accounts is no longer available."
	case errors.Is(err, errMailImportAccount):
		return "Gofer could not sign in to the account the messages go to. Check its settings, then continue."
	case errors.Is(err, errMailTransferDelete):
		return "The copies are saved, but the originals could not be deleted. Continue to try again."
	default:
		return "Stopped after several messages in a row could not be copied: " + err.Error()
	}
}

func (h *Handler) transferMail(ctx context.Context, job *mailTransferJob) error {
	// Messages copied before a pause are deleted before anything else.
	if err := h.deleteTransferredMessages(ctx, job); err != nil {
		return err
	}
	writer, err := h.mailAccountWriter(ctx, job.Owner, job.TargetAccountID)
	if err != nil {
		log.Printf("mail transfer %s: connect: %v", job.ID, err)
		return errMailImportAccount
	}
	defer writer.close()
	for {
		folder := job.current()
		if folder == nil {
			return nil
		}
		if err := h.transferMailFolder(ctx, job, folder, writer); err != nil {
			return err
		}
	}
}

func (h *Handler) transferMailFolder(ctx context.Context, job *mailTransferJob, folder *mailTransferFolder, writer mailImportWriter) error {
	m := h.mailTransfers
	if folder.Target == mailImportTargetNew && folder.TargetRemote == "" {
		remote, err := writer.ensureFolder(ctx, folder.NewName)
		if err != nil {
			return fmt.Errorf("create folder %s: %w", folder.NewName, err)
		}
		folder.TargetRemote = remote
		job.UpdatedAt = time.Now().UTC()
		if err := m.save(job); err != nil {
			return err
		}
	}
	existing := map[string]bool{}
	if folder.TargetFolderID != "" {
		err := h.withMailExportAccount(ctx, job.Owner, job.TargetAccountID, func(db *storage.DB) error {
			var err error
			existing, err = db.FolderMessageIDs(ctx, job.TargetAccountID, folder.TargetFolderID)
			return err
		})
		if err != nil {
			return err
		}
	}
	streak := 0
	for {
		var batch []storage.MailExportMessage
		var chosen []int64
		if len(folder.MessageIDs) > 0 {
			chosen = mailTransferNextIDs(folder.MessageIDs, folder.LastMessageID, mailTransferBatch)
		}
		err := h.withMailExportAccount(ctx, job.Owner, job.SourceAccountID, func(db *storage.DB) error {
			var err error
			if len(folder.MessageIDs) > 0 {
				batch, err = db.MailExportMessagesByID(ctx, job.SourceAccountID, folder.FolderID, chosen)
			} else {
				batch, err = db.MailExportMessages(ctx, job.SourceAccountID, folder.FolderID, folder.LastMessageID, mailTransferBatch)
			}
			return err
		})
		if err != nil {
			return err
		}
		for _, source := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			saved, subject, err := h.transferMailMessage(ctx, job, folder, source, writer, existing)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				var tooLarge *message.TooLargeError
				if !errors.As(err, &tooLarge) && !errors.Is(err, errMailImportTooLarge) {
					streak++
					if streak >= mailImportFailureStreak {
						return err
					}
				}
				folder.Failed++
				if len(job.Skipped) < mailImportSkipListMax {
					job.Skipped = append(job.Skipped, mailImportSkip{Source: folder.Path, Subject: subject, Reason: mailExportSkipReason(err)})
				}
			} else {
				streak = 0
			}
			if saved && job.Move {
				job.PendingDelete = append(job.PendingDelete, mailTransferDelete{ID: source.ID, Folder: folder.FolderID})
			}
			folder.Done++
			folder.LastMessageID = source.ID
			// Saved after every message so the transfers page counts up
			// as it goes and a restart picks up at the next message.
			job.UpdatedAt = time.Now().UTC()
			if err := m.save(job); err != nil {
				return err
			}
		}
		if len(folder.MessageIDs) > 0 {
			// Chosen messages that left the folder meanwhile are passed over.
			if len(chosen) > 0 {
				folder.LastMessageID = max(folder.LastMessageID, chosen[len(chosen)-1])
			}
			folder.Done = len(mailTransferNextIDs(folder.MessageIDs, 0, len(folder.MessageIDs))) - len(mailTransferNextIDs(folder.MessageIDs, folder.LastMessageID, len(folder.MessageIDs)))
			if len(chosen) < mailTransferBatch {
				folder.Finished = true
			}
		} else if len(batch) < mailTransferBatch {
			folder.Finished = true
		}
		job.UpdatedAt = time.Now().UTC()
		if err := m.save(job); err != nil {
			return err
		}
		if err := h.deleteTransferredMessages(ctx, job); err != nil {
			return err
		}
		if folder.Finished {
			return nil
		}
	}
}

// transferMailMessage copies one message unless the folder has it already.
// saved is set when the destination has the message afterwards.
func (h *Handler) transferMailMessage(ctx context.Context, job *mailTransferJob, folder *mailTransferFolder, source storage.MailExportMessage, writer mailImportWriter, existing map[string]bool) (saved bool, subject string, err error) {
	var raw []byte
	err = retryMailJobStep(ctx, func() error {
		var err error
		raw, err = h.readAccountMessage(ctx, job.Owner, job.SourceAccountID, folder.RemoteID, source, true)
		return err
	}, func(err error) bool {
		var tooLarge *message.TooLargeError
		return errors.As(err, &tooLarge) || errors.Is(err, mail.ErrRawMessageNotSaved)
	})
	if err != nil {
		return false, "", err
	}
	msg := prepareMailImportMessage(raw, "")
	msg.Read, msg.Flagged, msg.Draft = source.Read, source.Flagged, source.Draft
	if msg.Date.IsZero() {
		msg.Date = source.Date
	}
	if msg.MessageID != "" && existing[msg.MessageID] {
		folder.Duplicates++
		return true, msg.Subject, nil
	}
	if len(msg.Raw) > mailImportMaxMessageBytes {
		return false, msg.Subject, errMailImportTooLarge
	}
	err = retryMailJobStep(ctx, func() error { return writer.upload(ctx, folder.TargetRemote, msg) }, mailImportFinalError)
	if err != nil {
		return false, msg.Subject, err
	}
	folder.Copied++
	if msg.MessageID != "" {
		existing[msg.MessageID] = true
	}
	return true, msg.Subject, nil
}

// deleteTransferredMessages deletes moved messages from the first account,
// through the same action as deleting them in the message list.
func (h *Handler) deleteTransferredMessages(ctx context.Context, job *mailTransferJob) error {
	for len(job.PendingDelete) > 0 {
		folderID := job.PendingDelete[0].Folder
		var ids []int64
		count := 0
		for count < len(job.PendingDelete) && count < mailTransferDeleteBatch && job.PendingDelete[count].Folder == folderID {
			ids = append(ids, job.PendingDelete[count].ID)
			count++
		}
		deleted, err := h.deleteFolderMessages(ctx, job.Owner, folderID, ids)
		if err != nil {
			log.Printf("mail transfer %s: delete: %v", job.ID, err)
			return errMailTransferDelete
		}
		for _, folder := range job.Folders {
			if folder.FolderID == folderID {
				folder.Deleted += deleted
			}
		}
		job.PendingDelete = job.PendingDelete[count:]
		job.UpdatedAt = time.Now().UTC()
		if err := h.mailTransfers.save(job); err != nil {
			return err
		}
	}
	return nil
}

// deleteFolderMessages deletes messages from a folder the way the message
// list does, and returns how many it deleted. Messages already gone from the
// folder are skipped.
func (h *Handler) deleteFolderMessages(ctx context.Context, owner, folderID string, ids []int64) (int, error) {
	status, err := h.deleteMessagesAs(ctx, owner, folderID, ids)
	if err != nil {
		return 0, err
	}
	if status == http.StatusOK {
		return len(ids), nil
	}
	if len(ids) == 1 {
		return 0, nil
	}
	// One message that is gone refuses the whole group, so delete the rest
	// one at a time.
	deleted := 0
	for _, id := range ids {
		status, err := h.deleteMessagesAs(ctx, owner, folderID, []int64{id})
		if err != nil {
			return deleted, err
		}
		if status == http.StatusOK {
			deleted++
		}
	}
	return deleted, nil
}

func (h *Handler) deleteMessagesAs(ctx context.Context, owner, folderID string, ids []int64) (int, error) {
	targets := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, map[string]string{"id": strconv.FormatInt(id, 10)})
	}
	body, err := json.Marshal(map[string]any{"targets": targets, "folder_id": folderID})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(auth.ContextWithUser(ctx, &auth.User{ID: owner, Username: owner}), http.MethodPost, "/api/messages/delete", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	response := &userMutationResponse{header: make(http.Header)}
	if h.userIMAP != nil {
		h.userMessageMutationHandler(func(local *Handler) http.HandlerFunc { return local.handleDeleteMessages })(response, req)
	} else {
		h.handleDeleteMessages(response, req)
	}
	switch {
	case response.status == 0, response.status == http.StatusOK:
		return http.StatusOK, nil
	case response.status == http.StatusNotFound:
		return response.status, nil
	}
	return response.status, fmt.Errorf("delete answered %d: %s", response.status, strings.TrimSpace(response.body.String()))
}

// mailTransferNextIDs returns up to limit of the sorted ids after afterID.
func mailTransferNextIDs(ids []int64, afterID int64, limit int) []int64 {
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > afterID })
	end := min(len(ids), start+limit)
	return ids[start:end]
}

// mailTransferMessages starts a transfer of messages chosen in the message
// list into one folder of another account. ids maps each of the first
// account's folders to the messages chosen in it.
func (h *Handler) mailTransferMessages(ctx context.Context, owner string, source, target models.Account, destination storage.MailExportFolder, ids map[string][]int64, move bool) (*mailTransferJob, error) {
	if source.ID == target.ID {
		return nil, errMailTransferSameAccount
	}
	folders, err := h.mailExportFolders(ctx, owner, source.ID)
	if err != nil {
		return nil, err
	}
	targetRemote := mailImportFolderRemote(target.Provider, destination)
	now := time.Now().UTC()
	job := &mailTransferJob{
		mailJobInfo:     mailJobInfo{ID: newMailJobID(), Owner: owner, State: mailJobRunning, CreatedAt: now, UpdatedAt: now},
		SourceAccountID: source.ID, SourceEmail: source.Email,
		TargetAccountID: target.ID, TargetEmail: target.Email, TargetProvider: target.Provider, Move: move,
	}
	for _, folder := range folders {
		chosen := ids[folder.ID]
		if len(chosen) == 0 {
			continue
		}
		chosen = append([]int64(nil), chosen...)
		slices.Sort(chosen)
		chosen = slices.Compact(chosen)
		job.Folders = append(job.Folders, &mailTransferFolder{
			FolderID: folder.ID, Path: folder.Path, Role: folder.Role, RemoteID: folder.RemoteID, Total: len(chosen), MessageIDs: chosen,
			Target: destination.ID, TargetFolderID: destination.ID, TargetRemote: targetRemote, TargetName: destination.Path,
		})
	}
	if len(job.Folders) == 0 {
		return nil, errMailTransferNoFolders
	}
	if err := h.mailTransfers.save(job); err != nil {
		return nil, err
	}
	h.mailTransfers.start(job)
	return job, nil
}
