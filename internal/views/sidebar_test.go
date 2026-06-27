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
	if err := SidebarFolderTree(accounts, "acc-inbox", nil, 0, models.EmailFilters{}).Render(context.Background(), &visible); err != nil {
		t.Fatalf("SidebarFolderTree.Render() visible error = %v", err)
	}
	if !strings.Contains(visible.String(), "Unified folders") {
		t.Fatalf("default sidebar missing unified folders: %s", visible.String())
	}

	var hidden bytes.Buffer
	settings := map[string]string{"unified_folders_enabled": "false"}
	if err := SidebarFolderTree(accounts, "acc-inbox", settings, 0, models.EmailFilters{}).Render(context.Background(), &hidden); err != nil {
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
	if err := SidebarFolderTree(accounts, "acc-inbox", settings, 0, models.EmailFilters{}).Render(context.Background(), &hiddenInbox); err != nil {
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
		"unified_folder_inbox_enabled":   "false",
		"unified_folder_starred_enabled": "false",
		"unified_folder_sent_enabled":    "false",
		"unified_folder_drafts_enabled":  "false",
		"unified_folder_archive_enabled": "false",
		"unified_folder_spam_enabled":    "false",
		"unified_folder_trash_enabled":   "false",
	}
	var emptyUnified bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-inbox", allDisabled, 0, models.EmailFilters{}).Render(context.Background(), &emptyUnified); err != nil {
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

func TestUnifiedFoldersApplyAccountSettings(t *testing.T) {
	accounts := []models.Account{
		{
			ID:               "acc-a",
			Name:             "Primary",
			EmailSyncEnabled: true,
			Folders: []models.Folder{{
				ID:     "acc-a-inbox",
				Name:   "Inbox",
				Icon:   "inbox",
				Role:   "inbox",
				Unread: 3,
			}},
		},
		{
			ID:               "acc-b",
			Name:             "Archive",
			EmailSyncEnabled: true,
			Folders: []models.Folder{{
				ID:     "acc-b-inbox",
				Name:   "Inbox",
				Icon:   "inbox",
				Role:   "inbox",
				Unread: 5,
			}},
		},
	}
	settings := map[string]string{"unified_folder_inbox_account_acc-b_enabled": "false"}

	folders := unifiedFolders(accounts, settings)
	var inbox *models.Folder
	for i := range folders {
		if folders[i].ID == "inbox" {
			inbox = &folders[i]
			break
		}
	}
	if inbox == nil {
		t.Fatalf("unified inbox should render with included accounts: %#v", folders)
	}
	if inbox.Unread != 3 {
		t.Fatalf("unified inbox unread = %d, want only included account", inbox.Unread)
	}

	settings["unified_folder_inbox_account_acc-a_enabled"] = "false"
	folders = unifiedFolders(accounts, settings)
	for _, folder := range folders {
		if folder.ID == "inbox" {
			t.Fatalf("unified inbox should not render when all accounts are excluded: %#v", folders)
		}
	}
}

func TestSidebarFolderTreeShowsScheduledOnlyWithPendingCount(t *testing.T) {
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

	var empty bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-inbox", nil, 0, models.EmailFilters{}).Render(context.Background(), &empty); err != nil {
		t.Fatalf("SidebarFolderTree.Render() no scheduled error = %v", err)
	}
	if strings.Contains(empty.String(), `hx-get="/folder/scheduled"`) {
		t.Fatalf("scheduled folder rendered without pending messages: %s", empty.String())
	}

	var scheduled bytes.Buffer
	settings := map[string]string{"unified_folders_enabled": "false"}
	if err := SidebarFolderTree(accounts, "scheduled", settings, 2, models.EmailFilters{}).Render(context.Background(), &scheduled); err != nil {
		t.Fatalf("SidebarFolderTree.Render() scheduled error = %v", err)
	}
	html := scheduled.String()
	for _, want := range []string{`hx-get="/folder/scheduled"`, "Scheduled", ">2<"} {
		if !strings.Contains(html, want) {
			t.Fatalf("scheduled sidebar item missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "Unified folders") {
		t.Fatalf("scheduled folder should not depend on unified folders being enabled: %s", html)
	}
}

func TestSidebarFolderTreeHidesDeletingAccounts(t *testing.T) {
	accounts := []models.Account{
		{
			ID:               "acc-active",
			Name:             "Active",
			EmailSyncEnabled: true,
			Folders: []models.Folder{{
				ID:   "acc-active-inbox",
				Name: "Inbox",
				Icon: "inbox",
				Role: "inbox",
			}},
		},
		{
			ID:               "acc-deleting",
			Name:             "Deleting Gmail",
			IsDeleting:       true,
			EmailSyncEnabled: true,
			EmailSyncError:   "stale sync error",
			Folders: []models.Folder{{
				ID:     "acc-deleting-inbox",
				Name:   "Old Inbox",
				Icon:   "inbox",
				Role:   "inbox",
				Unread: 99,
			}},
		},
	}

	var buf bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-active-inbox", nil, 0, models.EmailFilters{}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "Active") {
		t.Fatalf("active account missing from sidebar: %s", html)
	}
	for _, unwanted := range []string{"Deleting Gmail", "Old Inbox", "stale sync error", `data-sidebar-account="acc-deleting"`} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("deleting account content %q rendered in sidebar: %s", unwanted, html)
		}
	}
}

