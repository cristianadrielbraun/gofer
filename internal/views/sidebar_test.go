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

// The unified folders and tags the user put in order show first, in it; the
// rest keep their own order after them.
func TestUnifiedFoldersAndTagsFollowTheUsersOrder(t *testing.T) {
	accounts := []models.Account{{
		ID:               "acc-a",
		EmailSyncEnabled: true,
		Folders: []models.Folder{
			{ID: "a-inbox", Role: "inbox"}, {ID: "a-sent", Role: "sent"},
			{ID: "a-archive", Role: "archive"}, {ID: "a-trash", Role: "trash"},
		},
		Labels: []models.Label{{ID: "l1", Name: "Art"}, {ID: "l2", Name: "Bills"}, {ID: "l3", Name: "Cats"}},
	}}
	settings := map[string]string{
		models.VirtualFolderOrderKey("trash"):                  "0",
		models.VirtualFolderOrderKey("archive"):                "1",
		models.UnifiedTagOrderKey(models.UnifiedTagID("Cats")): "0",
	}
	var ids []string
	for _, folder := range unifiedFolders(accounts, settings) {
		ids = append(ids, folder.ID)
	}
	if want := []string{"trash", "archive", "inbox", "starred", "sent"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("unified folders = %v, want %v", ids, want)
	}
	var names []string
	for _, label := range unifiedSidebarLabels(accounts, settings) {
		names = append(names, label.Name)
	}
	if want := "Cats,Art,Bills"; strings.Join(names, ",") != want {
		t.Fatalf("unified tags = %v, want %s", names, want)
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
	filters := models.EmailFilters{Tag: "Projects", SearchScope: &models.MailSearchScope{Kind: models.MailSearchAll, ExcludeSpamTrash: true}}
	if err := SidebarFolderTree(accounts, "search", nil, 0, filters).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		"Tags",
		`data-sidebar-tag-group="__unified__" data-sidebar-tag-active data-sidebar-tag-collapsed="false"`,
		`data-sidebar-tag-toggle="__unified__" aria-expanded="true"`,
		`class="sidebar-tag-children-inner pl-5 space-y-px"`,
		`data-sidebar-tag-label="Projects"`,
		`href="/mail/search?exclude_spam_trash=1&amp;scope=all&amp;search_from=inbox&amp;tag=Projects"`,
		`href="/mail/search?exclude_spam_trash=1&amp;scope=account&amp;scope_account_id=acc-a&amp;search_from=inbox&amp;tag=Projects&amp;tag_account_id=acc-a"`,
		`href="/mail/search?exclude_spam_trash=1&amp;scope=account&amp;scope_account_id=acc-a&amp;search_from=inbox&amp;tag=Invoices&amp;tag_account_id=acc-a"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("tag sidebar missing %q: %s", want, html)
		}
	}
	if strings.Count(html, `href="/mail/search?exclude_spam_trash=1&amp;scope=all&amp;search_from=inbox&amp;tag=Projects"`) != 1 {
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
	filters := models.EmailFilters{Tag: "Projects", TagAccountID: "acc", SearchScope: &models.MailSearchScope{Kind: models.MailSearchAccount, AccountID: "acc", ExcludeSpamTrash: true}}
	if err := SidebarFolderTree(accounts, "search", nil, 0, filters).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `data-sidebar-account="acc" data-sidebar-account-active`) {
		t.Fatalf("account tag filter should mark account section active: %s", html)
	}
	if !strings.Contains(html, `href="/mail/search?exclude_spam_trash=1&amp;scope=account&amp;scope_account_id=acc&amp;search_from=inbox&amp;tag=Projects&amp;tag_account_id=acc"`) {
		t.Fatalf("account tag should list the tag across the account's folders: %s", html)
	}
	if !strings.Contains(html, `bg-sidebar-accent text-sidebar-primary font-medium`) {
		t.Fatalf("active account tag should use active sidebar styling: %s", html)
	}
}

func TestSidebarFolderTreeIncludesProviderIdentityForAccountTag(t *testing.T) {
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
		Labels: []models.Label{{
			AccountID:    "acc",
			Name:         "Work",
			ProviderID:   "$label2",
			ProviderType: "imap_keyword",
		}},
	}}

	var buf bytes.Buffer
	if err := SidebarFolderTree(accounts, "acc-inbox", nil, 0, models.EmailFilters{}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	want := `href="/mail/search?exclude_spam_trash=1&amp;scope=account&amp;scope_account_id=acc&amp;search_from=acc-inbox&amp;tag=Work&amp;tag_account_id=acc&amp;tag_provider_id=%24label2&amp;tag_provider_type=imap_keyword"`
	if !strings.Contains(html, want) {
		t.Fatalf("account tag link missing provider identity %q: %s", want, html)
	}
}

func TestSidebarFolderTreeMarksTagFilterAsUnifiedTagActive(t *testing.T) {
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
	filters := models.EmailFilters{Tag: "Projects", AccountID: "acc", SearchScope: &models.MailSearchScope{Kind: models.MailSearchAll, ExcludeSpamTrash: true}}
	if err := SidebarFolderTree(accounts, "search", nil, 0, filters).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	if strings.Contains(html, `data-sidebar-account="acc" data-sidebar-account-active`) {
		t.Fatalf("unscoped tag filter should not mark account tag section active: %s", html)
	}
	if !strings.Contains(html, `data-sidebar-account="__unified__" data-sidebar-account-active`) {
		t.Fatalf("unscoped tag filter should mark unified tag section active: %s", html)
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
	filters := models.EmailFilters{Tag: "Projects", TagAccountID: "acc", SearchScope: &models.MailSearchScope{Kind: models.MailSearchAccount, AccountID: "acc", ExcludeSpamTrash: true}}
	if err := SidebarFolderTree(accounts, "search", settings, 0, filters).Render(context.Background(), &active); err != nil {
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
	if !strings.Contains(html, `class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] transition-colors duration-150 bg-sidebar-accent text-sidebar-primary font-medium" data-sidebar-folder-row`) {
		t.Fatalf("active parent folder should style the whole row including the toggle: %s", html)
	}
}

