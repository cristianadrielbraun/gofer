package handler

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
)

// mcpPanelError finds the error a handler rendered into an HTML panel, the
// element carrying marker, so a panel answered with 200 still fails.
func mcpPanelError(body []byte, marker string) error {
	re := regexp.MustCompile(`(?s)<[^>]*` + regexp.QuoteMeta(marker) + `[^>]*>(.*?)</`)
	if m := re.FindSubmatch(body); m != nil {
		return errors.New(strings.TrimSpace(html.UnescapeString(string(m[1]))))
	}
	return nil
}

func (h *Handler) mcpSimple(ctx context.Context, handler http.HandlerFunc, in mcpRequest) (map[string]any, error) {
	resp, err := h.mcpInvoke(ctx, handler, in)
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	out := map[string]any{}
	_ = resp.Decode(&out)
	return out, nil
}

type mcpFolderIn struct {
	FolderID string `json:"folder_id"`
}

type mcpCreateFolderIn struct {
	AccountID      string `json:"account_id"`
	Name           string `json:"name"`
	ParentFolderID string `json:"parent_folder_id,omitempty" jsonschema:"make it inside this folder; at the top when empty"`
}

type mcpRenameFolderIn struct {
	FolderID string `json:"folder_id"`
	Name     string `json:"name"`
}

type mcpMoveFolderIn struct {
	FolderID       string `json:"folder_id"`
	ParentFolderID string `json:"parent_folder_id,omitempty" jsonschema:"the folder to put it in; at the top when empty"`
}

type mcpDeleteFolderIn struct {
	FolderID  string `json:"folder_id"`
	Permanent bool   `json:"permanent,omitempty" jsonschema:"delete it and its mail for good instead of moving it to Trash"`
	Confirm   bool   `json:"confirm,omitempty" jsonschema:"must be true"`
}

type mcpEmptyFolderIn struct {
	FolderID string `json:"folder_id" jsonschema:"a Trash or Spam folder"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"must be true: its mail is deleted for good"`
}

type mcpAppearanceIn struct {
	FolderID string `json:"folder_id,omitempty"`
	TagID    string `json:"tag_id,omitempty"`
	Color    string `json:"color,omitempty" jsonschema:"#rrggbb, or empty for none"`
	Icon     string `json:"icon,omitempty" jsonschema:"one of the icon names in the tool's description, for a folder of your own; empty for the default"`
	Style    string `json:"style,omitempty" jsonschema:"outline or filled"`
}

type mcpFolderOrderIn struct {
	AccountID string   `json:"account_id"`
	FolderIDs []string `json:"folder_ids" jsonschema:"folders next to each other (same parent), in the order wanted"`
}

type mcpFolderRolesIn struct {
	AccountID string            `json:"account_id"`
	Roles     map[string]string `json:"roles" jsonschema:"role (sent, drafts, trash, junk, archive) to folder ID"`
}

type mcpTagIn2 struct {
	TagID   string `json:"tag_id"`
	Name    string `json:"name,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

type mcpTagsOut struct {
	Tags []mcpTag `json:"tags"`
}

type mcpTag struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id,omitempty"`
	Name      string `json:"name"`
	Color     string `json:"color,omitempty"`
}

type mcpIdentity struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	ReplyTo       string `json:"reply_to,omitempty"`
	SignatureID   string `json:"signature_id,omitempty"`
	Default       bool   `json:"default,omitempty"`
	FromProvider  bool   `json:"from_provider,omitempty"`
	SharedMailbox string `json:"shared_mailbox,omitempty"`
	State         string `json:"state,omitempty" jsonschema:"for a shared mailbox's identity: active, inactive (mailbox no longer shared) or disabled"`
	SenderCheck   string `json:"sender_check,omitempty" jsonschema:"accepted, rejected or unknown: what the server said about sending from it"`
	SenderMessage string `json:"sender_message,omitempty"`
}

