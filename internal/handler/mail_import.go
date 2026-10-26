package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

// A mail import uploads messages from mbox and eml files, or a ZIP of them
// such as a Gofer export, into folders of an account. Messages already in
// the folder are skipped.
const (
	mailImportTargetSkip = "skip"
	mailImportTargetNew  = "new"

	mailImportKindMbox = "mbox"
	mailImportKindEML  = "eml"

	// Progress is saved after this many messages, so a restart repeats few.
	mailImportSaveEvery     = 25
	mailImportFailureStreak = 5
	mailImportSkipListMax   = 200
)

type mailImportFile struct {
	Name   string `json:"name"`
	Stored string `json:"stored"`
	Size   int64  `json:"size"`
}

// mailImportPart is an uploaded file, or an entry in an uploaded ZIP.
type mailImportPart struct {
	File  int    `json:"file"`
	Entry string `json:"entry,omitempty"`
}

// mailImportSource is a folder's worth of messages in the upload: an mbox
// file, or the eml files of one folder.
type mailImportSource struct {
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	Role     string           `json:"role,omitempty"`
	Kind     string           `json:"kind"`
	Parts    []mailImportPart `json:"parts"`
	Messages int              `json:"messages"`

	// Target is the folder ID chosen for the messages, "new" for a new
	// folder named NewName, or "skip".
	Target  string `json:"target,omitempty"`
	NewName string `json:"new_name,omitempty"`
	// TargetRemote is the server's name for the folder, or the Gmail label;
	// TargetFolderID is Gofer's folder, to find messages already there.
	TargetRemote   string `json:"target_remote,omitempty"`
	TargetFolderID string `json:"target_folder_id,omitempty"`
	TargetName     string `json:"target_name,omitempty"`

	// Done counts the messages handled, and Offset is where the next one
	// starts in an mbox file.
	Done       int   `json:"done"`
	Offset     int64 `json:"offset"`
	Imported   int   `json:"imported"`
	Duplicates int   `json:"duplicates"`
	Failed     int   `json:"failed"`
	Finished   bool  `json:"finished"`
}

