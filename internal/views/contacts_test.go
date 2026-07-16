package views

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestContactsDetailShowsSyncQueuedNotice(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{ID: "contact-1", Name: "Jane", Email: "jane@example.com"}
	if err := ContactsDetail(&contact, nil, false, true, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactsDetail.Render() error = %v", err)
	}
	html := out.String()
	if !strings.Contains(html, "Sync queued") {
		t.Fatalf("rendered detail missing sync queued notice: %s", html)
	}
}

func TestContactActivityLabelsDescribeProfileTimestamps(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{CreatedAt: "Jan 2, 2025", UpdatedAt: "Mar 4, 2025"}
	if err := ContactReadOnlyActivityInfo(contact).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactReadOnlyActivityInfo.Render() error = %v", err)
	}
	html := out.String()
	for _, expected := range []string{"Added to Gofer", "Jan 2, 2025", "Contact updated", "Mar 4, 2025"} {
		if !strings.Contains(html, expected) {
			t.Fatalf("rendered activity section missing %q: %s", expected, html)
		}
	}
}

func TestContactReadOnlyViewLoadsRecentConversations(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{ID: "contact-1", Name: "Jane", Email: "jane@example.com"}
	if err := ContactReadOnlyView(contact, nil, false, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactReadOnlyView.Render() error = %v", err)
	}
	html := out.String()
	for _, expected := range []string{
		`data-contact-recent-activity-loading`,
		`hx-get="/contacts?contact=contact-1&amp;partial=activity"`,
		`>Recent activity<`,
		`Latest emails involving this contact.`,
		`>See all<`,
		`href="/?participant=jane%40example.com"`,
		`hx-get="/?participant=jane%40example.com"`,
		`data-sidebar-app-button="mail"`,
		`hx-select-oob="#sidebar-app-body:outerHTML,#sidebar-sync-controls:outerHTML,#mail-view:outerHTML,#app-pane-dialogs:outerHTML"`,
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("rendered contact view missing recent conversations marker %q: %s", expected, html)
		}
	}
}

func TestContactsDetailRendersGoferSyncHeaderAndFullWidthLocations(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{
		ID:               "contact-1",
		Name:             "Jane",
		Email:            "jane@example.com",
		GoferSyncEnabled: true,
		SaveTargets:      []string{"local", "account:account-1"},
		SyncStatus:       "pending",
	}
	profile := models.ContactProfile{
		ID:          "contact-1",
		Origin:      "manual",
		SyncEnabled: true,
		Cards:       []models.ContactCard{{Kind: "local"}},
		SyncMemberships: []models.ContactSyncMembership{{
			AccountID: "account-1",
			Enabled:   true,
		}},
	}
	accounts := []models.Account{{ID: "account-1", Email: "jane.account@example.com"}}
	if err := ContactsDetail(&contact, &profile, false, false, accounts).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactsDetail.Render() error = %v", err)
	}
	html := out.String()
	for _, expected := range []string{
		`data-contact-detail-id="contact-1"`,
		`>Gofer Sync<`,
		`data-contact-sync-enabled-status`,
		`border-emerald-500/20`,
		`data-contact-sync-now`,
		`animate-spin`,
		`Sync now</button>`,
		`disabled`,
		`>Sync locations<`,
		`lg:col-span-2`,
		`>Local<`,
		`>jane.account@example.com<`,
		`data-contact-sync-operation-status`,
		`>Sync pending<`,
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("rendered Gofer Sync section missing %q: %s", expected, html)
		}
	}
}

func TestExistingContactRendersFocusedEditDialog(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{ID: "contact-1", Name: "Jane Doe", Email: "jane@example.com"}
	profile := models.ContactProfile{ID: "contact-1", Origin: "observed", Cards: []models.ContactCard{{Kind: "local"}}}
	if err := ContactEditor(&contact, &profile, false, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactEditor.Render() error = %v", err)
	}
	html := out.String()
	for _, expected := range []string{
		`data-contact-edit-trigger`,
		`id="contact-edit-contact-1"`,
		`data-contact-edit-form`,
		`action="/api/contacts?id=contact-1"`,
		`Save changes`,
		`How this contact was first created`,
		`>Observed<`,
		`>Sync locations<`,
		`data-tui-selectbox-value="local"`,
		`name="sync_enabled"`,
		`data-contact-sync-enabled`,
		`data-contact-sync-targets`,
		`aria-disabled="true"`,
		`opacity-45`,
		`data-contact-avatar-editor`,
		`name="avatar_action"`,
		`data-contact-avatar-choose`,
		`title="Click to change profile picture"`,
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("rendered editor missing %q: %s", expected, html)
		}
	}
	if strings.Contains(html, "Choose a JPEG, PNG, or WebP image") {
		t.Fatalf("editor should not render the old standalone avatar section: %s", html)
	}

	dialogStart := strings.Index(html, `id="contact-edit-contact-1"`)
	if dialogStart < 0 {
		t.Fatal("edit dialog not found")
	}
	if strings.Contains(html[dialogStart:], ">Activity<") {
		t.Fatalf("edit dialog should not contain activity section: %s", html[dialogStart:])
	}
}

