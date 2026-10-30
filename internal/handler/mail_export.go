package handler

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A mailbox export downloads every message of the chosen folders and writes
// each folder to its own mbox file. The download is a ZIP built from those
// files as it is sent, so the mail is never on disk twice.
const (
	mailExportFormat  = "gofer-mailbox-export"
	mailExportVersion = 1

	mailExportBatch = 100
	// After this many messages in a row fail, the server is likely down or
	// the account signed out, so the export pauses instead of skipping on.
	mailExportFailureStreak = 5
	mailExportSkipListMax   = 200
)

type mailExportJob struct {
	mailJobInfo
	AccountID    string                 `json:"account_id"`
	AccountEmail string                 `json:"account_email"`
	AccountName  string                 `json:"account_name"`
	Provider     string                 `json:"provider"`
	LocalOnly    bool                   `json:"local_only"`
	Folders      []*mailExportFolderJob `json:"folders"`
	// Skipped lists the first messages that could not be exported; Failed
	// counts them all. NotSaved counts messages left out of an export of
	// saved copies because Gofer only had their headers.
	Skipped  []mailExportSkip `json:"skipped,omitempty"`
	Failed   int              `json:"failed"`
	NotSaved int              `json:"not_saved"`
}

type mailExportFolderJob struct {
	FolderID string `json:"folder_id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Role     string `json:"role"`
	RemoteID string `json:"remote_id"`
	// File is the folder's mbox in the export's directory; Archive is its
	// name in the downloaded ZIP.
	File     string `json:"file"`
	Archive  string `json:"archive"`
	Total    int    `json:"total"`
	Exported int    `json:"exported"`
	Failed   int    `json:"failed"`
	NotSaved int    `json:"not_saved"`
	// LastMessageID and Size are where the export continues after a pause:
	// messages are written in ID order, and the mbox is cut back to Size.
	LastMessageID int64 `json:"last_message_id"`
	Size          int64 `json:"size"`
	Done          bool  `json:"done"`
}

type mailExportSkip struct {
	Folder    string `json:"folder"`
	MessageID int64  `json:"message_id"`
	Reason    string `json:"reason"`
}

func (j *mailExportJob) totals() (exported, total int) {
	for _, folder := range j.Folders {
		exported += folder.Exported + folder.Failed + folder.NotSaved
		total += folder.Total
	}
	if exported > total {
		total = exported
	}
	return exported, total
}

func (j *mailExportJob) written() int {
	written := 0
	for _, folder := range j.Folders {
		written += folder.Exported
	}
	return written
}

func (j *mailExportJob) current() *mailExportFolderJob {
	for _, folder := range j.Folders {
		if !folder.Done {
			return folder
		}
	}
	return nil
}

// StartMailExports continues exports that were running when Gofer stopped
// and lets new ones start.
func (h *Handler) StartMailExports(ctx context.Context) {
	if h.db == nil || h.mailExports != nil {
		return
	}
	h.mailExports = newMailJobManager(ctx, "mail export", mailJobDirectory(h.db, "mail-exports"),
		func() *mailExportJob { return &mailExportJob{} }, h.runMailExport)
}

// mailExportArchiveNames gives each folder a file name in the ZIP that is
// safe on any system and unique even where names only differ in case.
func mailExportArchiveNames(folders []*mailExportFolderJob) {
	used := map[string]bool{"manifest.json": true}
	for _, folder := range folders {
		var parts []string
		for _, part := range strings.Split(folder.Path, "/") {
			if part = mailExportSafeName(part); part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			parts = []string{"Folder"}
		}
		base := strings.Join(parts, "/")
		name := base + ".mbox"
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s (%d).mbox", base, n)
		}
		used[strings.ToLower(name)] = true
		folder.Archive = name
	}
}

func mailExportSafeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	return strings.Trim(name, " .")
}

func (h *Handler) withMailExportAccount(ctx context.Context, owner, accountID string, fn func(*storage.DB) error) error {
	return h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		return fn(db)
	})
}

// mailExportAccounts lists the owner's accounts that have mail.
func (h *Handler) mailExportAccounts(ctx context.Context, owner string) ([]models.Account, error) {
	var accounts []models.Account
	err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		all, err := db.GetAccounts(ctx, owner)
		for _, account := range all {
			// An account with only calendars or contacts has no mail to move.
			if account.Provider != providers.ProviderDAV {
				accounts = append(accounts, account)
			}
		}
		return err
	})
	return accounts, err
}

func (h *Handler) mailExportAccount(ctx context.Context, owner, accountID string) (models.Account, bool, error) {
	accounts, err := h.mailExportAccounts(ctx, owner)
	if err != nil {
		return models.Account{}, false, err
	}
	for _, account := range accounts {
		if account.ID == accountID && !account.IsDeleting {
			return account, true, nil
		}
	}
	return models.Account{}, false, nil
}

func (h *Handler) mailExportFolders(ctx context.Context, owner, accountID string) ([]storage.MailExportFolder, error) {
	var folders []storage.MailExportFolder
	err := h.withMailExportAccount(ctx, owner, accountID, func(db *storage.DB) error {
		var err error
		folders, err = db.MailExportFolders(ctx, accountID)
		return err
	})
	return folders, err
}

// createMailExport records a new export of the chosen folders and starts it.
func (h *Handler) createMailExport(ctx context.Context, owner string, account models.Account, folderIDs []string, localOnly bool) (*mailExportJob, error) {
	folders, err := h.mailExportFolders(ctx, owner, account.ID)
	if err != nil {
		return nil, err
	}
	chosen := map[string]bool{}
	for _, id := range folderIDs {
		chosen[id] = true
	}
	now := time.Now().UTC()
	job := &mailExportJob{
		mailJobInfo: mailJobInfo{ID: newMailJobID(), Owner: owner, State: mailJobRunning, CreatedAt: now, UpdatedAt: now},
		AccountID:   account.ID, AccountEmail: account.Email, AccountName: account.Name, Provider: account.Provider, LocalOnly: localOnly,
	}
	for _, folder := range folders {
		if !chosen[folder.ID] {
			continue
		}
		job.Folders = append(job.Folders, &mailExportFolderJob{
			FolderID: folder.ID, Name: folder.Name, Path: folder.Path, Role: folder.Role, RemoteID: folder.RemoteID,
			File: fmt.Sprintf("folder-%d.mbox", len(job.Folders)+1), Total: folder.Messages,
		})
	}
	if len(job.Folders) == 0 {
		return nil, errors.New("choose at least one folder")
	}
	mailExportArchiveNames(job.Folders)
	if err := h.mailExports.save(job); err != nil {
		return nil, err
	}
	h.mailExports.start(job)
	return job, nil
}

func (h *Handler) runMailExport(ctx context.Context, job *mailExportJob) {
	m := h.mailExports
	err := h.exportMailFolders(ctx, job)
	if err == nil && ctx.Err() == nil {
		if err = writeMailExportManifest(m.jobDir(job.ID), job); err != nil {
			err = fmt.Errorf("write manifest: %w", err)
		}
	}
	m.finish(ctx, job, err, mailExportErrorText)
}

func mailExportErrorText(err error) string {
	switch {
	case errors.Is(err, storage.ErrAccountRoute), errors.Is(err, os.ErrNotExist):
		return "The account is no longer available."
	default:
		return "Stopped after several messages in a row could not be downloaded: " + err.Error()
	}
}

func (h *Handler) exportMailFolders(ctx context.Context, job *mailExportJob) error {
	for {
		folder := job.current()
		if folder == nil {
			return nil
		}
		if err := h.exportMailFolder(ctx, job, folder); err != nil {
			return err
		}
	}
}

func (h *Handler) exportMailFolder(ctx context.Context, job *mailExportJob, folder *mailExportFolderJob) error {
	m := h.mailExports
	file, err := os.OpenFile(filepath.Join(m.jobDir(job.ID), folder.File), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	// Anything written after the last saved position belongs to messages
	// that will be written again.
	if err := file.Truncate(folder.Size); err != nil {
		return err
	}
	if _, err := file.Seek(folder.Size, io.SeekStart); err != nil {
		return err
	}
	out := bufio.NewWriterSize(file, 256<<10)
	streak := 0
	for {
		var batch []storage.MailExportMessage
		err := h.withMailExportAccount(ctx, job.Owner, job.AccountID, func(db *storage.DB) error {
			var err error
			batch, err = db.MailExportMessages(ctx, job.AccountID, folder.FolderID, folder.LastMessageID, mailExportBatch)
			return err
		})
		if err != nil {
			return err
		}
		for _, msg := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			raw, err := h.mailExportRaw(ctx, job, folder, msg)
			switch {
			case err == nil:
				streak = 0
				if err := writeMboxMessage(out, msg, raw); err != nil {
					return err
				}
				folder.Exported++
			case errors.Is(err, mail.ErrRawMessageNotSaved):
				folder.NotSaved++
				job.NotSaved++
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				var tooLarge *message.TooLargeError
				if !errors.As(err, &tooLarge) {
					streak++
					if streak >= mailExportFailureStreak {
						return err
					}
				}
				folder.Failed++
				job.Failed++
				if len(job.Skipped) < mailExportSkipListMax {
					job.Skipped = append(job.Skipped, mailExportSkip{Folder: folder.Path, MessageID: msg.ID, Reason: mailExportSkipReason(err)})
				}
			}
			folder.LastMessageID = msg.ID
		}
		if err := out.Flush(); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if folder.Size, err = file.Seek(0, io.SeekCurrent); err != nil {
			return err
		}
		if len(batch) < mailExportBatch {
			folder.Done = true
		}
		job.UpdatedAt = time.Now().UTC()
		if err := m.save(job); err != nil {
			return err
		}
		if folder.Done {
			return nil
		}
	}
}

func mailExportSkipReason(err error) string {
	var tooLarge *message.TooLargeError
	if errors.As(err, &tooLarge) {
		return "Larger than Gofer downloads"
	}
	return err.Error()
}

// mailExportRaw reads a message's original, from Gofer's saved copy when it
// has one and otherwise from the server, trying again a few times before
// giving up on it.
func (h *Handler) mailExportRaw(ctx context.Context, job *mailExportJob, folder *mailExportFolderJob, msg storage.MailExportMessage) ([]byte, error) {
	var raw []byte
	err := retryMailJobStep(ctx, func() error {
		var err error
		raw, err = h.readMailExportRaw(ctx, job, folder, msg)
		return err
	}, func(err error) bool {
		var tooLarge *message.TooLargeError
		return errors.Is(err, mail.ErrRawMessageNotSaved) || errors.As(err, &tooLarge)
	})
	if err == nil && len(raw) == 0 {
		err = errors.New("the server returned an empty message")
	}
	return raw, err
}

func (h *Handler) readMailExportRaw(ctx context.Context, job *mailExportJob, folder *mailExportFolderJob, msg storage.MailExportMessage) ([]byte, error) {
	return h.readAccountMessage(ctx, job.Owner, job.AccountID, folder.RemoteID, msg, !job.LocalOnly)
}

// readAccountMessage reads a message's original, from Gofer's saved copy
// when it has one and, with download, otherwise from the server. A download
// is not kept.
func (h *Handler) readAccountMessage(ctx context.Context, owner, accountID, folderRemoteID string, msg storage.MailExportMessage, download bool) ([]byte, error) {
	if h.userIMAP != nil {
		return h.userIMAP.ReadRawMessageForExport(ctx, owner, msg.ID, download)
	}
	if msg.RawPath != "" {
		raw, err := message.ReadSavedMessage(ctx, msg.RawPath)
		if err != nil || len(raw) > 0 {
			return raw, err
		}
	}
	if !download {
		return nil, mail.ErrRawMessageNotSaved
	}
	info := &storage.MessageFetchInfo{AccountID: accountID, FolderRemoteID: folderRemoteID, RemoteUID: msg.RemoteUID}
	return h.fetchBodyRemote(ctx, msg.ID, info)
}

// writeMboxMessage appends one message in the mboxrd format: a "From " line,
// the read and flagged state as the Status and X-Status headers mail apps
// read from mbox files, then the message with its own "From " lines quoted.
func writeMboxMessage(w io.Writer, msg storage.MailExportMessage, raw []byte) error {
	sender := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, msg.FromEmail)
	if sender == "" {
		sender = "MAILER-DAEMON"
	}
	date := msg.Date
	if date.IsZero() {
		date = time.Unix(0, 0)
	}
	newline := "\n"
	if i := bytes.IndexByte(raw, '\n'); i > 0 && raw[i-1] == '\r' {
		newline = "\r\n"
	}
	var head strings.Builder
	head.WriteString("From " + sender + " " + date.UTC().Format("Mon Jan _2 15:04:05 2006") + "\n")
	if msg.Read {
		head.WriteString("Status: RO" + newline)
	} else {
		head.WriteString("Status: O" + newline)
	}
	var status string
	if msg.Flagged {
		status += "F"
	}
	if msg.Draft {
		status += "T"
	}
	if status != "" {
		head.WriteString("X-Status: " + status + newline)
	}
	if _, err := io.WriteString(w, head.String()); err != nil {
		return err
	}
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line = raw[:i+1]
		}
		raw = raw[len(line):]
		if bytes.HasPrefix(bytes.TrimLeft(line, ">"), []byte("From ")) {
			if _, err := io.WriteString(w, ">"); err != nil {
				return err
			}
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		if len(raw) == 0 && !bytes.HasSuffix(line, []byte("\n")) {
			if _, err := io.WriteString(w, newline); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(w, newline)
	return err
}

type mailExportManifest struct {
	Format     string                     `json:"format"`
	Version    int                        `json:"version"`
	ExportedAt string                     `json:"exported_at"`
	Account    mailExportManifestAccount  `json:"account"`
	SavedOnly  bool                       `json:"saved_copies_only,omitempty"`
	Folders    []mailExportManifestFolder `json:"folders"`
	Skipped    []mailExportSkip           `json:"skipped,omitempty"`
}

type mailExportManifestAccount struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Provider string `json:"provider"`
}

type mailExportManifestFolder struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	File     string `json:"file"`
	Messages int    `json:"messages"`
	Failed   int    `json:"failed,omitempty"`
	NotSaved int    `json:"not_saved,omitempty"`
}

func writeMailExportManifest(dir string, job *mailExportJob) error {
	manifest := mailExportManifest{
		Format: mailExportFormat, Version: mailExportVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Account:   mailExportManifestAccount{Email: job.AccountEmail, Name: job.AccountName, Provider: job.Provider},
		SavedOnly: job.LocalOnly, Skipped: job.Skipped,
	}
	for _, folder := range job.Folders {
		manifest.Folders = append(manifest.Folders, mailExportManifestFolder{
			Path: folder.Path, Name: folder.Name, Role: folder.Role, File: folder.Archive,
			Messages: folder.Exported, Failed: folder.Failed, NotSaved: folder.NotSaved,
		})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o600)
}

// writeArchive sends a finished export as a ZIP of its mbox files and
// manifest.
func writeMailExportArchive(dir string, w io.Writer, job *mailExportJob) error {
	archive := zip.NewWriter(w)
	add := func(name, file string) error {
		source, err := os.Open(filepath.Join(dir, file))
		if err != nil {
			return err
		}
		defer source.Close()
		info, err := source.Stat()
		if err != nil {
			return err
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: info.ModTime()}
		header.SetMode(0o644)
		entry, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, source)
		return err
	}
	if err := add("manifest.json", "manifest.json"); err != nil {
		return err
	}
	for _, folder := range job.Folders {
		if err := add(folder.Archive, folder.File); err != nil {
			return err
		}
	}
	return archive.Close()
}

func mailExportArchiveName(job *mailExportJob) string {
	name := mailExportSafeName(strings.ReplaceAll(job.AccountEmail, "@", "-at-"))
	name = strings.Map(func(r rune) rune {
		if r == ' ' || r > 0x7e {
			return '-'
		}
		return r
	}, name)
	if name == "" {
		name = "account"
	}
	return "gofer-mail-" + name + "-" + job.CreatedAt.Format("2006-01-02") + ".zip"
}
