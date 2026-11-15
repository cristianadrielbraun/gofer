package handler

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// MCP views of Gofer's data: what a client needs, with stable IDs, and
// nothing a client should not see (no passwords, no local paths).

type mcpAccount struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Email           string   `json:"email"`
	Provider        string   `json:"provider" jsonschema:"imap, gmail, outlook, jmap, dav or local"`
	Color           string   `json:"color,omitempty"`
	Hidden          bool     `json:"hidden,omitempty"`
	MailSync        bool     `json:"mail_sync"`
	ContactsSync    bool     `json:"contacts_sync"`
	CalendarSync    bool     `json:"calendar_sync"`
	SyncError       string   `json:"sync_error,omitempty"`
	SharedMailboxes []string `json:"shared_mailboxes,omitempty" jsonschema:"addresses of the shared mailboxes the account opens and uses"`
	// SharedMailboxesOff are shared mailboxes the user turned off.
	SharedMailboxesOff []string `json:"shared_mailboxes_off,omitempty" jsonschema:"shared mailboxes the account can open but the user turned off (set_shared_mailbox_enabled)"`
	Identities         []string `json:"identities,omitempty" jsonschema:"From addresses besides the account's own"`
}

type mcpFolder struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	ParentID      string `json:"parent_id,omitempty"`
	Name          string `json:"name"`
	Path          string `json:"path" jsonschema:"the folder's place, as shown in the sidebar"`
	Role          string `json:"role,omitempty" jsonschema:"inbox, sent, drafts, junk, trash, archive or starred; empty for other folders"`
	SharedMailbox string `json:"shared_mailbox,omitempty" jsonschema:"the shared mailbox the folder is in"`
	Unread        int    `json:"unread"`
	Color         string `json:"color,omitempty"`
}

type mcpAddress struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type mcpMessageSummary struct {
	ID             string       `json:"id"`
	AccountID      string       `json:"account_id"`
	FolderID       string       `json:"folder_id,omitempty"`
	Location       string       `json:"location,omitempty"`
	Date           string       `json:"date"`
	From           mcpAddress   `json:"from"`
	To             []mcpAddress `json:"to,omitempty"`
	Subject        string       `json:"subject"`
	Preview        string       `json:"preview,omitempty"`
	Read           bool         `json:"read"`
	Starred        bool         `json:"starred,omitempty"`
	HasAttachments bool         `json:"has_attachments,omitempty"`
	Tags           []string     `json:"tags,omitempty"`
	ThreadCount    int          `json:"thread_count,omitempty"`
	Draft          bool         `json:"draft,omitempty"`
}

type mcpMessagePage struct {
	Messages   []mcpMessageSummary `json:"messages"`
	Total      int                 `json:"total"`
	NextOffset int                 `json:"next_offset,omitempty" jsonschema:"pass as offset for the next page; absent on the last page"`
}

func mcpAddresses(contacts []models.Contact) []mcpAddress {
	out := make([]mcpAddress, 0, len(contacts))
	for _, c := range contacts {
		if strings.TrimSpace(c.Email) != "" || strings.TrimSpace(c.Name) != "" {
			out = append(out, mcpAddress{Name: c.Name, Email: c.Email})
		}
	}
	return out
}

func mcpSummary(e models.Email) mcpMessageSummary {
	date := e.DateFull
	if date == "" {
		date = e.DateWithTime
	}
	s := mcpMessageSummary{
		ID: e.ID, AccountID: e.AccountID, FolderID: e.FolderID, Location: e.SearchLocation, Date: date,
		From: mcpAddress{Name: e.From.Name, Email: e.From.Email}, To: mcpAddresses(e.To),
		Subject: e.Subject, Preview: e.Preview, Read: e.IsRead, Starred: e.IsStarred,
		HasAttachments: e.HasAttachment, ThreadCount: e.ThreadCount, Draft: e.IsDraft,
	}
	if s.ThreadCount <= 1 {
		s.ThreadCount = 0
	}
	for _, label := range e.Labels {
		s.Tags = append(s.Tags, label.Name)
	}
	return s
}

