package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailOperationsDetailsShowFailuresInFull(t *testing.T) {
	queued := time.Date(2026, time.November, 18, 14, 5, 0, 0, time.UTC)
	longError := "NO [TRYCREATE] " + strings.Repeat("the server explained at length ", 20) + "Mailbox Projects does not exist"
	status := models.MailOperationsList{Operations: []models.MailOperationSummary{
		{ID: "message_mutation:failed-1", Type: models.MailOperationMessageMutation, AccountEmail: "alex@example.com",
			Operation: "move", FolderName: "Inbox", DestinationFolderName: "Projects", State: "failed", Attempts: 3,
			LastError: longError, CreatedAt: queued, UpdatedAt: queued.Add(time.Hour), NextRetryAt: queued.Add(2 * time.Hour), CanRetry: true},
		{ID: "message_mutation:parked-1", Type: models.MailOperationMessageMutation, AccountEmail: "alex@example.com",
			Operation: "delete", FolderName: "Old", State: "failed", LastError: "folder removed remotely (imap discovery)",
			CreatedAt: queued, UpdatedAt: queued, Parked: true, ParkedReason: models.MailOperationParkedFolderRemoved, CanUndo: true},
		{ID: "label_mutation:9", Type: models.MailOperationLabelMutation, AccountEmail: "alex@example.com",
			Operation: "add", LabelName: "Travel", State: "pending", CreatedAt: queued},
	}}
	var out bytes.Buffer
	if err := SettingsOperationsContent(status).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()

	failed := operationRowHTML(t, html, "message_mutation:failed-1")
	if !strings.Contains(failed, "line-clamp-2") || !strings.Contains(failed, `data-mail-operation-details="message_mutation:failed-1"`) {
		t.Fatalf("failed row lacks the short preview or Details:\n%s", failed)
	}
	if strings.Count(failed, "Mailbox Projects does not exist") != 3 {
		t.Fatalf("the full error should be in the preview, Details and the copied text")
	}
	for _, want := range []string{"Queued", "Last change", "Attempts", "Next try", "Reference", "Copy details", "Account: alex@example.com", "Change: move · Inbox → Projects"} {
		if !strings.Contains(failed, want) {
			t.Fatalf("failed row Details lack %q", want)
		}
	}

	parked := operationRowHTML(t, html, "message_mutation:parked-1")
	if !strings.Contains(parked, "data-mail-operation-details") || strings.Contains(parked, "Next try") {
		t.Fatalf("parked row should have Details without a next try:\n%s", parked)
	}
	if !strings.Contains(parked, "folder removed remotely (imap discovery)") {
		t.Fatalf("parked row Details should keep the stored reason for bug reports")
	}

	queuedRow := operationRowHTML(t, html, "label_mutation:9")
	if strings.Contains(queuedRow, "data-mail-operation-details") {
		t.Fatalf("a change that hasn't failed has no Details")
	}
}

// operationRowHTML is the markup of one row of the list.
func operationRowHTML(t *testing.T, html, id string) string {
	t.Helper()
	start := strings.Index(html, `data-mail-operation-id="`+id+`"`)
	if start < 0 {
		t.Fatalf("row %s not rendered", id)
	}
	end := strings.Index(html[start+1:], `class="settings-item`)
	if end < 0 {
		return html[start:]
	}
	return html[start : start+1+end]
}

func TestRetryAllFailedShowsOnlyWithFailedWork(t *testing.T) {
	for _, tt := range []struct {
		retryable int
		want      string
	}{{0, ""}, {1, "Retry 1 failed"}, {12, "Retry all 12 failed"}} {
		var out bytes.Buffer
		if err := SettingsOperationsContent(models.MailOperationsList{Retryable: tt.retryable}).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		if has := strings.Contains(html, "data-mail-operations-retry-all"); has != (tt.want != "") {
			t.Fatalf("retryable %d: button shown = %v", tt.retryable, has)
		}
		if tt.want != "" && !strings.Contains(html, tt.want) {
			t.Fatalf("retryable %d: button lacks %q", tt.retryable, tt.want)
		}
	}
}

