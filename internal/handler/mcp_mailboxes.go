package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// An MCP token can be limited to some of the user's mailboxes: an account's
// own mail, and each shared mailbox it opens. A mailbox not allowed does not
// exist for the token: its folders, messages, unread mail and From addresses
// are left out everywhere, and nothing in it can be changed or sent from it.
//
// A token for every mailbox, the default, also reaches the ones added later;
// a limited one only those chosen. A limited token cannot manage accounts or
// settings, which reach every mailbox.

func mcpOwnMailboxKey(accountID string) string { return "account:" + accountID }

func mcpSharedMailboxKey(accountID, mailbox string) string {
	return "shared:" + accountID + ":" + strings.ToLower(strings.TrimSpace(mailbox))
}

// mcpFolderMailboxKey is the mailbox a folder is in.
func mcpFolderMailboxKey(accountID, sharedMailbox string) string {
	if sharedMailbox != "" {
		return mcpSharedMailboxKey(accountID, sharedMailbox)
	}
	return mcpOwnMailboxKey(accountID)
}

// mcpIdentityMailboxKey is the mailbox a From address belongs to: a shared
// mailbox's for the identity made for it, else the account's own.
func mcpIdentityMailboxKey(accountID string, identity models.Identity) string {
	if identity.SharedMailbox != "" {
		return mcpSharedMailboxKey(accountID, identity.SharedMailbox)
	}
	return mcpOwnMailboxKey(accountID)
}

// mcpMailboxPolicy is what one token may reach.
type mcpMailboxPolicy struct {
	allowed map[string]bool // nil: every mailbox
}

func mcpPolicyFrom(keys []string) mcpMailboxPolicy {
	if len(keys) == 0 {
		return mcpMailboxPolicy{}
	}
	p := mcpMailboxPolicy{allowed: map[string]bool{}}
	for _, key := range keys {
		p.allowed[key] = true
	}
	return p
}

// Limited reports a token for some mailboxes only.
func (p mcpMailboxPolicy) Limited() bool { return p.allowed != nil }

func (p mcpMailboxPolicy) Allows(key string) bool { return p.allowed == nil || p.allowed[key] }

func (p mcpMailboxPolicy) AllowsFolder(accountID, sharedMailbox string) bool {
	return p.Allows(mcpFolderMailboxKey(accountID, sharedMailbox))
}