func mcpPage(page *models.EmailPage, offset int) mcpMessagePage {
	out := mcpMessagePage{Messages: []mcpMessageSummary{}}
	if page == nil {
		return out
	}
	for _, e := range page.Emails {
		out.Messages = append(out.Messages, mcpSummary(e))
	}
	out.Total = page.TotalCount
	if next := offset + len(page.Emails); len(page.Emails) > 0 && (page.HasMore || next < page.TotalCount) {
		out.NextOffset = next
	}
	return out
}

func mcpFlattenFolders(accountID string, folders []models.Folder, parentID, parentPath string, out *[]mcpFolder) {
	for _, f := range folders {
		path := f.Name
		if f.SharedMailbox != "" && parentPath == "" {
			path = f.SharedMailbox + "/" + f.Name
		}
		if parentPath != "" {
			path = parentPath + "/" + f.Name
		}
		role := f.Role
		if f.SharedMailbox != "" {
			role = f.SharedRole
		}
		if role == "custom" {
			role = ""
		}
		*out = append(*out, mcpFolder{ID: f.ID, AccountID: accountID, ParentID: parentID, Name: f.Name, Path: path, Role: role, SharedMailbox: f.SharedMailbox, Unread: f.Unread, Color: f.Color})
		mcpFlattenFolders(accountID, f.Children, f.ID, path, out)
	}
}

// mcpAccounts returns the user's accounts, or one of them when accountID is
// set (an unknown one is an error).
func (h *Handler) mcpAccounts(ctx context.Context, userID, accountID string) ([]models.Account, error) {
	accounts, err := h.db.GetAccounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	if accountID = strings.TrimSpace(accountID); accountID == "" {
		return accounts, nil
	}
	for _, a := range accounts {
		if a.ID == accountID {
			return []models.Account{a}, nil
		}
	}
	return nil, errors.New("no account with that ID (see list_accounts)")
}

type mcpListAccountsIn struct{}

type mcpListAccountsOut struct {
	Accounts []mcpAccount `json:"accounts"`
}

type mcpListFoldersIn struct {
	AccountID string `json:"account_id,omitempty" jsonschema:"only this account's folders"`
}

type mcpListFoldersOut struct {
	Folders []mcpFolder `json:"folders"`
}

type mcpUnreadIn struct{}

type mcpUnreadFolder struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
	Unread   int    `json:"unread"`
}

type mcpUnreadAccount struct {
	AccountID string            `json:"account_id"`
	Name      string            `json:"name"`
	Unread    int               `json:"unread" jsonschema:"unread mail outside Sent, Drafts, Junk and Trash"`
	Folders   []mcpUnreadFolder `json:"folders" jsonschema:"the folders with unread mail, shared mailboxes' included"`
}

type mcpUnreadOut struct {
	Unread   int                `json:"unread"`
	Accounts []mcpUnreadAccount `json:"accounts"`
}

type mcpListMessagesIn struct {
	FolderID       string `json:"folder_id" jsonschema:"a folder ID from list_folders, or inbox, sent, drafts, archive, spam, trash or starred for that folder of every account"`
	AccountID      string `json:"account_id,omitempty" jsonschema:"with a folder of every account, only this account's"`
	UnreadOnly     bool   `json:"unread_only,omitempty"`
	StarredOnly    bool   `json:"starred_only,omitempty"`
	HasAttachments bool   `json:"has_attachments,omitempty"`
	From           string `json:"from,omitempty" jsonschema:"sender name or address contains this"`
	Subject        string `json:"subject,omitempty" jsonschema:"subject contains this"`
	After          string `json:"after,omitempty" jsonschema:"on or after this date, YYYY-MM-DD"`
	Before         string `json:"before,omitempty" jsonschema:"on or before this date, YYYY-MM-DD"`
	ByMessage      bool   `json:"by_message,omitempty" jsonschema:"one row per message instead of per conversation"`
	Offset         int    `json:"offset,omitempty"`
	Limit          int    `json:"limit,omitempty" jsonschema:"at most 100; 25 by default"`
}

