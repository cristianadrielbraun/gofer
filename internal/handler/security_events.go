package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const securityActivityPath = "/settings/security/activity"

func securityEventViewData(ctx context.Context, events []auth.SecurityEventSummary) []views.SecurityEventData {
	result := make([]views.SecurityEventData, 0, len(events))
	for _, event := range events {
		view := views.SecurityEventData{
			Title:      securityEventTitle(ctx, event.EventType, event.Success),
			Detail:     securityEventDetail(ctx, event.Reason, event.Success),
			OccurredAt: formatSecuritySessionTime(ctx, event.OccurredAt),
			Status:     i18n.T(ctx, "security.event.status.failed"),
			Successful: event.Success,
		}
		if event.Success {
			view.Status = i18n.T(ctx, "security.event.status.completed")
		}
		if strings.TrimSpace(event.UserAgent) != "" {
			view.Client = securitySessionClientLabel(ctx, event.UserAgent)
		}
		result = append(result, view)
	}
	return result
}

func securityActivityPageViewData(ctx context.Context, page *auth.SecurityEventPage) views.SecurityActivityPageData {
	if page == nil {
		return views.SecurityActivityPageData{}
	}
	result := views.SecurityActivityPageData{
		Events:       securityEventViewData(ctx, page.Events),
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
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusBadRequest)
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
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_security_activity"), http.StatusInternalServerError)
		return
	}
	var output bytes.Buffer
	if err := views.SecurityActivityDialogPage(securityActivityPageViewData(r.Context(), page)).Render(r.Context(), &output); err != nil {
		log.Printf("render security activity page: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_render_security_activity"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = output.WriteTo(w)
}

func securityEventTitle(ctx context.Context, eventType auth.AuthEventType, success bool) string {
	switch eventType {
	case auth.AuthEventPrimaryVerified:
		return i18n.T(ctx, "security.event.title.primary_verified")
	case auth.AuthEventLoginSucceeded:
		if success {
			return i18n.T(ctx, "security.event.title.signed_in")
		}
		return i18n.T(ctx, "security.event.title.sign_in_failed")
	case auth.AuthEventLoginFailed:
		return i18n.T(ctx, "security.event.title.sign_in_failed")
	case auth.AuthEventSessionRevoked:
		if success {
			return i18n.T(ctx, "security.event.title.signed_out")
		}
		return i18n.T(ctx, "security.event.title.sign_out_failed")
	case auth.AuthEventUserDisabled:
		return i18n.T(ctx, "security.event.title.account_disabled")
	case auth.AuthEventUserEnabled:
		return i18n.T(ctx, "security.event.title.account_enabled")
	case auth.AuthEventUserDeletionStarted:
		return i18n.T(ctx, "security.event.title.deletion_started")
	case auth.AuthEventUserDeleted:
		return i18n.T(ctx, "security.event.title.user_deleted")
	case auth.AuthEventPasswordChangeRequired:
		return i18n.T(ctx, "security.event.title.password_change_required")
	case auth.AuthEventCredentialChanged:
		if success {
			return i18n.T(ctx, "security.event.title.credential_changed")
		}
		return i18n.T(ctx, "security.event.title.credential_change_failed")
	case auth.AuthEventRecoveryUsed:
		return i18n.T(ctx, "security.event.title.recovery_code_used")
	case auth.AuthEventEnrollmentIssued:
		return i18n.T(ctx, "security.event.title.invitation_issued")
	case auth.AuthEventEnrollmentRevoked:
		return i18n.T(ctx, "security.event.title.invitation_revoked")
	case auth.AuthEventEnrollmentCompleted:
		if success {
			return i18n.T(ctx, "security.event.title.enrollment_completed")
		}
		return i18n.T(ctx, "security.event.title.enrollment_failed")
	case auth.AuthEventCredentialResetRequested:
		return i18n.T(ctx, "security.event.title.reset_requested")
	case auth.AuthEventCredentialResetCompleted:
		return i18n.T(ctx, "security.event.title.reset_completed")
	case auth.AuthEventLocalRecoveryStarted:
		return i18n.T(ctx, "security.event.title.local_recovery")
	case auth.AuthEventSetupTokenIssued:
		return i18n.T(ctx, "security.event.title.setup_issued")
	case auth.AuthEventSetupTokenRotated:
		return i18n.T(ctx, "security.event.title.setup_rotated")
	case auth.AuthEventSetupTokenVerified:
		return i18n.T(ctx, "security.event.title.setup_verified")
	case auth.AuthEventSetupTokenVerificationFailed:
		return i18n.T(ctx, "security.event.title.setup_failed")
	case auth.AuthEventSetupCompleted:
		return i18n.T(ctx, "security.event.title.setup_completed")
	case auth.AuthEventStepUpSucceeded:
		return i18n.T(ctx, "security.event.title.step_up_succeeded")
	case auth.AuthEventStepUpFailed:
		return i18n.T(ctx, "security.event.title.step_up_failed")
	case auth.AuthEventSecurityPolicyChanged:
		return i18n.T(ctx, "security.event.title.policy_changed")
	case auth.AuthEventIdentityLinked:
		if success {
			return i18n.T(ctx, "security.event.title.identity_linked")
		}
		return i18n.T(ctx, "security.event.title.identity_link_failed")
	case auth.AuthEventIdentityUnlinked:
		if success {
			return i18n.T(ctx, "security.event.title.identity_unlinked")
		}
		return i18n.T(ctx, "security.event.title.identity_unlink_failed")
	default:
		if success {
			return i18n.T(ctx, "security.event.title.activity_completed")
		}
		return i18n.T(ctx, "security.event.title.activity_failed")
	}
}

func securityEventDetail(ctx context.Context, reason auth.AuthEventReason, success bool) string {
	switch reason {
	case auth.AuthEventReasonInvalidCredentials:
		return i18n.T(ctx, "security.event.detail.invalid_credentials")
	case auth.AuthEventReasonUserInactive:
		return i18n.T(ctx, "security.event.detail.user_inactive")
	case auth.AuthEventReasonChallengeExpired:
		return i18n.T(ctx, "security.event.detail.challenge_expired")
	case auth.AuthEventReasonChallengeConsumed:
		return i18n.T(ctx, "security.event.detail.challenge_consumed")
	case auth.AuthEventReasonChallengeVerified:
		return i18n.T(ctx, "security.event.detail.challenge_verified")
	case auth.AuthEventReasonThrottled:
		return i18n.T(ctx, "security.event.detail.throttled")
	case auth.AuthEventReasonPolicyRequired:
		return i18n.T(ctx, "security.event.detail.policy_required")
	case auth.AuthEventReasonAdministratorAction:
		return i18n.T(ctx, "security.event.detail.administrator_action")
	case auth.AuthEventReasonUserAction:
		return i18n.T(ctx, "security.event.detail.user_action")
	case auth.AuthEventReasonLocalOperator:
		return i18n.T(ctx, "security.event.detail.local_operator")
	case auth.AuthEventReasonSystemInitialization:
		return i18n.T(ctx, "security.event.detail.system_initialization")
	default:
		if success {
			return i18n.T(ctx, "security.event.detail.completed")
		}
		return i18n.T(ctx, "security.event.detail.not_completed")
	}
}
