package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

var errInvalidMailSearch = errors.New("invalid mail search scope")

// Scoped flag actions tell the reader which messages changed; messages from
// the same conversation outside the selected folders keep their own state.
func writeMailSearchFlagResult(w http.ResponseWriter, updated int, targets []ownedMessageTarget) {
	ids := make([]string, 0)
	for _, target := range targets {
		for _, info := range target.Infos {
			ids = append(ids, strconv.FormatInt(info.MessageID, 10))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Updated    int      `json:"updated"`
		MessageIDs []string `json:"message_ids"`
	}{updated, ids})
}

// Scope parameters are separate from account_id, which remains an ordinary
// advanced filter. Repeated scope_folder_id values select independent roots.
func parseMailSearchScope(r *http.Request) (models.MailSearchScope, error) {
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("scope"))
	if kind == "" {
		kind = string(models.MailSearchAll)
	}
	scope := models.MailSearchScope{
		Kind: models.MailSearchScopeKind(kind), AccountID: strings.TrimSpace(q.Get("scope_account_id")),
		FolderIDs: q["scope_folder_id"],
	}
	for name, field := range map[string]*bool{"include_descendants": &scope.IncludeDescendants, "exclude_spam_trash": &scope.ExcludeSpamTrash} {
		switch q.Get(name) {
		case "", "0":
		case "1":
			*field = true
		default:
			return scope, errInvalidMailSearch
		}
	}
	switch scope.Kind {
	case models.MailSearchAll:
		if scope.AccountID != "" || len(scope.FolderIDs) != 0 || scope.IncludeDescendants {
			return scope, errInvalidMailSearch
		}
	case models.MailSearchAccount:
		if scope.AccountID == "" || len(scope.FolderIDs) != 0 || scope.IncludeDescendants {
			return scope, errInvalidMailSearch
		}
	case models.MailSearchFolders:
		if scope.AccountID != "" || len(scope.FolderIDs) == 0 || len(scope.FolderIDs) > storage.MaxMailSearchFolders {
			return scope, errInvalidMailSearch
		}
		for i, id := range scope.FolderIDs {
			scope.FolderIDs[i] = strings.TrimSpace(id)
			if scope.FolderIDs[i] == "" {
				return scope, errInvalidMailSearch
			}
		}
	default:
		return scope, errInvalidMailSearch
	}
	return scope, nil
}

func (h *Handler) handleMailSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.renderMailboxView(w, r, &ctx, func(local *Handler) (templ.Component, error) {
		scope, err := parseMailSearchScope(r)
		if err != nil {
			return nil, err
		}
		userID := local.userID(ctx)
		settings := local.db.GetUISettings(ctx, userID)
		ctx = storage.WithTimezone(ctx, settings["timezone"])
		filters := applyEmailListSettings(parseEmailFilters(r), r, settings)
		filters.SearchScope = &scope
		// A search reopened with a message open starts at that message's page.
		selected := strings.TrimSpace(r.URL.Query().Get("selected"))
		var page *models.EmailPage
		if selectedID, parseErr := strconv.ParseInt(selected, 10, 64); parseErr == nil && selectedID > 0 {
			page, err = local.db.GetMailSearchPageAroundForUser(ctx, userID, scope, selectedID, 50, filters)
		} else {
			page, err = local.db.GetMailSearchPageForUser(ctx, userID, scope, 0, 50, filters)
		}
		if err != nil {
			return nil, err
		}
		accounts, err := local.db.GetAccounts(ctx, userID)
		if err != nil {
			return nil, err
		}
		for i := range page.Emails {
			page.Emails[i].SearchLocation = views.MailSearchLocation(accounts, page.Emails[i])
		}
		var selectedEmail *models.Email
		if selected != "" {
			selectedEmail = &models.Email{ID: selected}
			if !filters.ByMessage {
				selectedEmail.ID = local.visibleMailListSelectionID(ctx, userID, page.Emails, selected)
			}
		}
		if r.Header.Get("HX-Request") == "true" {
			return views.MailContentPartial(accounts, page.Emails, "search", selectedEmail, page.DisplayTotalCount, page.TotalCount, nil, settings, selected, page.WindowStart, filters), nil
		}
		return views.Layout(accounts, "search", page.Emails, selectedEmail, page.DisplayTotalCount, page.TotalCount, page.WindowStart, settings, nil, selected, local.scheduledSidebarCount(ctx, userID), filters), nil
	})
}

func (h *Handler) handleMailSearchItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.renderMailboxView(w, r, &ctx, func(local *Handler) (templ.Component, error) {
		scope, err := parseMailSearchScope(r)
		if err != nil {
			return nil, err
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if start < 0 {
			start = 0
		}
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		userID := local.userID(ctx)
		settings := local.db.GetUISettings(ctx, userID)
		ctx = storage.WithTimezone(ctx, settings["timezone"])
		filters := applyEmailListSettings(parseEmailFilters(r), r, settings)
		page, err := local.db.GetMailSearchPageForUser(ctx, userID, scope, start, limit, filters)
		if err != nil {
			return nil, err
		}
		accounts, err := local.db.GetAccounts(ctx, userID)
		if err != nil {
			return nil, err
		}
		for i := range page.Emails {
			page.Emails[i].SearchLocation = views.MailSearchLocation(accounts, page.Emails[i])
		}
		view := r.URL.Query().Get("view")
		if view == "" {
			view = views.ListViewSetting(ctx, settings, "mail_list_view")
		}
		return views.MailListItemsFragment(accounts, page.Emails, "search", page.WindowStart, page.WindowEnd, page.TotalCount, page.DisplayTotalCount, page.NextCursor, page.HasMore, r.URL.Query().Get("selected"), settings["sender_display"], view), nil
	})
}

