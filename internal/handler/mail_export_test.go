package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// mailExportFixture is an IMAP account whose server holds the messages the
// test exports, with Gofer's index of them.
type mailExportFixture struct {
	h       *Handler
	db      *storage.DB
	account models.Account
	folders map[string]string
	ids     map[string]int64
	// imapAddr is the fake server, for checking what an import added.
	imapAddr string
	mem      *imapmemserver.Server
	port     int
}

func mailExportTestMessage(subject, body string) string {
	return "From: Juno Tester <juno@example.test>\r\n" +
		"To: Robin Example <robin@example.test>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + strings.ReplaceAll(strings.ToLower(subject), " ", "-") + "@example.test>\r\n" +
		"Date: Mon, 03 Mar 2025 10:00:00 +0000\r\n" +
		"\r\n" + body + "\r\n"
}

func newMailExportFixture(t *testing.T) *mailExportFixture {
	t.Helper()
	previousDelay := mailJobRetryDelay
	mailJobRetryDelay = time.Millisecond
	t.Cleanup(func() { mailJobRetryDelay = previousDelay })

	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()

	mem := imapmemserver.New()
	user := imapmemserver.NewUser("robin@example.test", "mailbox-secret")
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	client, err := imapclient.DialInsecure(listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Login("robin@example.test", "mailbox-secret").Wait(); err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range []string{"INBOX", "Archive", "Archive/2024"} {
		if err := client.Create(mailbox, nil).Wait(); err != nil && mailbox != "INBOX" {
			t.Fatal(err)
		}
	}
	appendMessage := func(mailbox, raw string) {
		command := client.Append(mailbox, int64(len(raw)), nil)
		if _, err := command.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := command.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage("INBOX", mailExportTestMessage("Garden plans", "The tomatoes go by the fence."))
	appendMessage("INBOX", mailExportTestMessage("Quoted line", "From the desk of Juno:\r\n>From an older note."))
	appendMessage("INBOX", mailExportTestMessage("Saved copy", "This is the copy on the server."))
	appendMessage("Archive/2024", mailExportTestMessage("Old receipt", "Paid in full."))

	for protocol, port := range map[string]int{"imap": port, "smtp": 1} {
		if err := db.AddPrivateTargetException(ctx, protocol, "127.0.0.1", port, "administrator"); err != nil {
			t.Fatal(err)
		}
		if err := db.AddPlaintextTransportException(ctx, protocol, "127.0.0.1", port, "administrator"); err != nil {
			t.Fatal(err)
		}
	}
	created, err := h.accountStore.CreateAccount(ctx, "owner", &models.CreateAccountRequest{
		Provider: "imap", AuthMethod: "plain", EmailAddress: "robin@example.test", DisplayName: "Robin Example",
		IMAPHost: "127.0.0.1", IMAPPort: port, IMAPTLSMode: "plaintext",
		SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPTLSMode: "plaintext",
		Username: "robin@example.test", Password: "mailbox-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &mailExportFixture{h: h, db: db, folders: map[string]string{}, ids: map[string]int64{}, imapAddr: listener.Addr().String(), mem: mem, port: port}
	account, found, err := h.mailExportAccount(ctx, "owner", created.ID)
	if err != nil || !found {
		t.Fatalf("account not listed: %v", err)
	}
	f.account = account

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Write().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, folder := range []struct{ id, parent, remote, name, role string }{
		{"inbox", "", "INBOX", "Inbox", "inbox"},
		{"archive", "", "Archive", "Archive", "custom"},
		{"archive-2024", "archive", "Archive/2024", "2024", "custom"},
		{"trash", "", "Trash", "Trash", "trash"},
	} {
		id := created.ID + "-" + folder.id
		f.folders[folder.id] = id
		var parent any
		if folder.parent != "" {
			parent = created.ID + "-" + folder.parent
		}
		selectable := 1
		if folder.id == "archive" {
			selectable = 0
		}
		order := 1
		if folder.id == "inbox" {
			order = 0
		}
		exec(`INSERT INTO folders (id, account_id, parent_id, remote_id, name, role, selectable, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, created.ID, parent, folder.remote, folder.name, folder.role, selectable, order)
	}
	savedPath := filepath.Join(t.TempDir(), "saved.eml")
	if err := os.WriteFile(savedPath, []byte(mailExportTestMessage("Saved copy", "This is the copy Gofer saved.")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, message := range []struct {
		key, folder, subject, rawPath string
		uid                           int
		read, flagged                 bool
	}{
		{"garden", "inbox", "Garden plans", "", 1, true, false},
		{"quoted", "inbox", "Quoted line", "", 2, false, true},
		{"missing", "inbox", "Gone from the server", "", 40, true, false},
		{"saved", "inbox", "Saved copy", savedPath, 3, true, false},
		{"receipt", "archive-2024", "Old receipt", "", 1, true, false},
		{"trashed", "trash", "Thrown away", "", 1, true, false},
	} {
		var rawPath any
		if message.rawPath != "" {
			rawPath = message.rawPath
		}
		messageID := strings.ReplaceAll(strings.ToLower(message.subject), " ", "-") + "@example.test"
		result, err := db.Write().ExecContext(ctx, `INSERT INTO messages (account_id, subject, from_email, date_received, raw_path, internet_message_id) VALUES (?, ?, 'juno@example.test', '2025-03-03 10:00:00', ?, ?)`,
			created.ID, message.subject, rawPath, "<"+messageID+">")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		f.ids[message.key] = id
		exec(`INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, is_flagged) VALUES (?, ?, ?, ?, ?)`,
			id, f.folders[message.folder], message.uid, message.read, message.flagged)
	}
	ctxExports, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		f.h.mailExports.wg.Wait()
		f.h.mailImports.wg.Wait()
		f.h.mailTransfers.wg.Wait()
	})
	h.StartMailExports(ctxExports)
	h.StartMailImports(ctxExports)
	h.StartMailTransfers(ctxExports)
	return f
}

func (f *mailExportFixture) waitFinished(t *testing.T, id string) *mailExportJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := f.h.mailExports.load(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != mailJobRunning && !f.h.mailExports.isRunning(id) {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("export did not finish")
	return nil
}

func (f *mailExportFixture) download(t *testing.T, id string, request func(*http.Request) *http.Request) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	req := request(httptest.NewRequest(http.MethodGet, "/api/mail/exports/"+id+"/download", nil))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	f.h.handleDownloadMailExport(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = string(data)
	}
	return rec, files
}

func TestMailExportWritesEachFolderAsMbox(t *testing.T) {
	f := newMailExportFixture(t)
	job, err := f.h.createMailExport(t.Context(), "owner", f.account, []string{f.folders["inbox"], f.folders["archive-2024"]}, false)
	if err != nil {
		t.Fatal(err)
	}
	job = f.waitFinished(t, job.ID)
	if job.State != mailJobDone {
		t.Fatalf("state = %s (%s)", job.State, job.Error)
	}
	if job.written() != 4 || job.Failed != 1 || len(job.Skipped) != 1 || job.Skipped[0].MessageID != f.ids["missing"] {
		t.Fatalf("exported %d, failed %d, skipped %+v", job.written(), job.Failed, job.Skipped)
	}

	rec, files := f.download(t, job.ID, ownerRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "gofer-mail-robin-at-example.test-") {
		t.Fatalf("file name = %q", got)
	}
	if len(files) != 3 || files["Inbox.mbox"] == "" || files["Archive/2024.mbox"] == "" {
		t.Fatalf("archive files = %v", mapKeys(files))
	}
	inbox := files["Inbox.mbox"]
	if count := strings.Count(inbox, "\nFrom juno@example.test ") + boolCount(strings.HasPrefix(inbox, "From juno@example.test ")); count != 3 {
		t.Fatalf("inbox has %d messages:\n%s", count, inbox)
	}
	for _, want := range []string{
		"From juno@example.test Mon Mar  3 10:00:00 2025\nStatus: RO\r\nFrom: Juno Tester",
		"Status: O\r\nX-Status: F\r\n",
		"\r\n>From the desk of Juno:\r\n>>From an older note.",
		"This is the copy Gofer saved.",
	} {
		if !strings.Contains(inbox, want) {
			t.Fatalf("inbox lacks %q:\n%s", want, inbox)
		}
	}
	if strings.Contains(inbox, "This is the copy on the server.") {
		t.Fatal("a saved copy was downloaded again")
	}
	if !strings.Contains(files["Archive/2024.mbox"], "Paid in full.") {
		t.Fatalf("archive folder = %q", files["Archive/2024.mbox"])
	}
	var manifest mailExportManifest
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != mailExportFormat || manifest.Account.Email != "robin@example.test" || len(manifest.Folders) != 2 ||
		manifest.Folders[0].Path != "Inbox" || manifest.Folders[0].Messages != 3 || manifest.Folders[0].Failed != 1 ||
		manifest.Folders[1].File != "Archive/2024.mbox" {
		t.Fatalf("manifest = %+v", manifest)
	}

	if rec, _ := f.download(t, job.ID, attackerRequest); rec.Code != http.StatusNotFound {
		t.Fatalf("another person's download = %d", rec.Code)
	}
}

func TestMailExportOfSavedCopiesLeavesOutHeadersOnlyMessages(t *testing.T) {
	f := newMailExportFixture(t)
	job, err := f.h.createMailExport(t.Context(), "owner", f.account, []string{f.folders["inbox"]}, true)
	if err != nil {
		t.Fatal(err)
	}
	job = f.waitFinished(t, job.ID)
	if job.State != mailJobDone || job.written() != 1 || job.NotSaved != 3 || job.Failed != 0 {
		t.Fatalf("state %s, exported %d, not saved %d, failed %d", job.State, job.written(), job.NotSaved, job.Failed)
	}
	_, files := f.download(t, job.ID, ownerRequest)
	if inbox := files["Inbox.mbox"]; !strings.Contains(inbox, "This is the copy Gofer saved.") || strings.Contains(inbox, "Garden plans") {
		t.Fatalf("inbox = %q", inbox)
	}
}

// An export stopped while writing a folder continues after the last message
// it recorded, dropping anything written after it.
func TestMailExportContinuesWhereItStopped(t *testing.T) {
	f := newMailExportFixture(t)
	now := time.Now().UTC()
	job := &mailExportJob{
		mailJobInfo: mailJobInfo{ID: newMailJobID(), Owner: "owner", State: mailJobPaused, CreatedAt: now, UpdatedAt: now},
		AccountID:   f.account.ID, AccountEmail: f.account.Email, Provider: "imap",
		Folders: []*mailExportFolderJob{{FolderID: f.folders["inbox"], Name: "Inbox", Path: "Inbox", Role: "inbox", RemoteID: "INBOX", File: "folder-1.mbox", Total: 4}},
	}
	mailExportArchiveNames(job.Folders)
	var first bytes.Buffer
	if err := writeMboxMessage(&first, storage.MailExportMessage{FromEmail: "juno@example.test", Read: true}, []byte(mailExportTestMessage("Garden plans", "The tomatoes go by the fence."))); err != nil {
		t.Fatal(err)
	}
	job.Folders[0].Exported, job.Folders[0].LastMessageID, job.Folders[0].Size = 1, f.ids["garden"], int64(first.Len())
	if err := f.h.mailExports.save(job); err != nil {
		t.Fatal(err)
	}
	partial := append(first.Bytes(), []byte("From juno@example.test half written")...)
	if err := os.WriteFile(filepath.Join(f.h.mailExports.jobDir(job.ID), "folder-1.mbox"), partial, 0o600); err != nil {
		t.Fatal(err)
	}

	req := ownerRequest(httptest.NewRequest(http.MethodPost, "/api/mail/exports/"+job.ID+"/resume", nil))
	req.SetPathValue("id", job.ID)
	f.h.handleResumeMailExport(httptest.NewRecorder(), req)
	job = f.waitFinished(t, job.ID)
	if job.State != mailJobDone || job.written() != 3 {
		t.Fatalf("state %s, exported %d", job.State, job.written())
	}
	_, files := f.download(t, job.ID, ownerRequest)
	inbox := files["Inbox.mbox"]
	if strings.Contains(inbox, "half written") || strings.Count(inbox, "Garden plans") != 1 || !strings.Contains(inbox, "Quoted line") {
		t.Fatalf("inbox after continuing:\n%s", inbox)
	}
}

func TestMailExportIsOnlyVisibleToItsOwner(t *testing.T) {
	f := newMailExportFixture(t)
	job, err := f.h.createMailExport(t.Context(), "owner", f.account, []string{f.folders["trash"]}, true)
	if err != nil {
		t.Fatal(err)
	}
	f.waitFinished(t, job.ID)
	for _, action := range []string{"cancel", "resume", "delete"} {
		req := attackerRequest(httptest.NewRequest(http.MethodPost, "/api/mail/exports/"+job.ID+"/"+action, nil))
		req.SetPathValue("id", job.ID)
		rec := httptest.NewRecorder()
		map[string]http.HandlerFunc{"cancel": f.h.handleCancelMailExport, "resume": f.h.handleResumeMailExport, "delete": f.h.handleDeleteMailExport}[action](rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s by another person = %d", action, rec.Code)
		}
	}
	jobs, err := f.h.mailExportJobViews(context.Background(), "attacker")
	if err != nil || len(jobs) != 0 {
		t.Fatalf("another person sees %d exports (%v)", len(jobs), err)
	}
	req := attackerRequest(httptest.NewRequest(http.MethodPost, "/api/mail/exports", strings.NewReader("account="+f.account.ID+"&folder="+f.folders["inbox"])))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.h.handleStartMailExport(rec, req)
	if !strings.Contains(rec.Body.String(), "no longer in Gofer") {
		t.Fatalf("another person's account export = %q", rec.Body.String())
	}
	if _, err := f.h.mailExports.load(job.ID); err != nil {
		t.Fatalf("export was removed: %v", err)
	}
}

func TestMailExportArchiveNamesAreSafeAndUnique(t *testing.T) {
	folders := []*mailExportFolderJob{{Path: "Inbox"}, {Path: "INBOX"}, {Path: "Work/Clients: A|B"}, {Path: "../.."}, {Path: "manifest"}}
	mailExportArchiveNames(folders)
	got := make([]string, len(folders))
	for i, folder := range folders {
		got[i] = folder.Archive
	}
	want := []string{"Inbox.mbox", "INBOX (2).mbox", "Work/Clients_ A_B.mbox", "Folder.mbox", "manifest.mbox"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("names = %q, want %q", got, want)
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestMailExportCanBeCanceledWhileWaiting(t *testing.T) {
	f := newMailExportFixture(t)
	// Both download slots are busy, so the new export waits its turn.
	for range mailJobWorkers {
		f.h.mailExports.slots <- struct{}{}
	}
	defer func() {
		for range mailJobWorkers {
			<-f.h.mailExports.slots
		}
	}()
	job, err := f.h.createMailExport(t.Context(), "owner", f.account, []string{f.folders["inbox"]}, false)
	if err != nil {
		t.Fatal(err)
	}
	views, err := f.h.mailExportJobViews(context.Background(), "owner")
	if err != nil || len(views) != 1 || views[0].State != mailJobRunning || !views[0].Waiting {
		t.Fatalf("waiting export = %+v, %v", views, err)
	}
	req := ownerRequest(httptest.NewRequest(http.MethodPost, "/api/mail/exports/"+job.ID+"/cancel", nil))
	req.SetPathValue("id", job.ID)
	rec := httptest.NewRecorder()
	f.h.handleCancelMailExport(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-mail-export-state="canceled"`) {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	job, err = f.h.mailExports.load(job.ID)
	if err != nil || job.State != mailJobCanceled || job.written() != 0 {
		t.Fatalf("canceled export = %+v, %v", job, err)
	}
}
