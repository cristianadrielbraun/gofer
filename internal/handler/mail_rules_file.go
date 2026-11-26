package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// mailRulesFileMaxBytes is the largest rules file an import reads: far more
// than the most rules one may have take.
const mailRulesFileMaxBytes = 4 << 20

// handleExportMailRules downloads the rules named in rule_ids, separated by
// commas, as a rules file, in the order they run; with none named, every
// rule. Naming only rules that aren't the user's any more is not found.
func (h *Handler) handleExportMailRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := mailRulesData(ctx, h.db, h.userID(ctx))
	if err != nil {
		log.Printf("export mail rules of %s: %v", h.userID(ctx), err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_rules"), http.StatusInternalServerError)
		return
	}
	rules := data.Rules
	if ids := mailRuleSelection(r); len(ids) > 0 {
		rules = slices.DeleteFunc(slices.Clone(rules), func(rule models.MailRule) bool { return !slices.Contains(ids, rule.ID) })
		if len(rules) == 0 {
			http.NotFound(w, r)
			return
		}
	}
	now := time.Now()
	encoded, err := json.MarshalIndent(mailrules.Export(rules, data.Accounts, now), "", "  ")
	if err != nil {
		log.Printf("export mail rules of %s: %v", h.userID(ctx), err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_rules"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gofer-rules-`+now.Format("2006-01-02")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(encoded, '\n'))
}

// handleMailRulesImportDialog opens the dialog that imports rules, asking
// for the file.
func (h *Handler) handleMailRulesImportDialog(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	data.Import = &models.MailRuleImport{}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	views.MailRuleDialog(data).Render(r.Context(), w)
}

// renderMailRulesImportError asks for the file again, saying why the one
// sent can't be imported.
func (h *Handler) renderMailRulesImportError(w http.ResponseWriter, r *http.Request, data models.MailRulesData, message string) {
	data.Import = &models.MailRuleImport{}
	data.Error = message
	h.renderMailRuleDialogPanel(w, r, data)
}

// mailRulesFileError says why a rules file can't be read.
func mailRulesFileError(r *http.Request, err error) string {
	switch {
	case errors.Is(err, mailrules.ErrNewerFile):
		return i18n.T(r.Context(), "rules.import.newer_file")
	case errors.Is(err, mailrules.ErrDamagedFile):
		return i18n.T(r.Context(), "rules.import.damaged_file")
	}
	return i18n.T(r.Context(), "rules.import.not_rules_file")
}

// handlePreviewMailRulesImport reads the rules file sent (file) and shows
// what importing it would add, rule by rule.
func (h *Handler) handlePreviewMailRulesImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, mailRulesFileMaxBytes+(64<<10))
	parseErr := r.ParseMultipartForm(mailRulesFileMaxBytes)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	if parseErr != nil {
		h.renderMailRulesImportError(w, r, data, i18n.T(r.Context(), "rules.import.too_big"))
		return
	}
	upload, _, err := r.FormFile("file")
	if err != nil {
		h.renderMailRulesImportError(w, r, data, i18n.T(r.Context(), "rules.import.choose_file"))
		return
	}
	defer upload.Close()
	content, err := io.ReadAll(io.LimitReader(upload, mailRulesFileMaxBytes+1))
	if err != nil || len(content) > mailRulesFileMaxBytes {
		h.renderMailRulesImportError(w, r, data, i18n.T(r.Context(), "rules.import.too_big"))
		return
	}
	file, err := mailrules.ParseFile(content)
	if err != nil {
		h.renderMailRulesImportError(w, r, data, mailRulesFileError(r, err))
		return
	}
	data.Import = mailRulesImportView(r.Context(), file, string(content), data, nil)
	h.renderMailRuleDialogPanel(w, r, data)
}

// mailRulesImportView is what the import dialog shows of a file: each rule
// as importing it would add it, with the choice made for a rule whose name
// is taken (choices, by its place in the file), or the one it starts with:
// skip a rule that is already here, keep both of two that differ.
func mailRulesImportView(ctx context.Context, file *mailrules.File, content string, data models.MailRulesData, choices map[int]string) *models.MailRuleImport {
	view := &models.MailRuleImport{File: content}
	if exported, err := time.Parse(time.RFC3339, file.ExportedAt); err == nil {
		view.ExportedAt = i18n.Dates(ctx).LongMonthDate(exported)
	}
	for _, plan := range mailrules.PlanImport(file, data.Accounts, data.Rules) {
		rule := models.MailRuleImportRule{
			Index: plan.Index, Rule: plan.Rule, MissingAccounts: plan.MissingAccounts, MissingFolders: plan.MissingFolders,
			TurnsOn: plan.TurnsOn, SameName: plan.SameName != "", Same: plan.Same,
		}
		if errors.Is(plan.Err, mailrules.ErrFlagColor) {
			rule.Problem = i18n.T(ctx, "mail.flag_color.choose")
		} else if plan.Err != nil {
			rule.Problem = serverText(ctx, sentence(plan.Err.Error()))
		}
		if rule.SameName {
			rule.Choice = models.MailRuleImportAdd
			if plan.Same {
				rule.Choice = models.MailRuleImportSkip
			}
			if choice := choices[plan.Index]; choice == models.MailRuleImportSkip || choice == models.MailRuleImportAdd {
				rule.Choice = choice
			}
		}
		view.Rules = append(view.Rules, rule)
	}
	return view
}