type mcpSearchIn struct {
	Query            string   `json:"query,omitempty" jsonschema:"words to find anywhere in a message, as in Gofer's search box"`
	AccountID        string   `json:"account_id,omitempty" jsonschema:"search only this account"`
	FolderIDs        []string `json:"folder_ids,omitempty" jsonschema:"search only these folders and their subfolders"`
	IncludeSpamTrash bool     `json:"include_spam_trash,omitempty"`
	From             string   `json:"from,omitempty"`
	To               string   `json:"to,omitempty"`
	Subject          string   `json:"subject,omitempty"`
	Body             string   `json:"body,omitempty" jsonschema:"text in the message body"`
	UnreadOnly       bool     `json:"unread_only,omitempty"`
	StarredOnly      bool     `json:"starred_only,omitempty"`
	HasAttachments   bool     `json:"has_attachments,omitempty"`
	After            string   `json:"after,omitempty" jsonschema:"YYYY-MM-DD"`
	Before           string   `json:"before,omitempty" jsonschema:"YYYY-MM-DD"`
	Offset           int      `json:"offset,omitempty"`
	Limit            int      `json:"limit,omitempty" jsonschema:"at most 100; 25 by default"`
}

type mcpReadMessageIn struct {
	MessageID string `json:"message_id"`
	Format    string `json:"format,omitempty" jsonschema:"text (default) or html"`
	MaxChars  int    `json:"max_chars,omitempty" jsonschema:"longest body returned; 20000 by default"`
}

type mcpAttachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	SizeBytes   int64  `json:"size_bytes"`
	Inline      bool   `json:"inline,omitempty"`
}

type mcpMessage struct {
	mcpMessageSummary
	CC          []mcpAddress    `json:"cc,omitempty"`
	BCC         []mcpAddress    `json:"bcc,omitempty"`
	MessageID   string          `json:"internet_message_id,omitempty"`
	InReplyTo   string          `json:"in_reply_to,omitempty"`
	Format      string          `json:"format"`
	Body        string          `json:"body"`
	Truncated   bool            `json:"truncated,omitempty"`
	Attachments []mcpAttachment `json:"attachments,omitempty"`
}