func TestContactEditSyncSwitchEnablesTargetSelector(t *testing.T) {
	var out bytes.Buffer
	contact := models.Contact{ID: "contact-1", Name: "Jane Doe", Email: "jane@example.com", GoferSyncEnabled: true}
	profile := models.ContactProfile{ID: "contact-1", Origin: "manual", SyncEnabled: true}
	if err := ContactEditSyncPanel(&contact, &profile, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("ContactEditSyncPanel.Render() error = %v", err)
	}
	html := out.String()
	if !strings.Contains(html, `name="sync_enabled"`) || !strings.Contains(html, `checked`) {
		t.Fatalf("enabled sync switch was not checked: %s", html)
	}
	if !strings.Contains(html, `data-contact-sync-targets`) || !strings.Contains(html, `aria-disabled="false"`) {
		t.Fatalf("enabled target selector was not interactive: %s", html)
	}
}

func TestContactSyncSetupDialogRendersDiscoveryAndConflictSteps(t *testing.T) {
	contact := models.Contact{ID: "contact-1", Name: "Jane", Email: "jane@example.com"}
	var searching bytes.Buffer
	if err := ContactSyncSetupDialog(models.ContactSyncSetup{Contact: contact, Phase: "searching", SearchMode: "automatic"}).Render(context.Background(), &searching); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Searching sync locations", "email addresses, phone numbers, and names", `data-contact-sync-setup-auto-search="/api/contacts/contact-1/sync-setup/findings?mode=automatic"`} {
		if !strings.Contains(searching.String(), expected) {
			t.Fatalf("searching dialog missing %q: %s", expected, searching.String())
		}
	}

	var discovery bytes.Buffer
	setup := models.ContactSyncSetup{
		Contact: contact,
		Phase:   "discover",
		Locations: []models.ContactSyncSetupLocation{{
			AccountID:  "acc",
			Label:      "account@example.com",
			Provider:   "gmail",
			Candidates: []models.ContactSyncSetupCandidate{{Key: "gmail:people/1", Name: "Jane", Email: "jane@example.com", Phone: "+420 123 456 789", MatchEmail: true, MatchPhone: true, MatchName: true}},
		}},
	}
	if err := ContactSyncSetupDialog(setup).Render(context.Background(), &discovery); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Set up Gofer Sync", "1 possible match", "Search manually", "Search this account", `data-contact-sync-location-search-url="/api/contacts/contact-1/sync-setup/findings?mode=custom&amp;account_id=acc"`, "Recommended", "Email + phone + name match", "None of these contacts are the same person", `id="contact-sync-setup-selection-contact-1"`, `form="contact-sync-setup-selection-contact-1"`, "Review values"} {
		if !strings.Contains(discovery.String(), expected) {
			t.Fatalf("discovery dialog missing %q: %s", expected, discovery.String())
		}
	}
	if strings.Contains(discovery.String(), "How should Gofer search?") {
		t.Fatalf("discovery dialog still contains the global search controls: %s", discovery.String())
	}

	var resolve bytes.Buffer
	setup.Phase = "resolve"
	setup.ConflictFields = []models.ContactField{
		{ID: "local-name", Kind: "name", Value: "Jane", NormalizedValue: "jane", IsPrimary: true, Source: "manual"},
		{ID: "remote-name", Kind: "name", Value: "Jane Remote", NormalizedValue: "jane remote", Source: "synced:acc"},
		{ID: "local-phone", Kind: "phone", Value: "+420 123 456 789", NormalizedValue: "+420123456789", IsPrimary: true, Source: "manual"},
		{ID: "remote-phone", Kind: "phone", Value: "+54 9 11 3118-0670", NormalizedValue: "+5491131180670", Source: "synced:acc"},
	}
	if err := ContactSyncSetupDialog(setup).Render(context.Background(), &resolve); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Step 2 of 2", `name="preferred_name"`, "Jane Remote", "Choose the primary name", "other values stop being active there", "Choose the primary phone", "Every other number shown below remains an additional phone number", "Use selected values and sync"} {
		if !strings.Contains(resolve.String(), expected) {
			t.Fatalf("resolve dialog missing %q: %s", expected, resolve.String())
		}
	}
}

