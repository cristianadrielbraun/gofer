package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Folders are created, renamed and deleted on the server while the request
// waits, so a refusal shows at once; the local folders change only after the
// server has. Gmail has labels rather than folders, which Gofer shows as
// tags, so on Gmail these are label changes.

const folderNameMaxLength = 200

// folderInfo is what the sidebar asks before renaming or deleting a folder.
type folderInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Messages   int    `json:"messages"`
	Subfolders int    `json:"subfolders"`
	// IDs are the folder's and those under it, which a delete removes.
	IDs []string `json:"ids"`
	// Fixed says the folder has a role, such as Inbox or Trash, and can't be
	// renamed or deleted.
	Fixed bool `json:"fixed"`
	// InTrash says the folder is in Trash, so deleting it is for good.
	InTrash bool `json:"in_trash"`
	// TrashName names the Trash a delete moves the folder to, or is "" when
	// a delete is for good.
	TrashName string `json:"trash_name"`
}

// folderChange is a folder change's answer: the folder made or renamed.
type folderChange struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// folderRefusal is a change refused for a reason the person can act on; its
// text is shown as it is.
type folderRefusal string

func (r folderRefusal) Error() string { return string(r) }

func writeFolderJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// writeFolderError answers a failed change: a refusal or a server's answer
// as text the page shows, anything else as a plain failure.
func writeFolderError(w http.ResponseWriter, r *http.Request, action string, err error) {
	var refusal folderRefusal
	var trashRefusal folderTrashRefusal
	switch {
	case errors.As(err, &trashRefusal):
		http.Error(w, string(trashRefusal), http.StatusConflict)
	case errors.As(err, &refusal):
		http.Error(w, string(refusal), http.StatusBadRequest)
	case errors.Is(err, storage.ErrFolderNotManaged), errors.Is(err, errFolderAccount):
		http.NotFound(w, r)
	case errors.Is(err, errFolderServer):
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		log.Printf("%s: %v", action, err)
		http.Error(w, "Couldn't "+action+".", http.StatusInternalServerError)
	}
}

var (
	errFolderAccount = errors.New("not an account of the owner")
	// errFolderServer wraps the server's refusal or a failed connection.
	errFolderServer = errors.New("the server refused")
)

func folderServerError(err error) error {
	return &folderServerFailure{err}
}

type folderServerFailure struct{ err error }

func (e *folderServerFailure) Error() string { return "The server said: " + e.err.Error() }
func (e *folderServerFailure) Unwrap() []error {
	return []error{errFolderServer, e.err}
}

// cleanFolderName checks a name typed for a folder or label. Gmail nests
// labels with "/", so only Gmail takes one.
func cleanFolderName(provider, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", folderRefusal("Give the folder a name.")
	}
	if utf8.RuneCountInString(name) > folderNameMaxLength {
		return "", folderRefusal("That name is too long.")
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return "", folderRefusal("A folder name can't have line breaks or tabs.")
	}
	if provider != providers.ProviderGmail && strings.Contains(name, "/") {
		return "", folderRefusal(`A folder name can't contain "/". To put a folder inside another, use New subfolder on that folder.`)
	}
	if provider == providers.ProviderGmail && (strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//")) {
		return "", folderRefusal(`A label name can't start or end with "/".`)
	}
	return name, nil
}

// folderAccountProvider returns the provider of one of the owner's accounts.
func (h *Handler) folderAccountProvider(ctx context.Context, owner, accountID string) (string, error) {
	var provider string
	err := h.withProfileAccount(ctx, owner, accountID, func(accounts *config.AccountStore, _ *storage.DB) error {
		account, err := accounts.GetAccountByIDForUser(ctx, owner, accountID)
		if err != nil {
			return err
		}
		if account == nil || account.IsDeleting {
			return errFolderAccount
		}
		provider = account.Provider
		return nil
	})
	return provider, err
}

// managedFolder returns one of the owner's folders, with its account's
// provider.
func (h *Handler) managedFolder(ctx context.Context, owner, folderID string) (storage.ManagedFolder, string, error) {
	var folder storage.ManagedFolder
	err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		folder, err = db.ManagedFolderForUser(ctx, owner, folderID)
		return err
	})
	if err != nil {
		return storage.ManagedFolder{}, "", err
	}
	provider, err := h.folderAccountProvider(ctx, owner, folder.AccountID)
	return folder, provider, err
}

