package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestSyncIdleLimitNoteExplainsWhatWasLearned(t *testing.T) {
	refused := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	stable := time.Date(2026, 11, 10, 13, 0, 0, 0, time.Local)
	limit := models.IdleLimitStatus{Learned: true, IdleConnectionLimit: models.IdleConnectionLimit{Limit: 3, FailedAt: 5, FailedTime: refused, StableAt: 3, StableSince: stable}}
	var out strings.Builder
	if err := syncIdleLimitNote(limit).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`data-idle-limit="3"`,
		"server has allowed 3.",
		"It refused a connection on November 10, 2026, when Gofer asked for 5.",
		"3 have worked since November 10, 2026.",
		"data-idle-limit-polled",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("note lacks %q:\n%s", want, html)
		}
	}
}

func TestSyncIdleLimitNoteBeforeAnythingWasLearned(t *testing.T) {
	var out strings.Builder
	if err := syncIdleLimitNote(models.IdleLimitStatus{IdleConnectionLimit: models.IdleConnectionLimit{Limit: 3}}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, "Gofer starts this account at 3") || strings.Contains(html, "refused a connection on") || strings.Contains(html, "worked since") {
		t.Fatalf("note before anything was learned:\n%s", html)
	}
}
