package handler

import (
	"database/sql"
	"errors"
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
var managementInvitationReissuePathPattern = regexp.MustCompile(`action="(/admin/separate/invitations/([^"/]+)/reissue)"`)
var managementHandoffCancelPathPattern = regexp.MustCompile(`action="(/admin/separate/handoffs/([^"/]+)/cancel)"`)

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
	manager := auth.NewManager(&auth.Config{Enabled: true, BaseURL: "https://gofer.example"}, db, auth.Dependencies{
		BucketHashKey: []byte("0123456789abcdef0123456789abcdef"),
	})
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

func TestManagementHandoffInvitationCanBeReissuedAndCanceledSecurely(t *testing.T) {
	manager, db, stack, sessionCookie := legacyManagementHandoffStack(t)
	created := postSecuritySettings(t, stack, managementHandoffInvitationPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, managementHandoffInvitationPath)},
		"name":                 {"Management Owner"},
		"username":             {"management-owner"},
	}, sessionCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create management handoff = %d %q", created.Code, created.Body.String())
	}
	initialTokenMatch := managementInvitationTokenPattern.FindStringSubmatch(created.Body.String())
	reissueMatch := managementInvitationReissuePathPattern.FindStringSubmatch(created.Body.String())
	cancelMatch := managementHandoffCancelPathPattern.FindStringSubmatch(created.Body.String())
	if len(initialTokenMatch) != 2 || len(reissueMatch) != 3 || len(cancelMatch) != 3 ||
		reissueMatch[2] == "" || reissueMatch[2] != cancelMatch[2] ||
		strings.Contains(created.Body.String(), "management-user") {
		t.Fatalf("management handoff recovery controls = %q", created.Body.String())
	}

	withoutReissueCSRF := postSecuritySettings(t, stack, reissueMatch[1], nil, sessionCookie)
	if withoutReissueCSRF.Code != http.StatusForbidden {
		t.Fatalf("management invitation reissue without CSRF = %d %q", withoutReissueCSRF.Code, withoutReissueCSRF.Body.String())
	}
	reissued := postSecuritySettings(t, stack, reissueMatch[1], url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, reissueMatch[1])},
	}, sessionCookie)
	if reissued.Code != http.StatusCreated || reissued.Header().Get("Cache-Control") != "no-store" ||
		reissued.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("reissued management invitation = status:%d headers:%v body:%q", reissued.Code, reissued.Header(), reissued.Body.String())
	}
	replacementTokenMatch := managementInvitationTokenPattern.FindStringSubmatch(reissued.Body.String())
	newCancelMatch := managementHandoffCancelPathPattern.FindStringSubmatch(reissued.Body.String())
	if len(replacementTokenMatch) != 2 || replacementTokenMatch[1] == initialTokenMatch[1] ||
		strings.Count(reissued.Body.String(), replacementTokenMatch[1]) != 1 || len(newCancelMatch) != 3 ||
		newCancelMatch[2] == cancelMatch[2] {
		t.Fatalf("replacement management invitation response = %q", reissued.Body.String())
	}
	if redeemed, err := manager.RedeemEnrollmentToken(t.Context(), auth.RedeemEnrollmentTokenOptions{
		Token: initialTokenMatch[1], NewPassword: "Correct Horse Battery Staple! 2026",
	}); redeemed != nil || !errors.Is(err, auth.ErrEnrollmentTokenInvalid) {
		t.Fatalf("redeem superseded management invitation = %#v, %v", redeemed, err)
	}

	replayed := postSecuritySettings(t, stack, reissueMatch[1], url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, reissueMatch[1])},
	}, sessionCookie)
	if replayed.Code != http.StatusConflict || !strings.Contains(replayed.Body.String(), "no longer available") {
		t.Fatalf("replayed management invitation action = %d %q", replayed.Code, replayed.Body.String())
	}
	withoutCancelCSRF := postSecuritySettings(t, stack, newCancelMatch[1], nil, sessionCookie)
	if withoutCancelCSRF.Code != http.StatusForbidden {
		t.Fatalf("management handoff cancel without CSRF = %d %q", withoutCancelCSRF.Code, withoutCancelCSRF.Body.String())
	}
	canceled := postSecuritySettings(t, stack, newCancelMatch[1], url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, newCancelMatch[1])},
	}, sessionCookie)
	if canceled.Code != http.StatusSeeOther || !strings.HasPrefix(canceled.Header().Get("Location"), "/admin/separate?notice=") ||
		canceled.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("canceled management handoff = status:%d headers:%v body:%q", canceled.Code, canceled.Header(), canceled.Body.String())
	}

	var sourceAdmin, targetAdmin, mailboxes int
	var targetStatus, handoffStatus string
	var canceledAt sql.NullTime
	if err := db.Read().QueryRow(`SELECT is_admin FROM users WHERE id = 'legacy-admin'`).Scan(&sourceAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`
		SELECT target.status, target.is_admin, handoff.status, handoff.canceled_at
		FROM management_handoffs handoff
		JOIN users target ON target.id = handoff.target_user_id
		WHERE handoff.source_user_id = 'legacy-admin'`).Scan(&targetStatus, &targetAdmin, &handoffStatus, &canceledAt); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM accounts WHERE user_id = 'legacy-admin'`).Scan(&mailboxes); err != nil {
		t.Fatal(err)
	}
	if sourceAdmin != 1 || targetAdmin != 0 || targetStatus != string(auth.UserStatusDisabled) ||
		handoffStatus != "canceled" || !canceledAt.Valid || mailboxes != 1 {
		t.Fatalf("canceled handoff boundaries = source-admin:%d target:%s/%d handoff:%s/%t mailboxes:%d",
			sourceAdmin, targetStatus, targetAdmin, handoffStatus, canceledAt.Valid, mailboxes)
	}
	if redeemed, err := manager.RedeemEnrollmentToken(t.Context(), auth.RedeemEnrollmentTokenOptions{
		Token: replacementTokenMatch[1], NewPassword: "Correct Horse Battery Staple! 2026",
	}); redeemed != nil || !errors.Is(err, auth.ErrEnrollmentTokenInvalid) {
		t.Fatalf("redeem canceled management invitation = %#v, %v", redeemed, err)
	}
	page := getSecuritySettingsPath(t, stack, canceled.Header().Get("Location"), sessionCookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Management handoff canceled") ||
		!strings.Contains(page.Body.String(), "Create management invitation") {
		t.Fatalf("management handoff page after cancellation = %d %q", page.Code, page.Body.String())
	}
}