type mcpIdentitiesOut struct {
	Identities []mcpIdentity `json:"identities"`
}

type mcpSaveIdentityIn struct {
	AccountID   string `json:"account_id"`
	IdentityID  string `json:"identity_id,omitempty" jsonschema:"to change an identity; a new one is made when empty"`
	Name        string `json:"name,omitempty"`
	Email       string `json:"email"`
	ReplyTo     string `json:"reply_to,omitempty"`
	SignatureID string `json:"signature_id,omitempty"`
}

type mcpIdentityIn struct {
	AccountID  string `json:"account_id"`
	IdentityID string `json:"identity_id"`
}

type mcpDefaultIdentityIn struct {
	AccountID  string `json:"account_id"`
	IdentityID string `json:"identity_id,omitempty" jsonschema:"empty for the account's own address"`
}

type mcpSharedIdentityIn struct {
	AccountID  string `json:"account_id"`
	IdentityID string `json:"identity_id"`
	Enabled    bool   `json:"enabled"`
}

type mcpSignature struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TextBody string `json:"text_body,omitempty"`
	HTMLBody string `json:"html_body,omitempty"`
}

type mcpSignaturesOut struct {
	Signatures []mcpSignature `json:"signatures"`
}

type mcpSaveSignatureIn struct {
	SignatureID string `json:"signature_id,omitempty" jsonschema:"to change a signature; a new one is made when empty"`
	Name        string `json:"name"`
	TextBody    string `json:"text_body,omitempty"`
	HTMLBody    string `json:"html_body,omitempty"`
}

type mcpSignatureIn struct {
	SignatureID string `json:"signature_id"`
}

type mcpSignatureSettingsIn struct {
	AccountID          string `json:"account_id"`
	NewSignatureID     string `json:"new_signature_id,omitempty" jsonschema:"signature for new messages; empty for none"`
	ReplySignatureID   string `json:"reply_signature_id,omitempty"`
	ForwardSignatureID string `json:"forward_signature_id,omitempty"`
	ReplyPlacement     string `json:"reply_placement,omitempty" jsonschema:"above or below the quoted message"`
	ForwardPlacement   string `json:"forward_placement,omitempty"`
}

type mcpSettingsOut struct {
	Settings        map[string]string      `json:"settings" jsonschema:"the user's interface settings"`
	SharedMailboxes sharedmailbox.Settings `json:"shared_mailboxes"`
	SyncMinutes     int                    `json:"sync_interval_minutes"`
}

type mcpUpdateSettingsIn struct {
	Settings            map[string]string `json:"settings,omitempty" jsonschema:"interface settings to change, by key (see get_settings), such as theme, language, timezone, shared_mailboxes_auto_identities"`
	SyncIntervalMinutes int               `json:"sync_interval_minutes,omitempty" jsonschema:"how often mail is checked, in minutes"`
}

type mcpCalendarsOut struct {
	Calendars []mcpCalendar `json:"calendars"`
}

type mcpCalendar struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Color     string `json:"color,omitempty"`
}

type mcpSubscribeIn struct {
	URL            string `json:"url" jsonschema:"the calendar's .ics or webcal link"`
	Name           string `json:"name,omitempty"`
	Color          string `json:"color,omitempty"`
	RefreshMinutes int    `json:"refresh_minutes,omitempty" jsonschema:"15, 30, 60, 180, 360, 720 or 1440; 60 by default"`
}

type mcpSyncContactsIn struct {
	AccountID string `json:"account_id"`
}

func (h *Handler) mcpFolderAccount(ctx context.Context, userID, folderID string) (string, error) {
	accountID, err := h.db.GetFolderAccountID(ctx, folderID)
	if err != nil {
		return "", err
	}
	if accountID == "" {
		return "", errors.New("no folder with that ID (see list_folders)")
	}
	if _, err := h.mcpAccounts(ctx, userID, accountID); err != nil {
		return "", errors.New("no folder with that ID (see list_folders)")
	}
	return accountID, nil
}

