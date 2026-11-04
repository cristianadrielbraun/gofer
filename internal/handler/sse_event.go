package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/mail"
)

// writeSSEEvent sends one event to a browser. Events are written in the
// background, so the English texts they carry are put in the language of the
// page receiving them (ctx is its request) here.
func writeSSEEvent(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, event mail.Event) {
	m := map[string]any{
		"type":       string(event.Type),
		"account_id": event.AccountID,
		"folder_id":  event.FolderID,
	}
	for key, value := range event.Payload {
		m[key] = value
	}
	if event.FolderRole != "" {
		m["folder_role"] = event.FolderRole
	}
	if event.Status != "" {
		m["status"] = event.Status
	}
	if event.Error != "" {
		m["error"] = event.Error
	}
	translateSSEText(ctx, m)
	includeProgress := event.Type == mail.EventSyncStarted || event.Type == mail.EventSyncProgress || event.Type == mail.EventSyncComplete ||
		event.Type == mail.EventManualSyncStarted || event.Type == mail.EventManualSyncProgress || event.Type == mail.EventManualSyncComplete ||
		event.Type == mail.EventScheduledSyncStarted || event.Type == mail.EventScheduledSyncProgress || event.Type == mail.EventScheduledSyncComplete
	if event.Current > 0 || includeProgress {
		m["current"] = event.Current
	}
	if event.Total > 0 || includeProgress {
		m["total"] = event.Total
	}
	if event.AvatarHash != "" {
		m["avatar_hash"] = event.AvatarHash
	}
	if event.AvatarURL != "" {
		m["avatar_url"] = event.AvatarURL
	}
	if event.AvatarDataURL != "" {
		m["avatar_data_url"] = event.AvatarDataURL
	}
	data, _ := json.Marshal(m)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
	flusher.Flush()
}

// translateSSEText puts the texts an event carries for people in ctx's
// language: errors and contact activity messages known to Gofer, why a
// folder fell back to polling, and the Gmail send-as warning. Text from a mail server stays as it came.
func translateSSEText(ctx context.Context, m map[string]any) {
	if text, ok := m["error"].(string); ok && text != "" {
		m["error"] = serverText(ctx, text)
	}
	if text, ok := m["message"].(string); ok && text != "" {
		if translated := i18n.Stored(ctx, "admin.contacts.message.", text); translated != text {
			m["message"] = translated
		} else {
			m["message"] = serverText(ctx, text)
		}
	}
	if text, ok := m["reason"].(string); ok && text != "" {
		m["reason"] = i18n.Stored(ctx, "sync.fallback_reason.", text)
	}
	if text, ok := m["from_warning"].(string); ok && text != "" {
		m["from_warning"] = i18n.Stored(ctx, "mail.from_warning.", text)
	}
}
