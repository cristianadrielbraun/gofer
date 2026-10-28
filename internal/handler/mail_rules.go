package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// mailRulesData gathers what the Rules settings show from a store.
func mailRulesData(ctx context.Context, db *storage.DB, userID string) (models.MailRulesData, error) {
	rules, err := db.ListMailRules(ctx, userID)
	if err != nil {
		return models.MailRulesData{}, fmt.Errorf("rules: %w", err)
	}
	accounts, err := db.GetAccounts(ctx, userID)
	if err != nil {
		return models.MailRulesData{}, fmt.Errorf("accounts: %w", err)
	}
	folders, err := db.ListMailRuleFolderChoices(ctx, userID)
	if err != nil {
		return models.MailRulesData{}, fmt.Errorf("folders: %w", err)
	}
	data := models.MailRulesData{Rules: rules}
	for _, account := range accounts {
		if account.IsDeleting {
			continue
		}
		tree, err := db.AccountFolderTree(ctx, account.ID)
		if err != nil {
			return models.MailRulesData{}, fmt.Errorf("folders of %s: %w", account.ID, err)
		}
		data.Accounts = append(data.Accounts, models.MailRuleAccount{
			ID: account.ID, Name: account.Name, Email: account.Email, Color: account.Color, Provider: account.Provider,
			Folders: folders[account.ID], Tree: tree,
		})
	}
	return data, nil
}

// loadMailRulesData gathers what the Rules settings show, or writes the
// error response and returns false.
func (h *Handler) loadMailRulesData(w http.ResponseWriter, r *http.Request) (models.MailRulesData, bool) {
	data, err := mailRulesData(r.Context(), h.db, h.userID(r.Context()))
	if err != nil {
		log.Printf("mail rules of %s: %v", h.userID(r.Context()), err)
		http.Error(w, "failed to load rules", http.StatusInternalServerError)
		return models.MailRulesData{}, false
	}
	return data, true
}

func (h *Handler) renderMailRulesPanel(w http.ResponseWriter, r *http.Request, data models.MailRulesData) {
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	views.MailRulesPanel(data).Render(r.Context(), w)
}

// renderMailRulesDone closes the rule dialog and shows the list in place of
// the request's own target.
func (h *Handler) renderMailRulesDone(w http.ResponseWriter, r *http.Request, data models.MailRulesData) {
	w.Header().Set("HX-Retarget", "#mail-rules-panel")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("HX-Trigger", "mail-rule-dialog-done")
	h.renderMailRulesPanel(w, r, data)
}

func (h *Handler) renderMailRuleDialogPanel(w http.ResponseWriter, r *http.Request, data models.MailRulesData) {
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	views.MailRuleDialogPanel(data).Render(r.Context(), w)
}

// handleMailRulesPanel renders the list of rules.
func (h *Handler) handleMailRulesPanel(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	h.renderMailRulesPanel(w, r, data)
}

// handleMailRuleDialog renders the rule dialog with the rule named by edit
// in the form, or a new rule's form.
func (h *Handler) handleMailRuleDialog(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	if id := strings.TrimSpace(r.URL.Query().Get("edit")); id == "" {
		rule := newMailRuleDraft()
		data.Editing = &rule
	} else {
		index := slices.IndexFunc(data.Rules, func(rule models.MailRule) bool { return rule.ID == id })
		if index < 0 {
			http.NotFound(w, r)
			return
		}
		rule := data.Rules[index]
		data.Editing = &rule
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	views.MailRuleDialog(data).Render(r.Context(), w)
}

// newMailRuleDraft is the rule a new rule's form starts with.
func newMailRuleDraft() models.MailRule {
	return models.MailRule{
		Enabled: true, MatchAll: true,
		Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldFrom, Operator: models.MailRuleOpContains}},
		Actions:    []models.MailRuleAction{{Kind: models.MailRuleActionArchive}},
	}
}

// handleMailRuleForm shows the rule form again as it was sent, with a
// condition or action added or removed, or with the choices that depend on
// another one, such as an account's folders, brought up to date. Nothing is
// saved.
func (h *Handler) handleMailRuleForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	rule := mailRuleFromForm(r)
	index, _ := strconv.Atoi(r.FormValue("index"))
	switch r.FormValue("op") {
	case "add_condition":
		// "And" adds to the group given; "Or" starts a group after the last.
		if len(rule.Conditions) < mailrules.MaxConditions {
			group, err := strconv.Atoi(r.FormValue("group"))
			if err != nil || group < 0 {
				group = 0
				for _, condition := range rule.Conditions {
					group = max(group, condition.Group+1)
				}
			}
			rule.Conditions = append(rule.Conditions, models.MailRuleCondition{Group: group, Field: models.MailRuleFieldFrom, Operator: models.MailRuleOpContains})
		}
	case "remove_condition":
		if index >= 0 && index < len(rule.Conditions) && len(rule.Conditions) > 1 {
			rule.Conditions = slices.Delete(rule.Conditions, index, index+1)
		}
	case "add_action":
		if len(rule.Actions) < mailrules.MaxActions {
			rule.Actions = append(rule.Actions, models.MailRuleAction{Kind: nextMailRuleAction(rule.Actions)})
		}
	case "remove_action":
		if index >= 0 && index < len(rule.Actions) && len(rule.Actions) > 1 {
			rule.Actions = slices.Delete(rule.Actions, index, index+1)
		}
	}
	fitMailRuleForm(&rule, data.Accounts)
	data.Editing = &rule
	h.renderMailRuleDialogPanel(w, r, data)
}

