package handler

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newMailRulesTestHandler(t *testing.T) (*Handler, *storage.DB) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, nil, nil, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{
		{ID: "owner-inbox", AccountID: "victim-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "owner-archive", AccountID: "victim-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "owner-receipts", AccountID: "victim-account", RemoteID: "Receipts", Name: "Receipts", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	messages := []storage.SyncMessage{
		{RemoteUID: 1, MessageID: "<offer@corner-shop.example>", Subject: "This week's offer", FromEmail: "deals@corner-shop.example"},
		{RemoteUID: 2, MessageID: "<order@corner-shop.example>", Subject: "Your order shipped", FromEmail: "orders@corner-shop.example"},
	}
	for i := range messages {
		messages[i].AccountID, messages[i].FolderID = "victim-account", "owner-inbox"
		messages[i].DateSent = time.Date(2026, 10, 1+i, 9, 0, 0, 0, time.UTC)
	}
	if err := db.UpsertSyncMessages(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	return h, db
}

func mailRuleForm(fields map[string]string) url.Values {
	form := url.Values{
		"name": {"Offers"}, "account_id": {"victim-account"}, "match": {"all"}, "enabled": {"true"},
		"cond_field": {"subject"}, "cond_op": {"contains"}, "cond_value": {"offer"}, "cond_folder": {""},
		"act_kind": {"archive"}, "act_folder": {""}, "act_label": {""},
	}
	for key, value := range fields {
		form.Set(key, value)
	}
	return form
}

var mailRulePreviewIDs = regexp.MustCompile(`name="message_id" value="(\d+)"`)

func TestMailRuleSettingsRoutes(t *testing.T) {
	h, db := newMailRulesTestHandler(t)
	ctx := t.Context()

	rec := identityRequest(t, h, h.handleMailRulesPanel, http.MethodGet, "/api/settings/rules/panel", nil, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No rules yet") {
		t.Fatalf("empty panel = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleMailRuleDialog, http.MethodGet, "/api/settings/rules/dialog?add=1", nil, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="mail-rule-dialog"`) || !strings.Contains(rec.Body.String(), "data-mail-rule-form") || !strings.Contains(rec.Body.String(), "New rule") {
		t.Fatalf("new rule dialog = %d %q", rec.Code, rec.Body.String())
	}

	// The form is shown again with a row added, or with a folder choice once
	// a condition tests the folder, and nothing is saved.
	// AND adds to the condition's group; OR starts a group of its own.
	rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, mailRuleForm(map[string]string{"op": "add_condition", "group": "0"}), true)
	if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Count(body, "data-mail-rule-condition>") != 2 || strings.Count(body, "data-mail-rule-condition-group=") != 1 || !strings.Contains(body, `value="offer"`) {
		t.Fatalf("add AND condition = %d %q", rec.Code, body)
	}
	rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, mailRuleForm(map[string]string{"op": "add_condition", "group": "-1"}), true)
	if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Count(body, "data-mail-rule-condition>") != 2 || strings.Count(body, "data-mail-rule-condition-group=") != 2 ||
		!strings.Contains(body, `name="cond_group" value="1"`) {
		t.Fatalf("add OR condition = %d %q", rec.Code, body)
	}
	rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, mailRuleForm(map[string]string{"cond_field": "folder", "act_kind": "move"}), true)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `name="cond_folder"`) || !strings.Contains(body, `data-tui-selectbox-value="id:owner-receipts"`) || !strings.Contains(body, `name="act_folder"`) {
		t.Fatalf("folder condition = %d %q", rec.Code, body)
	}
	// As in the account's Folders dialog: a new folder at the top, or inside
	// any folder.
	for _, want := range []string{`data-mail-rule-new-folder="victim-account"`, `data-mail-rule-new-folder-parent="owner-receipts"`, `data-mail-rule-new-folder-parent="owner-inbox"`, `data-tui-selectbox-value="role:inbox"`, "New subfolder in Receipts"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("move folder picker lacks %s: %q", want, rec.Body.String())
		}
	}
	// Mail goes to one place: once an action moves it, an added action
	// offers no move, and nothing already there.
	form := mailRuleForm(map[string]string{"op": "add_action"})
	rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, form, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Count(body, "data-mail-rule-action>") != 2 || strings.Count(body, `data-tui-selectbox-value="trash"`) != 1 ||
		strings.Count(body, `data-tui-selectbox-value="archive"`) != 1 || strings.Count(body, `data-tui-selectbox-value="mark_read"`) != 1 {
		t.Fatalf("second action = %d %q", rec.Code, body)
	}
	form.Del("op")
	form["act_kind"] = []string{"archive", "mark_read"}
	form["act_folder"], form["act_label"] = []string{"", ""}, []string{"", ""}
	if rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, form, true); !strings.Contains(rec.Body.String(), `data-tui-selectbox-value="mark_read" data-tui-selectbox-selected="true"`) {
		t.Fatalf("added action isn't Mark as read: %q", rec.Body.String())
	}
	// A rule for every account can't make a folder in one of them.
	rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, mailRuleForm(map[string]string{"account_id": "", "act_kind": "move"}), true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "data-mail-rule-new-folder") || !strings.Contains(rec.Body.String(), "Choose an account under For") {
		t.Fatalf("move for every account = %d %q", rec.Code, rec.Body.String())
	}
	if rules, _ := db.ListMailRules(ctx, "owner"); len(rules) != 0 {
		t.Fatalf("redrawing the form saved %+v", rules)
	}

	rec = identityRequest(t, h, h.handleSaveMailRule, http.MethodPost, "/api/settings/rules", nil, mailRuleForm(map[string]string{"name": ""}), true)
	// A refused rule stays in the dialog; a saved one closes it and shows the
	// list.
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Give the rule a name.") || !strings.Contains(rec.Body.String(), `value="offer"`) || rec.Header().Get("HX-Retarget") != "" {
		t.Fatalf("nameless rule = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleSaveMailRule, http.MethodPost, "/api/settings/rules", nil, mailRuleForm(nil), true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "owner@example.com · 1 condition · Archive") ||
		rec.Header().Get("HX-Retarget") != "#mail-rules-panel" || rec.Header().Get("HX-Trigger") != "mail-rule-dialog-done" {
		t.Fatalf("save = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	rules, err := db.ListMailRules(ctx, "owner")
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	rec = identityRequest(t, h, h.handleMailRuleDialog, http.MethodGet, "/api/settings/rules/dialog?edit="+rules[0].ID, nil, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Edit rule") || !strings.Contains(rec.Body.String(), `value="`+rules[0].ID+`"`) {
		t.Fatalf("edit dialog = %d %q", rec.Code, rec.Body.String())
	}
	// (From contains orders and Subject contains order) or Subject contains
	// shipped.
	grouped := mailRuleForm(map[string]string{"name": "Orders", "act_kind": "star"})
	grouped["cond_group"] = []string{"0", "1", "0"}
	grouped["cond_field"] = []string{"from", "subject", "subject"}
	grouped["cond_op"] = []string{"contains", "contains", "contains"}
	grouped["cond_value"] = []string{"orders", "shipped", "order"}
	grouped["cond_folder"] = []string{"", "", ""}
	rec = identityRequest(t, h, h.handleSaveMailRule, http.MethodPost, "/api/settings/rules", nil, grouped, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "owner@example.com · 3 conditions in 2 groups · Star") {
		t.Fatalf("save grouped = %d %q", rec.Code, rec.Body.String())
	}
	rules, err = db.ListMailRules(ctx, "owner")
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	offers, orders := rules[0], rules[1]
	route := map[string]string{"id": offers.ID}

	// Another user sees none of it and can change none of it.
	rec = identityRequest(t, h, h.handleMailRulesPanel, http.MethodGet, "/api/settings/rules/panel", nil, nil, false)
	if strings.Contains(rec.Body.String(), "Offers") {
		t.Fatalf("attacker panel = %q", rec.Body.String())
	}
	for name, rec := range map[string]interface{ Result() *http.Response }{
		"edit":    identityRequest(t, h, h.handleSaveMailRule, http.MethodPost, "/api/settings/rules", nil, mailRuleForm(map[string]string{"id": offers.ID, "account_id": ""}), false),
		"enabled": identityRequest(t, h, h.handleMailRuleEnabled, http.MethodPost, "/api/settings/rules/"+offers.ID+"/enabled", route, url.Values{}, false),
		"delete":  identityRequest(t, h, h.handleDeleteMailRule, http.MethodDelete, "/api/settings/rules/"+offers.ID, route, nil, false),
		"run":     identityRequest(t, h, h.handleMailRuleRunDialog, http.MethodGet, "/api/settings/rules/"+offers.ID+"/run", route, nil, false),
	} {
		if code := rec.Result().StatusCode; code != http.StatusNotFound {
			t.Fatalf("attacker %s = %d", name, code)
		}
	}
	if got, err := db.GetMailRule(ctx, "owner", offers.ID); err != nil || !got.Enabled || got.AccountID != "victim-account" {
		t.Fatalf("after attacker = %+v, %v", got, err)
	}

	rec = identityRequest(t, h, h.handleMailRuleOrder, http.MethodPost, "/api/settings/rules/order", nil, url.Values{"rule_id": {orders.ID, offers.ID}}, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("order = %d %q", rec.Code, rec.Body.String())
	}
	if rules, _ = db.ListMailRules(ctx, "owner"); rules[0].ID != orders.ID {
		t.Fatalf("after order = %+v", rules)
	}
	rec = identityRequest(t, h, h.handleMailRuleOrder, http.MethodPost, "/api/settings/rules/order", nil, url.Values{"rule_id": {orders.ID}}, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale order = %d", rec.Code)
	}
	rec = identityRequest(t, h, h.handleMailRuleEnabled, http.MethodPost, "/api/settings/rules/"+orders.ID+"/enabled", map[string]string{"id": orders.ID}, url.Values{}, true)
	if got, _ := db.GetMailRule(ctx, "owner", orders.ID); rec.Code != http.StatusNoContent || got.Enabled {
		t.Fatalf("turn off = %d, %+v", rec.Code, got)
	}
	rec = identityRequest(t, h, h.handleDeleteMailRule, http.MethodDelete, "/api/settings/rules/"+orders.ID, map[string]string{"id": orders.ID}, nil, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Orders") {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}

	// Running by hand counts in the background what the rule would change,
	// and changes nothing until started; then it changes it, queues the
	// changes, wakes the queue and tells pages, and records a summary.
	events := h.syncer.Events().Subscribe()
	defer h.syncer.Events().Unsubscribe(events)
	rec = identityRequest(t, h, h.handleMailRuleRunDialog, http.MethodGet, "/api/settings/rules/"+offers.ID+"/run", route, nil, true)
	job := mailRuleRunJobID.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="mail-rule-dialog"`) || job == nil ||
		!strings.Contains(rec.Body.String(), "owner@example.com · 1 condition · Archive") {
		t.Fatalf("run dialog = %d %q", rec.Code, rec.Body.String())
	}
	jobRoute := map[string]string{"job": job[1]}
	run := waitMailRuleRun(t, h, "owner", job[1], models.MailRuleRunCounted)
	if run.Matches != 1 || run.Scanned != 2 || run.Total != 2 {
		t.Fatalf("counted = %+v", run)
	}
	rec = identityRequest(t, h, h.handleMailRuleRunJob, http.MethodGet, "/api/settings/rule-runs/"+job[1], jobRoute, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-mail-rule-run-total="1"`) || !strings.Contains(rec.Body.String(), "Run on 1 message") {
		t.Fatalf("counted panel = %d %q", rec.Code, rec.Body.String())
	}
	var pending int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_mutations`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("counting queued %d changes, %v", pending, err)
	}
	// Another user can't see or start it.
	for name, handle := range map[string]func(http.ResponseWriter, *http.Request){"show": h.handleMailRuleRunJob, "start": h.handleStartMailRuleRun, "stop": h.handleStopMailRuleRun} {
		if rec := identityRequest(t, h, handle, http.MethodPost, "/api/settings/rule-runs/"+job[1], jobRoute, url.Values{}, false); rec.Code != http.StatusNotFound {
			t.Fatalf("attacker %s = %d", name, rec.Code)
		}
	}

	rec = identityRequest(t, h, h.handleStartMailRuleRun, http.MethodPost, "/api/settings/rule-runs/"+job[1]+"/start", jobRoute, url.Values{}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d %q", rec.Code, rec.Body.String())
	}
	run = waitMailRuleRun(t, h, "owner", job[1], models.MailRuleRunDone)
	if run.Changed != 1 || run.Processed != 1 {
		t.Fatalf("done = %+v", run)
	}
	rec = identityRequest(t, h, h.handleMailRuleRunJob, http.MethodGet, "/api/settings/rule-runs/"+job[1], jobRoute, nil, true)
	if !strings.Contains(rec.Body.String(), "Changed 1 message in Inbox.") {
		t.Fatalf("done panel = %q", rec.Body.String())
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_mutations WHERE kind = 'move'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("queued moves = %d, %v", pending, err)
	}
	select {
	case <-h.messageMutationWake:
	default:
		t.Fatal("the queued-changes worker wasn't woken")
	}
	refreshed, states := map[string]bool{}, []string{}
	for len(events) > 0 {
		switch event := <-events; event.Type {
		case mailpkg.EventMutation:
			refreshed[event.FolderID] = true
		case mailpkg.EventMailRuleRun:
			if event.UserID != "owner" {
				t.Fatalf("run event for %q", event.UserID)
			}
			if state := event.Payload["state"].(string); len(states) == 0 || states[len(states)-1] != state {
				states = append(states, state)
			}
		}
	}
	if !refreshed["owner-inbox"] || !refreshed["owner-archive"] {
		t.Fatalf("refreshed folders = %v", refreshed)
	}
	if strings.Join(states, ",") != "counted,running,done" {
		t.Fatalf("run events = %v", states)
	}
	runs, err := db.ListMailRuleRuns(ctx, "owner", 0)
	if err != nil || len(runs) != 1 || runs[0].Trigger != models.MailRuleTriggerManual || runs[0].Detail != "Run by hand on Inbox: changed 1 message" {
		t.Fatalf("activity = %+v, %v", runs, err)
	}

	// A second run finds nothing; a change of folder counts again there.
	rec = identityRequest(t, h, h.handleMailRuleRunScope, http.MethodPost, "/api/settings/rules/"+offers.ID+"/run/scope", route, url.Values{"scope": {"role:inbox"}}, true)
	again := mailRuleRunJobID.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || again == nil || again[1] == job[1] {
		t.Fatalf("count again = %d %q", rec.Code, rec.Body.String())
	}
	if run = waitMailRuleRun(t, h, "owner", again[1], models.MailRuleRunCounted); run.Matches != 0 {
		t.Fatalf("second count = %+v", run)
	}
	if h.mailRuleRuns.job("owner", job[1]) != nil {
		t.Fatal("the earlier run of the rule was kept")
	}
}

var mailRuleRunJobID = regexp.MustCompile(`data-mail-rule-run="([0-9a-f]+)"`)

// waitMailRuleRun waits for the owner's run to reach state.
func waitMailRuleRun(t *testing.T, h *Handler, owner, id, state string) models.MailRuleRunData {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job := h.mailRuleRuns.job(owner, id)
		if job == nil {
			t.Fatalf("run %s is gone", id)
		}
		run := h.mailRuleRuns.snapshot(job)
		if run.State == state {
			return run
		}
		if run.Finished() || time.Now().After(deadline) {
			t.Fatalf("run = %+v, want %s", run, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stopping a count leaves nothing to run.
func TestStopMailRuleCount(t *testing.T) {
	h, db := newMailRulesTestHandler(t)
	rule, err := db.SaveMailRuleGuarded(t.Context(), "owner", models.MailRule{Name: "Offers", AccountID: "victim-account", Enabled: true,
		Conditions: []models.MailRuleCondition{{Field: "subject", Operator: "contains", Value: "o"}},
		Actions:    []models.MailRuleAction{{Kind: models.MailRuleActionStar}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.mailRuleRuns.scanBatch, h.mailRuleRuns.pause = 1, time.Hour

	rec := identityRequest(t, h, h.handleMailRuleRunDialog, http.MethodGet, "/api/settings/rules/"+rule.ID+"/run", map[string]string{"id": rule.ID}, nil, true)
	job := mailRuleRunJobID.FindStringSubmatch(rec.Body.String())
	if job == nil {
		t.Fatalf("run dialog = %q", rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.mailRuleRuns.snapshot(h.mailRuleRuns.job("owner", job[1])).Scanned == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the count didn't start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec = identityRequest(t, h, h.handleStopMailRuleRun, http.MethodPost, "/api/settings/rule-runs/"+job[1]+"/stop", map[string]string{"job": job[1]}, url.Values{}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Stopped. Nothing was changed.") {
		t.Fatalf("stop = %d %q", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleStartMailRuleRun, http.MethodPost, "/api/settings/rule-runs/"+job[1]+"/start", map[string]string{"job": job[1]}, url.Values{}, true)
	if run := h.mailRuleRuns.snapshot(h.mailRuleRuns.job("owner", job[1])); run.State != models.MailRuleRunStopped {
		t.Fatalf("started a stopped count: %+v", run)
	}
}
