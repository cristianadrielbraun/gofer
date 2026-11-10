package handler

import (
	"context"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const adminSecurityActivityPath = "/admin/activity"
const adminSecurityActivityRetentionPath = "/admin/activity/retention"

func adminSecurityActivityViewData(ctx context.Context, page *auth.AdministratorSecurityEventPage) views.AdminSecurityActivityData {
	data := views.AdminSecurityActivityData{ManagedMode: true}
	if page == nil {
		return data
	}
	data.TotalEvents = page.TotalEvents
	data.Page = page.Page
	data.TotalPages = page.TotalPages
	data.PreviousPage = page.Page - 1
	data.NextPage = page.Page + 1
	data.HasPrevious = page.Page > 1
	data.HasNext = page.Page < page.TotalPages
	data.Events = make([]views.AdminSecurityActivityEventData, 0, len(page.Events))
	for _, event := range page.Events {
		actor := strings.TrimSpace(event.ActorUsername)
		if actor == "" {
			actor = i18n.T(ctx, "security.event.actor.system")
		}
		subject := strings.TrimSpace(event.SubjectUsername)
		if subject == "" {
			subject = i18n.T(ctx, "security.event.subject.instance")
		}
		status := i18n.T(ctx, "security.event.status.failed")
		if event.Success {
			status = i18n.T(ctx, "security.event.status.completed")
		}
		data.Events = append(data.Events, views.AdminSecurityActivityEventData{
			Title:          securityEventTitle(ctx, event.EventType, event.Success),
			Detail:         securityEventDetail(ctx, event.Reason, event.Success),
			OccurredAt:     formatSecuritySessionTime(ctx, event.OccurredAt),
			Client:         securitySessionClientLabel(ctx, event.UserAgent),
			Status:         status,
			Actor:          actor,
			Subject:        subject,
			SubjectDeleted: event.SubjectDeleted,
			Successful:     event.Success,
		})
	}
	if len(page.Events) > 0 {
		data.FirstEvent = (page.Page-1)*page.PageSize + 1
		data.LastEvent = data.FirstEvent + int64(len(page.Events)) - 1
	}
	return data
}

func parseAdminSecurityActivityFilter(r *http.Request) (auth.AdministratorSecurityEventFilter, error) {
	values, present := r.URL.Query()["filter"]
	if !present {
		return auth.AdministratorSecurityEventFilterAll, nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", errors.New("filter must be one supported activity category")
	}
	filter := auth.AdministratorSecurityEventFilter(values[0])
	if !filter.Valid() || filter == auth.AdministratorSecurityEventFilterAll {
		return "", errors.New("filter must be one supported activity category")
	}
	return filter, nil
}

func setAdminSecurityActivityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
}