func (h *Handler) addMCPManageTools(server *mcp.Server) {
	reads := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	changes := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(false)}
	destructive := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(true)}
	result := func(m map[string]any) mcpChangeOut { return mcpChangeOut{Result: m} }

	// Folders.
	mcp.AddTool(server, &mcp.Tool{Name: "create_folder", Title: "Make a folder", Annotations: changes,
		Description: "Make a folder in an account, on its server too, at the top or inside another folder."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpCreateFolderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleCreateFolder, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/folders", PathValues: map[string]string{"id": in.AccountID}, Form: url.Values{"name": {in.Name}, "parent": {in.ParentFolderID}}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "rename_folder", Title: "Rename a folder", Annotations: changes,
		Description: "Rename a folder, on its server too."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpRenameFolderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpFolderAccount(ctx, userID, in.FolderID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleRenameFolder, mcpRequest{Path: "/api/folders/" + in.FolderID + "/rename", PathValues: map[string]string{"id": in.FolderID}, Form: url.Values{"name": {in.Name}}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "move_folder", Title: "Move a folder", Annotations: changes,
		Description: "Put a folder inside another folder of the same account, or at the top."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpMoveFolderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpFolderAccount(ctx, userID, in.FolderID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleMoveFolderTo, mcpRequest{Path: "/api/folders/" + in.FolderID + "/move", PathValues: map[string]string{"id": in.FolderID}, Form: url.Values{"parent": {in.ParentFolderID}}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "delete_folder", Title: "Delete a folder", Annotations: destructive,
		Description: "Delete a folder: to Trash with its mail, or for good with permanent. Needs confirm: true."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpDeleteFolderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpFolderAccount(ctx, userID, in.FolderID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if !in.Confirm {
				return nil, mcpChangeOut{}, errors.New("deleting a folder takes its mail with it: ask the user, then call again with confirm: true")
			}
			form := url.Values{}
			if in.Permanent {
				form.Set("permanent", "1")
			}
			out, err := h.mcpSimple(ctx, h.handleDeleteFolder, mcpRequest{Path: "/api/folders/" + in.FolderID + "/delete", PathValues: map[string]string{"id": in.FolderID}, Form: form})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "empty_folder", Title: "Empty Trash or Spam", Annotations: destructive,
		Description: "Delete for good every message in a Trash or Spam folder. Needs confirm: true."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpEmptyFolderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpFolderAccount(ctx, userID, in.FolderID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckFolder(ctx, userID, in.FolderID, ""); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if !in.Confirm {
				return nil, mcpChangeOut{}, errors.New("emptying deletes its mail for good: ask the user, then call again with confirm: true")
			}
			out, err := h.mcpSimple(ctx, h.handleEmptyFolder, mcpRequest{Path: "/api/folders/" + in.FolderID + "/empty", PathValues: map[string]string{"id": in.FolderID}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "mark_folder_read", Title: "Mark a folder read", Annotations: changes,
		Description: "Mark every message in a folder read."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpFolderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpFolderAccount(ctx, userID, in.FolderID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckFolder(ctx, userID, in.FolderID, ""); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleMarkFolderRead, mcpRequest{Path: "/api/folders/" + in.FolderID + "/mark-read", PathValues: map[string]string{"id": in.FolderID}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_appearance", Title: "Color and icon", Annotations: changes,
		Description: "Set the color, icon and icon style of a folder (folder_id) or a tag (tag_id) in the sidebar. Only Gofer shows it; the server is not changed. Icons: " + mcpFolderIconNames() + "."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpAppearanceIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			form := url.Values{"color": {in.Color}}
			if in.Icon != "" {
				form.Set("icon", in.Icon)
			}
			if in.Style != "" {
				form.Set("style", in.Style)
			}
			var resp mcpResponse
			switch {
			case in.FolderID != "":
				if _, err := h.mcpFolderAccount(ctx, userID, in.FolderID); err != nil {
					return nil, mcpChangeOut{}, err
				}
				resp, err = h.mcpInvoke(ctx, h.handleSaveFolderAppearance, mcpRequest{Path: "/api/folders/" + in.FolderID + "/appearance", PathValues: map[string]string{"id": in.FolderID}, Form: form})
			case in.TagID != "":
				resp, err = h.mcpInvoke(ctx, h.handleSaveTagAppearance, mcpRequest{Path: "/api/tags/" + in.TagID + "/appearance", PathValues: map[string]string{"id": in.TagID}, Form: form})
			default:
				return nil, mcpChangeOut{}, errors.New("give folder_id or tag_id")
			}
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpChangeOut{}, err
			}
			return nil, result(map[string]any{"color": in.Color, "icon": in.Icon, "style": in.Style}), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "reorder_folders", Title: "Order folders", Annotations: changes,
		Description: "Put folders that sit next to each other (same parent, same account) in this order in the sidebar."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpFolderOrderIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleFolderOrder, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/folders/order", PathValues: map[string]string{"id": in.AccountID}, Form: url.Values{"folder_id": in.FolderIDs}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_folder_roles", Title: "Choose special folders", Annotations: changes,
		Description: "Choose which of an IMAP account's folders is its Sent, Drafts, Trash, Junk or Archive, when the server does not say."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpFolderRolesIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			form := url.Values{}
			for role, folderID := range in.Roles {
				form.Set("role_"+strings.ToLower(strings.TrimSpace(role)), folderID)
			}
			resp, err := h.mcpInvoke(ctx, h.handleSaveFolderRoles, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/folder-roles", PathValues: map[string]string{"id": in.AccountID}, Form: form})
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpChangeOut{}, err
			}
			return nil, result(map[string]any{"saved": true}), nil
		})

	// Tags.
	mcp.AddTool(server, &mcp.Tool{Name: "list_tags", Title: "List tags", Annotations: reads,
		Description: "The tags (labels) of the user's accounts, with their IDs for the other tag tools."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ mcpListAccountsIn) (*mcp.CallToolResult, mcpTagsOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpTagsOut{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, "")
			out := mcpTagsOut{Tags: []mcpTag{}}
			for _, a := range accounts {
				if !mcpPolicy(ctx).AllowsAccount(a.ID) {
					continue
				}
				for _, l := range a.Labels {
					out.Tags = append(out.Tags, mcpTag{ID: l.ID, AccountID: a.ID, Name: l.Name, Color: l.Color})
				}
			}
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "rename_tag", Title: "Rename a tag", Annotations: changes,
		Description: "Rename a tag, on its messages too."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpTagIn2) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleRenameLabel, mcpRequest{Path: "/api/labels/" + in.TagID + "/rename", PathValues: map[string]string{"id": in.TagID}, Form: url.Values{"name": {in.Name}}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "delete_tag", Title: "Delete a tag", Annotations: destructive,
		Description: "Delete a tag; its messages stay, without it. Needs confirm: true."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpTagIn2) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if !in.Confirm {
				return nil, mcpChangeOut{}, errors.New("ask the user, then call again with confirm: true")
			}
			out, err := h.mcpSimple(ctx, h.handleDeleteLabel, mcpRequest{Path: "/api/labels/" + in.TagID + "/delete", PathValues: map[string]string{"id": in.TagID}})
			return nil, result(out), err
		})

	// Identities.
	listIdentities := func(ctx context.Context, userID, accountID string) (mcpIdentitiesOut, error) {
		accounts, err := h.mcpAccounts(ctx, userID, accountID)
		out := mcpIdentitiesOut{Identities: []mcpIdentity{}}
		for _, a := range accounts {
			for _, i := range a.Identities {
				if !mcpPolicy(ctx).Allows(mcpIdentityMailboxKey(a.ID, i)) {
					continue
				}
				out.Identities = append(out.Identities, mcpIdentity{
					ID: i.ID, AccountID: a.ID, Name: i.Name, Email: i.Email, ReplyTo: i.ReplyTo, SignatureID: i.SignatureID,
					Default: a.DefaultIdentityID == i.ID, FromProvider: i.Source == models.IdentitySourceProvider,
					SharedMailbox: i.SharedMailbox, State: i.SharedState, SenderCheck: i.SenderCheck, SenderMessage: i.SenderCheckMessage,
				})
			}
		}
		return out, err
	}
	mcp.AddTool(server, &mcp.Tool{Name: "list_identities", Title: "List From addresses", Annotations: reads,
		Description: "The From addresses (identities) of the user's accounts, or of one, shared mailboxes' included, with what the server said about sending from each."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpListFoldersIn) (*mcp.CallToolResult, mcpIdentitiesOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpIdentitiesOut{}, err
			}
			out, err := listIdentities(ctx, userID, in.AccountID)
			return nil, out, err
		})

	identityPanel := func(ctx context.Context, userID, accountID string, handler http.HandlerFunc, in mcpRequest) (mcpIdentitiesOut, error) {
		resp, err := h.mcpInvoke(ctx, handler, in)
		if err != nil {
			return mcpIdentitiesOut{}, err
		}
		if err := resp.Err(); err != nil {
			return mcpIdentitiesOut{}, err
		}
		if err := mcpPanelError(resp.Body, "data-account-identity-error"); err != nil {
			return mcpIdentitiesOut{}, err
		}
		return listIdentities(ctx, userID, accountID)
	}
	mcp.AddTool(server, &mcp.Tool{Name: "save_identity", Title: "Add or change a From address", Annotations: changes,
		Description: "Add a From address to an account, or change one: its name, address, Reply-To and signature. It sends through the account's own server."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveIdentityIn) (*mcp.CallToolResult, mcpIdentitiesOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpIdentitiesOut{}, err
			}
			form := url.Values{"id": {in.IdentityID}, "name": {in.Name}, "email": {in.Email}, "reply_to": {in.ReplyTo}, "signature_id": {in.SignatureID}}
			out, err := identityPanel(ctx, userID, in.AccountID, h.handleSaveAccountIdentity, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/identities", PathValues: map[string]string{"id": in.AccountID}, Form: form})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "delete_identity", Title: "Delete a From address", Annotations: destructive,
		Description: "Delete a From address. One Gofer made for a shared mailbox is not made again."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpIdentityIn) (*mcp.CallToolResult, mcpIdentitiesOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpIdentitiesOut{}, err
			}
			out, err := identityPanel(ctx, userID, in.AccountID, h.handleDeleteAccountIdentity, mcpRequest{Method: http.MethodDelete, Path: "/api/accounts/" + in.AccountID + "/identities/" + in.IdentityID, PathValues: map[string]string{"id": in.AccountID, "identity": in.IdentityID}})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_default_identity", Title: "Choose the default From", Annotations: changes,
		Description: "Choose the From address new messages of an account start with; empty identity_id for the account's own address."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpDefaultIdentityIn) (*mcp.CallToolResult, mcpIdentitiesOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpIdentitiesOut{}, err
			}
			out, err := identityPanel(ctx, userID, in.AccountID, h.handleSetAccountDefaultIdentity, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/identities/default", PathValues: map[string]string{"id": in.AccountID}, Form: url.Values{"identity_id": {in.IdentityID}}})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_shared_identity_enabled", Title: "Turn a shared mailbox's From on or off", Annotations: changes,
		Description: "Turn off, or back on, the From address Gofer made for a shared mailbox. Off, it is not offered when writing."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSharedIdentityIn) (*mcp.CallToolResult, mcpIdentitiesOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpIdentitiesOut{}, err
			}
			out, err := identityPanel(ctx, userID, in.AccountID, h.handleSetSharedIdentityActive, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/identities/" + in.IdentityID + "/shared", PathValues: map[string]string{"id": in.AccountID, "identity": in.IdentityID}, Form: url.Values{"active": {strconv.FormatBool(in.Enabled)}}})
			return nil, out, err
		})

	// Signatures.
	mcp.AddTool(server, &mcp.Tool{Name: "list_signatures", Title: "List signatures", Annotations: reads,
		Description: "The user's signatures."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ mcpListAccountsIn) (*mcp.CallToolResult, mcpSignaturesOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpSignaturesOut{}, err
			}
			signatures, err := h.db.ListSignatures(ctx, userID)
			out := mcpSignaturesOut{Signatures: []mcpSignature{}}
			for _, s := range signatures {
				out.Signatures = append(out.Signatures, mcpSignature{ID: s.ID, Name: s.Name, TextBody: s.TextBody, HTMLBody: s.HTMLBody})
			}
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "save_signature", Title: "Add or change a signature", Annotations: changes,
		Description: "Add a signature, or change one, as text, HTML or both."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveSignatureIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleSaveSignature, mcpRequest{Path: "/api/signatures", Form: url.Values{"id": {in.SignatureID}, "name": {in.Name}, "text_body": {in.TextBody}, "html_body": {in.HTMLBody}}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "delete_signature", Title: "Delete a signature", Annotations: destructive,
		Description: "Delete a signature; accounts and identities using it go without."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSignatureIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpSimple(ctx, h.handleDeleteSignature, mcpRequest{Method: http.MethodDelete, Path: "/api/signatures/" + in.SignatureID, PathValues: map[string]string{"id": in.SignatureID}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_account_signatures", Title: "Choose an account's signatures", Annotations: changes,
		Description: "Choose the signature an account's new messages, replies and forwards get, and where it goes in replies and forwards."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSignatureSettingsIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			form := url.Values{"new_signature_id": {in.NewSignatureID}, "reply_signature_id": {in.ReplySignatureID}, "forward_signature_id": {in.ForwardSignatureID},
				"reply_placement": {firstNonEmptyString(in.ReplyPlacement, "above")}, "forward_placement": {firstNonEmptyString(in.ForwardPlacement, "above")}}
			out, err := h.mcpSimple(ctx, h.handleSaveAccountSignatureSettings, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/signature-settings", PathValues: map[string]string{"id": in.AccountID}, Form: form})
			return nil, result(out), err
		})

	// Settings.
	mcp.AddTool(server, &mcp.Tool{Name: "get_settings", Title: "Read settings", Annotations: reads,
		Description: "The user's Gofer settings: interface (theme, language, layout…), shared mailboxes, and how often mail is checked."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ mcpListAccountsIn) (*mcp.CallToolResult, mcpSettingsOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpSettingsOut{}, err
			}
			settings := h.db.GetUISettings(ctx, userID)
			return nil, mcpSettingsOut{Settings: settings, SharedMailboxes: sharedmailbox.SettingsFrom(settings), SyncMinutes: h.db.GetSyncInterval(ctx, userID)}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "update_settings", Title: "Change settings", Annotations: changes,
		Description: "Change interface settings by key (see get_settings), such as theme, theme_style, language or the shared_mailboxes_ ones, and how often mail is checked."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpUpdateSettingsIn) (*mcp.CallToolResult, mcpSettingsOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpSettingsOut{}, err
			}
			if len(in.Settings) > 0 {
				resp, err := h.mcpInvoke(ctx, h.handleSaveUISettings, mcpRequest{Method: http.MethodPatch, Path: "/api/settings/ui", JSON: in.Settings})
				if err != nil {
					return nil, mcpSettingsOut{}, err
				}
				if err := resp.Err(); err != nil {
					return nil, mcpSettingsOut{}, err
				}
			}
			if in.SyncIntervalMinutes > 0 {
				// Only the interval: the sync settings form also sets which
				// folders are watched, which this leaves as it is.
				if in.SyncIntervalMinutes > 1440 {
					return nil, mcpSettingsOut{}, errors.New("sync_interval_minutes is at most 1440")
				}
				if err := h.db.SetSetting(ctx, userID, "sync_interval_minutes", strconv.Itoa(in.SyncIntervalMinutes)); err != nil {
					return nil, mcpSettingsOut{}, fmt.Errorf("sync interval: %w", err)
				}
				if h.syncer != nil {
					h.syncer.UpdateInterval(in.SyncIntervalMinutes)
				}
			}
			settings := h.db.GetUISettings(ctx, userID)
			return nil, mcpSettingsOut{Settings: settings, SharedMailboxes: sharedmailbox.SettingsFrom(settings), SyncMinutes: h.db.GetSyncInterval(ctx, userID)}, nil
		})

	// Contacts and calendars.
	mcp.AddTool(server, &mcp.Tool{Name: "list_calendars", Title: "List calendars", Annotations: reads,
		Description: "The calendars of the user's accounts and their subscriptions."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ mcpListAccountsIn) (*mcp.CallToolResult, mcpCalendarsOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpCalendarsOut{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, "")
			out := mcpCalendarsOut{Calendars: []mcpCalendar{}}
			for _, a := range accounts {
				// Calendars belong to the account's own mailbox.
				if !mcpPolicy(ctx).Allows(mcpOwnMailboxKey(a.ID)) {
					continue
				}
				for _, c := range append(a.CalendarSources, a.OtherCalendarSources...) {
					out.Calendars = append(out.Calendars, mcpCalendar{ID: c.ID, AccountID: a.ID, Name: c.Name, Color: c.Color})
				}
			}
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "subscribe_calendar", Title: "Subscribe to a calendar", Annotations: &mcp.ToolAnnotations{OpenWorldHint: boolPtr(true)},
		Description: "Subscribe to a calendar by its .ics link; it is read-only and refreshed on its own."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSubscribeIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeSettings)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			refresh := in.RefreshMinutes
			if refresh == 0 {
				refresh = 60
			}
			out, err := h.mcpSimple(ctx, h.handleCalendarSubscriptionAdd, mcpRequest{Path: "/api/calendar/subscriptions", JSON: calendarSubscriptionRequest{URL: in.URL, Name: in.Name, Color: in.Color, RefreshMinutes: refresh}})
			return nil, result(out), err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sync_contacts_and_calendars", Title: "Sync contacts and calendars now", Annotations: &mcp.ToolAnnotations{OpenWorldHint: boolPtr(true), IdempotentHint: true},
		Description: "Fetch an account's contacts now, and every calendar."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSyncContactsIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			out := map[string]any{}
			if in.AccountID != "" {
				if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
					return nil, mcpChangeOut{}, err
				}
				contacts, err := h.mcpSimple(ctx, h.handleSyncContactNow, mcpRequest{Path: "/api/contacts/" + in.AccountID + "/sync-now", PathValues: map[string]string{"id": in.AccountID}})
				if err != nil {
					out["contacts_error"] = err.Error()
				} else {
					out["contacts"] = contacts
				}
			}
			resp, err := h.mcpInvoke(ctx, h.handleCalendarSync, mcpRequest{Path: "/api/calendar/sync"})
			if err == nil {
				err = resp.Err()
			}
			if err != nil {
				out["calendar_error"] = err.Error()
			} else {
				out["calendars"] = "syncing"
			}
			return nil, result(out), nil
		})
}

func mcpFolderIconNames() string {
	names := make([]string, 0, len(models.FolderIconChoices))
	for _, choice := range models.FolderIconChoices {
		names = append(names, choice.Icon)
	}
	return strings.Join(names, ", ")
}