// handleFolderInfo tells the sidebar about a folder before it's renamed or
// deleted.
func (h *Handler) handleFolderInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, provider, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err != nil {
		writeFolderError(w, r, "load the folder", err)
		return
	}
	info := folderInfo{
		ID: folder.ID, Name: folder.Name, Provider: provider,
		Messages: folder.Messages, Subfolders: len(folder.Descendants),
		IDs: folderAndDescendantIDs(folder), Fixed: folder.Role != "custom", InTrash: folder.InTrash,
	}
	if !folder.InTrash && provider != providers.ProviderGmail {
		err := h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			trash, err := db.TrashFolder(ctx, owner, folder.AccountID)
			if errors.Is(err, storage.ErrFolderNotManaged) {
				return nil
			}
			info.TrashName = trash.Name
			return err
		})
		if err != nil {
			writeFolderError(w, r, "load the folder", err)
			return
		}
	}
	writeFolderJSON(w, info)
}

// handleCreateFolder makes a folder on the server, inside the folder named
// by "parent" or at the top, and saves it.
func (h *Handler) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	accountID := r.PathValue("id")
	created, err := h.createFolder(ctx, owner, accountID, r.PostFormValue("parent"), r.PostFormValue("name"))
	if err != nil {
		writeFolderError(w, r, "create the folder", err)
		return
	}
	h.syncMailAccount(context.WithoutCancel(ctx), owner, accountID)
	writeFolderJSON(w, created)
}

func (h *Handler) createFolder(ctx context.Context, owner, accountID, parentID, typed string) (folderChange, error) {
	provider, err := h.folderAccountProvider(ctx, owner, accountID)
	if err != nil {
		return folderChange{}, err
	}
	if provider == providers.ProviderGmail {
		return folderChange{}, folderRefusal("Gmail has labels instead of folders. Use New label.")
	}
	name, err := cleanFolderName(provider, typed)
	if err != nil {
		return folderChange{}, err
	}
	var parent storage.ManagedFolder
	if parentID = strings.TrimSpace(parentID); parentID != "" {
		if parent, _, err = h.managedFolder(ctx, owner, parentID); err != nil {
			return folderChange{}, err
		}
		if parent.AccountID != accountID {
			return folderChange{}, storage.ErrFolderNotManaged
		}
	}
	var sortOrder int
	err = h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		taken, err := db.FolderNameTaken(ctx, accountID, parent.ID, name, "")
		if err != nil {
			return err
		}
		if taken {
			return folderRefusal("A folder named “" + name + "” is already there.")
		}
		sortOrder, err = db.NextFolderSortOrder(ctx, accountID)
		return err
	})
	if err != nil {
		return folderChange{}, err
	}
	cfg, password, err := h.mailAccountAccess(ctx, owner, accountID)
	if err != nil {
		return folderChange{}, err
	}

	switch provider {
	case providers.ProviderOutlook:
		collection := outlookGraphBaseURL + "/me/mailFolders"
		if parent.ID != "" {
			collection += "/" + url.PathEscape(parent.ProviderRemoteID) + "/childFolders"
		}
		var made struct {
			ID string `json:"id"`
		}
		if err := h.folderAPI(owner, accountID).send(ctx, http.MethodPost, collection, map[string]string{"displayName": name}, &made); err != nil {
			return folderChange{}, folderServerError(err)
		}
		if made.ID == "" {
			return folderChange{}, folderServerError(errors.New("Outlook did not create the folder"))
		}
		return h.saveProviderFolder(ctx, owner, accountID, storage.FolderDiscoveryOutlook, parent, made.ID, name, sortOrder)
	case providers.ProviderJMAP:
		folders, err := mail.OpenJMAPFolders(ctx, cfg, password)
		if err != nil {
			return folderChange{}, folderServerError(err)
		}
		id, err := folders.Create(ctx, name, parent.ProviderRemoteID)
		if err != nil {
			return folderChange{}, folderServerError(err)
		}
		return h.saveProviderFolder(ctx, owner, accountID, storage.FolderDiscoveryJMAP, parent, id, name, sortOrder)
	}

	client, err := imapclient.NewContextClient(ctx, cfg, password)
	if err != nil {
		return folderChange{}, folderServerError(err)
	}
	defer client.Close()
	mailbox, err := client.NewMailboxName(ctx, parent.RemoteID, name)
	if err != nil {
		return folderChange{}, folderRefusal(capitalize(err.Error()) + ".")
	}
	delimiter, err := client.Delimiter(ctx)
	if err != nil {
		return folderChange{}, folderServerError(err)
	}
	if err := client.CreateSubscribedMailbox(ctx, mailbox); err != nil {
		return folderChange{}, folderServerError(err)
	}
	var change folderChange
	err = h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		ids, err := db.IMAPFolderIDs(ctx, accountID)
		if err != nil {
			return err
		}
		// Placed as sync places it: under the folder its name is inside.
		parentID, shown := imapFolderPlace(mailbox, delimiter, ids)
		change.Name = shown
		change.ID, err = db.AddIMAPFolder(ctx, accountID, parentID, mailbox, shown, sortOrder)
		return err
	})
	return change, err
}

