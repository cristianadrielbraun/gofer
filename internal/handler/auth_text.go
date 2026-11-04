package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// authText shows a message from the auth package in the page's language.
// The auth package writes its validation messages and reasons in English,
// and its tests and callers match them as written, so they are translated
// here, where a page shows them. A message this doesn't know is shown as it
// came.
func authText(ctx context.Context, message string) string {
	switch message {
	case "That username is already used by another Gofer user.":
		return i18n.T(ctx, "auth.field.username_taken")
	case "The password confirmation does not match.":
		return i18n.T(ctx, "auth.field.confirmation_mismatch")
	case "Setup must create a separate management owner.":
		return i18n.T(ctx, "auth.field.separate_owner")
	case "Confirm that you saved the recovery codes before continuing.":
		return i18n.T(ctx, "auth.recovery.confirm_saved")
	case "That recovery-code batch is no longer current. Review the latest batch before continuing.":
		return i18n.T(ctx, "auth.field.recovery_batch_stale")
	case "That authenticator code is invalid or has already been used.":
		return i18n.T(ctx, "auth.mfa.code_invalid_used")
	case "Your administrator requires two-factor authentication, so this authenticator app can't be removed. You can replace it with another one.":
		return i18n.T(ctx, "auth.reason.totp_required")
	case "Add another sign-in method before disabling this authenticator.":
		return i18n.T(ctx, "auth.reason.totp_last")
	case "Keep at least one sign-in method available. Add another sign-in method before removing this passkey.":
		return i18n.T(ctx, "auth.reason.passkey_last")
	case "This sign-in provider cannot be disconnected here yet.":
		return i18n.T(ctx, "auth.reason.identity_provider")
	case "Add another usable sign-in method or required authenticator before disconnecting this identity.":
		return i18n.T(ctx, "auth.reason.identity_last")
	// The identity and password rules, from auth's Err… values.
	case "display name must contain 1 to 100 characters":
		return i18n.T(ctx, "auth.field.name_length")
	case "display name contains invalid characters":
		return i18n.T(ctx, "auth.field.name_invalid")
	case "username is required":
		return i18n.T(ctx, "auth.field.username_required")
	case "username must contain 3 to 32 characters":
		return i18n.T(ctx, "auth.field.username_length")
	case "username may contain ASCII letters, numbers, periods, underscores, and hyphens, and must start and end with a letter or number":
		return i18n.T(ctx, "auth.field.username_syntax")
	case "password contains invalid characters":
		return i18n.T(ctx, "auth.field.password_invalid")
	case "password must contain at least 15 characters":
		return i18n.T(ctx, "auth.field.password_short")
	case "password must contain no more than 256 characters":
		return i18n.T(ctx, "auth.field.password_long")
	case "password is commonly used, compromised, or too closely related to the account":
		return i18n.T(ctx, "auth.field.password_common")
	case "current password is incorrect":
		return i18n.T(ctx, "auth.field.current_password")
	case "new password must be different from the current password":
		return i18n.T(ctx, "auth.field.password_unchanged")
	}
	return message
}

// authFields is authText for a form's messages by field.
func authFields(ctx context.Context, fields map[string]string) map[string]string {
	if len(fields) == 0 {
		return fields
	}
	translated := make(map[string]string, len(fields))
	for field, message := range fields {
		translated[field] = authText(ctx, message)
	}
	return translated
}
