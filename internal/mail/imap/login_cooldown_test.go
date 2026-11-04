package imap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestLoginCooldownsDoubleUntilCapAndClearOnSuccess(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	cooldowns := &loginCooldowns{now: func() time.Time { return now }, entries: make(map[string]loginCooldown)}
	cfg := &models.AccountConfig{IMAPHost: "imap.example.com", IMAPPort: 993, Username: "someone@example.com"}
	limit := &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeLimit, Text: "Rate limit hit."}

	if err := cooldowns.check(cfg); err != nil {
		t.Fatalf("check() before any login = %v, want nil", err)
	}
	other := &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeAuthenticationFailed, Text: "Wrong password."}
	if err := cooldowns.record(cfg, other); err != other || cooldowns.check(cfg) != nil {
		t.Fatalf("record(wrong password) = %v, check = %v; want the error unchanged and no cooldown", err, cooldowns.check(cfg))
	}

	for _, wait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		err := cooldowns.record(cfg, limit)
		var cooldown *LoginCooldownError
		if !errors.As(err, &cooldown) || !errors.Is(err, ErrLoginRateLimited) || !strings.Contains(err.Error(), "[LIMIT]") {
			t.Fatalf("record([LIMIT]) = %v, want a cooldown error keeping the server's words", err)
		}
		if got := cooldown.Until.Sub(now); got != wait {
			t.Fatalf("cooldown = %s, want %s", got, wait)
		}
		// Another account name or case of the same login shares the limit.
		same := &models.AccountConfig{IMAPHost: "IMAP.example.com", IMAPPort: 993, Username: "Someone@example.com"}
		if !errors.Is(cooldowns.check(same), ErrLoginRateLimited) {
			t.Fatalf("check() during cooldown = nil, want refusal")
		}
		now = cooldown.Until
		if err := cooldowns.check(cfg); err != nil {
			t.Fatalf("check() once the cooldown ended = %v, want nil", err)
		}
	}

	if err := cooldowns.record(cfg, nil); err != nil {
		t.Fatalf("record(success) = %v", err)
	}
	err := cooldowns.record(cfg, limit)
	var cooldown *LoginCooldownError
	if !errors.As(err, &cooldown) || cooldown.Until.Sub(now) != 5*time.Minute {
		t.Fatalf("after a successful login the next limit = %v, want the first 5 minute wait", err)
	}
}

func TestClientStopsLoggingInAfterLoginLimit(t *testing.T) {
	cfg, _, recorder := startInboxServerWith(t, 0, func(tag, command string) (string, bool) {
		if strings.HasPrefix(strings.ToUpper(command), "AUTHENTICATE") {
			return tag + " NO [LIMIT] AUTHENTICATE Rate limit hit.\r\n", true
		}
		return "", false
	})
	t.Cleanup(func() {
		defaultLoginCooldowns.mu.Lock()
		delete(defaultLoginCooldowns.entries, loginCooldownKey(cfg))
		defaultLoginCooldowns.mu.Unlock()
	})

	_, err := NewClient(context.Background(), cfg, "secret")
	if !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("first NewClient() error = %v, want a login limit", err)
	}
	_, err = NewClient(context.Background(), cfg, "secret")
	if !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("second NewClient() error = %v, want a login limit", err)
	}
	if logins := strings.Count(strings.ToUpper(recorder.String()), "AUTHENTICATE"); logins != 1 {
		t.Fatalf("client tried to log in %d times, want 1; client sent:\n%s", logins, recorder.String())
	}
}
