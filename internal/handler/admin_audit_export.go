package handler

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// handleExportAdminAudit downloads the administration audit log, as the
// tab's filter keeps it, in CSV or JSON Lines. The export is recorded in
// the log first. A session that must verify again goes back to the tab,
// which asks for it.
func (h *Handler) handleExportAdminAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.auth == nil || !h.auth.IsEnabled() {
		http.NotFound(w, r)
		return
	}
	format := audit.Format(r.URL.Query().Get("format"))
	contentType := "text/csv; charset=utf-8"
	switch format {
	case audit.FormatCSV:
	case audit.FormatJSONL:
		contentType = "application/x-ndjson; charset=utf-8"
	default:
		http.Error(w, i18n.T(ctx, "auth.admin.form_invalid"), http.StatusBadRequest)
		return
	}
	kind, _ := adminAuditActivityQuery(r.URL.Query())
	filter := audit.Filter{}
	if kind != "" {
		filter.Action = kind + "."
	}
	user, session := auth.GetCurrentUser(ctx), auth.GetCurrentSession(ctx)
	if user == nil || session == nil {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	// Headers go out with the first byte the export writes, after the
	// permission check and its record, so a refusal can still redirect.
	written := false
	out := &lazyHeaderWriter{w: w, before: func() {
		written = true
		setAdminPageHeaders(w)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", `attachment; filename="gofer-administration-`+time.Now().UTC().Format("2006-01-02")+`.`+string(format)+`"`)
	}}
	count, err := h.auth.ExportAdministrationEvents(ctx, user.ID, session.ID, filter, format, out)
	switch {
	case err == nil:
		if !written {
			out.before()
			w.WriteHeader(http.StatusOK)
		}
		log.Printf("audit export: %d administration events as %s", count, format)
	case written:
		// The download already started; all that's left is to stop it.
		log.Printf("audit export: interrupted after %d events: %v", count, err)
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		http.Redirect(w, r, adminAuditActivityReturnTo(kind, 0)+adminAuditQuerySeparator(kind)+"verification_required=1", http.StatusSeeOther)
	case errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
	default:
		log.Printf("audit export: %v", err)
		h.setAdminFlashError(w, i18n.T(ctx, "admin.audit.export_failed"))
		http.Redirect(w, r, adminAuditActivityReturnTo(kind, 0), http.StatusSeeOther)
	}
}

func adminAuditQuerySeparator(kind string) string {
	if kind == "" {
		return "?"
	}
	return "&"
}

// lazyHeaderWriter runs before once, just before the first write.
type lazyHeaderWriter struct {
	w      http.ResponseWriter
	before func()
	ready  bool
}

func (l *lazyHeaderWriter) Write(data []byte) (int, error) {
	if !l.ready {
		l.ready = true
		l.before()
		l.w.WriteHeader(http.StatusOK)
	}
	return l.w.Write(data)
}

// handleSetAdminAuditRetention keeps administration events for the chosen
// number of days.
func (h *Handler) handleSetAdminAuditRetention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(ctx, "server.error.managed_authentication_is_required"), http.StatusForbidden)
		return
	}
	retry := views.AdminAuditActivityPath + "?retention=1"
	if err := r.ParseForm(); err != nil {
		h.setAdminFlashError(w, i18n.T(ctx, "auth.admin.form_invalid"))
		http.Redirect(w, r, retry, http.StatusSeeOther)
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < auth.MinimumAdminAuditRetentionDays || days > auth.MaximumAdminAuditRetentionDays {
		h.setAdminFlashError(w, i18n.T(ctx, "auth.admin.retention.invalid", "min", i18n.Number(ctx, auth.MinimumAdminAuditRetentionDays), "max", i18n.Number(ctx, auth.MaximumAdminAuditRetentionDays)))
		http.Redirect(w, r, retry, http.StatusSeeOther)
		return
	}
	user, session := auth.GetCurrentUser(ctx), auth.GetCurrentSession(ctx)
	if user == nil || session == nil {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	changed, err := h.auth.SetAdminAuditRetention(ctx, user.ID, session.ID, days)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		http.Redirect(w, r, views.AdminAuditActivityPath+"?verification_required=1", http.StatusSeeOther)
		return
	case errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	default:
		log.Printf("set administration event retention: %v", err)
		h.setAdminFlashError(w, i18n.T(ctx, "admin.audit.retention.failed"))
		http.Redirect(w, r, retry, http.StatusSeeOther)
		return
	}
	if changed {
		h.wakeAuthenticationEventRetention()
		h.setAdminFlashSuccess(w, i18n.N(ctx, "admin.audit.retention.saved", days, "count", i18n.Number(ctx, days)))
	} else {
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.audit.retention.unchanged"))
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, views.AdminAuditActivityPath, http.StatusSeeOther)
}