type mailImportSkip struct {
	Source  string `json:"source"`
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

type mailImportJob struct {
	mailJobInfo
	Files   []mailImportFile    `json:"files"`
	Sources []*mailImportSource `json:"sources"`
	// ExportedFrom is the account a Gofer export came from.
	ExportedFrom string `json:"exported_from,omitempty"`

	AccountID    string           `json:"account_id,omitempty"`
	AccountEmail string           `json:"account_email,omitempty"`
	Provider     string           `json:"provider,omitempty"`
	Skipped      []mailImportSkip `json:"skipped,omitempty"`
}

func (j *mailImportJob) totals() (done, total, imported, duplicates, failed int) {
	for _, source := range j.Sources {
		if source.Target == mailImportTargetSkip {
			continue
		}
		done += source.Done
		total += source.Messages
		imported += source.Imported
		duplicates += source.Duplicates
		failed += source.Failed
	}
	if done > total {
		total = done
	}
	return
}

func (j *mailImportJob) current() *mailImportSource {
	for _, source := range j.Sources {
		if !source.Finished && source.Target != mailImportTargetSkip {
			return source
		}
	}
	return nil
}

// StartMailImports continues imports that were running when Gofer stopped
// and lets new ones start.
func (h *Handler) StartMailImports(ctx context.Context) {
	if h.db == nil || h.mailImports != nil {
		return
	}
	h.mailImports = newMailJobManager(ctx, "mail import", mailJobDirectory(h.db, "mail-imports"),
		func() *mailImportJob { return &mailImportJob{} }, h.runMailImport)
}

// saveMailImportUpload stores the uploaded files of a new import and finds
// the messages in them.
func (h *Handler) saveMailImportUpload(owner string, reader *multipart.Reader) (*mailImportJob, error) {
	now := time.Now().UTC()
	job := &mailImportJob{mailJobInfo: mailJobInfo{ID: newMailJobID(), Owner: owner, State: mailJobReview, CreatedAt: now, UpdatedAt: now}}
	dir := h.mailImports.jobDir(job.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if part.FormName() != "files" || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
		stored := fmt.Sprintf("upload-%d%s", len(job.Files)+1, strings.ToLower(filepath.Ext(name)))
		file, err := os.OpenFile(filepath.Join(dir, stored), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = part.Close()
			return nil, err
		}
		size, err := io.Copy(file, part)
		_ = part.Close()
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		job.Files = append(job.Files, mailImportFile{Name: name, Stored: stored, Size: size})
	}
	if len(job.Files) == 0 {
		return nil, errMailImportNoFiles
	}
	if err := scanMailImport(dir, job); err != nil {
		return nil, err
	}
	if len(job.Sources) == 0 {
		return nil, errMailImportNoMessages
	}
	if err := h.mailImports.save(job); err != nil {
		return nil, err
	}
	keep = true
	return job, nil
}

var (
	errMailImportNoFiles    = errors.New("no files uploaded")
	errMailImportNoMessages = errors.New("no messages found")
)

// scanMailImport finds the folders of messages in the uploaded files and
// counts their messages.
func scanMailImport(dir string, job *mailImportJob) error {
	var loose []mailImportPart
	for i, file := range job.Files {
		stored := filepath.Join(dir, file.Stored)
		switch strings.ToLower(filepath.Ext(file.Name)) {
		case ".zip":
			if err := scanMailImportZip(stored, i, job); err != nil {
				return err
			}
		case ".eml":
			loose = append(loose, mailImportPart{File: i})
		default:
			count, err := countMboxFile(stored)
			if err != nil {
				return err
			}
			if count > 0 {
				name := strings.TrimSuffix(file.Name, filepath.Ext(file.Name))
				job.Sources = append(job.Sources, &mailImportSource{
					Name: file.Name, Path: name, Role: guessMailImportRole(name), Kind: mailImportKindMbox,
					Parts: []mailImportPart{{File: i}}, Messages: count,
				})
			}
		}
	}
	if len(loose) > 0 {
		job.Sources = append(job.Sources, &mailImportSource{
			Name: mailImportCount(len(loose), "message file", "message files"), Path: "Imported", Kind: mailImportKindEML, Parts: loose, Messages: len(loose),
		})
	}
	return nil
}

func countMboxFile(name string) (int, error) {
	file, err := os.Open(name)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return countMbox(file)
}

func countMbox(in io.Reader) (int, error) {
	reader := newMboxReader(in, 0)
	count := 0
	for {
		_, err := reader.Next(false)
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		count++
	}
}

func scanMailImportZip(name string, file int, job *mailImportJob) error {
	archive, err := zip.OpenReader(name)
	if err != nil {
		return errMailImportBadZip
	}
	defer archive.Close()
	// A Gofer export names each folder and its role.
	described := map[string]mailExportManifestFolder{}
	for _, entry := range archive.File {
		if entry.Name != "manifest.json" {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			break
		}
		var manifest mailExportManifest
		err = json.NewDecoder(io.LimitReader(reader, 16<<20)).Decode(&manifest)
		_ = reader.Close()
		if err == nil && manifest.Format == mailExportFormat {
			job.ExportedFrom = manifest.Account.Email
			for _, folder := range manifest.Folders {
				described[folder.File] = folder
			}
		}
	}
	emlFolders := map[string][]mailImportPart{}
	var emlOrder []string
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || strings.HasPrefix(path.Base(entry.Name), ".") || strings.HasPrefix(entry.Name, "__MACOSX/") {
			continue
		}
		switch strings.ToLower(path.Ext(entry.Name)) {
		case ".mbox", ".mbx":
			reader, err := entry.Open()
			if err != nil {
				return errMailImportBadZip
			}
			count, err := countMbox(reader)
			_ = reader.Close()
			if err != nil {
				return errMailImportBadZip
			}
			if count == 0 {
				continue
			}
			source := &mailImportSource{
				Name: path.Base(entry.Name), Path: strings.TrimSuffix(entry.Name, path.Ext(entry.Name)), Kind: mailImportKindMbox,
				Parts: []mailImportPart{{File: file, Entry: entry.Name}}, Messages: count,
			}
			if folder, found := described[entry.Name]; found {
				source.Path, source.Role = folder.Path, folder.Role
			} else {
				source.Role = guessMailImportRole(source.Path)
			}
			source.Name = entry.Name
			job.Sources = append(job.Sources, source)
		case ".eml":
			folder := path.Dir(entry.Name)
			if _, found := emlFolders[folder]; !found {
				emlOrder = append(emlOrder, folder)
			}
			emlFolders[folder] = append(emlFolders[folder], mailImportPart{File: file, Entry: entry.Name})
		}
	}
	for _, folder := range emlOrder {
		parts := emlFolders[folder]
		sort.Slice(parts, func(i, k int) bool { return parts[i].Entry < parts[k].Entry })
		name := folder
		if folder == "." {
			name = "Imported"
		}
		job.Sources = append(job.Sources, &mailImportSource{
			Name: name + "/ (" + mailImportCount(len(parts), "message file", "message files") + ")", Path: name, Role: guessMailImportRole(name),
			Kind: mailImportKindEML, Parts: parts, Messages: len(parts),
		})
	}
	return nil
}