// nextMailRuleAction is the kind a newly added action starts as: the first
// that the rule's other actions leave open, preferring ones that don't move
// the message.
func nextMailRuleAction(actions []models.MailRuleAction) string {
	added := append(slices.Clone(actions), models.MailRuleAction{})
	for _, kind := range []string{models.MailRuleActionMarkRead, models.MailRuleActionStar, models.MailRuleActionLabel,
		models.MailRuleActionArchive, models.MailRuleActionMove, models.MailRuleActionTrash, models.MailRuleActionSpam} {
		if views.MailRuleActionOffered(added, len(actions), kind) {
			return kind
		}
	}
	return models.MailRuleActionLabel
}

// fitMailRuleForm makes each condition and action of a rule in the form
// fit what it has become: a folder condition compares with is, a move has a
// folder, and a rule for every account names no folder of one account.
func fitMailRuleForm(rule *models.MailRule, accounts []models.MailRuleAccount) {
	mailrules.GroupConditions(rule)
	ownFolder := func(target *models.MailRuleFolderTarget) bool {
		if target == nil || target.FolderID == "" {
			return true
		}
		for _, account := range accounts {
			if account.ID != rule.AccountID {
				continue
			}
			for _, folder := range account.Folders {
				if folder.ID == target.FolderID {
					return true
				}
			}
		}
		return false
	}
	for i := range rule.Conditions {
		condition := &rule.Conditions[i]
		if condition.Field == models.MailRuleFieldFolder {
			if condition.Operator != models.MailRuleOpIs && condition.Operator != models.MailRuleOpIsNot {
				condition.Operator = models.MailRuleOpIs
			}
			if condition.Folder == nil || !ownFolder(condition.Folder) {
				condition.Folder = &models.MailRuleFolderTarget{Role: "inbox"}
			}
		} else {
			condition.Folder = nil
			if !slices.Contains(views.MailRuleTextOperators, condition.Operator) {
				condition.Operator = models.MailRuleOpContains
			}
		}
	}
	for i := range rule.Actions {
		action := &rule.Actions[i]
		if action.Kind == models.MailRuleActionMove {
			if action.Folder != nil && !ownFolder(action.Folder) {
				action.Folder = nil
			}
		} else {
			action.Folder = nil
		}
	}
}

// mailRuleFromForm reads a rule from the rule form. Each condition row sends
// every one of its fields, so the lists line up.
func mailRuleFromForm(r *http.Request) models.MailRule {
	rule := models.MailRule{
		ID:             strings.TrimSpace(r.FormValue("id")),
		AccountID:      strings.TrimSpace(r.FormValue("account_id")),
		Name:           r.FormValue("name"),
		Enabled:        r.FormValue("enabled") != "",
		MatchAll:       true,
		StopProcessing: r.FormValue("stop_processing") != "",
	}
	at := func(values []string, i int) string {
		if i < len(values) {
			return values[i]
		}
		return ""
	}
	fields, operators, values, folders, groups := r.PostForm["cond_field"], r.PostForm["cond_op"], r.PostForm["cond_value"], r.PostForm["cond_folder"], r.PostForm["cond_group"]
	for i, field := range fields {
		if i == mailrules.MaxConditions+1 {
			break
		}
		group, _ := strconv.Atoi(at(groups, i))
		condition := models.MailRuleCondition{Group: group, Field: field, Operator: at(operators, i), Value: at(values, i)}
		if field == models.MailRuleFieldFolder {
			condition.Value = ""
			condition.Folder = mailRuleFolderFromValue(at(folders, i))
		}
		rule.Conditions = append(rule.Conditions, condition)
	}
	kinds, actionFolders, labels := r.PostForm["act_kind"], r.PostForm["act_folder"], r.PostForm["act_label"]
	for i, kind := range kinds {
		if i == mailrules.MaxActions+1 {
			break
		}
		action := models.MailRuleAction{Kind: kind}
		switch kind {
		case models.MailRuleActionMove:
			action.Folder = mailRuleFolderFromValue(at(actionFolders, i))
		case models.MailRuleActionLabel:
			action.Label = at(labels, i)
		}
		rule.Actions = append(rule.Actions, action)
	}
	return rule
}

// mailRuleFolderFromValue reads a folder choice of the form: "role:" and a
// role, or "id:" and a folder ID.
func mailRuleFolderFromValue(value string) *models.MailRuleFolderTarget {
	kind, rest, _ := strings.Cut(strings.TrimSpace(value), ":")
	switch {
	case rest == "":
		return nil
	case kind == "role":
		return &models.MailRuleFolderTarget{Role: rest}
	case kind == "id":
		return &models.MailRuleFolderTarget{FolderID: rest}
	}
	return nil
}

