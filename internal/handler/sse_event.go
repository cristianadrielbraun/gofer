package handler

import (
	"encoding/json"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"net/http"
)

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event mail.Event) {
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
