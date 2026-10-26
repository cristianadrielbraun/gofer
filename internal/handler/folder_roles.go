package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// loadFolderRolesData gathers what the folder roles dialog of an IMAP account
// shows, or writes the error response and returns false.
func (h *Handler) loadFolderRolesData(w http.ResponseWriter, r *http.Request) (models.FolderRolesData, bool) {
	ctx := r.Context()
	accountID := r.PathValue("id")
	account, err := h.ownedAccount(ctx, accountID)
	if err != nil {
		log.Printf("folder roles account=%s: %v", accountID, err)
		http.Error(w, "failed to load account", http.StatusInternalServerError)
		return models.FolderRolesData{}, false
	}
	if account == nil || account.Provider != providers.ProviderIMAP {
		http.NotFound(w, r)
		return models.FolderRolesData{}, false
	}
	folders, err := h.db.ListFolderRoleFolders(ctx, accountID)
	if err != nil {
		log.Printf("folder roles account=%s: %v", accountID, err)
		http.Error(w, "failed to load folders", http.StatusInternalServerError)
		return models.FolderRolesData{}, false
	}
	data := models.FolderRolesData{Account: *account}
	for _, folder := range folders {
		if folder.Selectable && folder.Role != "inbox" {
			data.Folders = append(data.Folders, models.FolderOption{ID: folder.ID, Name: folder.RemoteID})
		}
	}
	for _, role := range storage.ChoosableFolderRoles {
		entry := models.FolderRole{Role: role}
		for _, folder := range folders {
			if folder.RoleChoice == role {
				entry.Chosen = folder.ID
			}
		}
		if entry.Chosen == "" {
			for _, folder := range folders {
				if folder.Role == role {
					entry.Detected = folder.RemoteID
					break
				}
			}
		}
		data.Roles = append(data.Roles, entry)
	}
	return data, true
}

// handleManageFolderRoles renders the folder roles dialog of an IMAP account.
func (h *Handler) handleManageFolderRoles(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadFolderRolesData(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html")
	views.FolderRolesDialog(data).Render(r.Context(), w)
}

// handleSaveFolderRoles saves the folders chosen for each role, a role left
// on Automatic following the server, and shows the roles again. A refused
// change shows why, with the choices as they were.
func (h *Handler) handleSaveFolderRoles(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	data, ok := h.loadFolderRolesData(w, r)
	if !ok {
		return
	}
	choices := map[string]string{}
	for _, role := range storage.ChoosableFolderRoles {
		if folderID := strings.TrimSpace(r.PostFormValue("role_" + role)); folderID != "" {
			choices[role] = folderID
		}
	}
	err := h.db.SetFolderRoleChoices(r.Context(), h.userID(r.Context()), data.Account.ID, choices)
	if errors.Is(err, storage.ErrFolderRoleChoice) {
		for i := range data.Roles {
			data.Roles[i].Chosen = choices[data.Roles[i].Role]
		}
		data.Error = "Give each folder one role at most."
		h.renderFolderRolesPanel(w, r, data)
		return
	}
	if err != nil {
		log.Printf("save folder roles account=%s: %v", data.Account.ID, err)
		http.Error(w, "failed to save folder roles", http.StatusInternalServerError)
		return
	}
	if data, ok = h.loadFolderRolesData(w, r); !ok {
		return
	}
	data.Saved = true
	h.renderFolderRolesPanel(w, r, data)
}

func (h *Handler) renderFolderRolesPanel(w http.ResponseWriter, r *http.Request, data models.FolderRolesData) {
	w.Header().Set("Content-Type", "text/html")
	views.FolderRolesPanel(data).Render(r.Context(), w)
}