// mailRulesImported counts what importing a rules file added: Imported
// rules, of which Waiting turn on once the folders they're missing are
// found and Off stay off until what they're missing is chosen, and the
// rules Skipped.
type mailRulesImported struct {
	Imported, Waiting, Off, Skipped int
}

// chooseMailRulesImport is what importing view adds after existing, the
// owner's rules: each rule that can be imported, but those whose name is
// taken that are chosen to be skipped. A rule kept beside one of the same
// name gets a number after its name.
func chooseMailRulesImport(view *models.MailRuleImport, existing []models.MailRule) ([]models.MailRule, mailRulesImported) {
	names := map[string]bool{}
	for _, rule := range existing {
		names[strings.ToLower(strings.TrimSpace(rule.Name))] = true
	}
	var rules []models.MailRule
	var counts mailRulesImported
	for _, item := range view.Rules {
		if item.Problem != "" || (item.SameName && item.Choice == models.MailRuleImportSkip) {
			counts.Skipped++
			continue
		}
		rule := item.Rule
		rule.Name = mailrules.UniqueName(rule.Name, func(name string) bool { return names[strings.ToLower(name)] })
		names[strings.ToLower(rule.Name)] = true
		switch {
		case item.TurnsOn:
			counts.Waiting++
		case len(item.MissingAccounts) > 0 || len(item.MissingFolders) > 0:
			counts.Off++
		}
		rules = append(rules, rule)
	}
	counts.Imported = len(rules)
	return rules, counts
}

// mailRulesImportNotice says what importing rules added.
func mailRulesImportNotice(ctx context.Context, counts mailRulesImported) string {
	if counts.Imported == 0 {
		return i18n.T(ctx, "rules.import.none")
	}
	notice := []string{i18n.N(ctx, "rules.import.done", counts.Imported, "count", i18n.Number(ctx, counts.Imported))}
	if counts.Waiting > 0 {
		notice = append(notice, i18n.N(ctx, "rules.import.done_waiting", counts.Waiting, "count", i18n.Number(ctx, counts.Waiting)))
	}
	if counts.Off > 0 {
		notice = append(notice, i18n.N(ctx, "rules.import.done_off", counts.Off, "count", i18n.Number(ctx, counts.Off)))
	}
	if counts.Skipped > 0 {
		notice = append(notice, i18n.N(ctx, "rules.import.skipped", counts.Skipped, "count", i18n.Number(ctx, counts.Skipped)))
	}
	return strings.Join(notice, " ")
}

// mailRulesImportChoices reads what to do with each rule whose name is
// taken: rule_action:N is "skip" or "add" for the file's rule N.
func mailRulesImportChoices(r *http.Request) map[int]string {
	choices := map[int]string{}
	for key, values := range r.PostForm {
		if index, ok := strings.CutPrefix(key, "rule_action:"); ok && len(values) > 0 {
			if i, err := strconv.Atoi(index); err == nil {
				choices[i] = values[0]
			}
		}
	}
	return choices
}

// handleImportMailRules adds the rules of the file sent back (file), after
// the user's own, as chosen for each rule whose name is taken, and shows
// the list saying what came in. Rules that can't be imported are left out;
// the dialog said so. A rule kept beside one of the same name gets a number
// after its name.
func (h *Handler) handleImportMailRules(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2*mailRulesFileMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	data, ok := h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	content := r.PostForm.Get("file")
	file, err := mailrules.ParseFile([]byte(content))
	if err != nil {
		h.renderMailRulesImportError(w, r, data, mailRulesFileError(r, err))
		return
	}
	view := mailRulesImportView(r.Context(), file, content, data, mailRulesImportChoices(r))
	rules, counts := chooseMailRulesImport(view, data.Rules)
	if len(rules) > 0 {
		if _, err := h.db.ImportMailRulesGuarded(r.Context(), h.userID(r.Context()), rules, h.signatureGuard); err != nil {
			if errors.Is(err, storage.ErrTooManyMailRules) {
				data.Error = i18n.N(r.Context(), "rules.import.too_many", mailrules.MaxRulesPerOwner, "count", i18n.Number(r.Context(), mailrules.MaxRulesPerOwner))
			} else {
				log.Printf("import %d mail rules: %v", len(rules), err)
				data.Error = i18n.T(r.Context(), "rules.import.failed")
			}
			data.Import = view
			h.renderMailRuleDialogPanel(w, r, data)
			return
		}
	}
	data, ok = h.loadMailRulesData(w, r)
	if !ok {
		return
	}
	data.Notice = mailRulesImportNotice(r.Context(), counts)
	// The rules that came in show even when the list's filters would hide
	// them, as a rule just added does.
	if len(rules) > 0 {
		data.FolderFilter, data.TagFilter, data.AccountFilter, data.StatusFilter, data.ActionFilter = "", nil, nil, "", nil
	}
	h.renderMailRulesDone(w, r, data)
}
