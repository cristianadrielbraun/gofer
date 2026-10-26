package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/emersion/go-imap/v2/imapclient"
)

func (f *mailExportFixture) serverFolders(t *testing.T) []string {
	t.Helper()
	client, err := imapclient.DialInsecure(f.imapAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Login("robin@example.test", "mailbox-secret").Wait(); err != nil {
		t.Fatal(err)
	}
	listed, err := client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mailbox := range listed {
		names = append(names, mailbox.Mailbox)
	}
	slices.Sort(names)
	return names
}

func (f *mailExportFixture) createServerFolder(t *testing.T, name string) {
	t.Helper()
	client, err := imapclient.DialInsecure(f.imapAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Login("robin@example.test", "mailbox-secret").Wait(); err != nil {
		t.Fatal(err)
	}
	if err := client.Create(name, nil).Wait(); err != nil {
		t.Fatal(err)
	}
}

func decodeFolderChange(t *testing.T, body string) folderChange {
	t.Helper()
	var change folderChange
	if err := json.Unmarshal([]byte(body), &change); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return change
}

// Folders are made, renamed and deleted on the server, and Gofer's copy
// follows: a renamed folder keeps its ID, and a deleted one goes with the
// folders inside it.
func TestManageIMAPFolders(t *testing.T) {
	f := newMailExportFixture(t)
	h, ctx := f.h, t.Context()
	accountID := f.account.ID
	account := map[string]string{"id": accountID}

	rec := identityRequest(t, h, h.handleCreateFolder, http.MethodPost, "/", account, url.Values{"name": {" Projects "}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	projects := decodeFolderChange(t, rec.Body.String())
	if projects.ID == "" || projects.Name != "Projects" {
		t.Fatalf("create = %+v", projects)
	}
	rec = identityRequest(t, h, h.handleCreateFolder, http.MethodPost, "/", account, url.Values{"name": {"Garden"}, "parent": {projects.ID}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create subfolder = %d %q", rec.Code, rec.Body.String())
	}
	garden := decodeFolderChange(t, rec.Body.String())
	var parent string
	if err := f.db.Read().QueryRowContext(ctx, `SELECT COALESCE(parent_id, '') FROM folders WHERE id = ?`, garden.ID).Scan(&parent); err != nil || parent != projects.ID {
		t.Fatalf("subfolder parent = %q, %v; want %q", parent, err, projects.ID)
	}
	if got, want := f.serverFolders(t), []string{"Archive", "Archive/2024", "INBOX", "Projects", "Projects/Garden"}; !slices.Equal(got, want) {
		t.Fatalf("server folders = %v, want %v", got, want)
	}

	// Refused before the server is asked.
	for name, form := range map[string]url.Values{
		"taken":     {"name": {"projects"}},
		"empty":     {"name": {"  "}},
		"delimiter": {"name": {"A/B"}},
	} {
		if rec := identityRequest(t, h, h.handleCreateFolder, http.MethodPost, "/", account, form, true); rec.Code != http.StatusBadRequest {
			t.Errorf("create %s = %d %q, want 400", name, rec.Code, rec.Body.String())
		}
	}

	folder := func(id string) map[string]string { return map[string]string{"id": id} }
	rec = identityRequest(t, h, h.handleFolderInfo, http.MethodGet, "/", folder(projects.ID), nil, true)
	var info folderInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.Subfolders != 1 || info.Provider != "imap" || info.Fixed || len(info.IDs) != 2 {
		t.Fatalf("info = %+v, %v (%q)", info, err, rec.Body.String())
	}

	rec = identityRequest(t, h, h.handleRenameFolder, http.MethodPost, "/", folder(projects.ID), url.Values{"name": {"Plans"}}, true)
	if rec.Code != http.StatusOK || decodeFolderChange(t, rec.Body.String()).Name != "Plans" {
		t.Fatalf("rename = %d %q", rec.Code, rec.Body.String())
	}
	if got, want := f.serverFolders(t), []string{"Archive", "Archive/2024", "INBOX", "Plans", "Plans/Garden"}; !slices.Equal(got, want) {
		t.Fatalf("server folders after rename = %v, want %v", got, want)
	}
	ids, err := f.db.IMAPFolderIDs(ctx, accountID)
	if err != nil || ids["Plans"] != projects.ID || ids["Plans/Garden"] != garden.ID {
		t.Fatalf("ids after rename = %v, %v", ids, err)
	}

	// The account's own folders stay as they are.
	if rec := identityRequest(t, h, h.handleRenameFolder, http.MethodPost, "/", folder(f.folders["inbox"]), url.Values{"name": {"Mail"}}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("rename inbox = %d, want 400", rec.Code)
	}
	if rec := identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder(f.folders["trash"]), nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete trash = %d, want 400", rec.Code)
	}
	// Another's folders can't be reached.
	for name, rec := range map[string]int{
		"info":   identityRequest(t, h, h.handleFolderInfo, http.MethodGet, "/", folder(projects.ID), nil, false).Code,
		"rename": identityRequest(t, h, h.handleRenameFolder, http.MethodPost, "/", folder(projects.ID), url.Values{"name": {"Mine"}}, false).Code,
		"delete": identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder(projects.ID), nil, false).Code,
		"create": identityRequest(t, h, h.handleCreateFolder, http.MethodPost, "/", account, url.Values{"name": {"Mine"}}, false).Code,
	} {
		if rec != http.StatusNotFound {
			t.Errorf("foreign %s = %d, want 404", name, rec)
		}
	}

	// Deleting moves the folder into Trash; it can come back out.
	f.createServerFolder(t, "Trash")
	rec = identityRequest(t, h, h.handleFolderInfo, http.MethodGet, "/", folder(projects.ID), nil, true)
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.TrashName != "Trash" || info.InTrash {
		t.Fatalf("info before delete = %+v, %v", info, err)
	}
	rec = identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder(projects.ID), nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}
	if got, want := f.serverFolders(t), []string{"Archive", "Archive/2024", "INBOX", "Trash", "Trash/Plans", "Trash/Plans/Garden"}; !slices.Equal(got, want) {
		t.Fatalf("server folders after delete = %v, want %v", got, want)
	}
	moved, err := f.db.ManagedFolderForUser(ctx, "owner", projects.ID)
	if err != nil || !moved.InTrash || moved.ParentID != f.folders["trash"] || moved.RemoteID != "Trash/Plans" || len(moved.Descendants) != 1 {
		t.Fatalf("folder in Trash = %+v, %v", moved, err)
	}
	// Deleting from Trash is for good, and only when asked to be.
	if rec := identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder(projects.ID), nil, true); rec.Code != http.StatusConflict {
		t.Fatalf("delete in Trash without permanent = %d %q, want 409", rec.Code, rec.Body.String())
	}

	rec = identityRequest(t, h, h.handleRestoreFolder, http.MethodPost, "/", folder(projects.ID), nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %q", rec.Code, rec.Body.String())
	}
	if got, want := f.serverFolders(t), []string{"Archive", "Archive/2024", "INBOX", "Plans", "Plans/Garden", "Trash"}; !slices.Equal(got, want) {
		t.Fatalf("server folders after restore = %v, want %v", got, want)
	}
	if back, err := f.db.ManagedFolderForUser(ctx, "owner", projects.ID); err != nil || back.InTrash || back.ParentID != "" {
		t.Fatalf("restored folder = %+v, %v", back, err)
	}
	if rec := identityRequest(t, h, h.handleRestoreFolder, http.MethodPost, "/", folder(projects.ID), nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("restore outside Trash = %d, want 400", rec.Code)
	}

	// Emptying Trash deletes the folders in it for good.
	if rec := identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder(projects.ID), nil, true); rec.Code != http.StatusOK {
		t.Fatalf("second delete = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleDeleteTrashFolders, http.MethodPost, "/", folder("trash"), nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("delete folders in Trash = %d %q", rec.Code, rec.Body.String())
	}
	if got, want := f.serverFolders(t), []string{"Archive", "Archive/2024", "INBOX", "Trash"}; !slices.Equal(got, want) {
		t.Fatalf("server folders after emptying Trash = %v, want %v", got, want)
	}
	var removed int
	if err := f.db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE id IN (?, ?) AND discovery_state = 'removed'`, projects.ID, garden.ID).Scan(&removed); err != nil || removed != 2 {
		t.Fatalf("removed folders = %d, %v; want 2", removed, err)
	}
	if rec := identityRequest(t, h, h.handleFolderInfo, http.MethodGet, "/", folder(projects.ID), nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted folder info = %d, want 404", rec.Code)
	}
}

// A folder whose account has no Trash isn't deleted unless the delete is
// asked to be permanent.
func TestDeleteIMAPFolderWithoutTrash(t *testing.T) {
	f := newMailExportFixture(t)
	h := f.h
	if _, err := f.db.Write().ExecContext(t.Context(), `DELETE FROM folders WHERE id = ?`, f.folders["trash"]); err != nil {
		t.Fatal(err)
	}
	folder := map[string]string{"id": f.folders["archive-2024"]}
	if rec := identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder, nil, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no Trash folder") {
		t.Fatalf("delete without Trash = %d %q, want 409", rec.Code, rec.Body.String())
	}
	if got := f.serverFolders(t); !slices.Contains(got, "Archive/2024") {
		t.Fatalf("server folders = %v; the folder went without being asked", got)
	}
	if rec := identityRequest(t, h, h.handleDeleteFolder, http.MethodPost, "/", folder, url.Values{"permanent": {"1"}}, true); rec.Code != http.StatusOK {
		t.Fatalf("permanent delete = %d %q", rec.Code, rec.Body.String())
	}
	if got, want := f.serverFolders(t), []string{"Archive", "INBOX"}; !slices.Equal(got, want) {
		t.Fatalf("server folders after delete = %v, want %v", got, want)
	}
}

func TestCleanFolderName(t *testing.T) {
	for _, tt := range []struct {
		provider, name, want string
		ok                   bool
	}{
		{"imap", "  Receipts ", "Receipts", true},
		{"imap", "", "", false},
		{"outlook", "Work/Old", "", false},
		{"gmail", "Work/Old", "Work/Old", true},
		{"gmail", "Work/", "", false},
		{"jmap", "Line\nbreak", "", false},
		{"imap", strings.Repeat("a", folderNameMaxLength+1), "", false},
	} {
		got, err := cleanFolderName(tt.provider, tt.name)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("cleanFolderName(%q, %q) = %q, %v", tt.provider, tt.name, got, err)
		}
	}
}

// The folders dialog lists the owner's folders, with rename and delete only
// on folders without a role and Move out of Trash only inside Trash. On
// Gmail it lists labels.
func TestManageFoldersDialog(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, parent_id, remote_id, name, role, selectable, sort_order) VALUES
		('f-inbox', 'victim-account', NULL, 'INBOX', 'Inbox', 'inbox', 1, 0),
		('f-trash', 'victim-account', NULL, 'Trash', 'Trash', 'trash', 1, 1),
		('f-old', 'victim-account', 'f-trash', 'Trash/Old plans', 'Old plans', 'custom', 1, 2),
		('f-trips', 'victim-account', NULL, 'Trips', 'Trips', 'custom', 1, 3)`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	account := map[string]string{"id": "victim-account"}
	rec := identityRequest(t, h, h.handleManageFolders, http.MethodGet, "/api/accounts/victim-account/folders/manage", account, nil, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `id="folders-dialog"`) || !strings.Contains(body, "Old plans") {
		t.Fatalf("dialog = %d %q", rec.Code, body)
	}
	for _, want := range []string{`aria-label="Rename Trips"`, `aria-label="Delete Trips"`, `aria-label="Move Old plans out of Trash"`, `aria-label="New folder in Inbox"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog lacks %s", want)
		}
	}
	for _, unwanted := range []string{`aria-label="Rename Inbox"`, `aria-label="Delete Trash"`, `aria-label="Move Trips out of Trash"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("dialog has %s", unwanted)
		}
	}
	rec = identityRequest(t, h, h.handleManageFolders, http.MethodGet, "/?panel=1", account, nil, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `id="folders-dialog"`) || !strings.Contains(rec.Body.String(), `id="folders-panel"`) {
		t.Fatalf("panel = %d %q", rec.Code, rec.Body.String())
	}
	if rec := identityRequest(t, h, h.handleManageFolders, http.MethodGet, "/", account, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign dialog = %d, want 404", rec.Code)
	}

	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET provider = 'gmail' WHERE id = 'victim-account'`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertLabels(ctx, []storage.LabelInput{{AccountID: "victim-account", Name: "Receipts", ProviderID: "Label_1", ProviderType: storage.LabelProviderGmail}}); err != nil {
		t.Fatal(err)
	}
	rec = identityRequest(t, h, h.handleManageFolders, http.MethodGet, "/", account, nil, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `aria-label="Delete Receipts"`) || !strings.Contains(body, "New label") || strings.Contains(body, "Trips") {
		t.Fatalf("gmail dialog = %d %q", rec.Code, body)
	}
}