// AllowsAccount reports whether any mailbox of the account is allowed.
func (p mcpMailboxPolicy) AllowsAccount(accountID string) bool {
	if p.allowed == nil {
		return true
	}
	if p.allowed[mcpOwnMailboxKey(accountID)] {
		return true
	}
	prefix := "shared:" + accountID + ":"
	for key := range p.allowed {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

type mcpPolicyKey struct{}

func withMCPPolicy(ctx context.Context, p mcpMailboxPolicy) context.Context {
	return context.WithValue(ctx, mcpPolicyKey{}, p)
}

func mcpPolicy(ctx context.Context) mcpMailboxPolicy {
	p, _ := ctx.Value(mcpPolicyKey{}).(mcpMailboxPolicy)
	return p
}

var errMCPMailboxNotAllowed = errors.New("this token is not allowed to reach that mailbox (Settings > Advanced > AI access)")

// errMCPLimitedToken is a tool that reaches every mailbox, refused to a token
// limited to some.
var errMCPLimitedToken = errors.New("this token is limited to some mailboxes, so it cannot manage accounts or settings, which reach every mailbox")

// mcpCheckFolder refuses a folder of a mailbox the token may not reach. A
// folder of every account ("inbox" and the like) is refused to a limited
// token unless accountID names an account whose own mailbox it may reach.
func (h *Handler) mcpCheckFolder(ctx context.Context, userID, folderID, accountID string) error {
	p := mcpPolicy(ctx)
	if !p.Limited() {
		return nil
	}
	if isCombinedFolderID(folderID) {
		if accountID == "" || !p.Allows(mcpOwnMailboxKey(accountID)) {
			return errors.New("this token is limited to some mailboxes: give the folder's own ID (list_folders), or account_id for an account whose own mailbox it may reach")
		}
		return nil
	}
	folderAccount, err := h.db.GetFolderAccountID(ctx, folderID)
	if err != nil {
		return err
	}
	if folderAccount == "" {
		return errors.New("no folder with that ID (see list_folders)")
	}
	mailbox, err := h.db.FolderSharedMailbox(ctx, folderID)
	if err != nil {
		return err
	}
	if !p.AllowsFolder(folderAccount, mailbox) {
		return errMCPMailboxNotAllowed
	}
	return nil
}

// isCombinedFolderID reports a folder of every account.
func isCombinedFolderID(folderID string) bool {
	switch folderID {
	case "inbox", "starred", "sent", "drafts", "scheduled", "archive", "spam", "trash":
		return true
	}
	return false
}

// mcpCheckMessages refuses messages in a mailbox the token may not reach. A
// limited token acts on messages one by one, not whole conversations, which
// can reach into other mailboxes.
func (h *Handler) mcpCheckMessages(ctx context.Context, userID string, ids []string, thread bool) error {
	p := mcpPolicy(ctx)
	if !p.Limited() {
		return nil
	}
	if thread {
		return errors.New("this token is limited to some mailboxes, so it acts on messages, not whole conversations: leave thread out")
	}
	for _, raw := range ids {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("%q is not a message ID", raw)
		}
		if err := h.mcpCheckMessage(ctx, userID, id); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) mcpCheckMessage(ctx context.Context, userID string, id int64) error {
	p := mcpPolicy(ctx)
	if !p.Limited() {
		return nil
	}
	info, err := h.db.GetMessageMutationInfoForUser(ctx, id, userID)
	if err != nil {
		return err
	}
	if info == nil {
		return errors.New("no message with that ID")
	}
	mailbox, err := h.db.FolderSharedMailbox(ctx, info.FolderID)
	if err != nil {
		return err
	}
	if !p.AllowsFolder(info.AccountID, mailbox) {
		return errMCPMailboxNotAllowed
	}
	return nil
}

// mcpCheckSender refuses sending as an address of a mailbox the token may
// not reach: a shared mailbox's From, or the account's own address, its
// other identities and any address typed for one message.
func mcpCheckSender(ctx context.Context, account *models.Account, address string) error {
	p := mcpPolicy(ctx)
	if !p.Limited() {
		return nil
	}
	key := mcpOwnMailboxKey(account.ID)
	for _, identity := range account.Identities {
		if strings.EqualFold(identity.Email, address) {
			key = mcpIdentityMailboxKey(account.ID, identity)
			break
		}
	}
	if !p.Allows(key) {
		return fmt.Errorf("this token may not send as %s: its mailbox is not one it may reach", address)
	}
	return nil
}

// mcpCheckDraft refuses a draft in a mailbox the token may not reach.
func (h *Handler) mcpCheckDraft(ctx context.Context, accountID, draftID string) error {
	if !mcpPolicy(ctx).Limited() {
		return nil
	}
	info, err := h.db.GetDraftProviderInfoInternal(ctx, accountID, draftID)
	if err != nil {
		return err
	}
	if info == nil {
		return sql.ErrNoRows
	}
	mailbox, err := h.db.FolderSharedMailbox(ctx, info.FolderID)
	if err != nil {
		return err
	}
	if !mcpPolicy(ctx).AllowsFolder(accountID, mailbox) {
		return errMCPMailboxNotAllowed
	}
	return nil
}

// mcpMailboxChoices lists the user's mailboxes, for choosing what a token
// may reach.
func (h *Handler) mcpMailboxChoices(ctx context.Context, userID string) ([]views.MCPMailboxChoice, error) {
	accounts, err := h.db.GetAccounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []views.MCPMailboxChoice
	for _, a := range accounts {
		label := a.Email
		if label == "" {
			label = a.Name
		}
		out = append(out, views.MCPMailboxChoice{Key: mcpOwnMailboxKey(a.ID), Label: label, Account: a.Name})
		seen := map[string]bool{}
		for _, f := range a.Folders {
			if f.SharedMailbox != "" && !seen[f.SharedMailbox] {
				seen[f.SharedMailbox] = true
				out = append(out, views.MCPMailboxChoice{Key: mcpSharedMailboxKey(a.ID, f.SharedMailbox), Label: f.SharedMailbox, Account: a.Name, Shared: true})
			}
		}
	}
	return out, nil
}

// mcpChosenMailboxes keeps the chosen keys that are the user's mailboxes;
// all means every mailbox, which is stored as none.
func (h *Handler) mcpChosenMailboxes(ctx context.Context, userID string, all bool, chosen []string) ([]string, error) {
	if all {
		return nil, nil
	}
	choices, err := h.mcpMailboxChoices(ctx, userID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, key := range chosen {
		want[key] = true
	}
	var keys []string
	for _, c := range choices {
		if want[c.Key] {
			keys = append(keys, c.Key)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("choose at least one mailbox")
	}
	return keys, nil
}

// mcpMailboxLabels names the mailboxes of keys, for showing a token.
func (h *Handler) mcpMailboxLabels(ctx context.Context, userID string, keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	choices, _ := h.mcpMailboxChoices(ctx, userID)
	names := map[string]string{}
	for _, c := range choices {
		names[c.Key] = c.Label
	}
	var out []string
	for _, key := range keys {
		if name := names[key]; name != "" {
			out = append(out, name)
		} else {
			out = append(out, key)
		}
	}
	return out
}

// mcpFormAllMailboxes reports whether a token or consent form chose every
// mailbox. The chooser marks its forms with mailbox_choice, since an
// unticked box sends nothing; a form without it, as before the chooser,
// means every mailbox.
func mcpFormAllMailboxes(form url.Values) bool {
	if form.Get("mailbox_choice") == "" {
		return true
	}
	return form.Get("mailbox_all") == "1"
}