// imapFolderPlace returns the folder a custom mailbox shows under, and the
// name it shows with, the way sync places it.
func imapFolderPlace(mailbox string, delimiter rune, ids map[string]string) (parentID, name string) {
	if delimiter == 0 {
		return "", mailbox
	}
	i := strings.LastIndex(mailbox, string(delimiter))
	if i <= 0 {
		return "", mailbox
	}
	if parentID = ids[mailbox[:i]]; parentID == "" {
		return "", mailbox
	}
	return parentID, mailbox[i+len(string(delimiter)):]
}

func (h *Handler) saveProviderFolder(ctx context.Context, owner, accountID, kind string, parent storage.ManagedFolder, providerID, name string, sortOrder int) (folderChange, error) {
	path := name
	if parent.ID != "" {
		path = parent.RemoteID + "/" + name
	}
	change := folderChange{Name: name}
	err := h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		change.ID, err = db.AddProviderFolder(ctx, accountID, kind, parent.ID, providerID, path, name, sortOrder)
		return err
	})
	return change, err
}

// handleRenameFolder renames a folder on the server and here.
func (h *Handler) handleRenameFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, provider, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err != nil {
		writeFolderError(w, r, "rename the folder", err)
		return
	}
	renamed, err := h.renameFolder(ctx, owner, folder, provider, r.PostFormValue("name"))
	if err != nil {
		writeFolderError(w, r, "rename the folder", err)
		return
	}
	h.folderMailboxesChanged(ctx, owner, folder.AccountID)
	writeFolderJSON(w, renamed)
}

func (h *Handler) renameFolder(ctx context.Context, owner string, folder storage.ManagedFolder, provider, typed string) (folderChange, error) {
	if folder.Role != "custom" {
		return folderChange{}, folderRefusal("“" + folder.Name + "” is one of the account's own folders, so it can't be renamed.")
	}
	name, err := cleanFolderName(provider, typed)
	if err != nil {
		return folderChange{}, err
	}
	change := folderChange{ID: folder.ID, Name: name}
	if name == folder.Name {
		return change, nil
	}
	err = h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		taken, err := db.FolderNameTaken(ctx, folder.AccountID, folder.ParentID, name, folder.ID)
		if err == nil && taken {
			err = folderRefusal("A folder named “" + name + "” is already there.")
		}
		return err
	})
	if err != nil {
		return folderChange{}, err
	}
	cfg, password, err := h.mailAccountAccess(ctx, owner, folder.AccountID)
	if err != nil {
		return folderChange{}, err
	}
	saveRename := func(oldRemote, newRemote string, delimiter rune) error {
		return h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			return db.RenameFolder(ctx, folder.AccountID, folder.ID, oldRemote, newRemote, name, delimiter)
		})
	}
	newPath := name
	if i := strings.LastIndex(folder.RemoteID, "/"); i >= 0 && folder.ParentID != "" {
		newPath = folder.RemoteID[:i+1] + name
	}

	switch provider {
	case providers.ProviderGmail:
		return folderChange{}, folderRefusal("Gmail has labels instead of folders.")
	case providers.ProviderOutlook:
		endpoint := outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(folder.ProviderRemoteID)
		if err := h.folderAPI(owner, folder.AccountID).send(ctx, http.MethodPatch, endpoint, map[string]string{"displayName": name}, nil); err != nil {
			return folderChange{}, folderServerError(err)
		}
		return change, saveRename(folder.RemoteID, newPath, '/')
	case providers.ProviderJMAP:
		folders, err := mail.OpenJMAPFolders(ctx, cfg, password)
		if err != nil {
			return folderChange{}, folderServerError(err)
		}
		if err := folders.Rename(ctx, folder.ProviderRemoteID, name); err != nil {
			return folderChange{}, folderServerError(err)
		}
		return change, saveRename(folder.RemoteID, newPath, '/')
	}

	client, err := imapclient.NewContextClient(ctx, cfg, password)
	if err != nil {
		return folderChange{}, folderServerError(err)
	}
	defer client.Close()
	mailbox, err := client.RenamedMailboxName(ctx, folder.RemoteID, name)
	if err != nil {
		return folderChange{}, folderRefusal(capitalize(err.Error()) + ".")
	}
	delimiter, err := client.Delimiter(ctx)
	if err != nil {
		return folderChange{}, folderServerError(err)
	}
	if err := client.RenameMailbox(ctx, folder.RemoteID, mailbox); err != nil {
		return folderChange{}, folderServerError(err)
	}
	// A top folder shows its whole mailbox name, as sync shows it.
	if folder.ParentID == "" {
		change.Name = mailbox
		name = mailbox
	}
	return change, saveRename(folder.RemoteID, mailbox, delimiter)
}

