package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestSidebarFolderTreeCanHideUnifiedFolders(t *testing.T) {
	accounts := []models.Account{{
		ID:               "acc",
		Name:             "Personal",
		EmailSyncEnabled: true,
		Folders: []models.Folder{{
			ID:   "acc-inbox",
			Name: "Inbox",
			Icon: "inbox",
			Role: "inbox",
		}},
	}}

	var visible bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-inbox", nil).Render(context.Background(), &visible); err != nil {
		t.Fatalf("SidebarFolderTree.Render() visible error = %v", err)
	}
	if !strings.Contains(visible.String(), "Unified folders") {
		t.Fatalf("default sidebar missing unified folders: %s", visible.String())
	}

	var hidden bytes.Buffer
	settings := map[string]string{"unified_folders_enabled": "false"}
	if err := SidebarFolderTree(accounts, "acc-inbox", settings).Render(context.Background(), &hidden); err != nil {
		t.Fatalf("SidebarFolderTree.Render() hidden error = %v", err)
	}
	html := hidden.String()
	if strings.Contains(html, "Unified folders") {
		t.Fatalf("disabled sidebar still rendered unified folders: %s", html)
	}
	if !strings.Contains(html, "Personal") {
		t.Fatalf("disabled sidebar should still render account folders: %s", html)
	}
}

func TestSidebarFolderTreeCanHideIndividualUnifiedFolders(t *testing.T) {
	accounts := []models.Account{{
		ID:               "acc",
		Name:             "Personal",
		EmailSyncEnabled: true,
		Folders: []models.Folder{
			{
				ID:   "acc-inbox",
				Name: "Inbox",
				Icon: "inbox",
				Role: "inbox",
			},
			{
				ID:   "acc-sent",
				Name: "Sent",
				Icon: "send",
				Role: "sent",
			},
		},
	}}

	var hiddenInbox bytes.Buffer
	settings := map[string]string{"unified_folder_inbox_enabled": "false"}
	if err := SidebarFolderTree(accounts, "acc-inbox", settings).Render(context.Background(), &hiddenInbox); err != nil {
		t.Fatalf("SidebarFolderTree.Render() hidden inbox error = %v", err)
	}
	html := hiddenInbox.String()
	if strings.Contains(html, `hx-get="/folder/inbox"`) {
		t.Fatalf("disabled unified inbox still rendered: %s", html)
	}
	if !strings.Contains(html, `hx-get="/folder/sent"`) {
		t.Fatalf("enabled unified sent should still render: %s", html)
	}
	if !strings.Contains(html, `hx-get="/folder/acc-inbox"`) {
		t.Fatalf("disabled unified inbox should not hide account inbox: %s", html)
	}

	allDisabled := map[string]string{
		"unified_folder_inbox_enabled":     "false",
		"unified_folder_starred_enabled":   "false",
		"unified_folder_sent_enabled":      "false",
		"unified_folder_drafts_enabled":    "false",
		"unified_folder_scheduled_enabled": "false",
		"unified_folder_archive_enabled":   "false",
		"unified_folder_spam_enabled":      "false",
		"unified_folder_trash_enabled":     "false",
	}
	var emptyUnified bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-inbox", allDisabled).Render(context.Background(), &emptyUnified); err != nil {
		t.Fatalf("SidebarFolderTree.Render() all disabled error = %v", err)
	}
	html = emptyUnified.String()
	if strings.Contains(html, "Unified folders") {
		t.Fatalf("all disabled unified section still rendered: %s", html)
	}
	if !strings.Contains(html, "Personal") {
		t.Fatalf("all disabled unified section should keep account folders: %s", html)
	}
}
