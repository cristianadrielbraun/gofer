//go:build livemail

// Checks shared mailboxes against a real IMAP/SMTP server. It runs only with
// the livemail build tag and reads the account from the environment; the
// password is never printed:
//
//	GOFER_LIVE_IMAP_HOST, GOFER_LIVE_IMAP_PORT (993)
//	GOFER_LIVE_SMTP_HOST, GOFER_LIVE_SMTP_PORT (465)
//	GOFER_LIVE_TLS: "tls" (default) or "starttls", for both
//	GOFER_LIVE_USER, GOFER_LIVE_PASSWORD
//	GOFER_LIVE_EXPECT_SHARED: the shared mailboxes the account must find,
//	  comma-separated
//	GOFER_LIVE_UNAUTHORIZED_FROM: optional, an address the account must not
//	  be allowed to send as
//	GOFER_LIVE_ALLOW_PRIVATE: "true" when the server's name resolves to a
//	  private address where the test runs, as on the mail server itself
//
//	go test -tags livemail -run Live -v ./internal/mail/
//
// It only reads from IMAP, and on SMTP it never sends DATA, so no mail is
// sent and nothing on the server changes.
package mail

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func liveEnv(t *testing.T, key, fallback string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	if fallback == "" {
		t.Skipf("%s is not set", key)
	}
	return fallback
}

func liveConfig(t *testing.T) (*models.AccountConfig, string) {
	t.Helper()
	port := func(key, fallback string) int {
		n, err := strconv.Atoi(liveEnv(t, key, fallback))
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return n
	}
	tlsMode := liveEnv(t, "GOFER_LIVE_TLS", "tls")
	user := liveEnv(t, "GOFER_LIVE_USER", "")
	password := os.Getenv("GOFER_LIVE_PASSWORD")
	if password == "" {
		t.Skip("GOFER_LIVE_PASSWORD is not set")
	}
	return &models.AccountConfig{
		AccountID:   "live",
		Provider:    "imap",
		IMAPHost:    liveEnv(t, "GOFER_LIVE_IMAP_HOST", ""),
		IMAPPort:    port("GOFER_LIVE_IMAP_PORT", "993"),
		IMAPTLSMode: tlsMode,
		SMTPHost:    liveEnv(t, "GOFER_LIVE_SMTP_HOST", os.Getenv("GOFER_LIVE_IMAP_HOST")),
		SMTPPort:    port("GOFER_LIVE_SMTP_PORT", "465"),
		SMTPTLSMode: tlsMode,
		Username:    user,
		AuthMethod:  "plain",

		IMAPAllowPrivate: os.Getenv("GOFER_LIVE_ALLOW_PRIVATE") == "true",
		SMTPAllowPrivate: os.Getenv("GOFER_LIVE_ALLOW_PRIVATE") == "true",
	}, password
}