// folderMailboxesChanged syncs an account whose folders were renamed or
// deleted, and moves its real-time watches off the folders' old names.
// With per-user storage the watches follow the folders on their own.
func (h *Handler) folderMailboxesChanged(ctx context.Context, owner, accountID string) {
	h.syncMailAccount(context.WithoutCancel(ctx), owner, accountID)
	if h.userIMAP == nil && h.syncer != nil {
		h.syncer.RestartIDLEWatchers([]string{accountID})
	}
}

// handleDeleteFolder moves a folder, with the folders and messages in it,
// into the account's Trash. With "permanent" set, or for a folder already
// in Trash, it deletes them on the server and here instead; a delete that
// wasn't asked to be permanent never is, so the page warns first.
func (h *Handler) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, provider, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err != nil {
		writeFolderError(w, r, "delete the folder", err)
		return
	}
	if r.PostFormValue("permanent") == "1" {
		err = h.deleteFolder(ctx, owner, folder, provider)
	} else {
		err = h.trashFolder(ctx, owner, folder, provider)
	}
	if err != nil {
		writeFolderError(w, r, "delete the folder", err)
		return
	}
	h.folderMailboxesChanged(ctx, owner, folder.AccountID)
	writeFolderJSON(w, folderChange{ID: folder.ID})
}

// folderTrashRefusal is a folder that can't go into Trash; the page offers
// to delete it for good instead.
type folderTrashRefusal string

func (r folderTrashRefusal) Error() string { return string(r) }

func checkFolderChangeable(folder storage.ManagedFolder, action string) error {
	if folder.Role != "custom" {
		return folderRefusal("“" + folder.Name + "” is one of the account's own folders, so it can't be " + action + ".")
	}
	for _, child := range folder.Descendants {
		if child.Role != "custom" {
			return folderRefusal("“" + child.Name + "”, inside this folder, is one of the account's own folders, so this folder can't be " + action + ".")
		}
	}
	return nil
}

// trashFolder moves a folder into its account's Trash.
func (h *Handler) trashFolder(ctx context.Context, owner string, folder storage.ManagedFolder, provider string) error {
	if err := checkFolderChangeable(folder, "deleted"); err != nil {
		return err
	}
	if provider == providers.ProviderGmail {
		return folderRefusal("Gmail has labels instead of folders.")
	}
	if folder.InTrash {
		return folderTrashRefusal("“" + folder.Name + "” is already in Trash.")
	}
	var trash storage.ManagedFolder
	err := h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		trash, err = db.TrashFolder(ctx, owner, folder.AccountID)
		return err
	})
	if errors.Is(err, storage.ErrFolderNotManaged) {
		return folderTrashRefusal("This account has no Trash folder to put “" + folder.Name + "” in.")
	}
	if err != nil {
		return err
	}
	err = h.moveFolder(ctx, owner, folder, provider, &trash)
	var refusal folderRefusal
	if errors.As(err, &refusal) || errors.Is(err, errFolderServer) {
		return folderTrashRefusal("“" + folder.Name + "” couldn't be moved to Trash. " + strings.TrimSuffix(err.Error(), ".") + ".")
	}
	return err
}