// An account with no Archive folder is marked, so its mail's Archive buttons
// can say so before anything is tried.
func TestSidebarMarksAccountsWithoutArchive(t *testing.T) {
	accounts := []models.Account{
		{ID: "no-archive", Name: "Work", EmailSyncEnabled: true, MissingFolderRoles: []string{"junk", "archive"}},
		{ID: "has-archive", Name: "Home", EmailSyncEnabled: true, MissingFolderRoles: []string{"junk"}},
	}
	var out bytes.Buffer
	if err := SidebarFolderTree(accounts, "", nil, 0, models.EmailFilters{}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for id, marked := range map[string]bool{"no-archive": true, "has-archive": false} {
		start := strings.Index(html, `data-sidebar-account="`+id+`"`)
		if start == -1 {
			t.Fatalf("account %s missing:\n%s", id, html)
		}
		tag := html[start : start+strings.Index(html[start:], ">")]
		if got := strings.Contains(tag, "data-sidebar-account-no-archive"); got != marked {
			t.Errorf("account %s marked %t, want %t: %s", id, got, marked, tag)
		}
	}
}

func TestVirtualViewAppearancesPreserveVisibilityAndCounts(t *testing.T) {
	accounts := []models.Account{{ID: "acc", EmailSyncEnabled: true, Folders: []models.Folder{{ID: "acc-inbox", Name: "Inbox", Icon: "inbox", Role: "inbox", Unread: 3, Color: "#ef4444"}}}}
	settings := map[string]string{models.VirtualFolderColorKey("inbox"): "#3b82f6", models.VirtualFolderStyleKey("inbox"): "filled", models.VirtualFolderColorKey("scheduled"): "#16a34a", models.VirtualFolderStyleKey("scheduled"): "filled"}
	folders := unifiedFolders(accounts, settings)
	for _, folder := range folders {
		if folder.ID == "inbox" && (folder.Color != "#3b82f6" || folder.IconStyle != "filled" || folder.Unread != 3 || !folder.IsVirtual) {
			t.Fatalf("unified inbox = %+v", folder)
		}
	}
	var out bytes.Buffer
	if err := SidebarScheduledFolder("scheduled", 2, settings).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-folder-appearance-virtual`, `data-folder-icon-style="filled"`, `color:#16a34a`, `>2<`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Scheduled missing %q: %s", want, out.String())
		}
	}
	settings["unified_folder_inbox_enabled"] = "false"
	for _, folder := range unifiedFolders(accounts, settings) {
		if folder.ID == "inbox" {
			t.Fatal("appearance preference made hidden inbox visible")
		}
	}
	out.Reset()
	if err := SettingsSyncTab(models.SyncSettings{}, settings).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), `data-folder-action="folder-appearance"`); got != 8 {
		t.Fatalf("settings appearance controls = %d, want 8", got)
	}
	if !strings.Contains(out.String(), `data-folder-id="scheduled"`) || !strings.Contains(out.String(), `data-folder-id="inbox"`) {
		t.Fatal("hidden views must remain configurable in Settings")
	}
}

// Filtering a folder by a tag narrows that folder; it is not the tag's own
// list, so the folder stays the marked item.
func TestSidebarFolderTreeFolderTagFilterLeavesTagInactive(t *testing.T) {
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
	filters := models.EmailFilters{Tag: "Projects", TagAccountID: "acc"}
	if err := SidebarFolderTree(accounts, "acc-inbox", nil, 0, filters).Render(context.Background(), &buf); err != nil {
		t.Fatalf("SidebarFolderTree.Render() error = %v", err)
	}
	html := buf.String()
	if strings.Contains(html, "data-sidebar-tag-active") {
		t.Fatalf("a folder filtered by a tag should not mark the tag group active: %s", html)
	}
	label := strings.Index(html, `data-sidebar-tag-label="Projects"`)
	if label < 0 {
		t.Fatalf("tag link missing: %s", html)
	}
	start := strings.LastIndex(html[:label], "<a")
	end := label + strings.Index(html[label:], ">")
	if strings.Contains(html[start:end], "bg-sidebar-accent text-sidebar-primary") {
		t.Fatalf("a folder filtered by a tag should not mark the tag link active: %s", html)
	}
}

// With the unified Inbox in the sidebar, the mail page opens it; without it,
// the first folder of the first account the sidebar shows.
func TestDefaultMailFolderIsTheFirstFolderShown(t *testing.T) {
	accounts := []models.Account{
		{ID: "hidden", EmailSyncEnabled: true, Hidden: true, Folders: []models.Folder{{ID: "hidden-inbox", Role: "inbox"}}},
		{ID: "acc-a", EmailSyncEnabled: true, Folders: []models.Folder{{ID: "a-archive", Role: "archive"}, {ID: "a-inbox", Role: "inbox"}}},
	}
	if got := DefaultMailFolder(accounts, map[string]string{}); got != "inbox" {
		t.Fatalf("with unified folders = %q, want the unified inbox", got)
	}
	for name, settings := range map[string]map[string]string{
		"unified folders off": {"unified_folders_enabled": "false"},
		"unified inbox off":   {"unified_folder_inbox_enabled": "false"},
	} {
		if got := DefaultMailFolder(accounts, settings); got != "a-archive" {
			t.Fatalf("%s = %q, want the first account's first folder", name, got)
		}
	}
}