var errMailImportBadZip = errors.New("the ZIP file could not be read")

// guessMailImportRole tells the role of a folder from its name in other mail
// apps' exports.
func guessMailImportRole(name string) string {
	base := strings.ToLower(strings.TrimSpace(path.Base(strings.ReplaceAll(name, `\`, "/"))))
	switch base {
	case "inbox":
		return "inbox"
	case "sent", "sent mail", "sent items", "sent messages":
		return "sent"
	case "drafts", "draft":
		return "drafts"
	case "trash", "deleted items", "deleted messages", "bin":
		return "trash"
	case "spam", "junk", "junk e-mail", "junk email", "bulk mail":
		return "junk"
	case "archive", "archives", "all mail":
		return "archive"
	}
	return ""
}

func sameMailImportRole(a, b string) bool {
	normalize := func(role string) string {
		if role == "spam" {
			return "junk"
		}
		return role
	}
	return a != "" && normalize(a) == normalize(b)
}

// mailImportTargets are the folders of an account messages can go into.
func mailImportTargets(provider string, folders []storage.MailExportFolder) []storage.MailExportFolder {
	var targets []storage.MailExportFolder
	for _, folder := range folders {
		// Gmail and Outlook only take drafts made as drafts.
		if (provider == "gmail" || provider == "outlook") && folder.Role == "drafts" {
			continue
		}
		targets = append(targets, folder)
	}
	return targets
}

// suggestMailImportTarget picks the folder a source's messages likely
// belong in: the one with the same role, else the same name, else a new
// folder.
func suggestMailImportTarget(source *mailImportSource, targets []storage.MailExportFolder) (string, string) {
	if source.Role != "" && source.Role != "custom" {
		for _, folder := range targets {
			if sameMailImportRole(source.Role, folder.Role) {
				return folder.ID, ""
			}
		}
	}
	for _, folder := range targets {
		if strings.EqualFold(folder.Path, source.Path) || strings.EqualFold(folder.Name, source.Path) {
			return folder.ID, ""
		}
	}
	return mailImportTargetNew, source.Path
}

// mailImportAccountProblem says why messages can't be imported into an
// account, or nothing when they can.
func mailImportAccountProblem(account models.Account) string {
	switch {
	case account.ReconnectPending:
		return "Enter its password first."
	}
	return ""
}

// startMailImport records where each source goes and starts the import.
func (h *Handler) startMailImport(ctx context.Context, job *mailImportJob, account models.Account, form url.Values) error {
	folders, err := h.mailExportFolders(ctx, job.Owner, account.ID)
	if err != nil {
		return err
	}
	targets := map[string]storage.MailExportFolder{}
	for _, folder := range mailImportTargets(account.Provider, folders) {
		targets[folder.ID] = folder
	}
	chosen := 0
	for i, source := range job.Sources {
		key := fmt.Sprint(i)
		target := form.Get("target:" + key)
		source.Target, source.NewName, source.TargetRemote, source.TargetFolderID, source.TargetName = target, "", "", "", ""
		switch target {
		case mailImportTargetSkip:
			continue
		case mailImportTargetNew:
			name := strings.Trim(strings.TrimSpace(form.Get("new:"+key)), "/")
			if name == "" {
				return errMailImportNoFolderName
			}
			source.NewName, source.TargetName = name, name
		default:
			folder, found := targets[target]
			if !found {
				return errMailImportNoTarget
			}
			source.TargetFolderID, source.TargetName = folder.ID, folder.Path
			source.TargetRemote = mailImportFolderRemote(account.Provider, folder)
		}
		chosen++
	}
	if chosen == 0 {
		return errMailImportNothingChosen
	}
	now := time.Now().UTC()
	job.AccountID, job.AccountEmail, job.Provider = account.ID, account.Email, account.Provider
	job.State, job.UpdatedAt = mailJobRunning, now
	if err := h.mailImports.save(job); err != nil {
		return err
	}
	h.mailImports.start(job)
	return nil
}

var (
	errMailImportNoFolderName  = errors.New("name the new folder")
	errMailImportNoTarget      = errors.New("folder not found")
	errMailImportNothingChosen = errors.New("nothing chosen")
)

func (h *Handler) runMailImport(ctx context.Context, job *mailImportJob) {
	err := h.importMail(ctx, job)
	if ctx.Err() == nil {
		h.syncMailAccount(h.mailImports.ctx, job.Owner, job.AccountID)
	}
	h.mailImports.finish(ctx, job, err, mailImportErrorText)
}

func mailImportErrorText(err error) string {
	switch {
	case errors.Is(err, storage.ErrAccountRoute), errors.Is(err, os.ErrNotExist):
		return "The account is no longer available."
	case errors.Is(err, errMailImportAccount):
		return "Gofer could not sign in to the account. Check its settings, then continue."
	default:
		return "Stopped after several messages in a row could not be uploaded: " + err.Error()
	}
}

var errMailImportAccount = errors.New("account unavailable")

// syncMailAccount fetches an account's mail in the background, so messages
// a job added show.
func (h *Handler) syncMailAccount(ctx context.Context, owner, accountID string) {
	if h.userIMAP != nil {
		go func() {
			if err := h.userIMAP.Sync(ctx, owner, accountID); err != nil {
				log.Printf("sync %s after mail job: %v", accountID, err)
			}
		}()
		return
	}
	if h.syncer != nil {
		go h.syncer.SyncAccount(ctx, accountID)
	}
}

func (h *Handler) importMail(ctx context.Context, job *mailImportJob) error {
	writer, err := h.mailAccountWriter(ctx, job.Owner, job.AccountID)
	if err != nil {
		log.Printf("mail import %s: connect: %v", job.ID, err)
		return errMailImportAccount
	}
	defer writer.close()
	for {
		source := job.current()
		if source == nil {
			return nil
		}
		if err := h.importMailSource(ctx, job, source, writer); err != nil {
			return err
		}
	}
}

func (h *Handler) importMailSource(ctx context.Context, job *mailImportJob, source *mailImportSource, writer mailImportWriter) error {
	m := h.mailImports
	if source.Target == mailImportTargetNew && source.TargetRemote == "" {
		remote, err := writer.ensureFolder(ctx, source.NewName)
		if err != nil {
			return fmt.Errorf("create folder %s: %w", source.NewName, err)
		}
		source.TargetRemote = remote
		job.UpdatedAt = time.Now().UTC()
		if err := m.save(job); err != nil {
			return err
		}
	}
	existing := map[string]bool{}
	if source.TargetFolderID != "" {
		err := h.withMailExportAccount(ctx, job.Owner, job.AccountID, func(db *storage.DB) error {
			var err error
			existing, err = db.FolderMessageIDs(ctx, job.AccountID, source.TargetFolderID)
			return err
		})
		if err != nil {
			return err
		}
	}
	streak := 0
	lastSave := time.Now()
	handle := func(raw []byte, fromLine string, tooLarge bool, next int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg := prepareMailImportMessage(raw, fromLine)
		var err error
		switch {
		case tooLarge:
			err = errMailImportTooLarge
		case len(bytes.TrimSpace(msg.Raw)) == 0:
			err = errors.New("the message is empty")
		case msg.MessageID != "" && existing[msg.MessageID]:
			source.Duplicates++
		default:
			err = retryMailJobStep(ctx, func() error { return writer.upload(ctx, source.TargetRemote, msg) }, mailImportFinalError)
			if err == nil {
				source.Imported++
				streak = 0
				if msg.MessageID != "" {
					existing[msg.MessageID] = true
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, errMailImportTooLarge) {
				streak++
				if streak >= mailImportFailureStreak {
					return err
				}
			}
			source.Failed++
			if len(job.Skipped) < mailImportSkipListMax {
				job.Skipped = append(job.Skipped, mailImportSkip{Source: source.Name, Subject: msg.Subject, Reason: err.Error()})
			}
		}
		source.Done++
		source.Offset = next
		if source.Done%mailImportSaveEvery == 0 || time.Since(lastSave) > 2*time.Second {
			lastSave = time.Now()
			job.UpdatedAt = lastSave.UTC()
			return m.save(job)
		}
		return nil
	}
	var err error
	if source.Kind == mailImportKindEML {
		err = h.importMailEML(job, source, handle)
	} else {
		err = h.importMailMbox(job, source, handle)
	}
	if err != nil {
		return err
	}
	source.Finished = true
	job.UpdatedAt = time.Now().UTC()
	return m.save(job)
}

// openMailImportPart opens an uploaded file or an entry of an uploaded ZIP.
func (h *Handler) openMailImportPart(job *mailImportJob, part mailImportPart) (io.ReadCloser, error) {
	if part.File < 0 || part.File >= len(job.Files) {
		return nil, os.ErrNotExist
	}
	name := filepath.Join(h.mailImports.jobDir(job.ID), job.Files[part.File].Stored)
	if part.Entry == "" {
		return os.Open(name)
	}
	archive, err := zip.OpenReader(name)
	if err != nil {
		return nil, err
	}
	for _, entry := range archive.File {
		if entry.Name == part.Entry {
			reader, err := entry.Open()
			if err != nil {
				_ = archive.Close()
				return nil, err
			}
			return struct {
				io.Reader
				io.Closer
			}{reader, closeBoth{reader, archive}}, nil
		}
	}
	_ = archive.Close()
	return nil, os.ErrNotExist
}

type closeBoth struct{ first, second io.Closer }

func (c closeBoth) Close() error { return errors.Join(c.first.Close(), c.second.Close()) }

func (h *Handler) importMailMbox(job *mailImportJob, source *mailImportSource, handle func([]byte, string, bool, int64) error) error {
	in, err := h.openMailImportPart(job, source.Parts[0])
	if err != nil {
		return err
	}
	defer in.Close()
	if seeker, ok := in.(io.Seeker); ok {
		if _, err := seeker.Seek(source.Offset, io.SeekStart); err != nil {
			return err
		}
	} else if _, err := io.CopyN(io.Discard, in, source.Offset); err != nil {
		return err
	}
	reader := newMboxReader(in, source.Offset)
	for {
		msg, err := reader.Next(true)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := handle(msg.Raw, msg.FromLine, msg.TooLarge, msg.End); err != nil {
			return err
		}
	}
}

func (h *Handler) importMailEML(job *mailImportJob, source *mailImportSource, handle func([]byte, string, bool, int64) error) error {
	for source.Done < len(source.Parts) {
		in, err := h.openMailImportPart(job, source.Parts[source.Done])
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(in, int64(mailImportMaxMessageBytes)+1))
		_ = in.Close()
		if err != nil {
			return err
		}
		tooLarge := len(raw) > mailImportMaxMessageBytes
		if err := handle(raw, "", tooLarge, 0); err != nil {
			return err
		}
	}
	return nil
}

// mailImportFinalError reports errors that trying again won't fix.
func mailImportFinalError(err error) bool {
	var statusErr *goimap.Error
	var apiErr *mailImportAPIError
	var jmapErr *jmap.ImportError
	return errors.Is(err, errOutlookImportDraft) || errors.As(err, &statusErr) || errors.As(err, &jmapErr) || (errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests && apiErr.Status != http.StatusUnauthorized)
}

// mailImportWriter uploads messages into an account's folders.
type mailImportWriter interface {
	// ensureFolder creates a folder, or finds one already named so, and
	// returns its name or label for upload.
	ensureFolder(ctx context.Context, name string) (string, error)
	upload(ctx context.Context, remote string, msg mailImportMessage) error
	close()
}

// mailAccountAccess returns what signing in to an account's mail takes:
// its settings, and its password unless it's Gmail or Outlook, which sign
// in with mailImportToken.
func (h *Handler) mailAccountAccess(ctx context.Context, owner, accountID string) (*models.AccountConfig, string, error) {
	var cfg *models.AccountConfig
	var password string
	err := h.withProfileAccount(ctx, owner, accountID, func(accounts *config.AccountStore, _ *storage.DB) error {
		var err error
		cfg, err = accounts.GetConfig(ctx, accountID)
		if err != nil {
			return err
		}
		if cfg.Provider == "gmail" || cfg.Provider == "outlook" {
			return nil
		}
		if h.userIMAP == nil {
			password, err = h.resolvePassword(ctx, cfg, accountID)
			return err
		}
		if cfg.AuthMethod != "plain" {
			return errAccountSignInUnsupported
		}
		password, err = accounts.DecryptPassword(ctx, accountID)
		return err
	})
	return cfg, password, err
}

var errAccountSignInUnsupported = errors.New("this account's sign-in isn't supported here")

// mailAccountWriter connects to an account to upload messages into it.
func (h *Handler) mailAccountWriter(ctx context.Context, owner, accountID string) (mailImportWriter, error) {
	cfg, password, err := h.mailAccountAccess(ctx, owner, accountID)
	if err != nil {
		return nil, err
	}
	switch cfg.Provider {
	case "gmail":
		return &gmailImportWriter{mailImportCaller{token: h.mailImportToken(owner, accountID)}}, nil
	case "outlook":
		return &outlookImportWriter{mailImportCaller{token: h.mailImportToken(owner, accountID)}}, nil
	case providers.ProviderJMAP:
		return &jmapImportWriter{mail.NewJMAPImport(cfg, password)}, nil
	}
	return &imapImportWriter{cfg: cfg, password: password}, nil
}

// jmapImportWriter uploads messages to a JMAP account, into mailboxes named
// by id.
type jmapImportWriter struct {
	imports *mail.JMAPImport
}

func (w *jmapImportWriter) close() {}

func (w *jmapImportWriter) ensureFolder(ctx context.Context, name string) (string, error) {
	return w.imports.EnsureMailbox(ctx, name)
}

func (w *jmapImportWriter) upload(ctx context.Context, mailboxID string, msg mailImportMessage) error {
	var keywords []string
	if msg.Read {
		keywords = append(keywords, jmap.KeywordSeen)
	}
	if msg.Flagged {
		keywords = append(keywords, jmap.KeywordFlagged)
	}
	if msg.Answered {
		keywords = append(keywords, jmap.KeywordAnswered)
	}
	if msg.Draft {
		keywords = append(keywords, jmap.KeywordDraft)
	}
	return w.imports.Import(ctx, mailboxID, msg.Raw, msg.MessageID, keywords, msg.Date)
}

type imapImportWriter struct {
	cfg       *models.AccountConfig
	password  string
	client    *imapclient.Client
	delimiter rune
}

func (w *imapImportWriter) connect(ctx context.Context) (*imapclient.Client, error) {
	if w.client != nil {
		return w.client, nil
	}
	client, err := imapclient.NewContextClient(ctx, w.cfg, w.password)
	if err != nil {
		return nil, err
	}
	w.client = client
	return client, nil
}

func (w *imapImportWriter) drop() {
	if w.client != nil {
		w.client.Close()
		w.client = nil
	}
}

func (w *imapImportWriter) close() { w.drop() }

func (w *imapImportWriter) ensureFolder(ctx context.Context, name string) (string, error) {
	client, err := w.connect(ctx)
	if err != nil {
		return "", err
	}
	if w.delimiter == 0 {
		if w.delimiter, err = client.Delimiter(ctx); err != nil {
			w.drop()
			return "", err
		}
	}
	remote := name
	if w.delimiter != 0 && w.delimiter != '/' {
		remote = strings.ReplaceAll(name, "/", string(w.delimiter))
	}
	if err := client.CreateMailbox(ctx, remote); err != nil {
		var statusErr *goimap.Error
		if !errors.As(err, &statusErr) {
			w.drop()
		}
		return "", err
	}
	return remote, nil
}

func (w *imapImportWriter) upload(ctx context.Context, remote string, msg mailImportMessage) error {
	client, err := w.connect(ctx)
	if err != nil {
		return err
	}
	var flags []goimap.Flag
	if msg.Read {
		flags = append(flags, goimap.FlagSeen)
	}
	if msg.Flagged {
		flags = append(flags, goimap.FlagFlagged)
	}
	if msg.Answered {
		flags = append(flags, goimap.FlagAnswered)
	}
	if msg.Draft {
		flags = append(flags, goimap.FlagDraft)
	}
	date := msg.Date
	if date.IsZero() {
		date = time.Now()
	}
	_, err = client.AppendMessage(ctx, remote, msg.Raw, flags, date)
	if err == nil {
		return nil
	}
	var statusErr *goimap.Error
	if !errors.As(err, &statusErr) {
		w.drop()
	}
	// The server may have saved the message before the connection broke;
	// look for it before trying again.
	if imapclient.IsAppendAmbiguous(err) && msg.MessageID != "" {
		if client, connectErr := w.connect(ctx); connectErr == nil {
			if uid, _, findErr := client.FindUIDByMessageIDWithValidity(ctx, remote, "<"+msg.MessageID+">"); findErr == nil && uid != 0 {
				return nil
			}
		}
	}
	return err
}

// mailImportToken returns a function giving a Gmail or Outlook access
// token, a new one when refresh is set.
func (h *Handler) mailImportToken(owner, accountID string) func(context.Context, bool) (string, error) {
	if h.userIMAP != nil {
		return func(ctx context.Context, refresh bool) (string, error) {
			if h.userCredentials == nil {
				return "", errors.New("mail credentials unavailable")
			}
			current, err := h.userCredentials.MailboxAuthorization(ctx, owner, accountID)
			if err != nil {
				return "", err
			}
			if refresh {
				if current, err = h.userCredentials.RefreshMailboxAuthorization(ctx, current); err != nil {
					return "", err
				}
			}
			return current.Token(), nil
		}
	}
	return func(ctx context.Context, _ bool) (string, error) {
		if h.mailCredentials() == nil {
			return "", errors.New("mail credentials unavailable")
		}
		return h.mailCredentials().GetOAuthTokenForAccount(ctx, accountID)
	}
}

type mailImportAPIError struct {
	Status int
	Body   string
}

func (e *mailImportAPIError) Error() string {
	return fmt.Sprintf("the server answered %d: %s", e.Status, e.Body)
}

// mailImportCaller sends requests to a provider's API, signing in again
// once if the token expired.
type mailImportCaller struct {
	token func(context.Context, bool) (string, error)
}

type gmailImportWriter struct {
	mailImportCaller
}

func (w *gmailImportWriter) close() {}

func gmailUploadBaseURL() string {
	return strings.Replace(gmailAPIBaseURL, "/gmail/v1", "/upload/gmail/v1", 1)
}

func (w mailImportCaller) call(ctx context.Context, method, endpoint, contentType string, body func() io.Reader, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := w.token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, body())
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &mailImportAPIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		if readErr != nil {
			return readErr
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

func (w *gmailImportWriter) ensureFolder(ctx context.Context, name string) (string, error) {
	var labels struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := w.call(ctx, http.MethodGet, gmailAPIBaseURL+"/users/me/labels", "", func() io.Reader { return nil }, &labels); err != nil {
		return "", err
	}
	for _, label := range labels.Labels {
		if strings.EqualFold(label.Name, name) {
			return label.ID, nil
		}
	}
	payload, _ := json.Marshal(map[string]string{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"})
	var created struct {
		ID string `json:"id"`
	}
	if err := w.call(ctx, http.MethodPost, gmailAPIBaseURL+"/users/me/labels", "application/json", func() io.Reader { return bytes.NewReader(payload) }, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("Gmail did not create the label")
	}
	return created.ID, nil
}

// upload adds a message to Gmail the way messages.import does for mail
// moved from elsewhere: kept as it is, dated by its Date header, and not
// checked for spam.
func (w *gmailImportWriter) upload(ctx context.Context, label string, msg mailImportMessage) error {
	labels := []string{}
	if label != "" && label != "ARCHIVE" {
		labels = append(labels, label)
	}
	if !msg.Read {
		labels = append(labels, "UNREAD")
	}
	if msg.Flagged {
		labels = append(labels, "STARRED")
	}
	metadata, _ := json.Marshal(map[string][]string{"labelIds": labels})
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	metaPart, _ := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_, _ = metaPart.Write(metadata)
	messagePart, _ := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {"message/rfc822"}})
	_, _ = messagePart.Write(msg.Raw)
	_ = parts.Close()
	query := url.Values{"uploadType": {"multipart"}, "internalDateSource": {"dateHeader"}, "neverMarkSpam": {"true"}, "processForCalendar": {"false"}}
	payload := body.Bytes()
	return w.call(ctx, http.MethodPost, gmailUploadBaseURL()+"/users/me/messages/import?"+query.Encode(), "multipart/related; boundary="+parts.Boundary(),
		func() io.Reader { return bytes.NewReader(payload) }, nil)
}

func mailImportCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}
