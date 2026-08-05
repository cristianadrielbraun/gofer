package auth

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareUsername(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		display    string
		normalized string
		err        error
	}{
		{name: "lowercase", value: "person", display: "person", normalized: "person"},
		{name: "display case", value: "  Person.Name_2  ", display: "Person.Name_2", normalized: "person.name_2"},
		{name: "minimum", value: "a-1", display: "a-1", normalized: "a-1"},
		{name: "required", value: "  ", err: ErrUsernameRequired},
		{name: "too short", value: "ab", err: ErrUsernameLength},
		{name: "too long", value: strings.Repeat("a", usernameMaximumLength+1), err: ErrUsernameLength},
		{name: "leading punctuation", value: "_person", err: ErrUsernameSyntax},
		{name: "trailing punctuation", value: "person-", err: ErrUsernameSyntax},
		{name: "email syntax", value: "person@example.com", err: ErrUsernameSyntax},
		{name: "space", value: "person name", err: ErrUsernameSyntax},
		{name: "unicode confusable", value: "persоn", err: ErrUsernameSyntax},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			display, normalized, err := PrepareUsername(test.value)
			if !errors.Is(err, test.err) || display != test.display || normalized != test.normalized {
				t.Fatalf("PrepareUsername(%q) = display:%q normalized:%q error:%v", test.value, display, normalized, err)
			}
		})
	}
}

func TestPrepareEmailUsesExistingLookupNormalization(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		display    string
		normalized string
		err        error
	}{
		{name: "normalized lookup", value: "  Person+Mail@Example.COM  ", display: "Person+Mail@Example.COM", normalized: "person+mail@example.com"},
		{name: "quoted local", value: `"person mail"@example.com`, err: ErrEmailInvalid},
		{name: "required", value: "  ", err: ErrEmailRequired},
		{name: "display name", value: "Person <person@example.com>", err: ErrEmailInvalid},
		{name: "missing local", value: "@example.com", err: ErrEmailInvalid},
		{name: "missing domain", value: "person@", err: ErrEmailInvalid},
		{name: "multiple separators", value: "person@@example.com", err: ErrEmailInvalid},
		{name: "oversized local", value: strings.Repeat("a", emailLocalMaximumLength+1) + "@example.com", err: ErrEmailInvalid},
		{name: "oversized address", value: strings.Repeat("a", emailMaximumLength) + "@x", err: ErrEmailInvalid},
		{name: "invalid UTF-8", value: string([]byte{'a', 0xff, '@', 'x'}), err: ErrEmailInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			display, normalized, err := PrepareEmail(test.value)
			if !errors.Is(err, test.err) || display != test.display || normalized != test.normalized {
				t.Fatalf("PrepareEmail(%q) = display:%q normalized:%q error:%v", test.value, display, normalized, err)
			}
		})
	}

	_, first, err := PrepareEmail("Person@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := PrepareEmail(" person@example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("normalized collision values = %q and %q", first, second)
	}
}

func TestPrepareNewPasswordLengthUnicodeAndWhitespacePolicy(t *testing.T) {
	valid := []string{
		"a long passphrase",
		"  leading and trailing spaces  ",
		"correct 🐾 horse battery staple",
		strings.Repeat("界", passwordMaximumLength),
	}
	for _, password := range valid {
		prepared, err := PrepareNewPassword(password, PasswordPolicyContext{})
		if err != nil || prepared != password {
			t.Fatalf("PrepareNewPassword(%q) = %q, %v", password, prepared, err)
		}
	}

	decomposed := "Cafe\u0301 has a long password"
	composed := "Café has a long password"
	prepared, err := PrepareNewPassword(decomposed, PasswordPolicyContext{})
	if err != nil || prepared != composed {
		t.Fatalf("NFC password = %q, %v, want %q", prepared, err, composed)
	}
	if utf8.RuneCountInString(prepared) < passwordMinimumLength {
		t.Fatalf("prepared password length = %d", utf8.RuneCountInString(prepared))
	}

	tests := []struct {
		name     string
		password string
		err      error
	}{
		{name: "too short", password: strings.Repeat("a", passwordMinimumLength-1), err: ErrPasswordTooShort},
		{name: "too many characters", password: strings.Repeat("界", passwordMaximumLength+1), err: ErrPasswordTooLong},
		{name: "too many bytes", password: strings.Repeat("界", passwordMaximumLength) + strings.Repeat("a", passwordMaximumBytes), err: ErrPasswordTooLong},
		{name: "invalid UTF-8", password: string([]byte{'a', 0xff, 'b'}), err: ErrPasswordInvalid},
		{name: "newline control", password: "a sufficiently long\npassword", err: ErrPasswordInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareNewPassword(test.password, PasswordPolicyContext{})
			if prepared != "" || !errors.Is(err, test.err) {
				t.Fatalf("PrepareNewPassword() = %q, %v, want %v", prepared, err, test.err)
			}
		})
	}
}

func TestPrepareNewPasswordRejectsCommonAndContextualValues(t *testing.T) {
	tests := []struct {
		name     string
		password string
		context  PasswordPolicyContext
	}{
		{name: "compromised exact", password: "passwordpassword"},
		{name: "compromised case folded", password: "PASSWORDPASSWORD"},
		{name: "username derivative", password: "cristianpassword", context: PasswordPolicyContext{Username: "Cristian"}},
		{name: "username numeric derivative", password: "longaccountname2042", context: PasswordPolicyContext{Username: "longaccountname"}},
		{name: "email local derivative", password: "securitypassword", context: PasswordPolicyContext{Email: "security@example.com"}},
		{name: "service specific", password: "gofer-authentication"},
		{name: "service derivative", password: "goferpassword123"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareNewPassword(test.password, test.context)
			if prepared != "" || !errors.Is(err, ErrPasswordCommon) {
				t.Fatalf("PrepareNewPassword() = %q, %v, want ErrPasswordCommon", prepared, err)
			}
		})
	}

	const allowed = "a safe gofer sentence with several words"
	prepared, err := PrepareNewPassword(allowed, PasswordPolicyContext{Username: "person", Email: "person@example.com"})
	if err != nil || prepared != allowed {
		t.Fatalf("PrepareNewPassword(non-substring policy) = %q, %v", prepared, err)
	}
}

func TestPasswordHashingUsesNFCWithoutTrimmingOrCaseFolding(t *testing.T) {
	encoded, err := HashPassword("Cafe\u0301 has a long password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	matches, needsRehash, err := VerifyPassword(encoded, "Café has a long password")
	if err != nil || !matches || needsRehash {
		t.Fatalf("VerifyPassword(NFC equivalent) = matches:%t needsRehash:%t error:%v", matches, needsRehash, err)
	}
	matches, _, err = VerifyPassword(encoded, " café has a long password ")
	if err != nil || matches {
		t.Fatalf("VerifyPassword(trimmed/case-folded variant) = matches:%t error:%v", matches, err)
	}
}

func TestEmbeddedPasswordBlocklistIntegrity(t *testing.T) {
	if passwordBlocklist.count != 97746 {
		t.Fatalf("password blocklist entries = %d, want 97746", passwordBlocklist.count)
	}
	if !commonPasswordBlocklistContains("passwordpassword") || !commonPasswordBlocklistContains("PASSWORDPASSWORD") {
		t.Fatal("embedded password blocklist does not contain a known compromised value")
	}
	if commonPasswordBlocklistContains("this sentence should not be in a compromised password list") {
		t.Fatal("embedded password blocklist matched an unrelated passphrase")
	}
}