func TestSidebarFolderTreeRendersUnifiedAndAccountTags(t *testing.T) {
	accounts := []models.Account{
		{
			ID:               "acc-a",
			Name:             "Primary",
			EmailSyncEnabled: true,
			Folders: []models.Folder{{
				ID:   "acc-a-inbox",
				Name: "Inbox",
				Icon: "inbox",
				Role: "inbox",
			}},
			Labels: []models.Label{
				{AccountID: "acc-a", Name: "Projects"},
				{AccountID: "acc-a", Name: "Invoices"},
			},
		},
		{
			ID:               "acc-b",
			Name:             "Work",
			EmailSyncEnabled: true,
			Folders: []models.Folder{{
				ID:   "acc-b-inbox",
				Name: "Inbox",
				Icon: "inbox",
				Role: "inbox",
			}},
			Labels: []models.Label{
				{AccountID: "acc-b", Name: "Projects"},
			},
		},
	}

	var buf bytes.Buffer
	if err := SidebarFolderTree(accounts, "inbox", nil, 0, models.EmailFilters{SidebarTag: "Projects"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		"Tags",
		`data-sidebar-tag-group="__unified__" data-sidebar-tag-active data-sidebar-tag-collapsed="false"`,
		`data-sidebar-tag-toggle="__unified__" aria-expanded="true"`,
		`class="sidebar-tag-children-inner pl-5 space-y-px"`,
		`data-sidebar-tag-label="Projects"`,
		`hx-get="/folder/inbox?tag=Projects"`,
		`hx-get="/folder/inbox?tag=Projects&amp;tag_account_id=acc-a"`,
		`hx-get="/folder/inbox?tag=Invoices&amp;tag_account_id=acc-a"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("tag sidebar missing %q: %s", want, html)
		}
	}
	if strings.Count(html, `hx-get="/folder/inbox?tag=Projects"`) != 1 {
		t.Fatalf("unified tags should dedupe labels by name: %s", html)
	}
	if !strings.Contains(html, `data-sidebar-account="__unified__" data-sidebar-account-active`) {
		t.Fatalf("unified tag filter should mark unified section active: %s", html)
	}
}

func TestSidebarFolderTreeMarksAccountTagActive(t *testing.T) {
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
		Labels: []models.Label{{AccountID: "acc", Name: "Projects"}},
	}}

	var buf bytes.Buffer
	filters := models.EmailFilters{SidebarTag: "Projects", SidebarTagAccountID: "acc"}
	if err := SidebarFolderTree(accounts, "acc-inbox", nil, 0, filters).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `data-sidebar-account="acc" data-sidebar-account-active`) {
		t.Fatalf("account tag filter should mark account section active: %s", html)
	}
	if !strings.Contains(html, `hx-get="/folder/acc-inbox?tag=Projects&amp;tag_account_id=acc"`) {
		t.Fatalf("account tag filter should target active account folder: %s", html)
	}
	if !strings.Contains(html, `bg-sidebar-accent text-sidebar-primary font-medium`) {
		t.Fatalf("active account tag should use active sidebar styling: %s", html)
	}
}

func TestSidebarFolderTreeDoesNotMarkAdvancedLabelFilterAsTagActive(t *testing.T) {
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
		Labels: []models.Label{{AccountID: "acc", Name: "Projects"}},
	}}

	var buf bytes.Buffer
	filters := models.EmailFilters{Label: "Projects", AccountID: "acc"}
	if err := SidebarFolderTree(accounts, "unrelated-folder", nil, 0, filters).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	if strings.Contains(html, `data-sidebar-account="acc" data-sidebar-account-active`) {
		t.Fatalf("advanced label filter should not mark account tag section active: %s", html)
	}
	if strings.Contains(html, `data-sidebar-account="__unified__" data-sidebar-account-active`) {
		t.Fatalf("advanced label filter should not mark unified tag section active: %s", html)
	}
}

func TestSidebarFolderTreeCollapsesInactiveTagsFromSettings(t *testing.T) {
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
		Labels: []models.Label{{AccountID: "acc", Name: "Projects"}},
	}}
	settings := map[string]string{"sidebar_tag_group_collapsed": `{"acc":true}`}

	var collapsed bytes.Buffer
	if err := SidebarFolderTree(accounts, "inbox", settings, 0, models.EmailFilters{}).Render(context.Background(), &collapsed); err != nil {
		t.Fatalf("SidebarFolderTree.Render() collapsed error = %v", err)
	}
	html := collapsed.String()
	if !strings.Contains(html, `data-sidebar-tag-group="acc" data-sidebar-tag-collapsed="true"`) {
		t.Fatalf("inactive tag group should render collapsed from settings: %s", html)
	}
	if !strings.Contains(html, `data-sidebar-tag-toggle="acc" aria-expanded="false"`) {
		t.Fatalf("collapsed tag group should expose aria-expanded=false: %s", html)
	}

	var active bytes.Buffer
	filters := models.EmailFilters{SidebarTag: "Projects", SidebarTagAccountID: "acc"}
	if err := SidebarFolderTree(accounts, "inbox", settings, 0, filters).Render(context.Background(), &active); err != nil {
		t.Fatalf("SidebarFolderTree.Render() active error = %v", err)
	}
	html = active.String()
	if !strings.Contains(html, `data-sidebar-tag-group="acc" data-sidebar-tag-active data-sidebar-tag-collapsed="false"`) {
		t.Fatalf("active tag group should force open despite saved collapse state: %s", html)
	}
}

func TestSidebarFolderTreeCollapsesFolderChildrenFromSettings(t *testing.T) {
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
				ID:   "acc-parent",
				Name: "Parent",
				Icon: "folder",
				Children: []models.Folder{{
					ID:   "acc-child",
					Name: "Child",
					Icon: "folder",
				}},
			},
		},
	}}
	settings := map[string]string{
		"unified_folders_enabled":  "false",
		"sidebar_folder_collapsed": `{"acc:acc-parent":true}`,
	}

	var collapsed bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-inbox", settings, 0, models.EmailFilters{}).Render(context.Background(), &collapsed); err != nil {
		t.Fatalf("SidebarFolderTree.Render() collapsed error = %v", err)
	}
	html := collapsed.String()
	for _, want := range []string{
		`data-sidebar-folder="acc:acc-parent" data-sidebar-folder-collapsed="true"`,
		`hx-get="/folder/acc-parent"`,
		`data-sidebar-folder-row data-sidebar-folder-toggle="acc:acc-parent" aria-label="Toggle Parent" aria-expanded="false"`,
		`class="sidebar-folder-children-inner pl-5 space-y-px"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("collapsed folder group missing %q: %s", want, html)
		}
	}

	var active bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-child", settings, 0, models.EmailFilters{}).Render(context.Background(), &active); err != nil {
		t.Fatalf("SidebarFolderTree.Render() active error = %v", err)
	}
	html = active.String()
	for _, want := range []string{
		`data-sidebar-folder="acc:acc-parent" data-sidebar-folder-active data-sidebar-folder-collapsed="false"`,
		`data-sidebar-folder-row data-sidebar-folder-toggle="acc:acc-parent" aria-label="Toggle Parent" aria-expanded="true"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("active child folder should force parent open, missing %q: %s", want, html)
		}
	}

	var parentActive bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-parent", settings, 0, models.EmailFilters{}).Render(context.Background(), &parentActive); err != nil {
		t.Fatalf("SidebarFolderTree.Render() parent active error = %v", err)
	}
	html = parentActive.String()
	if !strings.Contains(html, `class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] transition-all duration-150 bg-sidebar-accent text-sidebar-primary font-medium" data-sidebar-folder-row`) {
		t.Fatalf("active parent folder should style the whole row including the toggle: %s", html)
	}
}
