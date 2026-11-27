package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
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
	tags, err := db.ListMailRuleTags(ctx, userID)
	if err != nil {
		return models.MailRulesData{}, fmt.Errorf("tags: %w", err)
	}
	data := models.MailRulesData{Rules: rules, Tags: tags, RetiredFlagLabels: map[string][]string{}}
	for _, account := range accounts {
		// Local folders are only somewhere to move mail to; the form
		// doesn't offer them as an account to watch.
		if account.IsDeleting || account.Provider == providers.ProviderDAV {
			continue
		}
		tree, err := db.AccountFolderTree(ctx, account.ID)
		if err != nil {
			return models.MailRulesData{}, fmt.Errorf("folders of %s: %w", account.ID, err)
		}
		data.Accounts = append(data.Accounts, models.MailRuleAccount{
			ID: account.ID, Name: account.Name, Email: account.Email, Color: account.Color, Provider: account.Provider, Hidden: account.Hidden,
			Folders: folders[account.ID], Tree: tree,
		})
	}
	for _, rule := range rules {
		for _, action := range rule.Actions {
			if action.Kind != models.MailRuleActionLabel {
				continue
			}
			for _, account := range data.Accounts {
				if !rule.WatchesAccount(account.ID) || !storage.SupportsFlagColors(account.Provider) {
					continue
				}
				retired, err := db.RetiredFlagLabelReference(ctx, userID, account.ID, action.Label, "", "")
				if err != nil {
					return models.MailRulesData{}, fmt.Errorf("check rule flag labels: %w", err)
				}
				if retired && !slices.Contains(data.RetiredFlagLabels[rule.ID], action.Label) {
					data.RetiredFlagLabels[rule.ID] = append(data.RetiredFlagLabels[rule.ID], action.Label)
				}
			}
		}
	}
	return data, nil
}

