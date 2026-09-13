package handler

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestSecuritySessionViewDataUsesBoundedHumanReadableMetadata(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 30, 0, 0, time.Local)
	revokedAt := now.Add(15 * time.Minute)
	list := &auth.SecuritySessionList{
		Truncated: true,
		Sessions: []auth.SecuritySessionSummary{
			{
				ID: "current-internal-id", Current: true, Active: true,
				AuthenticationMethod: auth.AuthenticationMethodPassword,
				AssuranceLevel:       auth.AssuranceLevelMultiFactor,
				UserAgent:            "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/140.0 Safari/537.36",
				AuthenticatedAt:      now, LastUsedAt: now.Add(5 * time.Minute),
			},
			{
				ID: "active-internal-id", ActionReference: "opaque_action_reference", Active: true,
				AuthenticationMethod: auth.AuthenticationMethodPasskey,
				AssuranceLevel:       auth.AssuranceLevelPhishingResistant,
				UserAgent:            "Mozilla/5.0 (Macintosh) Version/19.0 Safari/605.1.15",
				AuthenticatedAt:      now.Add(-time.Hour), LastUsedAt: now.Add(-time.Minute),
			},
			{
				ID: "signed-out-internal-id", Active: false,
				AuthenticationMethod: auth.AuthenticationMethodFederatedOIDC,
				AssuranceLevel:       auth.AssuranceLevelSingleFactor,
				UserAgent:            "  <unrecognized\x00client>  ",
				AuthenticatedAt:      now.Add(-time.Hour), LastUsedAt: now.Add(-time.Minute), RevokedAt: &revokedAt,
			},
		},
	}
	views, truncated := securitySessionViewData(list, "Company Login")
	if !truncated || len(views) != 3 {
		t.Fatalf("securitySessionViewData() = %#v, %t", views, truncated)
	}
	if views[0].Client != "Chrome on Linux" || views[0].Authentication != "Password" ||
		views[0].Assurance != "Multi-factor" || !views[0].Current || !views[0].Active ||
		views[0].SignedInAt != "Aug 19, 2026 at 12:30 PM" || views[0].EndedAt != "" {
		t.Fatalf("current session view = %#v", views[0])
	}
	if views[1].Client != "Safari on macOS" || views[1].Authentication != "Passkey" ||
		views[1].Assurance != "Phishing-resistant" || !views[1].Active || views[1].Current ||
		views[1].RevokePath != "/settings/security/sessions/opaque_action_reference/revoke" {
		t.Fatalf("active session view = %#v", views[1])
	}
	if views[2].Client != "<unrecognized client>" || views[2].Authentication != "Company Login" ||
		views[2].Assurance != "Single factor" || views[2].Current || views[2].Active ||
		views[2].EndedAt != "Aug 19, 2026 at 12:45 PM" || views[2].RevokePath != "" {
		t.Fatalf("signed-out session view = %#v", views[2])
	}
	for _, view := range views {
		if strings.Contains(view.Client, "internal-id") {
			t.Fatalf("session view exposed internal identifier: %#v", view)
		}
	}
}

func TestSecuritySessionClientLabelRecognizesCommonBrowsersAndBoundsFallback(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{name: "edge windows", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/140.0 Safari/537.36 Edg/140.0", want: "Microsoft Edge on Windows"},
		{name: "firefox mac", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15) Gecko/20100101 Firefox/141.0", want: "Firefox on macOS"},
		{name: "safari iphone", userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) Version/19.0 Mobile/15E148 Safari/604.1", want: "Safari on iPhone"},
		{name: "unknown", userAgent: "", want: "Unknown browser or device"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := securitySessionClientLabel(test.userAgent); got != test.want {
				t.Fatalf("securitySessionClientLabel() = %q, want %q", got, test.want)
			}
		})
	}
	if got := securitySessionClientLabel(strings.Repeat("界", 120)); len([]rune(got)) != 96 || !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded fallback = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestSecuritySessionHistoryBoundsPagesAndRequiresFreshVerification(t *testing.T) {
	manager, db, stack, cookie, _ := completedSecuritySettingsStack(t)
	current, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || current == nil {
		t.Fatalf("current session: %v", err)
	}
	for i := 0; i < 11; i++ {
		_, err := manager.CreateAuthenticatedSession(t.Context(), current.UserID, fmt.Sprintf("History browser %02d", i), auth.AuthenticationMethodPassword, current.AssuranceLevel)
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO users (id, username, username_normalized, name, status, auth_version, created_at, updated_at) VALUES ('foreign-history-user', 'foreign-history', 'foreign-history', 'Foreign', 'active', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	foreign, err := manager.CreateAuthenticatedSession(t.Context(), "foreign-history-user", "Foreign private browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.ListSecuritySessionPage(t.Context(), cookie.Value, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.ListSecuritySessionPage(t.Context(), cookie.Value, 2)
	if err != nil {
		t.Fatal(err)
	}
	last, err := manager.ListSecuritySessionPage(t.Context(), cookie.Value, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 5 || !first.Sessions[0].Current || !first.Truncated || len(second.Sessions) != 5 || last.Page != last.TotalPages || len(last.Sessions) > 5 || last.Truncated {
		t.Fatalf("unexpected pages: %#v %#v %#v", first, second, last)
	}
	for _, list := range []*auth.SecuritySessionList{first, second, last} {
		for _, session := range list.Sessions {
			if session.ID == foreign.ID {
				t.Fatal("foreign session disclosed")
			}
		}
	}
	for _, a := range first.Sessions {
		for _, b := range second.Sessions {
			if a.ID == b.ID {
				t.Fatal("overlapping pages")
			}
		}
	}
	request := func(path string, hx bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(cookie)
		if hx {
			r.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		stack.ServeHTTP(w, r)
		return w
	}
	path := "/settings/security/sessions/history"
	page := request(path+"?page=2", true)
	if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" || !strings.Contains(page.Body.String(), "history-session-revoke-0") {
		t.Fatalf("history: %d %s", page.Code, page.Body.String())
	}
	for _, session := range second.Sessions {
		if strings.Contains(page.Body.String(), session.ID) || strings.Contains(page.Body.String(), cookie.Value) {
			t.Fatal("history disclosed session secrets")
		}
		if !strings.Contains(page.Body.String(), session.ActionReference) {
			t.Fatal("missing revocation action")
		}
	}
	settings := request("/admin/account/security", false)
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), "Show older") || strings.Count(settings.Body.String(), "<dd>Signed in ") != 5 {
		t.Fatalf("main sessions not bounded: %d", settings.Code)
	}
	if got := request(path, false); got.Code != http.StatusSeeOther {
		t.Fatalf("direct request: %d", got.Code)
	}
	if got := request(path+"?page=0", true); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid page: %d", got.Code)
	}
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), current.ID); err != nil {
		t.Fatal(err)
	}
	if got := request(path+"?page=2", true); got.Code != http.StatusNoContent || got.Header().Get("HX-Redirect") != "/settings/security?verification_required=1" || got.Body.Len() != 0 {
		t.Fatalf("stale verification: %d %s", got.Code, got.Body.String())
	}
}