func (h *Handler) handleAdminSecurityActivity(w http.ResponseWriter, r *http.Request) {
	setAdminSecurityActivityHeaders(w)
	requestedPage, err := parseSecurityActivityPage(r)
	if err != nil {
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
		return
	}
	filter, err := parseAdminSecurityActivityFilter(r)
	if err != nil {
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
		return
	}

	data := views.AdminSecurityActivityData{
		ManagedMode: h.auth != nil && h.auth.IsEnabled(),
		Filter:      string(filter),
	}
	verification := views.AdminSecurityVerificationData{}
	if data.ManagedMode {
		access, accessErr := h.auth.GetSecuritySettingsAccess(r.Context(), auth.GetSessionToken(r))
		if errors.Is(accessErr, auth.ErrSecuritySessionInvalid) {
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if accessErr != nil {
			log.Printf("authorize administrator security activity: %v", accessErr)
			http.Error(w, i18n.T(r.Context(), "server.error.failed_to_authorize_administrator_security_activity"), http.StatusInternalServerError)
			return
		}
		if !access.StepUpFresh {
			data.StepUpRequired = true
			verification = adminSecurityVerificationViewData(
				r.Context(), access, adminSecurityActivityReturnTo(requestedPage, filter),
				r.URL.Query().Get("verification_required") == "1",
			)
		}

		currentUser := auth.GetCurrentUser(r.Context())
		currentSession := auth.GetCurrentSession(r.Context())
		actorUserID := ""
		actorSessionID := ""
		if currentUser != nil {
			actorUserID = currentUser.ID
		}
		if currentSession != nil {
			actorSessionID = currentSession.ID
		}
		if !data.StepUpRequired {
			retention, retentionErr := h.auth.AuthenticationEventRetention(r.Context())
			if retentionErr != nil {
				log.Printf("load authentication event retention: %v", retentionErr)
				http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_administrator_security_activity"), http.StatusInternalServerError)
				return
			}
			page, loadErr := h.auth.ListAdministratorSecurityEventPage(
				r.Context(), actorUserID, actorSessionID, filter, requestedPage,
			)
			switch {
			case loadErr == nil:
				data = adminSecurityActivityViewData(r.Context(), page)
				data.Filter = string(filter)
				data.RetentionDays = retention.Days
				data.RetentionMinimumDays = auth.MinimumAuthenticationEventRetentionDays
				data.RetentionMaximumDays = auth.MaximumAuthenticationEventRetentionDays
				data.RetentionCSRFToken = auth.CSRFToken(
					r.Context(), http.MethodPost, adminSecurityActivityRetentionPath,
				)
			case errors.Is(loadErr, auth.ErrRecentStepUpRequired):
				data.StepUpRequired = true
				data.Filter = string(filter)
				access.StepUpFresh = false
				verification = adminSecurityVerificationViewData(
					r.Context(), access, adminSecurityActivityReturnTo(requestedPage, filter), true,
				)
			case errors.Is(loadErr, auth.ErrAdministratorRequired):
				http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
				return
			default:
				log.Printf("load administrator security activity: %v", loadErr)
				http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_administrator_security_activity"), http.StatusInternalServerError)
				return
			}
		}
	}
	data.RetentionOpen = r.URL.Query().Get("retention") == "1"

	h.renderAdminPage(w, r, http.StatusOK, views.AdminShellPage{
		Section: views.AdminSectionActivity,
		Content: views.AdminSecurityActivityPage(data, verification),
	})
}

func (h *Handler) handleSetAuthenticationEventRetention(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.managed_authentication_is_required"), http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirectAdminSecurityActivity(w, r, "", i18n.T(r.Context(), "auth.admin.form_invalid"), false)
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < auth.MinimumAuthenticationEventRetentionDays ||
		days > auth.MaximumAuthenticationEventRetentionDays {
		h.redirectAdminSecurityActivity(
			w, r, "",
			i18n.T(r.Context(), "auth.admin.retention.invalid", "min", i18n.Number(r.Context(), auth.MinimumAuthenticationEventRetentionDays), "max", i18n.Number(r.Context(), auth.MaximumAuthenticationEventRetentionDays)),
			false,
		)
		return
	}
	currentUser := auth.GetCurrentUser(r.Context())
	currentSession := auth.GetCurrentSession(r.Context())
	if currentUser == nil || currentSession == nil {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	result, err := h.auth.SetAuthenticationEventRetention(
		r.Context(), auth.SetAuthenticationEventRetentionOptions{
			ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID, Days: days,
		},
	)
	switch {
	case err == nil:
		if result.Changed {
			h.wakeAuthenticationEventRetention()
			h.redirectAdminSecurityActivity(
				w, r, i18n.N(r.Context(), "auth.admin.retention.changed", days, "count", i18n.Number(r.Context(), days)), "", false,
			)
			return
		}
		h.redirectAdminSecurityActivity(
			w, r, i18n.N(r.Context(), "auth.admin.retention.unchanged", days, "count", i18n.Number(r.Context(), days)), "", false,
		)
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		h.redirectAdminSecurityActivity(w, r, "", "", true)
	case errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
	case errors.Is(err, auth.ErrAuthenticationEventRetentionInvalid):
		h.redirectAdminSecurityActivity(
			w, r, "",
			i18n.T(r.Context(), "auth.admin.retention.invalid", "min", i18n.Number(r.Context(), auth.MinimumAuthenticationEventRetentionDays), "max", i18n.Number(r.Context(), auth.MaximumAuthenticationEventRetentionDays)),
			false,
		)
	default:
		log.Printf("set authentication event retention: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_update_security_activity_retention"), http.StatusInternalServerError)
	}
}

// redirectAdminSecurityActivity returns to the Activity page, which shows the
// result as a toast. A refused value reopens the settings to correct it.
func (h *Handler) redirectAdminSecurityActivity(
	w http.ResponseWriter,
	r *http.Request,
	notice string,
	message string,
	verificationRequired bool,
) {
	h.setAdminFlashSuccess(w, notice)
	h.setAdminFlashError(w, message)
	values := url.Values{}
	if message != "" {
		values.Set("retention", "1")
	}
	if verificationRequired {
		values.Set("verification_required", "1")
	}
	target := adminSecurityActivityPath
	if query := values.Encode(); query != "" {
		target += "?" + query
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}
