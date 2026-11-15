package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func sharedSidebarAccount() models.Account {
	return models.Account{
		ID:               "acc",
		Name:             "Ana",
		EmailSyncEnabled: true,
		Folders: []models.Folder{
			{ID: "own-inbox", Name: "Inbox", Role: "inbox", Unread: 1},
			{ID: "own-sent", Name: "Sent", Role: "sent"},
			{ID: "support-inbox", Name: "Inbox", Role: "custom", SharedMailbox: "support@example.com", SharedRole: "inbox", Unread: 4,
				Children: []models.Folder{{ID: "support-orders", Name: "Orders", Role: "custom", SharedMailbox: "support@example.com", SharedRole: "custom", Unread: 2}}},
			{ID: "support-sent", Name: "Sent", Role: "custom", SharedMailbox: "support@example.com", SharedRole: "sent", Unread: 9},
			{ID: "sales-inbox", Name: "Inbox", Role: "custom", SharedMailbox: "sales-team@example.com", SharedRole: "inbox"},
			{ID: "sales-sent", Name: "Sent", Role: "custom", SharedMailbox: "sales-team@example.com", SharedRole: "sent"},
		},
	}
}

func TestSidebarSplitSharedFolders(t *testing.T) {
	own, shared := sidebarSplitSharedFolders(sharedSidebarAccount().Folders)
	if len(own) != 2 || own[0].ID != "own-inbox" || own[1].ID != "own-sent" {
		t.Fatalf("own = %+v", own)
	}
	if len(shared) != 2 {
		t.Fatalf("shared = %+v", shared)
	}
	support := shared[0]
	if support.Email != "support@example.com" || support.Name != "Support" || len(support.Folders) != 2 {
		t.Fatalf("support = %+v", support)
	}
	// Inbox and its subfolder count; Sent does not.
	if support.Unread != 6 {
		t.Fatalf("support unread = %d", support.Unread)
	}
	if shared[1].Name != "Sales Team" {
		t.Fatalf("sales = %+v", shared[1])
	}
}

func TestSidebarAccountSectionGroupsSharedMailboxes(t *testing.T) {
	var buf bytes.Buffer
	if err := SidebarAccountSection(sharedSidebarAccount(), "support-sent", nil, models.EmailFilters{}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{
		`data-sidebar-shared-mailboxes="acc"`,
		`data-sidebar-shared-mailbox="support@example.com"`,
		`data-sidebar-shared-mailbox="sales-team@example.com"`,
		"Shared mailboxes",
		"support@example.com",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sidebar is missing %q:\n%s", want, html)
		}
	}
	// Each shared Sent sits inside its own mailbox's entry.
	support := strings.Index(html, `data-sidebar-shared-mailbox="support@example.com"`)
	sales := strings.Index(html, `data-sidebar-shared-mailbox="sales-team@example.com"`)
	supportSent := strings.Index(html, `/folder/support-sent`)
	salesSent := strings.Index(html, `/folder/sales-sent`)
	ownSent := strings.Index(html, `/folder/own-sent`)
	if !(ownSent < support && support < supportSent && supportSent < sales && sales < salesSent) {
		t.Fatalf("folders out of place: own %d, support %d/%d, sales %d/%d", ownSent, support, supportSent, sales, salesSent)
	}
	// The group with the open folder is open.
	groupStart := strings.LastIndex(html[:supportSent], `data-sidebar-shared-mailbox="support@example.com"`)
	if !strings.Contains(html[groupStart:supportSent], `data-sidebar-folder-collapsed="false"`) {
		t.Fatal("the open folder's mailbox is folded")
	}
}

func TestSidebarSharedMailboxRemembersFolding(t *testing.T) {
	settings := map[string]string{
		"sidebar_folder_collapsed":      `{"acc:shared-mailbox:sales-team@example.com":true}`,
		"shared_mailboxes_show_address": "false",
	}
	var buf bytes.Buffer
	if err := SidebarAccountSection(sharedSidebarAccount(), "own-inbox", settings, models.EmailFilters{}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	sales := strings.Index(html, `data-sidebar-shared-mailbox="sales-team@example.com"`)
	if sales < 0 || !strings.Contains(html[sales:sales+300], `data-sidebar-folder-collapsed="true"`) {
		t.Fatalf("sales mailbox not folded: %s", html[sales:min(len(html), sales+300)])
	}
	if strings.Contains(html, "data-sidebar-shared-mailbox-address") {
		t.Fatal("address shown although turned off")
	}
}

func TestComposeFromMenuLeavesOutIdentitiesPutAside(t *testing.T) {
	accounts := []models.Account{{ID: "acc", Email: "ana@example.com", Identities: []models.Identity{
		{ID: "support", Email: "support@example.com", SharedMailbox: "support@example.com", SharedState: "active"},
		{ID: "sales", Email: "sales@example.com", SharedMailbox: "sales@example.com", SharedState: "disabled"},
		{ID: "old", Email: "old@example.com", SharedMailbox: "old@example.com", SharedState: "inactive"},
		{ID: "manual", Email: "team@example.com"},
	}}}
	var buf bytes.Buffer
	if err := composeFromMenuItems(accounts, "dialog").Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"support@example.com", "team@example.com"} {
		if !strings.Contains(html, `data-identity-email="`+want+`"`) {
			t.Errorf("From menu is missing %s", want)
		}
	}
	for _, gone := range []string{"sales@example.com", "old@example.com"} {
		if strings.Contains(html, `data-identity-email="`+gone+`"`) {
			t.Errorf("From menu offers %s, which is put aside", gone)
		}
	}
	if strings.Contains(html, "continue") {
		t.Error("the menu's markup has stray text")
	}
}