func (h *Handler) handleMailSearchIDs(w http.ResponseWriter, r *http.Request) {
	scope, err := parseMailSearchScope(r)
	if err != nil {
		http.Error(w, "invalid mail search scope", http.StatusBadRequest)
		return
	}
	result := mailItemIDsResponse{IDs: []mailItemID{}}
	err = h.withUserDB(r.Context(), h.userID(r.Context()), func(db *storage.DB) error {
		userID := h.userID(r.Context())
		settings := db.GetUISettings(r.Context(), userID)
		ctx := storage.WithTimezone(r.Context(), settings["timezone"])
		filters := applyEmailListSettings(parseEmailFilters(r), r, settings)
		ids, total, err := db.GetMailSearchRowIDsForUser(ctx, userID, scope, filters, mailItemIDsLimit+1)
		if err != nil {
			return err
		}
		result.Total = total
		result.Truncated = len(ids) > mailItemIDsLimit
		if result.Truncated {
			ids = ids[:mailItemIDsLimit]
		}
		for _, id := range ids {
			result.IDs = append(result.IDs, mailItemID{ID: id.ID, Account: id.Account, FolderID: id.FolderID, Thread: id.Thread})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, storage.ErrMailSearchScope) || errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "mailbox unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) handleMailSearchThreadSubItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.renderMailboxView(w, r, &ctx, func(local *Handler) (templ.Component, error) {
		scope, err := parseMailSearchScope(r)
		if err != nil {
			return nil, err
		}
		messageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || messageID <= 0 {
			return nil, sql.ErrNoRows
		}
		userID := local.userID(ctx)
		members, err := local.db.GetMailSearchMembersForUser(ctx, userID, scope, messageID, true)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			return nil, sql.ErrNoRows
		}
		info, err := local.db.GetMessageMutationInfoForUser(ctx, messageID, userID)
		if err != nil {
			return nil, err
		}
		if info == nil || info.ThreadID == "" {
			return nil, sql.ErrNoRows
		}
		ctx = local.contextWithUserTimezone(ctx, userID)
		items, err := local.db.GetThreadMessagesForUser(ctx, info.AccountID, info.ThreadID, userID)
		if err != nil {
			return nil, err
		}
		folders := make(map[string]string, len(members))
		for _, member := range members {
			folders[strconv.FormatInt(member.MessageID, 10)] = member.FolderID
		}
		visible := make([]models.ThreadItem, 0, len(members))
		for _, item := range items {
			if folderID, ok := folders[item.ID]; ok {
				item.FolderID = folderID
				visible = append(visible, item)
			}
		}
		settings := local.db.GetUISettings(ctx, userID)
		return views.MailListThreadSubItems(visible, settings["sender_display"]), nil
	})
}

// A scoped conversation may span folders. Group its real memberships by
// source folder so existing action handlers apply their role-specific rules
// (for example Trash deletion) to each group separately.
func (h *Handler) resolveMessageBulkTargets(ctx context.Context, payload messageBulkRequest, sourceFolderID string, wholeThread bool) ([]ownedMessageTarget, error) {
	targets := messageBulkTargets(payload)
	if payload.SearchScope == nil {
		return h.resolveOwnedMessageTargets(ctx, targets, sourceFolderID, wholeThread)
	}
	if len(targets) == 0 || len(targets) > 256 || strings.TrimSpace(sourceFolderID) != "" {
		return nil, errInvalidMessageTarget
	}
	var resolved []ownedMessageTarget
	seen := make(map[int64]bool)
	for _, target := range targets {
		messageID, err := strconv.ParseInt(target.ID, 10, 64)
		if err != nil || messageID <= 0 {
			return nil, errInvalidMessageTarget
		}
		members, err := h.db.GetMailSearchMembersForUser(ctx, h.userID(ctx), *payload.SearchScope, messageID, target.Thread)
		if errors.Is(err, storage.ErrMailSearchScope) {
			return nil, errMessageTargetNotFound
		}
		if err != nil {
			return nil, err
		}
		if len(members) == 0 || (target.FolderID != "" && target.FolderID != members[0].FolderID) {
			return nil, errMessageTargetNotFound
		}
		groups := make(map[string]int)
		for _, member := range members {
			if seen[member.MessageID] {
				continue
			}
			_, info, err := h.getMessageInfoForFolder(ctx, strconv.FormatInt(member.MessageID, 10), member.FolderID)
			if err != nil {
				return nil, err
			}
			if info.FolderID != member.FolderID {
				return nil, errMessageTargetNotFound
			}
			index, ok := groups[info.FolderID]
			if !ok {
				index = len(resolved)
				groups[info.FolderID] = index
				resolved = append(resolved, ownedMessageTarget{Target: target})
			}
			resolved[index].Infos = append(resolved[index].Infos, storage.ThreadMessageMutationInfo{
				MessageID: member.MessageID, MessageMutationInfo: *info, IsRead: info.IsRead, IsStarred: info.IsStarred,
			})
			seen[member.MessageID] = true
		}
	}
	return resolved, nil
}
