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
	status := models.MailOperationsStatus{Total: 3, Operations: []models.MailOperationSummary{
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
		if err := SettingsOperationsContent(models.MailOperationsStatus{Retryable: tt.retryable}).Render(context.Background(), &out); err != nil {
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
