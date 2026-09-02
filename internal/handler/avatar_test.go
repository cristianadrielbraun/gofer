package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestResolveAvatarWithRetryRetriesOnce(t *testing.T) {
	attempts := 0
	image, found, err := resolveAvatarWithRetry(context.Background(), func(context.Context) (avatarresolver.Image, bool, error) {
		attempts++
		if attempts == 1 {
			return avatarresolver.Image{}, false, errors.New("temporary failure")
		}
		return avatarresolver.Image{Source: "gravatar"}, true, nil
	})
	if err != nil {
		t.Fatalf("resolveAvatarWithRetry() error = %v", err)
	}
	if !found || image.Source != "gravatar" {
		t.Fatalf("resolveAvatarWithRetry() = (%+v, %v), want found gravatar", image, found)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestResolveAvatarWithRetryStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, _, err := resolveAvatarWithRetry(ctx, func(context.Context) (avatarresolver.Image, bool, error) {
		attempts++
		cancel()
		return avatarresolver.Image{}, false, errors.New("temporary failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveAvatarWithRetry() error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestAvatarProviderSpecsCheckFallbackOrder(t *testing.T) {
	providers := avatarProviderSpecs()
	if len(providers) < 4 {
		t.Fatalf("provider count = %d, want at least 4", len(providers))
	}
	want := []string{"gravatar", "libravatar", "bimi", "domain_icon"}
	for i, name := range want {
		if providers[i].name != name {
			t.Fatalf("provider[%d] = %q, want %q", i, providers[i].name, name)
		}
	}
}

func TestAvatarProviderSpecsThrottleSharedProvidersOnly(t *testing.T) {
	providers := avatarProviderSpecs()
	throttled := map[string]bool{}
	for _, provider := range providers {
		throttled[provider.name] = provider.throttled
	}
	if !throttled["gravatar"] || !throttled["libravatar"] {
		t.Fatalf("throttled providers = %+v, want gravatar and libravatar throttled", throttled)
	}
	if throttled["bimi"] || throttled["domain_icon"] {
		t.Fatalf("throttled providers = %+v, want domain providers unthrottled", throttled)
	}
}

func TestAllowedProviderAvatarHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"lh3.googleusercontent.com", true},
		{"googleusercontent.com", true},
		{"example.googleusercontent.com", true},
		{"lh3.ggpht.com", true},
		{"ggpht.com", true},
		{"evilgoogleusercontent.com", false},
		{"googleusercontent.com.evil.example", false},
		{"example.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isAllowedProviderAvatarHost(tt.host); got != tt.want {
			t.Fatalf("isAllowedProviderAvatarHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestAvatarProviderPlanForEmail(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		domainCnt int
		want      []string
		skipped   []string
	}{
		{
			name:    "public mailbox uses per-sender providers only",
			email:   "person@gmail.com",
			want:    []string{"gravatar", "libravatar"},
			skipped: []string{"bimi", "domain_icon"},
		},
		{
			name:    "role sender uses domain providers only",
			email:   "support@brand.example",
			want:    []string{"bimi", "domain_icon"},
			skipped: []string{"gravatar", "libravatar"},
		},
		{
			name:      "high volume custom domain uses domain providers only",
			email:     "alice@brand.example",
			domainCnt: avatarDomainFirstThreshold,
			want:      []string{"bimi", "domain_icon"},
			skipped:   []string{"gravatar", "libravatar"},
		},
		{
			name:      "low volume custom person uses quality order",
			email:     "alice@brand.example",
			domainCnt: avatarDomainFirstThreshold - 1,
			want:      []string{"gravatar", "libravatar", "bimi", "domain_icon"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := avatarProviderPlanForEmail(tt.email, tt.domainCnt)
			if strings.Join(plan.providers, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("providers = %v, want %v", plan.providers, tt.want)
			}
			for _, provider := range tt.skipped {
				if plan.skipped[provider] == "" {
					t.Fatalf("skipped[%q] missing in %+v", provider, plan.skipped)
				}
			}
		})
	}
}

func TestIsRoleLikeSender(t *testing.T) {
	for _, email := range []string{"no-reply@brand.example", "support@brand.example", "sales.eu@brand.example", "reporting@brand.example"} {
		if !isRoleLikeSender(email) {
			t.Fatalf("isRoleLikeSender(%q) = false, want true", email)
		}
	}
	if isRoleLikeSender("alice.smith@brand.example") {
		t.Fatal("isRoleLikeSender(person email) = true, want false")
	}
}

func TestLibravatarAttemptMessages(t *testing.T) {
	hash := "0bc83cb571cd1c50ba6f3e8a78ef1346"
	if got := libravatarMissingAttemptMessage(hash); !strings.Contains(got, "seccdn.libravatar.org") || !strings.Contains(got, "no Libravatar image") {
		t.Fatalf("libravatarMissingAttemptMessage() = %q", got)
	}
	if got := libravatarErrorAttemptMessage(hash, errors.New("boom")); !strings.Contains(got, "seccdn.libravatar.org") || !strings.Contains(got, "boom") {
		t.Fatalf("libravatarErrorAttemptMessage() = %q", got)
	}
}

func TestDomainIconAttemptMessages(t *testing.T) {
	if got := domainIconSkippedAttemptMessage("gmail.com"); !strings.Contains(got, "public mailbox domain") {
		t.Fatalf("domainIconSkippedAttemptMessage() = %q", got)
	}
	if got := domainIconMissingAttemptMessage("brand.example"); !strings.Contains(got, "https://brand.example/favicon.ico") {
		t.Fatalf("domainIconMissingAttemptMessage() = %q", got)
	}
	if got := domainIconErrorAttemptMessage("brand.example", errors.New("boom")); !strings.Contains(got, "brand.example") || !strings.Contains(got, "boom") {
		t.Fatalf("domainIconErrorAttemptMessage() = %q", got)
	}
}

func TestAddAvatarProviderOutcomesCountsRunStats(t *testing.T) {
	stats := emptyAvatarProviderStats()
	stats = addAvatarProviderOutcomes(stats, []avatarProviderOutcome{
		{provider: "gravatar", status: "missing"},
		{provider: "libravatar", status: "found"},
		{provider: "bimi", status: "skipped"},
		{provider: "domain_icon", status: "error"},
	})

	byProvider := map[string]models.AvatarProviderStats{}
	for _, stat := range stats {
		byProvider[stat.Provider] = stat
	}
	if got := byProvider["gravatar"]; got.Checked != 1 || got.Missing != 1 {
		t.Fatalf("gravatar stats = %+v, want checked=1 missing=1", got)
	}
	if got := byProvider["libravatar"]; got.Checked != 1 || got.Found != 1 {
		t.Fatalf("libravatar stats = %+v, want checked=1 found=1", got)
	}
	if got := byProvider["bimi"]; got.Checked != 0 || got.Skipped != 1 {
		t.Fatalf("bimi stats = %+v, want checked=0 skipped=1", got)
	}
	if got := byProvider["domain_icon"]; got.Checked != 1 || got.Error != 1 {
		t.Fatalf("domain_icon stats = %+v, want checked=1 error=1", got)
	}
}

func TestHandleAvatarImageAddsStrictHeadersForSVG(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	defer db.Close()

	blobs := store.NewBlobStore(filepath.Join(dir, "blobs"))
	h := &Handler{db: db, blobStore: blobs}
	email := "brand@example.com"
	hash := avatarresolver.GravatarHash(email)
	data := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h1v1z"/></svg>`)
	storagePath, err := blobs.StoreAvatar(hash, "image/svg+xml", data)
	if err != nil {
		t.Fatalf("StoreAvatar() error = %v", err)
	}
	if err := db.SaveSenderAvatarFound(ctx, hash, email, "bimi", "image/svg+xml", storagePath, nil, time.Now().Add(time.Hour), "missing", "found"); err != nil {
		t.Fatalf("SaveSenderAvatarFound() error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, username, username_normalized, name) VALUES ('viewer', 'viewer', 'viewer', 'Viewer');
		INSERT INTO accounts (id, user_id, email_address) VALUES ('viewer-account', 'viewer', 'viewer@example.com');
		INSERT INTO messages (account_id, internet_message_id, from_email) VALUES ('viewer-account', '<brand@example.com>', ?)
	`, email); err != nil {
		t.Fatalf("seed avatar viewer: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/avatars/"+hash, nil)
	req.SetPathValue("hash", hash)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "viewer"}))
	rec := httptest.NewRecorder()
	h.handleAvatarImage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=604800") || !strings.Contains(got, "immutable") {
		t.Fatalf("Cache-Control = %q, want week-long immutable cache", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'none'") || !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q, want strict SVG policy", csp)
	}
}

func TestHandleAvatarSendersUsesProviderContactAvatarsFromAllWebmailUsers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, username, username_normalized, name, user_type, is_admin)
		VALUES ('admin-user', 'admin-user', 'admin-user', 'Admin User', 'management', 1),
		       ('mail-user-a', 'mail-user-a', 'mail-user-a', 'Mail User A', 'webmail', 0),
		       ('mail-user-b', 'mail-user-b', 'mail-user-b', 'Mail User B', 'webmail', 0);
		INSERT INTO contact_profiles (id, user_id, display_name, primary_email, avatar_url)
		VALUES ('contact-a', 'mail-user-a', 'Sender A', 'sender-a@example.com', 'https://photos.example/sender-a.jpg'),
		       ('contact-b', 'mail-user-b', 'Sender B', 'sender-b@example.com', 'https://photos.example/sender-b.jpg');
		INSERT INTO contact_identities (user_id, profile_id, kind, normalized_value)
		VALUES ('mail-user-a', 'contact-a', 'email', 'sender-a@example.com'),
		       ('mail-user-b', 'contact-b', 'email', 'sender-b@example.com')`); err != nil {
		t.Fatalf("seed admin and webmail contacts: %v", err)
	}
	for _, email := range []string{"sender-a@example.com", "sender-b@example.com"} {
		if err := db.SaveSenderAvatarMissing(ctx, avatarresolver.GravatarHash(email), email, "none", time.Now().Add(time.Hour), "missing", "missing"); err != nil {
			t.Fatalf("SaveSenderAvatarMissing(%q) error = %v", email, err)
		}
	}
	cachedEmail := "cached@example.com"
	cachedHash := avatarresolver.GravatarHash(cachedEmail)
	if err := db.SaveSenderAvatarFound(ctx, cachedHash, cachedEmail, "gravatar", "image/png", "", []byte("png"), time.Now().Add(time.Hour), "found", "missing"); err != nil {
		t.Fatalf("SaveSenderAvatarFound(%q) error = %v", cachedEmail, err)
	}

	h := &Handler{db: db}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/avatars/senders", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{
		ID: "admin-user", UserType: auth.UserTypeManagement, IsAdmin: true,
	}))
	rec := httptest.NewRecorder()
	h.handleAvatarSenders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200", rec.Code, rec.Body.String())
	}
	var response struct {
		Items []models.AvatarSenderRow `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	avatars := make(map[string]models.AvatarSenderRow, len(response.Items))
	for _, item := range response.Items {
		avatars[item.Email] = item
	}
	for email, wantURL := range map[string]string{
		"sender-a@example.com": "https://photos.example/sender-a.jpg",
		"sender-b@example.com": "https://photos.example/sender-b.jpg",
	} {
		got, ok := avatars[email]
		if !ok || got.InUse.Status != "found" || got.InUse.Source != "provider_contact" || got.InUse.AvatarURL != wantURL {
			t.Fatalf("avatar for %q = %#v, want provider contact avatar %q", email, got.InUse, wantURL)
		}
	}
	if got := avatars[cachedEmail]; got.InUse.Source != "gravatar" || !strings.HasPrefix(got.AvatarURL, "/api/admin/avatars/"+cachedHash) {
		t.Fatalf("cached avatar = %#v, want management-scoped image URL", got)
	}

	scopedReq := httptest.NewRequest(http.MethodGet, "/api/admin/avatars/senders?user_id=mail-user-a", nil)
	scopedRec := httptest.NewRecorder()
	h.handleAvatarSenders(scopedRec, scopedReq)
	if scopedRec.Code != http.StatusOK {
		t.Fatalf("scoped status = %d body = %q, want 200", scopedRec.Code, scopedRec.Body.String())
	}
	var scopedResponse struct {
		Items []models.AvatarSenderRow `json:"items"`
	}
	if err := json.NewDecoder(scopedRec.Body).Decode(&scopedResponse); err != nil {
		t.Fatalf("decode scoped response: %v", err)
	}
	if len(scopedResponse.Items) != 1 || scopedResponse.Items[0].Email != "sender-a@example.com" || scopedResponse.Items[0].InUse.AvatarURL != "https://photos.example/sender-a.jpg" {
		t.Fatalf("scoped avatars = %#v, want only mail-user-a's sender", scopedResponse.Items)
	}

	imageReq := httptest.NewRequest(http.MethodGet, "/api/admin/avatars/"+cachedHash, nil)
	imageReq.SetPathValue("hash", cachedHash)
	imageRec := httptest.NewRecorder()
	h.handleAdminAvatarImage(imageRec, imageReq)
	if imageRec.Code != http.StatusOK || imageRec.Body.String() != "png" {
		t.Fatalf("admin avatar image = status:%d body:%q, want cached image", imageRec.Code, imageRec.Body.String())
	}
}

func TestReuseStoredDomainIconAvatarMarksSenderFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	defer db.Close()

	blobs := store.NewBlobStore(filepath.Join(dir, "blobs"))
	h := &Handler{db: db, blobStore: blobs}
	expiresAt := time.Now().Add(time.Hour)
	existingEmail := "alice@brand.example"
	existingHash := avatarresolver.GravatarHash(existingEmail)
	storagePath, err := blobs.StoreAvatar(existingHash, "image/png", []byte("png"))
	if err != nil {
		t.Fatalf("StoreAvatar() error = %v", err)
	}
	if err := db.SaveSenderAvatarFound(ctx, existingHash, existingEmail, "domain_icon", "image/png", storagePath, nil, expiresAt, "missing", "missing"); err != nil {
		t.Fatalf("SaveSenderAvatarFound() error = %v", err)
	}

	newEmail := "bob@brand.example"
	newHash := avatarresolver.GravatarHash(newEmail)
	image, message, found, err := h.reuseStoredDomainIconAvatar(ctx, newHash, newEmail, "missing", "missing")
	if err != nil {
		t.Fatalf("reuseStoredDomainIconAvatar() error = %v", err)
	}
	if !found || image.Source != "domain_icon" || image.ContentType != "image/png" || image.SourceURL == "" {
		t.Fatalf("reuseStoredDomainIconAvatar() = (%+v, %q, %v), want reused domain icon", image, message, found)
	}
	if !strings.Contains(message, "Reused stored domain icon") || !strings.Contains(message, existingHash) {
		t.Fatalf("message = %q, want reuse details", message)
	}

	rec, err := db.GetSenderAvatarByHash(ctx, newHash)
	if err != nil {
		t.Fatalf("GetSenderAvatarByHash() error = %v", err)
	}
	if rec == nil || rec.Status != "found" || rec.StoragePath != storagePath {
		t.Fatalf("reused record = %+v, want found sender with reused storage path %q", rec, storagePath)
	}
}
