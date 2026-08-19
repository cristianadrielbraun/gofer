package handler

import (
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func securityEventViewData(list *auth.SecurityEventList) ([]views.SecurityEventData, bool) {
	if list == nil {
		return nil, false
	}
	result := make([]views.SecurityEventData, 0, len(list.Events))
	for _, event := range list.Events {
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
	return result, list.Truncated
}

func securityEventTitle(eventType auth.AuthEventType, success bool) string {
	switch eventType {
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