// handleRestoreFolder moves a folder out of Trash, to the top of its
// account.
func (h *Handler) handleRestoreFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, provider, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err == nil {
		if err = checkFolderChangeable(folder, "moved"); err == nil && !folder.InTrash {
			err = folderRefusal("“" + folder.Name + "” isn't in Trash.")
		}
	}
	if err == nil {
		err = h.moveFolder(ctx, owner, folder, provider, nil)
	}
	if err != nil {
		writeFolderError(w, r, "move the folder out of Trash", err)
		return
	}
	h.folderMailboxesChanged(ctx, owner, folder.AccountID)
	writeFolderJSON(w, folderChange{ID: folder.ID})
}

// freeFolderName returns name, or name with a number after it, so no other
// folder under parentID has it.
func (h *Handler) freeFolderName(ctx context.Context, owner, accountID, parentID, name, exceptID string) (string, error) {
	free := name
	err := h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		for n := 2; ; n++ {
			taken, err := db.FolderNameTaken(ctx, accountID, parentID, free, exceptID)
			if err != nil || !taken {
				return err
			}
			free = name + " " + strconv.Itoa(n)
		}
	})
	return free, err
}

// moveFolder moves a folder, with what's in it, inside target, or to the
// top of the account when target is nil. It keeps its name unless a folder
// there has it already.
func (h *Handler) moveFolder(ctx context.Context, owner string, folder storage.ManagedFolder, provider string, target *storage.ManagedFolder) error {
	targetID := ""
	if target != nil {
		targetID = target.ID
	}
	name, err := h.freeFolderName(ctx, owner, folder.AccountID, targetID, folder.Name, folder.ID)
	if err != nil {
		return err
	}
	path := name
	if target != nil {
		path = target.RemoteID + "/" + name
	}
	saveMove := func(parentID, newRemote, shown string, delimiter rune) error {
		return h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			return db.MoveFolder(ctx, folder.AccountID, folder.ID, parentID, folder.RemoteID, newRemote, shown, delimiter)
		})
	}
	cfg, password, err := h.mailAccountAccess(ctx, owner, folder.AccountID)
	if err != nil {
		return err
	}

	switch provider {
	case providers.ProviderGmail:
		return folderRefusal("Gmail has labels instead of folders.")
	case providers.ProviderOutlook:
		api := h.folderAPI(owner, folder.AccountID)
		folderURL := outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(folder.ProviderRemoteID)
		if name != folder.Name {
			if err := api.send(ctx, http.MethodPatch, folderURL, map[string]string{"displayName": name}, nil); err != nil {
				return folderServerError(err)
			}
		}
		destination := "msgfolderroot"
		if target != nil {
			destination = target.ProviderRemoteID
		}
		var moved struct {
			ID string `json:"id"`
		}
		if err := api.send(ctx, http.MethodPost, folderURL+"/move", map[string]string{"destinationId": destination}, &moved); err != nil {
			return folderServerError(err)
		}
		if moved.ID != "" && moved.ID != folder.ProviderRemoteID {
			// Outlook gave the moved folder a new id, so it's a new folder
			// here: the old one goes, and sync brings the new one.
			return h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
				return db.RemoveDeletedFolders(ctx, folder.AccountID, storage.FolderDiscoveryOutlook, folderAndDescendantIDs(folder))
			})
		}
		return saveMove(targetID, path, name, '/')
	case providers.ProviderJMAP:
		folders, err := mail.OpenJMAPFolders(ctx, cfg, password)
		if err != nil {
			return folderServerError(err)
		}
		parent := ""
		if target != nil {
			parent = target.ProviderRemoteID
		}
		if err := folders.Move(ctx, folder.ProviderRemoteID, parent, name); err != nil {
			return folderServerError(err)
		}
		return saveMove(targetID, path, name, '/')
	}

	client, err := imapclient.NewContextClient(ctx, cfg, password)
	if err != nil {
		return folderServerError(err)
	}
	defer client.Close()
	delimiter, err := client.Delimiter(ctx)
	if err != nil {
		return folderServerError(err)
	}
	// A top folder Gofer shows by its whole mailbox name moves by its own.
	if i := strings.LastIndex(name, string(delimiter)); delimiter != 0 && i >= 0 {
		name = name[i+len(string(delimiter)):]
	}
	var mailbox string
	if target != nil {
		if delimiter == 0 {
			return folderRefusal("This server doesn't have folders inside folders.")
		}
		mailbox = target.RemoteID + string(delimiter) + name
	} else if mailbox, err = client.NewMailboxName(ctx, "", name); err != nil {
		return folderRefusal(capitalize(err.Error()) + ".")
	}
	if err := client.RenameMailbox(ctx, folder.RemoteID, mailbox); err != nil {
		return folderServerError(err)
	}
	return h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		ids, err := db.IMAPFolderIDs(ctx, folder.AccountID)
		if err != nil {
			return err
		}
		parentID, shown := imapFolderPlace(mailbox, delimiter, ids)
		return db.MoveFolder(ctx, folder.AccountID, folder.ID, parentID, folder.RemoteID, mailbox, shown, delimiter)
	})
}

