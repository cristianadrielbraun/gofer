package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

// In a shared database, opening mail makes the user's Local folders, which
// then can't be deleted.
func TestSharedLocalFoldersAreMadeAndKept(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "owner", Username: "owner"})
	if err := h.ensureLocalFolders(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	id, err := db.LocalFoldersAccountIDForUser(t.Context(), "owner")
	if err != nil || id == "" {
		t.Fatalf("local folders = %q, %v", id, err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/"+id, nil).WithContext(ctx)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.handleDeleteAccount(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if still, err := db.LocalFoldersAccountIDForUser(t.Context(), "owner"); err != nil || still != id {
		t.Fatalf("after delete = %q, %v", still, err)
	}
}

// Local folders can be hidden and shown again, and no other account can be
// hidden.
func TestHideLocalFolders(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "owner", Username: "owner"})
	account, err := db.EnsureLocalFoldersAccount(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	shown := func(id, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+id+"/shown", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.handleLocalFoldersShown(rec, req)
		return rec.Code
	}
	hidden := func() bool {
		accounts, err := db.GetAccounts(t.Context(), "owner")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range accounts {
			if a.ID == account {
				return a.Hidden
			}
		}
		t.Fatal("Local folders missing")
		return false
	}
	if code := shown(account, ""); code != http.StatusNoContent || !hidden() {
		t.Fatalf("hide = %d, hidden %v", code, hidden())
	}
	if code := shown(account, "shown=true"); code != http.StatusNoContent || hidden() {
		t.Fatalf("show = %d, hidden %v", code, hidden())
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('mail', 'owner', 'imap', 'robin@example.org')`); err != nil {
		t.Fatal(err)
	}
	if code := shown("mail", ""); code != http.StatusNotFound {
		t.Fatalf("hide a mail account = %d", code)
	}
}

// Local folders are made, renamed, put in Trash and deleted with no server,
// and a folder deleted for good takes its mail with it.
func TestManageLocalFolders(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "owner", Username: "owner"})
	account, err := db.EnsureLocalFoldersAccount(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	trips, err := h.createFolder(ctx, "owner", account, "", "Trips")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.createFolder(ctx, "owner", account, "", "Trips"); err == nil {
		t.Fatal("made a second folder named Trips")
	}
	spain, err := h.createFolder(ctx, "owner", account, trips.ID, "Spain")
	if err != nil {
		t.Fatal(err)
	}
	folder, provider, err := h.managedFolder(ctx, "owner", trips.ID)
	if err != nil || provider != providers.ProviderLocal {
		t.Fatalf("trips = %+v %q %v", folder, provider, err)
	}
	if _, err := h.renameFolder(ctx, "owner", folder, provider, "Holidays"); err != nil {
		t.Fatal(err)
	}
	child, _, err := h.managedFolder(ctx, "owner", spain.ID)
	if err != nil || child.ParentID != trips.ID || child.RemoteID != "Holidays/Spain" {
		t.Fatalf("subfolder after rename = %+v %v", child, err)
	}
	result, err := db.Write().Exec(`INSERT INTO messages (account_id, subject) VALUES (?, 'Ferry times')`, account)
	if err != nil {
		t.Fatal(err)
	}
	messageID, _ := result.LastInsertId()
	if _, err := db.Write().Exec(`INSERT INTO message_folder_state (message_id, folder_id) VALUES (?, ?)`, messageID, spain.ID); err != nil {
		t.Fatal(err)
	}

	folder, provider, err = h.managedFolder(ctx, "owner", trips.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.trashFolder(ctx, "owner", folder, provider); err != nil {
		t.Fatal(err)
	}
	folder, provider, err = h.managedFolder(ctx, "owner", trips.ID)
	if err != nil || !folder.InTrash {
		t.Fatalf("after trash = %+v %v", folder, err)
	}
	if err := h.deleteFolder(ctx, "owner", folder, provider); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.managedFolder(ctx, "owner", trips.ID); err == nil {
		t.Fatal("deleted folder still there")
	}
	var messages int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, messageID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("message in deleted folder kept: %d %v", messages, err)
	}

	archive, provider, err := h.managedFolder(ctx, "owner", storage.LocalFolderID(account, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.renameFolder(ctx, "owner", archive, provider, "Old"); err == nil {
		t.Fatal("renamed Archive")
	}
}

const localWriterTestMessage = "Message-ID: <picnic-1@example.test>\r\n" +
	"From: Jordan Reyes <jordan@example.test>\r\n" +
	"To: Owner <owner@example.com>\r\n" +
	"Subject: Picnic on Saturday\r\n" +
	"Date: Tue, 06 Oct 2026 09:30:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Bring the blue blanket.\r\n"

// The Local folders writer makes the folders it's given by path and saves a
// message there whole: its raw message, body, sender, recipients and state.
func TestLocalFoldersWriterSavesMail(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "owner", Username: "owner"})
	account, err := db.EnsureLocalFoldersAccount(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := h.mailAccountWriter(ctx, "owner", account)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	folderID, err := writer.ensureFolder(ctx, "Family/2026")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := writer.ensureFolder(ctx, "family/2026"); err != nil || again != folderID {
		t.Fatalf("second ensure = %q, %v; want %q", again, err, folderID)
	}
	folder, _, err := h.managedFolder(ctx, "owner", folderID)
	if err != nil || folder.Name != "2026" || folder.ParentID == "" {
		t.Fatalf("folder = %+v %v", folder, err)
	}
	date := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	if err := writer.upload(ctx, folderID, mailImportMessage{Raw: []byte(localWriterTestMessage), MessageID: "<picnic-1@example.test>", Subject: "Picnic on Saturday", Date: date, Read: true, Flagged: true}); err != nil {
		t.Fatal(err)
	}
	var id int64
	var subject, fromEmail, rawPath, textPath string
	if err := db.Read().QueryRow(`SELECT m.id, m.subject, m.from_email, COALESCE(m.raw_path, ''), COALESCE(m.body_text_path, '') FROM messages m WHERE m.account_id = ?`, account).
		Scan(&id, &subject, &fromEmail, &rawPath, &textPath); err != nil {
		t.Fatal(err)
	}
	if subject != "Picnic on Saturday" || fromEmail != "jordan@example.test" || rawPath == "" || textPath == "" {
		t.Fatalf("saved %q from %q raw=%q text=%q", subject, fromEmail, rawPath, textPath)
	}
	if raw, err := store.ReadMessageFile(rawPath); err != nil || string(raw) != localWriterTestMessage {
		t.Fatalf("raw = %q, %v", raw, err)
	}
	var read, starred, recipients int
	if err := db.Read().QueryRow(`SELECT is_read, is_starred FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, id, folderID).Scan(&read, &starred); err != nil || read != 1 || starred != 1 {
		t.Fatalf("state read=%d starred=%d %v", read, starred, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND email = 'owner@example.com'`, id).Scan(&recipients); err != nil || recipients != 1 {
		t.Fatalf("recipients = %d, %v", recipients, err)
	}
}

// A Local folder moves with its subfolders inside another folder or back to
// the top, and never into itself, its own subfolders or next to a folder of
// the same name. The Move folder dialog offers only the places it can go.
func TestMoveLocalFolders(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "owner", Username: "owner"})
	account, err := db.EnsureLocalFoldersAccount(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parent, name string) string {
		t.Helper()
		made, err := h.createFolder(ctx, "owner", account, parent, name)
		if err != nil {
			t.Fatal(err)
		}
		return made.ID
	}
	trips := mk("", "Trips")
	spain := mk(trips, "Spain")
	ferries := mk(spain, "Ferries")
	archive := mk("", "Archive 2025")
	move := func(id, parent string) *httptest.ResponseRecorder {
		return identityRequest(t, h, h.handleMoveFolderTo, http.MethodPost, "/", map[string]string{"id": id}, url.Values{"parent": {parent}}, true)
	}

	rec := identityRequest(t, h, h.handleFolderMoveDialog, http.MethodGet, "/", map[string]string{"id": trips}, nil, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `id="folder-move-dialog"`) || !strings.Contains(body, "Archive 2025") {
		t.Fatalf("dialog = %d %q", rec.Code, body)
	}
	for _, id := range []string{trips, spain, ferries} {
		if !regexp.MustCompile(`<button[^>]* disabled [^>]*data-folder-move-parent="` + id + `"`).MatchString(body) {
			t.Errorf("dialog lets %s be picked", id)
		}
	}
	if regexp.MustCompile(`<button[^>]* disabled [^>]*data-folder-move-parent="` + archive + `"`).MatchString(body) {
		t.Error("dialog doesn't let Archive 2025 be picked")
	}

	if rec := move(trips, spain); rec.Code != http.StatusBadRequest {
		t.Fatalf("into own subfolder = %d %q", rec.Code, rec.Body.String())
	}
	if rec := move(trips, trips); rec.Code != http.StatusBadRequest {
		t.Fatalf("into itself = %d", rec.Code)
	}
	if rec := move(trips, archive); rec.Code != http.StatusOK {
		t.Fatalf("into Archive 2025 = %d %q", rec.Code, rec.Body.String())
	}
	moved, _, err := h.managedFolder(ctx, "owner", ferries)
	if err != nil || moved.RemoteID != "Archive 2025/Trips/Spain/Ferries" {
		t.Fatalf("ferries after move = %+v %v", moved, err)
	}
	if rec := move(spain, ""); rec.Code != http.StatusOK {
		t.Fatalf("to the top = %d %q", rec.Code, rec.Body.String())
	}
	top, _, err := h.managedFolder(ctx, "owner", spain)
	if err != nil || top.ParentID != "" || top.RemoteID != "Spain" {
		t.Fatalf("spain at the top = %+v %v", top, err)
	}
	mk(trips, "Spain")
	if rec := move(spain, trips); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already there") {
		t.Fatalf("onto a name in use = %d %q", rec.Code, rec.Body.String())
	}
	if rec := move(storage.LocalFolderID(account, "trash"), archive); rec.Code != http.StatusBadRequest {
		t.Fatalf("Trash moved = %d", rec.Code)
	}
	if rec := identityRequest(t, h, h.handleMoveFolderTo, http.MethodPost, "/", map[string]string{"id": spain}, url.Values{"parent": {""}}, false); rec.Code != http.StatusNotFound {
		t.Fatalf("someone else's folder = %d", rec.Code)
	}

	// Mail servers' folders don't move this way yet.
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO folders (id, account_id, parent_id, remote_id, name, role, selectable, sort_order) VALUES
		('f-plans', 'victim-account', NULL, 'Plans', 'Plans', 'custom', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if rec := move("f-plans", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("IMAP folder moved = %d", rec.Code)
	}
}