func (h *Handler) addMCPReadTools(server *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}

	mcp.AddTool(server, &mcp.Tool{Name: "list_accounts", Title: "List mail accounts", Annotations: readOnly,
		Description: "The user's mail accounts: IDs, addresses, what each syncs, its shared mailboxes and From addresses."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ mcpListAccountsIn) (*mcp.CallToolResult, mcpListAccountsOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpListAccountsOut{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, "")
			if err != nil {
				return nil, mcpListAccountsOut{}, err
			}
			out := mcpListAccountsOut{Accounts: []mcpAccount{}}
			policy := mcpPolicy(ctx)
			for _, a := range accounts {
				if !policy.AllowsAccount(a.ID) {
					continue
				}
				out.Accounts = append(out.Accounts, h.mcpAccountDetails(ctx, a))
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "list_folders", Title: "List folders", Annotations: readOnly,
		Description: "Every folder of the user's accounts, or of one account, with its role, the shared mailbox it is in, and unread count."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpListFoldersIn) (*mcp.CallToolResult, mcpListFoldersOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpListFoldersOut{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, in.AccountID)
			if err != nil {
				return nil, mcpListFoldersOut{}, err
			}
			out := mcpListFoldersOut{Folders: []mcpFolder{}}
			for _, a := range accounts {
				mcpFlattenFolders(a.ID, a.Folders, "", "", &out.Folders)
			}
			out.Folders = mcpAllowedFolders(ctx, out.Folders)
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "get_unread_summary", Title: "Unread mail", Annotations: readOnly,
		Description: "How much unread mail each account has, and in which folders, shared mailboxes included."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ mcpUnreadIn) (*mcp.CallToolResult, mcpUnreadOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpUnreadOut{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, "")
			if err != nil {
				return nil, mcpUnreadOut{}, err
			}
			out := mcpUnreadOut{Accounts: []mcpUnreadAccount{}}
			for _, a := range accounts {
				if !mcpPolicy(ctx).AllowsAccount(a.ID) {
					continue
				}
				var folders []mcpFolder
				mcpFlattenFolders(a.ID, a.Folders, "", "", &folders)
				folders = mcpAllowedFolders(ctx, folders)
				entry := mcpUnreadAccount{AccountID: a.ID, Name: a.Name, Folders: []mcpUnreadFolder{}}
				for _, f := range folders {
					if f.Unread == 0 {
						continue
					}
					entry.Folders = append(entry.Folders, mcpUnreadFolder{FolderID: f.ID, Path: f.Path, Unread: f.Unread})
					switch f.Role {
					case "sent", "drafts", "junk", "trash":
					default:
						entry.Unread += f.Unread
					}
				}
				out.Unread += entry.Unread
				out.Accounts = append(out.Accounts, entry)
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "list_messages", Title: "List messages in a folder", Annotations: readOnly,
		Description: "Messages in a folder, newest first, a page at a time, optionally only unread, starred or matching a sender, subject or dates."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpListMessagesIn) (*mcp.CallToolResult, mcpMessagePage, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpMessagePage{}, err
			}
			folderID := strings.TrimSpace(in.FolderID)
			if folderID == "" {
				return nil, mcpMessagePage{}, errors.New("folder_id is required (see list_folders)")
			}
			// Only one of the user's own folders, or a folder of every
			// account, which covers only theirs.
			folderID, err = h.db.ResolveFolderIDForUser(ctx, userID, folderID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, mcpMessagePage{}, errors.New("no folder with that ID (see list_folders)")
			}
			if err != nil {
				return nil, mcpMessagePage{}, err
			}
			if err := h.mcpCheckFolder(ctx, userID, folderID, strings.TrimSpace(in.AccountID)); err != nil {
				return nil, mcpMessagePage{}, err
			}
			filters := models.EmailFilters{
				Unread: in.UnreadOnly, Starred: in.StarredOnly, Attachments: in.HasAttachments,
				From: strings.TrimSpace(in.From), Subject: strings.TrimSpace(in.Subject),
				After: strings.TrimSpace(in.After), Before: strings.TrimSpace(in.Before),
				AccountID: strings.TrimSpace(in.AccountID), ByMessage: in.ByMessage,
			}
			offset := max(in.Offset, 0)
			ctx = storage.WithTimezone(ctx, h.db.GetUISettings(ctx, userID)["timezone"])
			page, err := h.db.GetEmailsRangeFilteredForUser(ctx, userID, folderID, offset, mcpLimit(in.Limit, 25, 100), filters)
			if err != nil {
				return nil, mcpMessagePage{}, err
			}
			return nil, mcpPage(page, offset), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "search_messages", Title: "Search mail", Annotations: readOnly,
		Description: "Search all accounts, one account, or some folders by words, sender, recipient, subject, body, dates and flags."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSearchIn) (*mcp.CallToolResult, mcpMessagePage, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpMessagePage{}, err
			}
			scope := models.MailSearchScope{Kind: models.MailSearchAll, ExcludeSpamTrash: !in.IncludeSpamTrash}
			switch {
			case len(in.FolderIDs) > 0:
				if len(in.FolderIDs) > storage.MaxMailSearchFolders {
					return nil, mcpMessagePage{}, errors.New("too many folders to search at once")
				}
				scope = models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: in.FolderIDs, IncludeDescendants: true, ExcludeSpamTrash: !in.IncludeSpamTrash}
			case strings.TrimSpace(in.AccountID) != "":
				scope = models.MailSearchScope{Kind: models.MailSearchAccount, AccountID: strings.TrimSpace(in.AccountID), ExcludeSpamTrash: !in.IncludeSpamTrash}
			}
			// A limited token searches only the folders of its mailboxes.
			if mcpPolicy(ctx).Limited() {
				folderIDs, err := h.mcpSearchFolders(ctx, userID, in.FolderIDs, strings.TrimSpace(in.AccountID))
				if err != nil {
					return nil, mcpMessagePage{}, err
				}
				scope = models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: folderIDs, IncludeDescendants: false, ExcludeSpamTrash: !in.IncludeSpamTrash}
			}
			filters := models.EmailFilters{
				SearchScope: &scope, Query: strings.TrimSpace(in.Query),
				From: strings.TrimSpace(in.From), To: strings.TrimSpace(in.To), Subject: strings.TrimSpace(in.Subject), Body: strings.TrimSpace(in.Body),
				Unread: in.UnreadOnly, Starred: in.StarredOnly, Attachments: in.HasAttachments,
				After: strings.TrimSpace(in.After), Before: strings.TrimSpace(in.Before), ByMessage: true,
			}
			offset := max(in.Offset, 0)
			ctx = storage.WithTimezone(ctx, h.db.GetUISettings(ctx, userID)["timezone"])
			page, err := h.db.GetMailSearchPageForUser(ctx, userID, scope, offset, mcpLimit(in.Limit, 25, 100), filters)
			if errors.Is(err, storage.ErrMailSearchScope) {
				return nil, mcpMessagePage{}, errors.New("unknown account or folder to search (see list_accounts and list_folders)")
			}
			if err != nil {
				return nil, mcpMessagePage{}, err
			}
			if accounts, err := h.db.GetAccounts(ctx, userID); err == nil {
				setSearchLocations(accounts, page)
			}
			return nil, mcpPage(page, offset), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "read_message", Title: "Read a message", Annotations: readOnly,
		Description: "One message: its headers, recipients, attachments and body as text (or HTML). Reading it does not mark it read; use mark_messages for that."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpReadMessageIn) (*mcp.CallToolResult, mcpMessage, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailRead)
			if err != nil {
				return nil, mcpMessage{}, err
			}
			msgID, err := strconv.ParseInt(strings.TrimSpace(in.MessageID), 10, 64)
			if err != nil || msgID <= 0 {
				return nil, mcpMessage{}, errors.New("message_id is a message ID from list_messages or search_messages")
			}
			// The body is downloaded the first time it is read, as the reading
			// pane does.
			if !h.db.IsBodyFetchedInternal(ctx, msgID) {
				if info, err := h.db.GetMessageFetchInfoForUser(ctx, msgID, userID); err == nil && info != nil {
					h.ensureBodyFetched(message.WithDownloadLimit(ctx, message.AutoDownloadMaxBytes), msgID, info.AccountID)
				}
			}
			ctx = storage.WithTimezone(ctx, h.db.GetUISettings(ctx, userID)["timezone"])
			email, err := h.db.GetEmailByIDForUser(ctx, strconv.FormatInt(msgID, 10), userID)
			if err != nil {
				return nil, mcpMessage{}, err
			}
			if email == nil {
				return nil, mcpMessage{}, errors.New("no message with that ID")
			}
			if err := h.mcpCheckMessage(ctx, userID, msgID); err != nil {
				return nil, mcpMessage{}, err
			}
			out := mcpMessage{
				mcpMessageSummary: mcpSummary(*email),
				CC:                mcpAddresses(email.CC), BCC: mcpAddresses(email.BCC),
				MessageID: email.InternetMessageID, InReplyTo: email.InReplyTo, Format: "text",
			}
			out.Preview = ""
			html := ""
			if raw, err := h.db.GetEmailBodyForUser(ctx, email.ID, userID); err == nil {
				html = string(raw)
			}
			body := email.TextBody
			if in.Format == "html" && html != "" {
				out.Format, body = "html", html
			} else if strings.TrimSpace(body) == "" || body == email.Preview {
				if html != "" {
					body = message.TextFromHTML([]byte(html))
				}
			}
			out.Body, out.Truncated = mcpTruncate(body, mcpLimit(in.MaxChars, 20000, 200000))
			for _, a := range email.Attachments {
				out.Attachments = append(out.Attachments, mcpAttachment{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, SizeBytes: a.SizeBytes, Inline: a.Inline})
			}
			return nil, out, nil
		})
}

