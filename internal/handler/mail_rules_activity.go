package handler

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// mailRuleActivityPage is how many entries the activity shows at first, and
// how many more each "Show more" adds.
const mailRuleActivityPage = 50

// loadMailRuleActivity gathers the rules' activity as the request filters
// it, with times in the owner's time zone.
func (h *Handler) loadMailRuleActivity(r *http.Request) (models.MailRuleActivityData, error) {
	ctx, userID := r.Context(), h.userID(r.Context())
	query := r.URL.Query()
	data := models.MailRuleActivityData{
		RuleID:     strings.TrimSpace(query.Get("rule")),
		FailedOnly: query.Get("failed") != "",
		Limit:      mailRuleActivityPage,
	}
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit > 0 {
		data.Limit = limit
	}
	rules, err := h.db.ListMailRules(ctx, userID)
	if err != nil {
		return data, err
	}
	data.Rules = rules
	data.Runs, data.More, err = h.db.ListMailRuleActivity(ctx, userID, storage.MailRuleActivityFilter{
		RuleID: data.RuleID, FailedOnly: data.FailedOnly, Limit: data.Limit,
	})
	if err != nil {
		return data, err
	}
	location := time.Local
	if zone := h.db.GetUISettings(ctx, userID)["timezone"]; zone != "" && zone != "local" {
		if loaded, err := time.LoadLocation(zone); err == nil {
			location = loaded
		}
	}
	for i := range data.Runs {
		data.Runs[i].CreatedAt = data.Runs[i].CreatedAt.In(location)
	}
	return data, nil
}

func (h *Handler) renderMailRuleActivity(w http.ResponseWriter, r *http.Request, dialog bool) {
	data, err := h.loadMailRuleActivity(r)
	if err != nil {
		log.Printf("mail rule activity of %s: %v", h.userID(r.Context()), err)
		http.Error(w, "failed to load the activity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	if dialog {
		views.MailRuleActivityDialog(data, time.Now()).Render(r.Context(), w)
		return
	}
	views.MailRuleActivityList(data, time.Now()).Render(r.Context(), w)
}

// handleMailRuleActivity opens the rules' activity.
func (h *Handler) handleMailRuleActivity(w http.ResponseWriter, r *http.Request) {
	h.renderMailRuleActivity(w, r, true)
}

// handleMailRuleActivityList shows the activity again, filtered, or with
// more entries.
func (h *Handler) handleMailRuleActivityList(w http.ResponseWriter, r *http.Request) {
	h.renderMailRuleActivity(w, r, false)
}

// handleClearMailRuleActivity forgets what the rules did.
func (h *Handler) handleClearMailRuleActivity(w http.ResponseWriter, r *http.Request) {
	if err := h.db.ClearMailRuleRunsGuarded(r.Context(), h.userID(r.Context()), h.signatureGuard); err != nil {
		log.Printf("clear mail rule activity of %s: %v", h.userID(r.Context()), err)
		http.Error(w, "failed to clear the activity", http.StatusInternalServerError)
		return
	}
	h.renderMailRuleActivity(w, r, false)
}