func folderAndDescendantIDs(folder storage.ManagedFolder) []string {
	ids := []string{folder.ID}
	for _, child := range folder.Descendants {
		ids = append(ids, child.ID)
	}
	return ids
}

// handleDeleteTrashFolders deletes for good the folders inside a Trash
// folder, or inside every account's for the unified Trash, as emptying
// Trash does with its messages.
func (h *Handler) handleDeleteTrashFolders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	var ids []string
	err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		ids, err = db.FoldersInTrash(ctx, owner, r.PathValue("id"))
		return err
	})
	if err != nil {
		writeFolderError(w, r, "delete the folders in Trash", err)
		return
	}
	deleted, failed := 0, 0
	changed := map[string]bool{}
	for _, id := range ids {
		folder, provider, err := h.managedFolder(ctx, owner, id)
		if err == nil {
			err = h.deleteFolder(ctx, owner, folder, provider)
		}
		if err != nil {
			log.Printf("delete folder %s in Trash: %v", id, err)
			failed++
			continue
		}
		deleted++
		changed[folder.AccountID] = true
	}
	for accountID := range changed {
		h.folderMailboxesChanged(ctx, owner, accountID)
	}
	writeFolderJSON(w, map[string]int{"deleted": deleted, "failed": failed})
}

// deleteFolder deletes a folder, the folders under it and their messages
// for good. On Outlook the folder goes to Deleted Items, or out of reach
// from there.
func (h *Handler) deleteFolder(ctx context.Context, owner string, folder storage.ManagedFolder, provider string) error {
	if err := checkFolderChangeable(folder, "deleted"); err != nil {
		return err
	}
	cfg, password, err := h.mailAccountAccess(ctx, owner, folder.AccountID)
	if err != nil {
		return err
	}
	// Children come first, deepest first, then the folder itself.
	all := append(append([]storage.ManagedFolder{}, folder.Descendants...), folder)
	ids := make([]string, 0, len(all))
	for _, entry := range all {
		ids = append(ids, entry.ID)
	}
	forget := func(kind string, deleted []string) error {
		return h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			return db.RemoveDeletedFolders(ctx, folder.AccountID, kind, deleted)
		})
	}

	switch provider {
	case providers.ProviderGmail:
		return folderRefusal("Gmail has labels instead of folders.")
	case providers.ProviderOutlook:
		// Outlook moves a folder outside Deleted Items there, and one inside
		// it out of reach.
		endpoint := outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(folder.ProviderRemoteID)
		if err := h.folderAPI(owner, folder.AccountID).send(ctx, http.MethodDelete, endpoint, nil, nil); err != nil {
			return folderServerError(err)
		}
		return forget(storage.FolderDiscoveryOutlook, ids)
	case providers.ProviderJMAP:
		folders, err := mail.OpenJMAPFolders(ctx, cfg, password)
		if err != nil {
			return folderServerError(err)
		}
		mailboxes := make([]string, 0, len(all))
		for _, entry := range all {
			mailboxes = append(mailboxes, entry.ProviderRemoteID)
		}
		if err := folders.Delete(ctx, mailboxes); err != nil {
			// Some may be gone; the sync that follows shows which.
			return folderServerError(err)
		}
		return forget(storage.FolderDiscoveryJMAP, ids)
	}

	client, err := imapclient.NewContextClient(ctx, cfg, password)
	if err != nil {
		return folderServerError(err)
	}
	defer client.Close()
	var deleted []string
	for _, entry := range all {
		if err := client.DeleteMailbox(ctx, entry.RemoteID); err != nil {
			if forgetErr := forget(storage.FolderDiscoveryIMAP, deleted); forgetErr != nil {
				log.Printf("forget deleted folders account=%s: %v", folder.AccountID, forgetErr)
			}
			return folderServerError(err)
		}
		deleted = append(deleted, entry.ID)
	}
	return forget(storage.FolderDiscoveryIMAP, deleted)
}