// mcpAllowedFolders keeps the folders of the mailboxes the token may reach.
func mcpAllowedFolders(ctx context.Context, folders []mcpFolder) []mcpFolder {
	policy := mcpPolicy(ctx)
	if !policy.Limited() {
		return folders
	}
	out := []mcpFolder{}
	for _, f := range folders {
		if policy.AllowsFolder(f.AccountID, f.SharedMailbox) {
			out = append(out, f)
		}
	}
	return out
}

// mcpSearchFolders is what a limited token searches: the folders asked for,
// each of a mailbox it may reach, or else every folder of those mailboxes,
// of one account when accountID is set.
func (h *Handler) mcpSearchFolders(ctx context.Context, userID string, asked []string, accountID string) ([]string, error) {
	if len(asked) > 0 {
		for _, id := range asked {
			if err := h.mcpCheckFolder(ctx, userID, strings.TrimSpace(id), ""); err != nil {
				return nil, err
			}
		}
		return asked, nil
	}
	accounts, err := h.mcpAccounts(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	var folders []mcpFolder
	for _, a := range accounts {
		mcpFlattenFolders(a.ID, a.Folders, "", "", &folders)
	}
	var ids []string
	for _, f := range mcpAllowedFolders(ctx, folders) {
		ids = append(ids, f.ID)
	}
	if len(ids) == 0 {
		return nil, errMCPMailboxNotAllowed
	}
	if len(ids) > storage.MaxMailSearchFolders {
		return nil, errors.New("too many folders to search at once: give folder_ids")
	}
	return ids, nil
}

// mcpAccountDetails is one account as list_accounts shows it, within what the
// token may reach.
func (h *Handler) mcpAccountDetails(ctx context.Context, a models.Account) mcpAccount {
	policy := mcpPolicy(ctx)
	view := mcpAccount{ID: a.ID, Name: a.Name, Email: a.Email, Provider: a.Provider, Color: a.Color, Hidden: a.Hidden,
		MailSync: a.EmailSyncEnabled, ContactsSync: a.ContactSyncEnabled, CalendarSync: a.CalendarSyncEnabled, SyncError: a.EmailSyncError}
	seen := map[string]bool{}
	for _, f := range a.Folders {
		if f.SharedMailbox != "" && !seen[f.SharedMailbox] && policy.AllowsFolder(a.ID, f.SharedMailbox) {
			seen[f.SharedMailbox] = true
			view.SharedMailboxes = append(view.SharedMailboxes, f.SharedMailbox)
		}
	}
	if !policy.Limited() {
		if off, err := h.db.DisabledSharedMailboxes(ctx, a.ID); err == nil {
			for address := range off {
				view.SharedMailboxesOff = append(view.SharedMailboxesOff, address)
			}
			slices.Sort(view.SharedMailboxesOff)
		}
	}
	for _, identity := range a.Identities {
		if identity.Usable() && policy.Allows(mcpIdentityMailboxKey(a.ID, identity)) {
			view.Identities = append(view.Identities, identity.Email)
		}
	}
	return view
}
