package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const adminSecurityActivityPath = "/admin/activity"

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

	data := views.AdminSecurityActivityData{ManagedMode: h.auth != nil && h.auth.IsEnabled()}
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
				r.Context(), access, adminSecurityActivityReturnTo(requestedPage),
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
			page, loadErr := h.auth.ListAdministratorSecurityEventPage(
				r.Context(), actorUserID, actorSessionID, requestedPage,
			)
			switch {
			case loadErr == nil:
				data = adminSecurityActivityViewData(page)
			case errors.Is(loadErr, auth.ErrRecentStepUpRequired):
				data.StepUpRequired = true
				access.StepUpFresh = false
				verification = adminSecurityVerificationViewData(
					r.Context(), access, adminSecurityActivityReturnTo(requestedPage), true,
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