// folderAPI calls the Gmail or Outlook API as an account.
func (h *Handler) folderAPI(owner, accountID string) folderAPICaller {
	return folderAPICaller{mailImportCaller{token: h.mailImportToken(owner, accountID)}}
}

type folderAPICaller struct {
	mailImportCaller
}

// send makes one API request with a JSON body, when body isn't nil, and
// reads a JSON answer into out, when it isn't nil.
func (c folderAPICaller) send(ctx context.Context, method, endpoint string, body, out any) error {
	var payload []byte
	contentType := ""
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
		contentType = "application/json"
	}
	return c.call(ctx, method, endpoint, contentType, func() io.Reader {
		if payload == nil {
			return nil
		}
		return bytes.NewReader(payload)
	}, out)
}

// Gmail labels.

// gmailLabelAccount returns one of the owner's Gmail accounts by ID, or a
// refusal for any other account.
func (h *Handler) gmailLabelAccount(ctx context.Context, owner, accountID string) error {
	provider, err := h.folderAccountProvider(ctx, owner, accountID)
	if err != nil {
		return err
	}
	if provider != providers.ProviderGmail {
		return folderRefusal("Only Gmail accounts have labels to manage here.")
	}
	return nil
}

// ownedGmailLabel returns one of the owner's Gmail labels with the labels
// nested under it.
func (h *Handler) ownedGmailLabel(ctx context.Context, owner, labelID string) (models.Label, []models.Label, error) {
	var label models.Label
	var nested []models.Label
	err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		if label, err = db.ProviderLabelForUser(ctx, owner, labelID); err != nil {
			return err
		}
		if label.ProviderType != storage.LabelProviderGmail {
			return storage.ErrFolderNotManaged
		}
		all, err := db.ProviderLabels(ctx, label.AccountID, storage.LabelProviderGmail)
		if err != nil {
			return err
		}
		prefix := strings.ToLower(label.Name + "/")
		for _, other := range all {
			if strings.HasPrefix(strings.ToLower(other.Name), prefix) {
				nested = append(nested, other)
			}
		}
		return nil
	})
	if err != nil {
		return models.Label{}, nil, err
	}
	return label, nested, h.gmailLabelAccount(ctx, owner, label.AccountID)
}

func gmailLabelTaken(labels []models.Label, name, exceptID string) bool {
	for _, label := range labels {
		if label.ID != exceptID && strings.EqualFold(label.Name, name) {
			return true
		}
	}
	return false
}

func (h *Handler) accountGmailLabels(ctx context.Context, owner, accountID string) ([]models.Label, error) {
	var labels []models.Label
	err := h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		labels, err = db.ProviderLabels(ctx, accountID, storage.LabelProviderGmail)
		return err
	})
	return labels, err
}

// labelInfo is what the sidebar asks before renaming or deleting a label.
type labelInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Nested int    `json:"nested"`
}

func (h *Handler) handleLabelInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	label, nested, err := h.ownedGmailLabel(ctx, h.userID(ctx), r.PathValue("id"))
	if err != nil {
		writeFolderError(w, r, "load the label", err)
		return
	}
	writeFolderJSON(w, labelInfo{ID: label.ID, Name: label.Name, Nested: len(nested)})
}

// handleCreateLabel makes a Gmail label. Gmail shows a label with "/" in
// its name inside the label named by what comes before.
func (h *Handler) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	accountID := r.PathValue("id")
	created, err := h.createGmailLabel(ctx, owner, accountID, r.PostFormValue("name"))
	if err != nil {
		writeFolderError(w, r, "create the label", err)
		return
	}
	writeFolderJSON(w, created)
}

