package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
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
