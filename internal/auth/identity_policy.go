package auth

import (
	"errors"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	ErrUsernameRequired = errors.New("username is required")
	ErrUsernameLength   = errors.New("username must contain 3 to 32 characters")
	ErrUsernameSyntax   = errors.New("username may contain ASCII letters, numbers, periods, underscores, and hyphens, and must start and end with a letter or number")
	ErrEmailRequired    = errors.New("email is required")
	ErrEmailInvalid     = errors.New("email address is invalid")
	ErrPasswordInvalid  = errors.New("password contains invalid characters")
	ErrPasswordTooShort = errors.New("password must contain at least 15 characters")
	ErrPasswordTooLong  = errors.New("password must contain no more than 256 characters")
	ErrPasswordCommon   = errors.New("password is commonly used, compromised, or too closely related to the account")
)

const (
	usernameMinimumLength   = 3
	usernameMaximumLength   = 32
	emailMaximumLength      = 254
	emailLocalMaximumLength = 64
	passwordMinimumLength   = 15
	passwordMaximumLength   = 256
	passwordMaximumBytes    = passwordMaximumLength * utf8.UTFMax
)

type PasswordPolicyContext struct {
	Username string
	Email    string
}

// PrepareUsername preserves the user's trimmed display spelling while
// returning a lowercase ASCII value for lookup and uniqueness enforcement.
func PrepareUsername(value string) (display, normalized string, err error) {
	display = strings.TrimSpace(value)
	if display == "" {
		return "", "", ErrUsernameRequired
	}
	if len(display) < usernameMinimumLength || len(display) > usernameMaximumLength {
		return "", "", ErrUsernameLength
	}

	normalized = strings.ToLower(display)
	for index := 0; index < len(normalized); index++ {
		character := normalized[index]
		if isASCIIAlphanumeric(character) {
			continue
		}
		if character != '.' && character != '_' && character != '-' {
			return "", "", ErrUsernameSyntax
		}
	}
	if !isASCIIAlphanumeric(normalized[0]) || !isASCIIAlphanumeric(normalized[len(normalized)-1]) {
		return "", "", ErrUsernameSyntax
	}
	return display, normalized, nil
}

// PrepareEmail validates a mailbox address without accepting display-name
// syntax. Its normalization intentionally matches the existing user lookup
// and schema migration behavior: trim surrounding space and lowercase.
func PrepareEmail(value string) (display, normalized string, err error) {
	if !utf8.ValidString(value) {
		return "", "", ErrEmailInvalid
	}
	display = strings.TrimSpace(value)
	if display == "" {
		return "", "", ErrEmailRequired
	}
	if len(display) > emailMaximumLength || strings.Count(display, "@") != 1 {
		return "", "", ErrEmailInvalid
	}
	separator := strings.LastIndexByte(display, '@')
	if separator <= 0 || separator == len(display)-1 || separator > emailLocalMaximumLength {
		return "", "", ErrEmailInvalid
	}
	address, parseErr := mail.ParseAddress(display)
	if parseErr != nil || address.Name != "" || address.Address != display {
		return "", "", ErrEmailInvalid
	}
	return display, normalizeLoginIdentifier(display), nil
}

// PrepareNewPassword applies the establishment/change policy and returns the
// exact NFC-normalized value that must be passed to HashPassword. It does not
// trim, case-fold, or silently truncate the password.
func PrepareNewPassword(value string, context PasswordPolicyContext) (string, error) {
	prepared, err := normalizePasswordForHashing(value)
	if err != nil {
		return "", err
	}
	if utf8.RuneCountInString(prepared) < passwordMinimumLength {
		return "", ErrPasswordTooShort
	}
	if commonPasswordBlocklistContains(prepared) || isContextSpecificPassword(prepared, context) {
		return "", ErrPasswordCommon
	}
	return prepared, nil
}

func normalizePasswordForHashing(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrPasswordInvalid
	}
	if len(value) > passwordMaximumBytes {
		return "", ErrPasswordTooLong
	}
	prepared := norm.NFC.String(value)
	if len(prepared) > passwordMaximumBytes || utf8.RuneCountInString(prepared) > passwordMaximumLength {
		return "", ErrPasswordTooLong
	}
	for _, character := range prepared {
		if unicode.IsControl(character) {
			return "", ErrPasswordInvalid
		}
	}
	return prepared, nil
}

func isContextSpecificPassword(password string, context PasswordPolicyContext) bool {
	canonical := canonicalPasswordBlocklistValue(password)
	bases := []string{
		"gofer",
		"gofermail",
		"gofer-authentication",
		context.Username,
		context.Email,
	}
	if separator := strings.LastIndexByte(context.Email, '@'); separator > 0 {
		bases = append(bases, context.Email[:separator])
	}
	for _, base := range bases {
		base = canonicalPasswordBlocklistValue(strings.TrimSpace(base))
		if base == "" {
			continue
		}
		if isSimpleContextDerivative(canonical, base) || isSimpleContextDerivative(canonical, "password"+base) {
			return true
		}
	}
	return false
}

func isSimpleContextDerivative(password, base string) bool {
	if password == base {
		return true
	}
	suffix, found := strings.CutPrefix(password, base)
	if !found {
		return false
	}
	switch suffix {
	case "!", "!1", "password", "password1", "password123":
		return true
	}
	if len(suffix) == 0 || len(suffix) > 4 {
		return false
	}
	for index := 0; index < len(suffix); index++ {
		if suffix[index] < '0' || suffix[index] > '9' {
			return false
		}
	}
	return true
}

func canonicalPasswordBlocklistValue(value string) string {
	return cases.Fold().String(norm.NFC.String(value))
}

func isASCIIAlphanumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}