// loadMailRulesData gathers what the Rules settings show, with the filters
// the request carries, or writes the error response and returns false.
func (h *Handler) loadMailRulesData(w http.ResponseWriter, r *http.Request) (models.MailRulesData, bool) {
	data, err := mailRulesData(r.Context(), h.db, h.userID(r.Context()))
	if err != nil {
		log.Printf("mail rules of %s: %v", h.userID(r.Context()), err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_rules"), http.StatusInternalServerError)
		return models.MailRulesData{}, false
	}
	mailRulesFilters(r, &data)
	return data, true
}

// mailRulesFilters reads the filters the rules list had when a request was
// made (settings.js sends them with each), so the list drawn again keeps
// them: the folder whose rules it shows (filter_folder), the categories,
// accounts and kinds of action it shows rules of (filter_tag,
// filter_account, filter_action, each separated by commas, which none of
// them holds), and whether it shows rules turned on or off (filter_status).
// A value that names nothing there is any more, such as a category no rule
// has, is dropped.
func mailRulesFilters(r *http.Request, data *models.MailRulesData) {
	data.FolderFilter = strings.TrimSpace(r.FormValue("filter_folder"))
	data.TagFilter = mailRulesFilterValues(r, "filter_tag", func(tag string) (string, bool) {
		for _, known := range data.Tags {
			if strings.EqualFold(tag, known) {
				return known, true
			}
		}
		return "", false
	})
	data.AccountFilter = mailRulesFilterValues(r, "filter_account", func(id string) (string, bool) {
		for _, account := range data.Accounts {
			if account.ID == id && account.Provider != providers.ProviderLocal {
				return id, true
			}
		}
		return "", false
	})
	data.ActionFilter = mailRulesFilterValues(r, "filter_action", func(kind string) (string, bool) {
		return kind, views.MailRuleActionKnown(kind)
	})
	switch status := r.FormValue("filter_status"); status {
	case "on", "off":
		data.StatusFilter = status
	default:
		data.StatusFilter = ""
	}
}

// mailRulesFilterValues are the values of a filter separated by commas,
// each once, as known keeps them; known drops one that names nothing.
func mailRulesFilterValues(r *http.Request, name string, known func(string) (string, bool)) []string {
	var values []string
	for _, value := range r.Form[name] {
		for _, part := range strings.Split(value, ",") {
			if kept, ok := known(strings.TrimSpace(part)); ok && !slices.Contains(values, kept) {
				values = append(values, kept)
			}
		}
	}
	return values
}

// mailRulesFiltering reports whether any of the list's filters is on.
func mailRulesFiltering(data models.MailRulesData) bool {
	return data.FolderFilter != "" || len(data.TagFilter) > 0 || len(data.AccountFilter) > 0 || data.StatusFilter != "" || len(data.ActionFilter) > 0
}

// mailRuleFiltered reports whether the list's filters would hide a rule:
// it moves no mail into the folder, has none of the categories, runs on
// none of the accounts, is turned on or off the other way, or does none of
// the kinds of action.
func mailRuleFiltered(data models.MailRulesData, rule models.MailRule, destinations []string) bool {
	if data.FolderFilter != "" && !slices.Contains(destinations, data.FolderFilter) {
		return true
	}
	if len(data.TagFilter) > 0 && !slices.ContainsFunc(rule.Tags, func(tag string) bool {
		return slices.ContainsFunc(data.TagFilter, func(t string) bool { return strings.EqualFold(t, tag) })
	}) {
		return true
	}
	if len(data.AccountFilter) > 0 && rule.AccountsChosen && !slices.ContainsFunc(rule.AccountIDs, func(id string) bool { return slices.Contains(data.AccountFilter, id) }) {
		return true
	}
	if (data.StatusFilter == "on" && !rule.Enabled) || (data.StatusFilter == "off" && rule.Enabled) {
		return true
	}
	return len(data.ActionFilter) > 0 && !slices.ContainsFunc(rule.Actions, func(action models.MailRuleAction) bool { return slices.Contains(data.ActionFilter, action.Kind) })
}

// renderMailRulesPanel draws the list of rules, each with the folders it
// moves mail into, which the list can be filtered by.
func (h *Handler) renderMailRulesPanel(w http.ResponseWriter, r *http.Request, data models.MailRulesData) {
	destinations, err := h.db.MailRuleDestinations(r.Context(), h.userID(r.Context()), data.Rules)
	if err != nil {
		log.Printf("mail rule folders of %s: %v", h.userID(r.Context()), err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_rules"), http.StatusInternalServerError)
		return
	}
	data.Destinations = destinations
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

// handleMailRuleFolderCount answers how many rules move mail into a folder
// (folder): the rules the list shows when opened for it. A folder's menu
// asks when it opens, to show the number beside its Rules item. A folder
// that isn't the user's has none.
func (h *Handler) handleMailRuleFolderCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	folderID := strings.TrimSpace(r.URL.Query().Get("folder"))
	rules, err := h.db.ListMailRules(ctx, userID)
	var destinations map[string][]string
	if err == nil {
		destinations, err = h.db.MailRuleDestinations(ctx, userID, rules)
	}
	if err != nil {
		log.Printf("mail rules for folder %s of %s: %v", folderID, userID, err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_rules"), http.StatusInternalServerError)
		return
	}
	count := 0
	for _, rule := range rules {
		if folderID != "" && slices.Contains(destinations[rule.ID], folderID) {
			count++
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeFolderJSON(w, map[string]int{"count": count})
}

// handleMailRulesPanel renders the list of rules, with the filters asked
// for (mailRulesFilters).
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
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	rule, forGoodChosen := mailRuleFromForm(r)
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
	fitMailRuleForm(&rule, data.Accounts, forGoodChosen)
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
// folder that is still there, and a move to Local folders starts out
// deleting mail it moves from another account for good.
// forGoodChosen says, by action, whether the form already showed the choice
// of what happens to mail from another account, which is then kept.
func fitMailRuleForm(rule *models.MailRule, accounts []models.MailRuleAccount, forGoodChosen []bool) {
	mailrules.GroupConditions(rule)
	folderAccount := func(target *models.MailRuleFolderTarget) (models.MailRuleAccount, bool) {
		for _, account := range accounts {
			for _, folder := range account.Folders {
				if folder.ID == target.FolderID {
					return account, true
				}
			}
		}
		return models.MailRuleAccount{}, false
	}
	ownFolder := func(target *models.MailRuleFolderTarget) bool {
		if target == nil || target.FolderID == "" {
			return true
		}
		_, ok := folderAccount(target)
		return ok
	}
	for i := range rule.Conditions {
		condition := &rule.Conditions[i]
		if condition.Field == models.MailRuleFieldFlagColor {
			condition.Folder = nil
			if condition.Operator != models.MailRuleOpIsNot {
				condition.Operator = models.MailRuleOpIs
			}
			continue
		}
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
			if action.Folder == nil || action.Folder.FolderID == "" {
				action.ForGood = false
			} else if i >= len(forGoodChosen) || !forGoodChosen[i] {
				account, _ := folderAccount(action.Folder)
				action.ForGood = account.Provider == providers.ProviderLocal
			}
		} else {
			action.Folder = nil
		}
	}
}

// mailRuleFromForm reads a rule from the rule form. Each condition and
// action row sends every one of its fields, so the lists line up. It also
// says, by action, whether the row had the choice of what happens to mail
// from another account.
func mailRuleFromForm(r *http.Request) (models.MailRule, []bool) {
	rule := models.MailRule{
		ID:             strings.TrimSpace(r.FormValue("id")),
		Name:           r.FormValue("name"),
		Enabled:        r.FormValue("enabled") != "",
		MatchAll:       true,
		StopProcessing: r.FormValue("stop_processing") != "",
		Tags:           r.PostForm["tags"],
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
	rule.AccountsChosen = true
	for _, id := range strings.Split(r.FormValue("account_ids"), ",") {
		switch id = strings.TrimSpace(id); id {
		case "":
		case "all":
			rule.AccountsChosen = false
		default:
			rule.AccountIDs = append(rule.AccountIDs, id)
		}
	}
	kinds, actionFolders, labels, forGood := r.PostForm["act_kind"], r.PostForm["act_folder"], r.PostForm["act_label"], r.PostForm["act_for_good"]
	chosen := []bool{}
	for i, kind := range kinds {
		if i == mailrules.MaxActions+1 {
			break
		}
		action := models.MailRuleAction{Kind: kind}
		switch kind {
		case models.MailRuleActionMove:
			action.Folder = mailRuleFolderFromValue(at(actionFolders, i))
			action.ForGood = at(forGood, i) == "1"
		case models.MailRuleActionLabel:
			action.Label = at(labels, i)
		case models.MailRuleActionSetFlagColor, models.MailRuleActionRemoveFlagColor:
			action.FlagColor = at(labels, i)
		}
		chosen = append(chosen, at(forGood, i) != "")
		rule.Actions = append(rule.Actions, action)
	}
	return rule, chosen
}

// mailRuleFolderFromValue reads a folder choice of the form: "role:" and a
// role, "id:" and a folder ID, or a folder an import didn't find, while no
// other is chosen (mailrules.MissingFolderValue).
func mailRuleFolderFromValue(value string) *models.MailRuleFolderTarget {
	if target, ok := mailrules.ParseMissingFolderValue(value); ok {
		return target
	}
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
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	rule, _ := mailRuleFromForm(r)
	if missing := mailRuleMissingFolderName(rule); missing != "" {
		// A folder an import didn't find is kept until another is chosen;
		// the rule can't be saved with it.
		data, ok := h.loadMailRulesData(w, r)
		if !ok {
			return
		}
		data.Error = i18n.T(r.Context(), "rules.missing.choose_folder", "folder", missing)
		rule, forGoodChosen := mailRuleFromForm(r)
		fitMailRuleForm(&rule, data.Accounts, forGoodChosen)
		data.Editing = &rule
		h.renderMailRuleDialogPanel(w, r, data)
		return
	}
	saved, saveErr := h.db.SaveMailRuleGuarded(r.Context(), h.userID(r.Context()), rule, h.signatureGuard)
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
			data.Error = i18n.T(r.Context(), "server.msg.that_account_is_no_longer_here_choose")
		case errors.Is(saveErr, storage.ErrMailRuleFolder):
			data.Error = i18n.T(r.Context(), "server.msg.a_folder_condition_names_a_folder_of")
		case errors.Is(saveErr, storage.ErrTooManyMailRules):
			data.Error = serverText(r.Context(), sentence(saveErr.Error()))
		case errors.Is(saveErr, mailrules.ErrFlagColor):
			data.Error = i18n.T(r.Context(), "mail.flag_color.choose")
		default:
			if mailrules.Validate(&rule) == nil {
				log.Printf("save mail rule: %v", saveErr)
				data.Error = i18n.T(r.Context(), "server.msg.the_rule_could_not_be_saved")
			} else {
				data.Error = serverText(r.Context(), sentence(saveErr.Error()))
			}
		}
		rule, forGoodChosen := mailRuleFromForm(r)
		fitMailRuleForm(&rule, data.Accounts, forGoodChosen)
		data.Editing = &rule
		h.renderMailRuleDialogPanel(w, r, data)
		return
	}
	// A rule just added shows even when the list's filters would hide it:
	// they are cleared, as the search is (settings.js).
	if rule.ID == "" && mailRulesFiltering(data) {
		destinations, err := h.db.MailRuleDestinations(r.Context(), h.userID(r.Context()), []models.MailRule{saved})
		if err != nil {
			log.Printf("folders of a new mail rule: %v", err)
		}
		if err != nil || mailRuleFiltered(data, saved, destinations[saved.ID]) {
			data.FolderFilter, data.TagFilter, data.AccountFilter, data.StatusFilter, data.ActionFilter = "", nil, nil, "", nil
		}
	}
	h.renderMailRulesDone(w, r, data)
}

// mailRuleMissingFolderName names the first folder of a rule that an
// import didn't find, by its path, or is "" when there's none.
func mailRuleMissingFolderName(rule models.MailRule) string {
	for _, condition := range rule.Conditions {
		if condition.Folder != nil && condition.Folder.Missing() {
			return condition.Folder.Path
		}
	}
	for _, action := range rule.Actions {
		if action.Folder != nil && action.Folder.Missing() {
			return action.Folder.Path
		}
	}
	return ""
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
		data.Error = i18n.T(r.Context(), "server.msg.the_rule_could_not_be_deleted")
	}
	h.renderMailRulesPanel(w, r, data)
}

// handleMailRuleEnabled makes a rule active or inactive.
func (h *Handler) handleMailRuleEnabled(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
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
	if errors.Is(err, storage.ErrMailRuleMissingFolder) {
		data.Error = i18n.T(r.Context(), "rules.missing.cant_turn_on")
	} else {
		log.Printf("make mail rule %s active or inactive: %v", r.PathValue("id"), err)
		data.Error = i18n.T(r.Context(), "server.msg.the_rule_could_not_be_made_active")
	}
	w.Header().Set("HX-Retarget", "#mail-rules-panel")
	w.Header().Set("HX-Reswap", "outerHTML")
	h.renderMailRulesPanel(w, r, data)
}

// mailRuleSelection reads the rules a change to several of them names: the
// IDs in rule_ids, separated by commas.
func mailRuleSelection(r *http.Request) []string {
	var ids []string
	for _, id := range strings.Split(r.FormValue("rule_ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// handleMailRulesEnabled turns the rules selected in the list on, or off
// when enabled is empty, and shows the list again saying how many changed.
func (h *Handler) handleMailRulesEnabled(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ids := mailRuleSelection(r)
	if len(ids) == 0 {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	enabled := r.FormValue("enabled") != ""
	changed, err := h.db.SetMailRulesEnabledGuarded(r.Context(), h.userID(r.Context()), ids, enabled, h.signatureGuard)
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	switch {
	case err != nil:
		log.Printf("make %d mail rules active or inactive: %v", len(ids), err)
		data.Error = i18n.T(r.Context(), "rules.bulk.failed")
	case enabled:
		data.Notice = i18n.N(r.Context(), "rules.bulk.turned_on", changed)
		// Rules that name a folder an import didn't find stay off.
		missing := 0
		for _, rule := range data.Rules {
			if slices.Contains(ids, rule.ID) && !rule.Enabled && rule.MissingFolders() {
				missing++
			}
		}
		if missing > 0 {
			data.Notice += " " + i18n.N(r.Context(), "rules.bulk.missing_folders", missing, "count", i18n.Number(r.Context(), missing))
		}
	default:
		data.Notice = i18n.N(r.Context(), "rules.bulk.turned_off", changed)
	}
	h.renderMailRulesPanel(w, r, data)
}

// mailRuleTagChangeData is the dialog that adds tags to the rules selected
// in the list, or takes them off, with the tags those rules have.
func mailRuleTagChangeData(data *models.MailRulesData, remove bool, ids []string) {
	change := &models.MailRuleTagChange{Remove: remove, RuleIDs: ids}
	counts := map[string]int{}
	for _, rule := range data.Rules {
		if slices.Contains(ids, rule.ID) {
			for _, tag := range rule.Tags {
				counts[tag]++
			}
		}
	}
	for _, tag := range data.Tags {
		if counts[tag] > 0 {
			change.Present = append(change.Present, models.MailRuleTagCount{Tag: tag, Rules: counts[tag]})
		}
	}
	data.TagChange = change
}

// handleMailRuleTagDialog shows the dialog that adds categories (tags) to
// the rules selected in the list, or with remove, takes some off them.
func (h *Handler) handleMailRuleTagDialog(w http.ResponseWriter, r *http.Request) {
	ids := mailRuleSelection(r)
	if len(ids) == 0 {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	mailRuleTagChangeData(&data, r.FormValue("remove") != "", ids)
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	views.MailRuleDialog(data).Render(r.Context(), w)
}

// handleMailRulesTags adds the tags given to the rules selected in the list,
// or with remove, takes them off, and shows the list again saying how many
// rules changed. A tag that can't be one shows the dialog again, saying why.
func (h *Handler) handleMailRulesTags(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ids := mailRuleSelection(r)
	if len(ids) == 0 {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	remove := r.FormValue("remove") != ""
	tags := r.PostForm["tags"]
	userID := h.userID(r.Context())
	var changed int
	var err error
	switch {
	case remove && len(tags) == 0:
		err = mailrules.TagError("choose the categories to remove")
	case remove:
		changed, err = h.db.RemoveMailRulesTagsGuarded(r.Context(), userID, ids, tags, h.signatureGuard)
	default:
		changed, err = h.db.AddMailRulesTagsGuarded(r.Context(), userID, ids, tags, h.signatureGuard)
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	if err != nil {
		// The dialog stays open with what was typed or chosen, saying why.
		mailRuleTagChangeData(&data, remove, ids)
		data.TagChange.Tags = tags
		var refused mailrules.TagError
		if errors.As(err, &refused) {
			data.Error = serverText(r.Context(), sentence(refused.Error()))
		} else {
			log.Printf("change the tags of %d mail rules: %v", len(ids), err)
			data.Error = i18n.T(r.Context(), "rules.bulk.failed")
		}
		h.renderMailRuleDialogPanel(w, r, data)
		return
	}
	switch {
	case remove:
		data.Notice = i18n.N(r.Context(), "rules.categories.removed", changed)
	case changed == 0:
		data.Notice = i18n.T(r.Context(), "rules.categories.added_none")
	default:
		data.Notice = i18n.N(r.Context(), "rules.categories.added", changed)
	}
	h.renderMailRulesDone(w, r, data)
}

// handleDeleteMailRules removes the rules selected in the list, and shows
// the list again saying how many went.
func (h *Handler) handleDeleteMailRules(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ids := mailRuleSelection(r)
	if len(ids) == 0 {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	deleted, err := h.db.DeleteMailRulesGuarded(r.Context(), h.userID(r.Context()), ids, h.signatureGuard)
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	if err != nil {
		log.Printf("delete %d mail rules: %v", len(ids), err)
		data.Error = i18n.T(r.Context(), "rules.bulk.failed")
	} else {
		data.Notice = i18n.N(r.Context(), "rules.bulk.deleted", deleted)
	}
	h.renderMailRulesPanel(w, r, data)
}

// handleMailRuleOrder puts the rules in the order given, one rule_id value
// per rule. It refuses a list that doesn't name each rule once, so the page
// can reload the rules and try again.
func (h *Handler) handleMailRuleOrder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ids := r.PostForm["rule_id"]
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	err := h.db.SetMailRuleOrderGuarded(r.Context(), h.userID(r.Context()), ids, h.signatureGuard)
	if errors.Is(err, storage.ErrMailRuleOrderChanged) {
		http.Error(w, i18n.T(r.Context(), "server.error.your_rules_changed_reload_the_page_and"), http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("order mail rules: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_save_the_order"), http.StatusInternalServerError)
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
	{"GET /api/settings/rules/folder-count", func(h *Handler) http.HandlerFunc { return h.handleMailRuleFolderCount }},
	{"GET /api/settings/rules/dialog", func(h *Handler) http.HandlerFunc { return h.handleMailRuleDialog }},
	{"POST /api/settings/rules/form", func(h *Handler) http.HandlerFunc { return h.handleMailRuleForm }},
	{"POST /api/settings/rules", func(h *Handler) http.HandlerFunc { return h.handleSaveMailRule }},
	{"POST /api/settings/rules/order", func(h *Handler) http.HandlerFunc { return h.handleMailRuleOrder }},
	{"POST /api/settings/rules/selected/enabled", func(h *Handler) http.HandlerFunc { return h.handleMailRulesEnabled }},
	{"POST /api/settings/rules/selected/delete", func(h *Handler) http.HandlerFunc { return h.handleDeleteMailRules }},
	{"GET /api/settings/rules/selected/tags", func(h *Handler) http.HandlerFunc { return h.handleMailRuleTagDialog }},
	{"GET /api/settings/rules/export", func(h *Handler) http.HandlerFunc { return h.handleExportMailRules }},
	{"GET /api/settings/rules/import", func(h *Handler) http.HandlerFunc { return h.handleMailRulesImportDialog }},
	{"POST /api/settings/rules/import/preview", func(h *Handler) http.HandlerFunc { return h.handlePreviewMailRulesImport }},
	{"POST /api/settings/rules/import", func(h *Handler) http.HandlerFunc { return h.handleImportMailRules }},
	{"POST /api/settings/rules/selected/tags", func(h *Handler) http.HandlerFunc { return h.handleMailRulesTags }},
	{"DELETE /api/settings/rules/{id}", func(h *Handler) http.HandlerFunc { return h.handleDeleteMailRule }},
	{"POST /api/settings/rules/{id}/enabled", func(h *Handler) http.HandlerFunc { return h.handleMailRuleEnabled }},
	{"GET /api/settings/rules/activity", func(h *Handler) http.HandlerFunc { return h.handleMailRuleActivity }},
	{"GET /api/settings/rules/activity/list", func(h *Handler) http.HandlerFunc { return h.handleMailRuleActivityList }},
	{"POST /api/settings/rules/activity/clear", func(h *Handler) http.HandlerFunc { return h.handleClearMailRuleActivity }},
}
