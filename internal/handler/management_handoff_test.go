package handler

import (
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var managementInvitationTokenPattern = regexp.MustCompile(`<code[^>]*>([^<]+)</code>`)

func legacyManagementHandoffStack(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, *http.Cookie) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name,
			status, user_type, is_admin, created_at, updated_at
		) VALUES ('legacy-admin', 'legacy', 'legacy', 'Legacy Admin', 'active', 'webmail', 0, ?, ?);
		INSERT INTO accounts (id, user_id, email_address)
		VALUES ('legacy-mailbox', 'legacy-admin', 'mailbox@example.com');
		DROP TRIGGER users_management_type_update;
		UPDATE users SET is_admin = 1 WHERE id = 'legacy-admin';
		CREATE TRIGGER users_management_type_update
		BEFORE UPDATE OF is_admin, user_type ON users
		WHEN NEW.is_admin = 1 AND NEW.user_type != 'management'
		 AND (OLD.is_admin != NEW.is_admin OR OLD.user_type != NEW.user_type)
		BEGIN
			SELECT RAISE(ABORT, 'administrator must be a management user');
		END`, now, now); err != nil {
		t.Fatal(err)
	}
	manager := auth.NewManager(&auth.Config{Enabled: true, BaseURL: "https://gofer.example"}, db)
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), "legacy-admin", "Legacy Browser",
		auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), session.UserID, session.ID, auth.AuthenticationMethodTOTP); err != nil || !steppedUp {
		t.Fatalf("step up legacy administrator = %t, %v", steppedUp, err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), &http.Cookie{Name: "gofer_session", Value: session.Token}
}

func TestManagementHandoffInvitationRouteIsAtomicAndShowsTokenOnce(t *testing.T) {
	manager, db, stack, sessionCookie := legacyManagementHandoffStack(t)
	page := getSecuritySettingsPath(t, stack, "/admin/separate", sessionCookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/admin/separate/invitations"`) ||
		!strings.Contains(page.Body.String(), "Separate administration from webmail") {
		t.Fatalf("management separation page = %d %q", page.Code, page.Body.String())
	}

	withoutCSRF := postSecuritySettings(t, stack, managementHandoffInvitationPath, url.Values{
		"name": {"Management Owner"}, "username": {"management-owner"},
	}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("handoff invitation without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}

	created := postSecuritySettings(t, stack, managementHandoffInvitationPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, managementHandoffInvitationPath)},
		"name":                 {"Management Owner"},
		"username":             {"management-owner"},
	}, sessionCookie)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" ||
		created.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("created handoff invitation = status:%d headers:%v body:%q", created.Code, created.Header(), created.Body.String())
	}
	match := managementInvitationTokenPattern.FindStringSubmatch(created.Body.String())
	if len(match) != 2 || match[1] == "" || strings.Count(created.Body.String(), match[1]) != 1 {
		t.Fatalf("one-time management token rendering = %q", created.Body.String())
	}
	rawToken := match[1]
	var userType string
	var isAdmin, mailboxes, handoffs int
	var storedHash string
	if err := db.Read().QueryRow(`
		SELECT target.user_type, target.is_admin,
		       (SELECT COUNT(*) FROM accounts WHERE user_id = target.id), token.token_hash
		FROM management_handoffs handoff
		JOIN users target ON target.id = handoff.target_user_id
		JOIN user_enrollment_tokens token ON token.user_id = target.id
		WHERE handoff.source_user_id = 'legacy-admin' AND handoff.status = 'pending'`,
	).Scan(&userType, &isAdmin, &mailboxes, &storedHash); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM management_handoffs`).Scan(&handoffs); err != nil {
		t.Fatal(err)
	}
	if userType != string(auth.UserTypeManagement) || isAdmin != 0 || mailboxes != 0 || handoffs != 1 ||
		storedHash == rawToken || len(storedHash) != 64 {
		t.Fatalf("pending handoff state = type:%q admin:%d mailboxes:%d handoffs:%d hash:%q", userType, isAdmin, mailboxes, handoffs, storedHash)
	}

	reloaded := getSecuritySettingsPath(t, stack, "/admin/separate", sessionCookie)
	if reloaded.Code != http.StatusOK || strings.Contains(reloaded.Body.String(), rawToken) ||
		!strings.Contains(reloaded.Body.String(), "Management invitation pending") {
		t.Fatalf("reloaded handoff page = %d %q", reloaded.Code, reloaded.Body.String())
	}
}
