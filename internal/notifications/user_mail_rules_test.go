package notifications

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var userMailRuleRunJob = regexp.MustCompile(`data-mail-rule-run="([0-9a-f]+)"`)

// waitUserMailRuleRun asks for a run until it is in state, and returns its
// panel.
func (f *userStorageFixture) waitUserMailRuleRun(t *testing.T, owner, job, state string) string {
	t.Helper()
	var body string
	waitUserIMAP(t, func() bool {
		rec := f.request(owner, http.MethodGet, "/api/settings/rule-runs/"+job, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("run %s: %d %s", job, rec.Code, rec.Body.String())
		}
		body = rec.Body.String()
		return strings.Contains(body, `data-mail-rule-run-state="`+state+`"`)
	})
	return body
}

func (f *userStorageFixture) mailRules(t *testing.T, owner string) []models.MailRule {
	t.Helper()
	var rules []models.MailRule
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var err error
		rules, err = db.ListMailRules(t.Context(), owner)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rules
}

// Rules live in each owner's own store; running one by hand sends its
// changes to that owner's server only.
func TestUserMailRulesHTTPRunAndIsolation(t *testing.T) {
	f, _ := newUserMutationFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		form := url.Values{
			"name": {owner + " archive"}, "account_id": {f.accounts[owner].ID}, "match": {"all"}, "enabled": {"true"},
			"cond_field": {"subject"}, "cond_op": {"contains"}, "cond_value": {"private subject 1"}, "cond_folder": {""},
			"act_kind": {"archive"}, "act_folder": {""}, "act_label": {""},
		}
		if rec := f.request(owner, http.MethodPost, "/api/settings/rules", form.Encode()); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), owner+" archive") || strings.Contains(rec.Body.String(), "data-mail-rule-error") {
			t.Fatalf("save %s: %d %s", owner, rec.Code, rec.Body.String())
		}
	}
	if rec := f.request("alice", http.MethodGet, "/settings/rules", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `hx-get="/api/settings/rules/panel"`) {
		t.Fatalf("rules tab: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.request("alice", http.MethodGet, "/api/settings/rules/panel", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice archive") || strings.Contains(rec.Body.String(), "bob archive") {
		t.Fatalf("alice panel: %d %s", rec.Code, rec.Body.String())
	}
	alice, bob := f.mailRules(t, "alice")[0], f.mailRules(t, "bob")[0]
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/settings/rules/" + bob.ID + "/run", ""},
		{http.MethodPost, "/api/settings/rules/" + bob.ID + "/run/scope", url.Values{"scope": {"role:inbox"}}.Encode()},
		{http.MethodPost, "/api/settings/rules/" + bob.ID + "/enabled", ""},
		{http.MethodDelete, "/api/settings/rules/" + bob.ID, ""},
	} {
		if rec := f.request("alice", request.method, request.path, request.body); rec.Code != http.StatusNotFound {
			t.Fatalf("alice on bob's rule %s %s: %d %s", request.method, request.path, rec.Code, rec.Body.String())
		}
	}
	if got := f.mailRules(t, "bob"); len(got) != 1 || !got[0].Enabled {
		t.Fatalf("bob's rules after alice = %+v", got)
	}

	rec = f.request("alice", http.MethodGet, "/api/settings/rules/"+alice.ID+"/run", "")
	job := userMailRuleRunJob.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || job == nil {
		t.Fatalf("run dialog: %d %s", rec.Code, rec.Body.String())
	}
	if body := f.waitUserMailRuleRun(t, "alice", job[1], "counted"); !strings.Contains(body, `data-mail-rule-run-total="1"`) {
		t.Fatalf("counted: %s", body)
	}
	if pending := f.pendingMutations(t, "alice"); pending != 0 {
		t.Fatalf("counting queued %d changes", pending)
	}
	// Bob can't see or start Alice's run.
	if rec := f.request("bob", http.MethodPost, "/api/settings/rule-runs/"+job[1]+"/start", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob started alice's run: %d", rec.Code)
	}
	if rec := f.request("alice", http.MethodPost, "/api/settings/rule-runs/"+job[1]+"/start", ""); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if body := f.waitUserMailRuleRun(t, "alice", job[1], "done"); !strings.Contains(body, "Changed 1 message in Inbox.") {
		t.Fatalf("done: %s", body)
	}
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
	archive := f.remoteFolderID(t, "alice", "Archive")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var uid int
		return db.Read().QueryRow(`SELECT COALESCE(remote_uid, 0) FROM message_folder_state WHERE folder_id = ? AND is_deleted = 0`, archive).Scan(&uid)
	}); err != nil {
		t.Fatalf("archived message: %v", err)
	}
	if pending := f.pendingMutations(t, "bob"); pending != 0 {
		t.Fatalf("bob has %d changes from alice's run", pending)
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
		var archived int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_state s JOIN folders f ON f.id = s.folder_id WHERE f.role = 'archive' AND s.is_deleted = 0`).Scan(&archived); err != nil {
			return err
		}
		if archived != 0 {
			t.Fatalf("bob has %d archived messages", archived)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var central int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM mail_rules`).Scan(&central); err != nil || central != 0 {
		t.Fatalf("central rules = %d, %v", central, err)
	}
}