func liveExpectedShared() []string {
	var out []string
	for _, part := range strings.Split(os.Getenv("GOFER_LIVE_EXPECT_SHARED"), ",") {
		if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func liveFolders(t *testing.T, ctx context.Context, cfg *models.AccountConfig, password string) ([]imap.FolderInfo, bool) {
	t.Helper()
	client, err := imap.NewContextClient(ctx, cfg, password)
	if err != nil {
		t.Fatalf("IMAP sign-in to %s:%d (%s) as %s: %v", cfg.IMAPHost, cfg.IMAPPort, cfg.IMAPTLSMode, cfg.Username, err)
	}
	defer client.Close()
	t.Logf("IMAP: signed in to %s:%d (%s) as %s", cfg.IMAPHost, cfg.IMAPPort, cfg.IMAPTLSMode, cfg.Username)

	personal, shared, known, err := client.NamespaceReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NAMESPACE answered=%v personal=%q shared=%q", known, personal, shared)

	folders, err := client.ListFolders(ctx)
	if err != nil {
		t.Fatalf("LIST: %v", err)
	}
	for _, f := range folders {
		attrs := make([]string, 0, len(f.Attributes))
		for _, a := range f.Attributes {
			attrs = append(attrs, string(a))
		}
		t.Logf("LIST %-55q delim=%q attrs=%v selectable=%v role=%s shared_mailbox=%q shared_role=%q",
			f.Name, string(f.Delimiter), attrs, f.Selectable, f.Role, f.SharedMailbox, f.SharedRole)
	}
	return folders, client.SharedMailboxesComplete()
}

func TestLiveSharedMailboxDiscovery(t *testing.T) {
	cfg, password := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	folders, complete := liveFolders(t, ctx, cfg, password)
	if !complete {
		t.Error("the listing is not trusted as complete: the server did not answer NAMESPACE")
	}

	// Each expected shared mailbox is found, with its special folders.
	found := sharedMailboxesListed(folders)
	t.Logf("shared mailboxes found: %v", found)
	for _, want := range liveExpectedShared() {
		if !slices.Contains(found, want) {
			t.Errorf("shared mailbox %s was not found", want)
			continue
		}
		roles := map[string]string{}
		for _, f := range folders {
			if f.SharedMailbox == want && f.SharedRole != "" && f.SharedRole != imap.SharedRoleRoot {
				roles[f.SharedRole] = f.Name
			}
		}
		for _, role := range []string{"inbox", "sent", "drafts", "junk", "trash"} {
			if roles[role] == "" {
				t.Errorf("%s: no folder found for role %s (roles found: %v)", want, role, roles)
			} else {
				t.Logf("%s: %-6s = %q", want, role, roles[role])
			}
		}
	}

	// Only the account's own folders hold the account's roles.
	inputs, err := imapFolderInputs("live", imapFoldersWithParents(folders), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	holders := map[string][]string{}
	for _, in := range inputs {
		if in.Role != "custom" {
			holders[in.Role] = append(holders[in.Role], in.RemoteID)
		}
		if in.SharedMailbox != "" && in.Role != "custom" {
			t.Errorf("shared folder %q holds the account role %s", in.RemoteID, in.Role)
		}
	}
	for role, names := range holders {
		if len(names) > 1 {
			t.Errorf("role %s is held by %d folders: %v", role, len(names), names)
		} else {
			t.Logf("own %-7s = %q", role, names[0])
		}
	}

	// Saved twice into a scratch database, the folders give one identity per
	// shared mailbox, and the second sync changes nothing.
	db, err := storage.New(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('live', 'default', 'imap', ?)`, cfg.Username); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, inputs); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		mailboxes, identities, err := db.ApplySharedMailboxDiscovery(ctx, "live", found, complete, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("sync %d: mailboxes added=%v identities created=%v", run, mailboxes.Added, identities.Created)
		if run == 2 && (mailboxes.Changed() || identities.Changed()) {
			t.Errorf("second sync changed something: %+v %+v", mailboxes, identities)
		}
	}
	identities, err := db.ListAccountIdentitiesInternal(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range identities {
		t.Logf("identity: %q <%s>", identity.Name, identity.Email)
	}
	if len(identities) != len(found) {
		t.Errorf("%d identities for %d shared mailboxes", len(identities), len(found))
	}
	for _, mailbox := range found {
		if _, _, ok, err := db.SharedMailboxFolder(ctx, "live", mailbox, "sent"); err != nil || !ok {
			t.Errorf("%s: its Sent folder is not found by role (%v)", mailbox, err)
		}
		if _, _, ok, err := db.SharedMailboxFolder(ctx, "live", mailbox, "drafts"); err != nil || !ok {
			t.Errorf("%s: its Drafts folder is not found by role (%v)", mailbox, err)
		}
	}
}

func TestLiveSharedMailboxSenders(t *testing.T) {
	cfg, password := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	check := func(address string) smtp.SenderCheck {
		t.Helper()
		result, err := smtp.CheckSender(ctx, cfg, password, address, cfg.Username)
		if err != nil {
			t.Fatalf("SMTP %s:%d (%s) as %s: %v", cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPTLSMode, cfg.Username, err)
		}
		t.Logf("SMTP MAIL FROM:<%s> accepted=%v stage=%s code=%d message=%q", address, result.Accepted, result.Stage, result.Code, result.Message)
		return result
	}

	if result := check(cfg.Username); !result.Accepted {
		t.Errorf("the account's own address was refused")
	}
	for _, mailbox := range liveExpectedShared() {
		if result := check(mailbox); !result.Accepted {
			t.Errorf("shared mailbox %s was refused as a sender", mailbox)
		}
	}
	if address := strings.TrimSpace(os.Getenv("GOFER_LIVE_UNAUTHORIZED_FROM")); address != "" {
		if result := check(address); result.Accepted {
			t.Logf("NOTE: %s was accepted at MAIL FROM/RCPT TO; the server may only refuse it once the message is sent", address)
		}
	}
}