// handleSaveMailRule adds a rule, or changes the one named by id. A refused
// rule shows why, with the form as it was sent.
func (h *Handler) handleSaveMailRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	rule := mailRuleFromForm(r)
	_, saveErr := h.db.SaveMailRuleGuarded(r.Context(), h.userID(r.Context()), rule, h.signatureGuard)
	if errors.Is(saveErr, sql.ErrNoRows) && rule.ID != "" {
		http.NotFound(w, r)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	if saveErr != nil {
		switch {
		case errors.Is(saveErr, sql.ErrNoRows):
			data.Error = "That account is no longer here. Choose another."
		case errors.Is(saveErr, storage.ErrMailRuleFolder):
			data.Error = "A folder the rule names is not in the account it is for. Choose the folder again."
		case errors.Is(saveErr, storage.ErrTooManyMailRules):
			data.Error = sentence(saveErr.Error())
		default:
			if mailrules.Validate(&rule) == nil {
				log.Printf("save mail rule: %v", saveErr)
				data.Error = "The rule could not be saved."
			} else {
				data.Error = sentence(saveErr.Error())
			}
		}
		rule = mailRuleFromForm(r)
		fitMailRuleForm(&rule, data.Accounts)
		data.Editing = &rule
		h.renderMailRuleDialogPanel(w, r, data)
		return
	}
	h.renderMailRulesDone(w, r, data)
}

// sentence starts a message with a capital and ends it with a period.
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	first, size := utf8.DecodeRuneInString(text)
	text = string(unicode.ToUpper(first)) + text[size:]
	if !strings.HasSuffix(text, ".") {
		text += "."
	}
	return text
}

// handleDeleteMailRule removes a rule.
func (h *Handler) handleDeleteMailRule(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteMailRuleGuarded(r.Context(), h.userID(r.Context()), r.PathValue("id"), h.signatureGuard)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	if err != nil {
		log.Printf("delete mail rule %s: %v", r.PathValue("id"), err)
		data.Error = "The rule could not be deleted."
	}
	h.renderMailRulesPanel(w, r, data)
}

// handleMailRuleEnabled makes a rule active or inactive.
func (h *Handler) handleMailRuleEnabled(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	err := h.db.SetMailRuleEnabledGuarded(r.Context(), h.userID(r.Context()), r.PathValue("id"), r.FormValue("enabled") != "", h.signatureGuard)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	// The switch has already moved, and the card follows it; only a failure
	// shows the list again, as it is.
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	log.Printf("make mail rule %s active or inactive: %v", r.PathValue("id"), err)
	data.Error = "The rule could not be made active or inactive."
	w.Header().Set("HX-Retarget", "#mail-rules-panel")
	w.Header().Set("HX-Reswap", "outerHTML")
	h.renderMailRulesPanel(w, r, data)
}

// handleMailRuleOrder puts the rules in the order given, one rule_id value
// per rule. It refuses a list that doesn't name each rule once, so the page
// can reload the rules and try again.
func (h *Handler) handleMailRuleOrder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	ids := r.PostForm["rule_id"]
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	err := h.db.SetMailRuleOrderGuarded(r.Context(), h.userID(r.Context()), ids, h.signatureGuard)
	if errors.Is(err, storage.ErrMailRuleOrderChanged) {
		http.Error(w, "Your rules changed. Reload the page and try again.", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("order mail rules: %v", err)
		http.Error(w, "failed to save the order", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mailRuleRunScope reads the folder a run is for, Inbox when none is given.
func mailRuleRunScope(value string) (storage.MailRuleRunScope, string) {
	target := mailRuleFolderFromValue(value)
	if target == nil {
		value, target = "role:inbox", &models.MailRuleFolderTarget{Role: "inbox"}
	}
	return storage.MailRuleRunScope{Folder: *target}, value
}

type mailRuleRoute struct {
	pattern string
	handler func(*Handler) http.HandlerFunc
}

// mailRuleLocalRoutes change only the rules themselves.
var mailRuleLocalRoutes = []mailRuleRoute{
	{"GET /api/settings/rules/panel", func(h *Handler) http.HandlerFunc { return h.handleMailRulesPanel }},
	{"GET /api/settings/rules/dialog", func(h *Handler) http.HandlerFunc { return h.handleMailRuleDialog }},
	{"POST /api/settings/rules/form", func(h *Handler) http.HandlerFunc { return h.handleMailRuleForm }},
	{"POST /api/settings/rules", func(h *Handler) http.HandlerFunc { return h.handleSaveMailRule }},
	{"POST /api/settings/rules/order", func(h *Handler) http.HandlerFunc { return h.handleMailRuleOrder }},
	{"DELETE /api/settings/rules/{id}", func(h *Handler) http.HandlerFunc { return h.handleDeleteMailRule }},
	{"POST /api/settings/rules/{id}/enabled", func(h *Handler) http.HandlerFunc { return h.handleMailRuleEnabled }},
}