func TestContactResolverGroupsEqualValuesAcrossSources(t *testing.T) {
	fields := []models.ContactField{
		{ID: "local-primary", ProfileID: "contact-1", Kind: "phone", Label: "mobile", Value: "+54 9 11 3118-0670", NormalizedValue: "5491131180670", IsPrimary: true, Source: "manual"},
		{ID: "canonical-primary", ProfileID: "contact-1", Kind: "phone", Label: "mobile", Value: "+54 9 11 3118-0670", NormalizedValue: "5491131180670", IsPrimary: true, Source: "canonical"},
		{ID: "gmail-primary", ProfileID: "contact-1", Kind: "phone", Label: "mobile", Value: "+54 9 11 3118-0670", NormalizedValue: "5491131180670", IsPrimary: true, Source: "synced:gmail"},
		{ID: "local-alternative", ProfileID: "contact-1", Kind: "phone", Label: "mobile", Value: "+420703979854", NormalizedValue: "420703979854", Source: "manual"},
		{ID: "canonical-alternative", ProfileID: "contact-1", Kind: "phone", Label: "mobile", Value: "+420703979854", NormalizedValue: "420703979854", Source: "canonical"},
		{ID: "gmail-alternative", ProfileID: "contact-1", Kind: "phone", Label: "work", Value: "+420703979854", NormalizedValue: "420703979854", Source: "synced:gmail"},
	}
	groups := contactResolverValueGroups(fields, "phone")
	if len(groups) != 2 {
		t.Fatalf("resolver groups = %#v, want two distinct phone values", groups)
	}
	if !groups[0].IsPrimary || len(groups[0].Sources) != 3 {
		t.Fatalf("primary group = %#v, want one bootstrap value with three stored sources", groups[0])
	}
	if len(groups[1].Labels) != 2 || len(groups[1].Sources) != 3 {
		t.Fatalf("alternative group = %#v, want combined labels and sources", groups[1])
	}
	if !groups[0].IsCanonicalPrimary || groups[1].IsCanonicalPrimary {
		t.Fatalf("canonical primary flags = %#v, want only the first number marked current primary", groups)
	}

	checked := 0
	for index := range groups {
		if contactResolverValueGroupChecked(groups, index) {
			checked++
		}
	}
	if checked != 1 {
		t.Fatalf("checked resolver groups = %d, want exactly one bootstrap value", checked)
	}

	locations := []models.ContactSyncSetupLocation{{AccountID: "gmail", Label: "account@example.com", Provider: "gmail"}}
	locationLabels := contactSyncSetupLocationLabels(groups[0].Sources, locations)
	if !reflect.DeepEqual(locationLabels, []string{"Local", "account@example.com"}) {
		t.Fatalf("visible sync locations = %#v, want local and provider without canonical", locationLabels)
	}
	if got := contactSyncSetupSourceDisplay("canonical", locations); got != "" {
		t.Fatalf("canonical display = %q, want hidden internal source", got)
	}

	var out bytes.Buffer
	setup := models.ContactSyncSetup{Contact: models.Contact{ID: "contact-1"}, ConflictFields: fields, Locations: locations}
	if err := ContactSyncSetupResolve(setup).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, expected := range []string{"+54 9 11 3118-0670", "Current primary", "Found in 2 locations", "account@example.com"} {
		if !strings.Contains(html, expected) {
			t.Fatalf("sync resolver missing %q: %s", expected, html)
		}
	}
	if strings.Contains(html, ">Canonical<") {
		t.Fatalf("sync resolver exposed internal canonical source: %s", html)
	}
	if count := strings.Count(html, "Current primary"); count != 1 {
		t.Fatalf("current primary markers = %d, want exactly one: %s", count, html)
	}
}

func TestContactAvatarPreviewURLRequestsLargerGooglePhoto(t *testing.T) {
	got := contactAvatarPreviewURL("https://lh3.googleusercontent.com/-abc/s100/photo.jpg?sz=50")
	if !strings.Contains(got, "/s1024/photo.jpg") || !strings.Contains(got, "sz=1024") {
		t.Fatalf("preview URL = %q, want Google size override", got)
	}

	got = contactAvatarPreviewURL("https://lh3.googleusercontent.com/a-/ALV-UjV=s96-c")
	if !strings.Contains(got, "=s1024-c") || !strings.Contains(got, "sz=1024") {
		t.Fatalf("preview URL = %q, want Google path size override", got)
	}

	dataURL := "data:image/png;base64,abc"
	if got := contactAvatarPreviewURL(dataURL); got != dataURL {
		t.Fatalf("data URL = %q, want unchanged", got)
	}

	otherURL := "https://photos.example/jane.jpg?sz=50"
	if got := contactAvatarPreviewURL(otherURL); got != otherURL {
		t.Fatalf("non-Google URL = %q, want unchanged", got)
	}
}

func TestContactAvatarRenderURLProxiesGoogleProviderPhotos(t *testing.T) {
	raw := "https://lh3.googleusercontent.com/a-/ALV-UjV=s100"
	got := contactAvatarRenderURL(raw)
	if !strings.HasPrefix(got, "/api/provider-avatar?url=") || !strings.Contains(got, "lh3.googleusercontent.com") {
		t.Fatalf("render URL = %q, want provider avatar proxy", got)
	}

	for _, raw := range []string{
		"/api/avatars/hash",
		"data:image/png;base64,abc",
		"https://photos.example/jane.jpg",
	} {
		if got := contactAvatarRenderURL(raw); got != raw {
			t.Fatalf("render URL = %q, want %q unchanged", got, raw)
		}
	}
}
