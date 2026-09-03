package handler

import (
	"bytes"
	"errors"
	"fmt"
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

func adminSecurityActivityViewData(page *auth.AdministratorSecurityEventPage) views.AdminSecurityActivityData {
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
			actor = "System"
		}
		subject := strings.TrimSpace(event.SubjectUsername)
		if subject == "" {
			subject = "Instance"
		}
		status := "Failed"
		if event.Success {
			status = "Completed"
		}
		data.Events = append(data.Events, views.AdminSecurityActivityEventData{
			Title:          securityEventTitle(event.EventType, event.Success),
			Detail:         securityEventDetail(event.Reason, event.Success),
			OccurredAt:     formatSecuritySessionTime(event.OccurredAt),
			Client:         securitySessionClientLabel(event.UserAgent),
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter, err := parseAdminSecurityActivityFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
			http.Error(w, "failed to authorize administrator security activity", http.StatusInternalServerError)
			return
		}
		if !access.StepUpFresh {
			data.StepUpRequired = true
			verification = adminSecurityVerificationViewData(
				r.Context(), access, adminSecurityActivityReturnTo(requestedPage, filter),
				r.URL.Query().Get("verification_required") == "1" ||
					strings.TrimSpace(r.URL.Query().Get("error")) != "",
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
				http.Error(w, "failed to load administrator security activity", http.StatusInternalServerError)
				return
			}
			page, loadErr := h.auth.ListAdministratorSecurityEventPage(
				r.Context(), actorUserID, actorSessionID, filter, requestedPage,
			)
			switch {
			case loadErr == nil:
				data = adminSecurityActivityViewData(page)
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
				http.Error(w, "admin access required", http.StatusForbidden)
				return
			default:
				log.Printf("load administrator security activity: %v", loadErr)
				http.Error(w, "failed to load administrator security activity", http.StatusInternalServerError)
				return
			}
		}
	}
	data.Notice = strings.TrimSpace(r.URL.Query().Get("notice"))
	data.Error = strings.TrimSpace(r.URL.Query().Get("error"))

	uiSettings := map[string]string(nil)
	if h.db != nil {
		if currentUser := auth.GetCurrentUser(r.Context()); currentUser != nil {
			uiSettings = h.db.GetUISettings(r.Context(), currentUser.ID)
		}
	}
	var output bytes.Buffer
	if r.Header.Get("HX-Request") == "true" {
		err = views.AdminSecurityActivityPartial(data, verification).Render(r.Context(), &output)
	} else {
		err = views.ManagementAdminActivityLayout(uiSettings, data, verification).Render(r.Context(), &output)
	}
	if err != nil {
		log.Printf("render administrator security activity: %v", err)
		http.Error(w, "failed to render administrator security activity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = output.WriteTo(w)
}

func (h *Handler) handleSetAuthenticationEventRetention(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, "managed authentication is required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectAdminSecurityActivity(w, r, "", "Invalid form data.", false)
		return
	}
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < auth.MinimumAuthenticationEventRetentionDays ||
		days > auth.MaximumAuthenticationEventRetentionDays {
		redirectAdminSecurityActivity(
			w, r, "",
			fmt.Sprintf(
				"Enter a retention period between %d and %d days.",
				auth.MinimumAuthenticationEventRetentionDays,
				auth.MaximumAuthenticationEventRetentionDays,
			),
			false,
		)
		return
	}
	currentUser := auth.GetCurrentUser(r.Context())
	currentSession := auth.GetCurrentSession(r.Context())
	if currentUser == nil || currentSession == nil {
		http.Error(w, "admin access required", http.StatusForbidden)
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
			redirectAdminSecurityActivity(
				w, r, fmt.Sprintf("Security activity will now be retained for %d days.", days), "", false,
			)
			return
		}
		redirectAdminSecurityActivity(
			w, r, fmt.Sprintf("Security activity retention is already %d days.", days), "", false,
		)
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		redirectAdminSecurityActivity(w, r, "", "", true)
	case errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, "admin access required", http.StatusForbidden)
	case errors.Is(err, auth.ErrAuthenticationEventRetentionInvalid):
		redirectAdminSecurityActivity(
			w, r, "",
			fmt.Sprintf(
				"Enter a retention period between %d and %d days.",
				auth.MinimumAuthenticationEventRetentionDays,
				auth.MaximumAuthenticationEventRetentionDays,
			),
			false,
		)
	default:
		log.Printf("set authentication event retention: %v", err)
		http.Error(w, "failed to update security activity retention", http.StatusInternalServerError)
	}
}

func redirectAdminSecurityActivity(
	w http.ResponseWriter,
	r *http.Request,
	notice string,
	message string,
	verificationRequired bool,
) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if message != "" {
		values.Set("error", message)
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
