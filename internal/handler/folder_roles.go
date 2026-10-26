package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// missingFolderHeader names, on a mail action a missing folder held back, the
// account and the role of the folder it lacks, as "<account> <role>", so the
// page can offer to choose one. A batch names the first such account.
const missingFolderHeader = "X-Gofer-Missing-Folder"

func noteMissingFolder(w http.ResponseWriter, accountID, role string) {
	if w.Header().Get(missingFolderHeader) == "" {
		w.Header().Set(missingFolderHeader, accountID+" "+role)
	}
}

// loadFolderRolesData gathers what the folder roles dialog of an IMAP account
// shows, or writes the error response and returns false.
func (h *Handler) loadFolderRolesData(w http.ResponseWriter, r *http.Request) (models.FolderRolesData, bool) {
	ctx := r.Context()
	accountID := r.PathValue("id")
	if _, err := h.ownedAccount(ctx, accountID); err != nil {
		log.Printf("folder roles account=%s: %v", accountID, err)
		http.Error(w, "failed to load account", http.StatusInternalServerError)
		return models.FolderRolesData{}, false
	}
	data, err := folderRolesData(ctx, h.accountStore, h.db, h.userID(ctx), accountID)
	if errors.Is(err, errFolderRolesAccount) {
		http.NotFound(w, r)
		return models.FolderRolesData{}, false
	}
	if err != nil {
		log.Printf("folder roles account=%s: %v", accountID, err)
		http.Error(w, "failed to load folders", http.StatusInternalServerError)
		return models.FolderRolesData{}, false
	}
	return data, true
}

// errFolderRolesAccount means the account isn't one of the owner's IMAP
// accounts.
var errFolderRolesAccount = errors.New("not an IMAP account of the owner")

func folderRolesData(ctx context.Context, accounts *config.AccountStore, db *storage.DB, owner, accountID string) (models.FolderRolesData, error) {
	account, err := accounts.GetAccountByIDForUser(ctx, owner, accountID)
	if err != nil {
		return models.FolderRolesData{}, err
	}
	if account == nil || account.Provider != providers.ProviderIMAP {
		return models.FolderRolesData{}, errFolderRolesAccount
	}
	folders, err := db.ListFolderRoleFolders(ctx, accountID)
	if err != nil {
		return models.FolderRolesData{}, err
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
	return data, nil
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

// handleCreateFolderRoleFolder makes a folder on the server for a role no
// folder has, such as Trash, and syncs the account, which gives it the role.
// The dialog shows the roles again, with why it failed if it did.
func (h *Handler) handleCreateFolderRoleFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	accountID := r.PathValue("id")
	role := r.URL.Query().Get("role")
	var data models.FolderRolesData
	var cfg *models.AccountConfig
	var password string
	err := h.withProfileAccount(ctx, owner, accountID, func(accounts *config.AccountStore, db *storage.DB) error {
		var err error
		if data, err = folderRolesData(ctx, accounts, db, owner, accountID); err != nil {
			return err
		}
		if cfg, err = accounts.GetConfig(ctx, accountID); err != nil {
			return err
		}
		if h.userIMAP == nil {
			password, err = h.resolvePassword(ctx, cfg, accountID)
		} else {
			password, err = accounts.DecryptPassword(ctx, accountID)
		}
		return err
	})
	if errors.Is(err, errFolderRolesAccount) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if !slices.Contains(storage.ChoosableFolderRoles, role) {
		http.Error(w, "unknown folder role", http.StatusBadRequest)
		return
	}
	// The connection is made with no account store held, as with sending.
	name, err := createRoleFolder(ctx, cfg, password, role)
	if err != nil {
		log.Printf("create %s folder account=%s: %v", role, accountID, err)
		data.Error = "Couldn't create the folder: " + err.Error()
	} else {
		data.Created = name
	}
	h.renderFolderRolesPanel(w, r, data)
}

func createRoleFolder(ctx context.Context, cfg *models.AccountConfig, password, role string) (string, error) {
	client, err := imapclient.NewContextClient(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return client.CreateRoleMailbox(ctx, role)
}
