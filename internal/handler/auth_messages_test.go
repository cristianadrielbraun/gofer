package handler

import (
	"context"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// The English the sign-in pages show, which the tests look for.
const (
	loginFailureMessage                    = "Unable to sign in with those credentials."
	loginMFAFailureMessage                 = "That authenticator code is invalid or has already been used."
	securityTOTPFailureMessage             = loginMFAFailureMessage
	invitationEnrollmentFailureMessage     = "That invitation token is invalid or no longer active."
	credentialRedemptionFailureMessage     = "That reset token is invalid or no longer active."
	enrollmentRedemptionMFARequiredMessage = "This account must set up an authenticator app before it can be activated. Contact an administrator to finish two-factor authentication setup."
	recoveryCodeFailureMessage             = "That recovery code is invalid or has already been used."
	setupTokenFailureMessage               = "That setup token is invalid or no longer active."
)

func TestAuthTextTranslatesKnownAuthMessages(t *testing.T) {
	spanish := i18n.WithLocale(context.Background(), "es")
	english := context.Background()
	for _, message := range []string{
		"That username is already used by another Gofer user.",
		"The password confirmation does not match.",
		"Setup must create a separate management owner.",
		"Confirm that you saved the recovery codes before continuing.",
		"That recovery-code batch is no longer current. Review the latest batch before continuing.",
		loginMFAFailureMessage,
		"Your administrator requires two-factor authentication, so this authenticator app can't be removed. You can replace it with another one.",
		"Add another sign-in method before disabling this authenticator.",
		"Keep at least one sign-in method available. Add another sign-in method before removing this passkey.",
		"This sign-in provider cannot be disconnected here yet.",
		"Add another usable sign-in method or required authenticator before disconnecting this identity.",
		"display name must contain 1 to 100 characters",
		"display name contains invalid characters",
		"username is required",
		"username must contain 3 to 32 characters",
		"username may contain ASCII letters, numbers, periods, underscores, and hyphens, and must start and end with a letter or number",
		"password contains invalid characters",
		"password must contain at least 15 characters",
		"password must contain no more than 256 characters",
		"password is commonly used, compromised, or too closely related to the account",
		"current password is incorrect",
		"new password must be different from the current password",
	} {
		if got := authText(english, message); got != message {
			t.Errorf("English changed %q to %q", message, got)
		}
		if got := authText(spanish, message); got == message || got == "" {
			t.Errorf("no Spanish for %q", message)
		}
	}
	if got := authText(spanish, "something new from a server"); got != "something new from a server" {
		t.Fatalf("an unknown message changed: %q", got)
	}
}
