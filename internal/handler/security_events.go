package handler

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const securityActivityPath = "/settings/security/activity"

func securityEventViewData(events []auth.SecurityEventSummary) []views.SecurityEventData {
	result := make([]views.SecurityEventData, 0, len(events))
	for _, event := range events {
		view := views.SecurityEventData{
			Title:      securityEventTitle(event.EventType, event.Success),
			Detail:     securityEventDetail(event.Reason, event.Success),
			OccurredAt: formatSecuritySessionTime(event.OccurredAt),
			Status:     "Failed",
			Successful: event.Success,
		}
		if event.Success {
			view.Status = "Completed"
		}
		if strings.TrimSpace(event.UserAgent) != "" {
			view.Client = securitySessionClientLabel(event.UserAgent)
		}
		result = append(result, view)
	}
	return result
}

func securityActivityPageViewData(page *auth.SecurityEventPage) views.SecurityActivityPageData {
	if page == nil {
		return views.SecurityActivityPageData{}
	}
	result := views.SecurityActivityPageData{
		Events:       securityEventViewData(page.Events),
		TotalEvents:  page.TotalEvents,
		Page:         page.Page,
		TotalPages:   page.TotalPages,
		PreviousPage: page.Page - 1,
		NextPage:     page.Page + 1,
		HasPrevious:  page.Page > 1,
		HasNext:      page.Page < page.TotalPages,
	}
	if len(page.Events) > 0 {
		result.FirstEvent = (page.Page-1)*page.PageSize + 1
		result.LastEvent = result.FirstEvent + int64(len(page.Events)) - 1
	}
	return result
}

func parseSecurityActivityPage(r *http.Request) (int64, error) {
	values, present := r.URL.Query()["page"]
	if !present {
		return 1, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, fmt.Errorf("page must be one positive integer")
	}
	page, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || page < 1 {
		return 0, fmt.Errorf("page must be one positive integer")
	}
	return page, nil
}

func setSecurityActivityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
}

func (h *Handler) handleSecurityActivityPage(w http.ResponseWriter, r *http.Request) {
	setSecurityActivityHeaders(w)
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
		return
	}
	requestedPage, err := parseSecurityActivityPage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, err := h.auth.ListSecurityEventPage(r.Context(), auth.GetSessionToken(r), requestedPage)
	if errors.Is(err, auth.ErrRecentStepUpRequired) {
		w.Header().Set("HX-Redirect", "/settings/security?verification_required=1")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, auth.ErrSecuritySessionInvalid) {
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		log.Printf("load security activity page: %v", err)
		http.Error(w, "failed to load security activity", http.StatusInternalServerError)
		return
	}
	var output bytes.Buffer
	if err := views.SecurityActivityDialogPage(securityActivityPageViewData(page)).Render(r.Context(), &output); err != nil {
		log.Printf("render security activity page: %v", err)
		http.Error(w, "failed to render security activity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = output.WriteTo(w)
}

func securityEventTitle(eventType auth.AuthEventType, success bool) string {
	switch eventType {
	case auth.AuthEventPrimaryVerified:
		return "Primary sign-in verified; MFA pending"
	case auth.AuthEventLoginSucceeded:
		if success {
			return "Signed in"
		}
		return "Sign-in attempt failed"
	case auth.AuthEventLoginFailed:
		return "Sign-in attempt failed"
	case auth.AuthEventSessionRevoked:
		if success {
			return "Session signed out"
		}
		return "Session sign-out failed"
	case auth.AuthEventUserDisabled:
		return "Account disabled"
	case auth.AuthEventUserEnabled:
		return "Account enabled"
	case auth.AuthEventUserDeletionStarted:
		return "User deletion started"
	case auth.AuthEventUserDeleted:
		return "User deleted"
	case auth.AuthEventCredentialChanged:
		if success {
			return "Security credential changed"
		}
		return "Credential change failed"
	case auth.AuthEventRecoveryUsed:
		return "Recovery code used"
	case auth.AuthEventEnrollmentIssued:
		return "Enrollment invitation issued"
	case auth.AuthEventEnrollmentRevoked:
		return "Enrollment invitation revoked"
	case auth.AuthEventEnrollmentCompleted:
		if success {
			return "Account enrollment completed"
		}
		return "Account enrollment failed"
	case auth.AuthEventCredentialResetRequested:
		return "Password reset requested"
	case auth.AuthEventCredentialResetCompleted:
		return "Credential reset completed"
	case auth.AuthEventLocalRecoveryStarted:
		return "Local account recovery started"
	case auth.AuthEventSetupTokenIssued:
		return "Setup access issued"
	case auth.AuthEventSetupTokenRotated:
		return "Setup access rotated"
	case auth.AuthEventSetupTokenVerified:
		return "Setup access verified"
	case auth.AuthEventSetupTokenVerificationFailed:
		return "Setup verification failed"
	case auth.AuthEventSetupCompleted:
		return "Initial security setup completed"
	case auth.AuthEventStepUpSucceeded:
		return "Security verification completed"
	case auth.AuthEventStepUpFailed:
		return "Security verification failed"
	case auth.AuthEventSecurityPolicyChanged:
		return "Security policy changed"
	case auth.AuthEventIdentityLinked:
		if success {
			return "Sign-in identity connected"
		}
		return "Identity connection failed"
	case auth.AuthEventIdentityUnlinked:
		if success {
			return "Sign-in identity disconnected"
		}
		return "Identity disconnection failed"
	default:
		if success {
			return "Security activity completed"
		}
		return "Security activity failed"
	}
}

func securityEventDetail(reason auth.AuthEventReason, success bool) string {
	switch reason {
	case auth.AuthEventReasonInvalidCredentials:
		return "The submitted verification was not accepted."
	case auth.AuthEventReasonUserInactive:
		return "The account was not active."
	case auth.AuthEventReasonChallengeExpired:
		return "The security request expired."
	case auth.AuthEventReasonChallengeConsumed:
		return "A one-time security request was completed."
	case auth.AuthEventReasonChallengeVerified:
		return "A verified security request was completed."
	case auth.AuthEventReasonThrottled:
		return "Too many attempts were made."
	case auth.AuthEventReasonPolicyRequired:
		return "Additional security requirements applied."
	case auth.AuthEventReasonAdministratorAction:
		return "Performed by an administrator."
	case auth.AuthEventReasonUserAction:
		return "Requested from this account."
	case auth.AuthEventReasonLocalOperator:
		return "Performed through local recovery tools."
	case auth.AuthEventReasonSystemInitialization:
		return "Performed during initial setup."
	default:
		if success {
			return "Completed successfully."
		}
		return "The attempt did not complete."
	}
}