func TestMailOperationGroupsSayWhatTheActionDid(t *testing.T) {
	queued := time.Date(2026, time.November, 18, 14, 55, 0, 0, time.UTC)
	list := models.MailOperationsList{Groups: []models.MailOperationGroup{
		{ActionID: "a-move", ActionKind: "move", ActionSource: "you", AccountEmail: "alex@example.com", Accounts: 1, Messages: 3, Jobs: 3,
			FolderID: "f-inbox", FolderName: "Inbox", FolderRole: "inbox", DestinationFolderID: "f-projects", DestinationName: "Projects", DestinationRole: "custom",
			Failed: 1, Parked: 2, CreatedAt: queued, UpdatedAt: queued},
		{ActionID: "a-empty", ActionKind: "empty_folder", ActionSource: "you", AccountEmail: "alex@example.com", Accounts: 1, Messages: 340, Jobs: 340,
			FolderID: "f-trash", FolderName: "Trash", FolderRole: "trash", CreatedAt: queued, UpdatedAt: queued},
		{ActionID: "a-rule", ActionKind: "label_add", ActionSource: "rule", ActionRuleName: "Newsletters", Accounts: 2, Messages: 1, Jobs: 1,
			LabelName: "Reading", CreatedAt: queued, UpdatedAt: queued},
		{ActionID: "a-gone", ActionKind: "move", ActionSource: "rule", AccountEmail: "alex@example.com", Accounts: 1, Messages: 2, Jobs: 2,
			DestinationFolderID: "f-old", CreatedAt: queued, UpdatedAt: queued},
	}}
	var out bytes.Buffer
	if err := SettingsOperationsContent(list).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for id, wants := range map[string][]string{
		"a-move":  {"Move 3 messages from Inbox to Projects", "You · ", "1 of 3 changes failed", "2 of 3 changes can", "Retry 1 failed", `data-mail-operation-undo-url="/api/mail-operations/actions/a-move/undo"`, `data-mail-operation-retry-url="/api/mail-operations/actions/a-move/retry"`, "3 messages"},
		"a-empty": {"Empty Trash", "Queued"},
		"a-rule":  {"Add the label “Reading” to 1 message", "Rule “Newsletters” · ", "2 accounts"},
		"a-gone":  {"Move 2 messages to a deleted folder", "A rule that was deleted · "},
	} {
		row := groupRowHTML(t, html, id)
		for _, want := range wants {
			if !strings.Contains(row, want) {
				t.Fatalf("group %s lacks %q:\n%s", id, want, row)
			}
		}
	}
	empty := groupRowHTML(t, html, "a-empty")
	if strings.Contains(empty, "data-mail-operation-undo") || strings.Contains(empty, "data-mail-operation-retry") || strings.Contains(empty, "data-mail-operation-group-problems") {
		t.Fatalf("a group with nothing wrong offers Undo or Retry:\n%s", empty)
	}
	// A closed group loads its messages when first opened.
	if !strings.Contains(empty, `hx-get="/settings/operations/actions/a-empty"`) || !strings.Contains(empty, `data-tui-collapsible-state="closed"`) {
		t.Fatalf("closed group can't load its messages:\n%s", empty)
	}
	// Groups that need attention come first.
	if strings.Index(html, `data-mail-operation-group-row="a-move"`) > strings.Index(html, `data-mail-operation-group-row="a-empty"`) {
		t.Fatalf("the group that needs attention isn't first")
	}
}

func TestOpenMailOperationGroupListsItsMessages(t *testing.T) {
	queued := time.Date(2026, time.November, 18, 14, 55, 0, 0, time.UTC)
	change := func(id, subject, from string, parked bool) models.MailOperationGroupMessage {
		return models.MailOperationGroupMessage{
			Operation: models.MailOperationSummary{ID: id, Type: models.MailOperationMessageMutation, AccountEmail: "alex@example.com",
				Operation: "move", FolderID: "f-inbox", DestinationFolderID: "f-archive", State: "failed", CreatedAt: queued,
				ActionID: "a-move", ActionKind: "move", ActionSource: "you", Parked: parked, CanUndo: parked,
				ParkedReason: models.MailOperationParkedFolderRemoved, LastError: "folder removed remotely (imap discovery)"},
			Subject: subject, FromEmail: from, FolderName: "Inbox", FolderRole: "inbox", DestinationName: "Archive", DestinationRole: "archive",
		}
	}
	group := models.MailOperationGroup{ActionID: "a-move", ActionKind: "move", ActionSource: "you", AccountEmail: "alex@example.com",
		Accounts: 1, Messages: 2, Jobs: 2, Parked: 2, CreatedAt: queued, UpdatedAt: queued, ChangesLoaded: true,
		Changes: []models.MailOperationGroupMessage{change("message_mutation:1", "Lunch on Friday?", "sam@example.com", true), change("message_mutation:2", "", "", true)}}
	var out bytes.Buffer
	if err := SettingsOperationsContent(models.MailOperationsList{Groups: []models.MailOperationGroup{group}}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, `data-tui-collapsible-state="open"`) || strings.Contains(html, `hx-get="/settings/operations/actions/`) {
		t.Fatalf("an open group should come back open with its messages, without loading them again")
	}
	for _, want := range []string{"Lunch on Friday?", "sam@example.com", "A message Gofer no longer has",
		`data-mail-operation-undo-url="/api/mail-operations/message_mutation:1/undo"`, `data-mail-operation-details="message_mutation:1"`,
		"You · Move · "} {
		if !strings.Contains(html, want) {
			t.Fatalf("open group lacks %q", want)
		}
	}
	// Every change moves between the same folders, so the rows don't repeat
	// them; the title already says.
	if strings.Contains(html, "Inbox → Archive") {
		t.Fatalf("rows repeat the folders every change shares")
	}

	out.Reset()
	other := change("message_mutation:3", "Weekend plans", "sam@example.com", false)
	other.Operation.DestinationFolderID, other.DestinationName, other.DestinationRole = "f-projects", "Projects", "custom"
	if err := MailOperationGroupChanges([]models.MailOperationGroupMessage{change("message_mutation:1", "Lunch on Friday?", "sam@example.com", true), other}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Inbox → Archive") || !strings.Contains(out.String(), "Inbox → Projects") {
		t.Fatalf("changes to different folders should say where each goes:\n%s", out.String())
	}
}

// groupRowHTML is the markup of one group row of the list.
func groupRowHTML(t *testing.T, html, actionID string) string {
	t.Helper()
	start := strings.Index(html, `data-mail-operation-group-row="`+actionID+`"`)
	if start < 0 {
		t.Fatalf("group %s not rendered", actionID)
	}
	end := strings.Index(html[start+1:], `class="settings-item`)
	if end < 0 {
		return html[start:]
	}
	return html[start : start+1+end]
}