func (h *Handler) createGmailLabel(ctx context.Context, owner, accountID, typed string) (folderChange, error) {
	if err := h.gmailLabelAccount(ctx, owner, accountID); err != nil {
		return folderChange{}, err
	}
	name, err := cleanFolderName(providers.ProviderGmail, typed)
	if err != nil {
		return folderChange{}, err
	}
	labels, err := h.accountGmailLabels(ctx, owner, accountID)
	if err != nil {
		return folderChange{}, err
	}
	if gmailLabelTaken(labels, name, "") {
		return folderChange{}, folderRefusal("There's already a label named “" + name + "”.")
	}
	var made struct {
		ID string `json:"id"`
	}
	request := map[string]string{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"}
	if err := h.folderAPI(owner, accountID).send(ctx, http.MethodPost, gmailAPIBaseURL+"/users/me/labels", request, &made); err != nil {
		return folderChange{}, folderServerError(err)
	}
	if made.ID == "" {
		return folderChange{}, folderServerError(errors.New("Gmail did not create the label"))
	}
	err = h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		return db.UpsertLabels(ctx, []storage.LabelInput{{
			AccountID: accountID, Name: name, ProviderID: made.ID, ProviderType: storage.LabelProviderGmail,
		}})
	})
	return folderChange{Name: name}, err
}

// handleRenameLabel renames a Gmail label and the labels nested in it.
func (h *Handler) handleRenameLabel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	label, nested, err := h.ownedGmailLabel(ctx, owner, r.PathValue("id"))
	if err != nil {
		writeFolderError(w, r, "rename the label", err)
		return
	}
	renamed, err := h.renameGmailLabel(ctx, owner, label, nested, r.PostFormValue("name"))
	if err != nil {
		writeFolderError(w, r, "rename the label", err)
		return
	}
	writeFolderJSON(w, renamed)
}

func (h *Handler) renameGmailLabel(ctx context.Context, owner string, label models.Label, nested []models.Label, typed string) (folderChange, error) {
	name, err := cleanFolderName(providers.ProviderGmail, typed)
	if err != nil {
		return folderChange{}, err
	}
	change := folderChange{ID: label.ID, Name: name}
	if name == label.Name {
		return change, nil
	}
	labels, err := h.accountGmailLabels(ctx, owner, label.AccountID)
	if err != nil {
		return folderChange{}, err
	}
	if gmailLabelTaken(labels, name, label.ID) {
		return folderChange{}, folderRefusal("There's already a label named “" + name + "”.")
	}
	names := map[string]string{label.ProviderID: name}
	for _, child := range nested {
		names[child.ProviderID] = name + child.Name[len(label.Name):]
	}
	api := h.folderAPI(owner, label.AccountID)
	renamed := map[string]string{}
	var failure error
	for providerID, newName := range names {
		endpoint := gmailAPIBaseURL + "/users/me/labels/" + url.PathEscape(providerID)
		if err := api.send(ctx, http.MethodPatch, endpoint, map[string]string{"name": newName}, nil); err != nil {
			failure = folderServerError(err)
			break
		}
		renamed[providerID] = newName
	}
	err = h.withProfileAccount(ctx, owner, label.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		return db.RenameProviderLabels(ctx, label.AccountID, storage.LabelProviderGmail, renamed)
	})
	return change, errors.Join(failure, err)
}

// handleDeleteLabel deletes a Gmail label and the labels nested in it. The
// messages stay in Gmail, without the labels.
func (h *Handler) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	label, nested, err := h.ownedGmailLabel(ctx, owner, r.PathValue("id"))
	if err != nil {
		writeFolderError(w, r, "delete the label", err)
		return
	}
	api := h.folderAPI(owner, label.AccountID)
	var deleted []string
	var failure error
	for _, entry := range append(nested, label) {
		endpoint := gmailAPIBaseURL + "/users/me/labels/" + url.PathEscape(entry.ProviderID)
		if err := api.send(ctx, http.MethodDelete, endpoint, nil, nil); err != nil {
			var apiErr *mailImportAPIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
				failure = folderServerError(err)
				break
			}
		}
		deleted = append(deleted, entry.ProviderID)
	}
	err = h.withProfileAccount(ctx, owner, label.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		return db.DeleteProviderLabels(ctx, label.AccountID, storage.LabelProviderGmail, deleted)
	})
	if err = errors.Join(failure, err); err != nil {
		writeFolderError(w, r, "delete the label", err)
		return
	}
	writeFolderJSON(w, folderChange{ID: label.ID})
}

// capitalize starts an error's text with a capital, to show it as a
// sentence.
func capitalize(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return text
	}
	return strings.ToUpper(string(first)) + text[size:]
}
