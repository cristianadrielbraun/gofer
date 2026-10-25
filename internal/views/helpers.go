package views

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/themecolors"

	"github.com/a-h/templ"
)

// jmapAccountPaused is a JMAP account while JMAP is turned off: Gofer keeps
// its mail but doesn't sync it.
func jmapAccountPaused(account models.Account) bool {
	return account.Provider == providers.ProviderJMAP && !providers.JMAPEnabled()
}

type unifiedFolderOption struct {
	ID   string
	Name string
	Icon string
}

func unifiedFolderOptions() []unifiedFolderOption {
	return []unifiedFolderOption{
		{"inbox", "Inbox", "inbox"},
		{"starred", "Starred", "starred"},
		{"sent", "Sent", "send"},
		{"drafts", "Drafts", "file"},
		{"archive", "Archive", "archive"},
		{"spam", "Spam", "alert-circle"},
		{"trash", "Trash", "trash"},
	}
}

func unifiedFolderSettingKey(folderID string) string {
	return "unified_folder_" + strings.TrimSpace(folderID) + "_enabled"
}

func unifiedFolderAccountSettingKey(folderID, accountID string) string {
	return "unified_folder_" + strings.TrimSpace(folderID) + "_account_" + strings.TrimSpace(accountID) + "_enabled"
}

func unifiedFolderEnabled(settings map[string]string, folderID string) bool {
	return uiSettingGet(settings, unifiedFolderSettingKey(folderID), "true") != "false"
}

func unifiedFolderAccountEnabled(settings map[string]string, folderID, accountID string) bool {
	return uiSettingGet(settings, unifiedFolderAccountSettingKey(folderID, accountID), "true") != "false"
}

func folderDisplayName(folderID string) string {
	for _, folder := range unifiedFolderOptions() {
		if folder.ID == folderID {
			return folder.Name
		}
	}
	return "Inbox"
}

func mailRoleIsSpam(role string) bool {
	role = strings.TrimSpace(strings.ToLower(role))
	return role == "spam" || role == "junk"
}

func mailFolderIDIsSpam(folderID string, accounts []models.Account) bool {
	if strings.TrimSpace(folderID) == "spam" {
		return true
	}
	for _, account := range accounts {
		for _, folder := range account.Folders {
			if folder.ID == folderID && mailRoleIsSpam(folder.Role) {
				return true
			}
		}
	}
	return false
}

func mailEmailIsSpam(email *models.Email) bool {
	if email == nil {
		return false
	}
	return email.FolderID == "spam" || mailRoleIsSpam(email.FolderRole)
}

func mailRoleIsArchive(role string) bool {
	return strings.TrimSpace(strings.ToLower(role)) == "archive"
}

func mailFolderIDIsArchive(folderID string, accounts []models.Account) bool {
	if strings.TrimSpace(folderID) == "archive" {
		return true
	}
	for _, account := range accounts {
		for _, folder := range account.Folders {
			if folder.ID == folderID && mailRoleIsArchive(folder.Role) {
				return true
			}
		}
	}
	return false
}

func mailEmailIsArchived(email *models.Email) bool {
	return email != nil && (email.FolderID == "archive" || mailRoleIsArchive(email.FolderRole))
}

// MailLabelOption is a label in the Labels dialog, with how many of the
// emails have it.
type MailLabelOption struct {
	Name  string
	State string
}

const (
	MailLabelOnAll  = "all"
	MailLabelOnSome = "some"
	MailLabelOnNone = "none"
)

type mailMoveTarget struct {
	Folder models.Folder
	Depth  int
}

// mailMoveTargets flattens an account's folder tree for the Move to dialog,
// leaving out Starred, which is not a place.
func mailMoveTargets(folders []models.Folder) []mailMoveTarget {
	var targets []mailMoveTarget
	var walk func([]models.Folder, int)
	walk = func(folders []models.Folder, depth int) {
		for _, folder := range folders {
			if folder.Role != "starred" {
				targets = append(targets, mailMoveTarget{Folder: folder, Depth: depth})
			}
			walk(folder.Children, depth+1)
		}
	}
	walk(folders, 0)
	return targets
}

// mailMoveAccountProblem says why another account can't take an email from
// the Move dialog, or nothing when it can.
func mailMoveAccountProblem(account models.Account, own bool) string {
	switch {
	case own:
		return ""
	case account.ReconnectPending:
		return "Needs its password"
	}
	return ""
}

// mailMoveTakesOtherAccounts reports whether a folder can take email from
// another account. Gmail and Outlook only take drafts made as drafts.
func mailMoveTakesOtherAccounts(account models.Account, folder models.Folder) bool {
	return !((account.Provider == "gmail" || account.Provider == "outlook") && folder.Role == "drafts")
}

func mailRoleIsTrash(role string) bool {
	return strings.TrimSpace(strings.ToLower(role)) == "trash"
}

func mailFolderIDIsTrash(folderID string, accounts []models.Account) bool {
	if strings.TrimSpace(folderID) == "trash" {
		return true
	}
	for _, account := range accounts {
		for _, folder := range account.Folders {
			if folder.ID == folderID && mailRoleIsTrash(folder.Role) {
				return true
			}
		}
	}
	return false
}

func mailEmailIsTrash(email *models.Email) bool {
	return email != nil && (email.FolderID == "trash" || mailRoleIsTrash(email.FolderRole))
}

func mailThreadItemIsTrash(item *models.ThreadItem) bool {
	return item != nil && mailRoleIsTrash(item.FolderRole)
}

func mailDeleteActionClass(size, normal string, permanent bool) string {
	if permanent {
		return size + " border border-red-500/35 bg-red-500/12 text-red-700 shadow-[0_1px_2px_rgba(0,0,0,0.04)] hover:bg-red-500/20 hover:text-red-800 dark:border-red-300/20 dark:bg-red-300/10 dark:text-red-300 dark:hover:text-red-200"
	}
	return size + " " + normal
}

func mailArchiveSelectionLabel(unarchive bool) string {
	if unarchive {
		return "Move selected messages to Inbox"
	}
	return "Archive selected messages"
}

func mailHiddenClass(class string, hidden bool) string {
	if hidden {
		return strings.TrimSpace(class + " hidden")
	}
	return class
}

// menuDestructiveClass gives a menu item that deletes the red highlight the
// account menu's Delete has, instead of the ordinary one.
const menuDestructiveClass = "hover:bg-destructive/10 hover:text-destructive focus:bg-destructive/10 focus:text-destructive"

// mailContextItemClass highlights the message menu's Delete in red.
func mailContextItemClass(action string) string {
	if action == "delete" {
		return menuDestructiveClass
	}
	return ""
}

// mailDeleteAllFolder names the folder kind whose list toolbar Delete acts on
// the whole folder while nothing is selected: "trash", "spam" or "".
func mailDeleteAllFolder(activeFolder string, accounts []models.Account) string {
	switch {
	case mailFolderIDIsTrash(activeFolder, accounts):
		return "trash"
	case mailFolderIDIsSpam(activeFolder, accounts):
		return "spam"
	}
	return ""
}

// Nothing is selected at first, so Delete starts out acting on the whole
// folder where it can. app.js keeps these in step with the selection.
func mailDeleteAllLabel(folder string) string {
	switch folder {
	case "trash":
		return "Empty trash permanently"
	case "spam":
		return "Move all spam to Trash"
	}
	return "Delete selected messages"
}

func mailDeleteAllTooltip(folder string) string {
	switch folder {
	case "trash":
		return "Empty trash permanently"
	case "spam":
		return "Move all to Trash"
	}
	return "Delete"
}

func mailViewPartialURL(emailID, folderID string, single bool) templ.SafeURL {
	values := url.Values{}
	if folderID = strings.TrimSpace(folderID); folderID != "" {
		values.Set("folder_id", folderID)
	}
	if single {
		values.Set("single", "1")
	}
	path := "/email/" + url.PathEscape(strings.TrimSpace(emailID))
	if query := values.Encode(); query != "" {
		path += "?" + query
	}
	return templ.URL(path)
}

func composeDefaultAccountID(accounts []models.Account) string {
	if len(accounts) > 0 {
		return accounts[0].ID
	}
	return ""
}

func composeDefaultEmail(accounts []models.Account) string {
	if len(accounts) > 0 {
		return accounts[0].Email
	}
	return ""
}

func composeDefaultName(accounts []models.Account) string {
	if len(accounts) > 0 {
		return accounts[0].Name
	}
	return ""
}

func syncAccountDisplayName(account models.AccountSyncStatus) string {
	if strings.TrimSpace(account.AccountName) != "" {
		return account.AccountName
	}
	if strings.TrimSpace(account.AccountEmail) != "" {
		return account.AccountEmail
	}
	return account.AccountID
}

func uiSettingsJSON(settings map[string]string) string {
	b, _ := json.Marshal(settings)
	return string(b)
}

func uiSettingGet(settings map[string]string, key, fallback string) string {
	if v, ok := settings[key]; ok && v != "" {
		return v
	}
	return fallback
}

// panelSectionKeys names the panel sizes contacts and calendar keep apart from
// mail. Mail owns the original keys.
var panelSectionKeys = map[string]map[string]string{
	"contacts": {"sidebar_width": "contacts_sidebar_width", "mail_list_width": "contacts_list_width", "mail_list_height": "contacts_list_height"},
	"calendar": {"sidebar_width": "calendar_sidebar_width", "mail_list_width": "calendar_list_width", "mail_list_height": "calendar_list_height"},
}

// PanelSetting reads a panel size for one app section. A section that was
// never resized uses mail's size.
func PanelSetting(settings map[string]string, section, key, fallback string) string {
	if sectionKey := panelSectionKeys[section][key]; sectionKey != "" {
		if v := settings[sectionKey]; v != "" {
			return v
		}
	}
	return uiSettingGet(settings, key, fallback)
}

func uiSettingsLocation(settings map[string]string) *time.Location {
	timezone := strings.TrimSpace(uiSettingGet(settings, "timezone", "local"))
	if timezone != "" && timezone != "local" {
		if loc, err := time.LoadLocation(timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func formatAccountSyncErrorAt(raw string, settings map[string]string) string {
	t, ok := parseAccountSyncErrorAt(raw)
	if !ok {
		return strings.TrimSpace(raw)
	}
	return t.In(uiSettingsLocation(settings)).Format("Jan 2, 2006 15:04 MST")
}

func parseAccountSyncErrorAt(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func mailListWidthCSS(width string) string {
	v := strings.TrimSpace(width)
	if v == "" {
		return "clamp(300px,50%,calc(100% - 300px))"
	}

	if strings.HasSuffix(v, "%") {
		n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "%")), 64)
		if err == nil && n > 0 {
			return fmt.Sprintf("clamp(300px,%s%%,calc(100%% - 300px))", strconv.FormatFloat(n, 'f', -1, 64))
		}
	}

	if strings.HasSuffix(v, "px") {
		n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "px")), 64)
		if err == nil && n > 0 {
			return strconv.FormatFloat(n, 'f', -1, 64) + "px"
		}
	}

	n, err := strconv.ParseFloat(v, 64)
	if err == nil && n > 0 {
		return strconv.FormatFloat(n, 'f', -1, 64) + "px"
	}

	return "clamp(300px,50%,calc(100% - 300px))"
}

func uiSettingCSVHas(settings map[string]string, key, fallback, value string) bool {
	for _, part := range strings.Split(uiSettingGet(settings, key, fallback), ",") {
		if strings.TrimSpace(part) == value {
			return true
		}
	}
	return false
}

func sidebarAccountCollapsed(settings map[string]string, accountID string, active bool) bool {
	if active {
		return false
	}
	var state map[string]bool
	if err := json.Unmarshal([]byte(uiSettingGet(settings, "sidebar_account_collapsed", "{}")), &state); err != nil {
		return false
	}
	return state[accountID]
}

func sidebarTagGroupID(accountID string) string {
	if strings.TrimSpace(accountID) == "" {
		return "__unified__"
	}
	return strings.TrimSpace(accountID)
}

func sidebarTagGroupCollapsed(settings map[string]string, accountID string, active bool) bool {
	if active {
		return false
	}
	var state map[string]bool
	if err := json.Unmarshal([]byte(uiSettingGet(settings, "sidebar_tag_group_collapsed", "{}")), &state); err != nil {
		return false
	}
	return state[sidebarTagGroupID(accountID)]
}

func sidebarFolderGroupID(accountID string, folderID string) string {
	scope := strings.TrimSpace(accountID)
	if scope == "" {
		scope = "__unified__"
	}
	return scope + ":" + strings.TrimSpace(folderID)
}

func sidebarFolderCollapsed(settings map[string]string, accountID string, folder models.Folder, activeFolder string) bool {
	if folderHasActiveDescendant(folder, activeFolder) {
		return false
	}
	var state map[string]bool
	if err := json.Unmarshal([]byte(uiSettingGet(settings, "sidebar_folder_collapsed", "{}")), &state); err != nil {
		return false
	}
	return state[sidebarFolderGroupID(accountID, folder.ID)]
}

func accountHasActiveFolder(account models.Account, activeFolder string, filters models.EmailFilters) bool {
	if sidebarAccountHasActiveTag(account.ID, filters) {
		return true
	}
	for _, folder := range account.Folders {
		if folderHasActiveID(folder, activeFolder) {
			return true
		}
	}
	return false
}

func folderHasActiveID(folder models.Folder, activeFolder string) bool {
	if folder.ID == activeFolder {
		return true
	}
	for _, child := range folder.Children {
		if folderHasActiveID(child, activeFolder) {
			return true
		}
	}
	return false
}

func folderHasActiveDescendant(folder models.Folder, activeFolder string) bool {
	for _, child := range folder.Children {
		if folderHasActiveID(child, activeFolder) {
			return true
		}
	}
	return false
}

func unifiedHasActiveFolder(activeFolder string, settings map[string]string, filters models.EmailFilters) bool {
	if sidebarUnifiedHasActiveTag(filters) {
		return true
	}
	for _, folder := range unifiedFolderOptions() {
		if folder.ID == activeFolder {
			return unifiedFolderEnabled(settings, activeFolder)
		}
	}
	return false
}

func unifiedFolders(accounts []models.Account, settings map[string]string) []models.Folder {
	unreadByRole := make(map[string]int)
	seenRole := make(map[string]bool)
	for _, account := range accounts {
		if account.IsDeleting || !account.EmailSyncEnabled {
			continue
		}
		collectUnifiedFolders(account.Folders, account.ID, settings, unreadByRole, seenRole)
	}

	options := unifiedFolderOptions()
	folders := make([]models.Folder, 0, len(options))
	for _, option := range options {
		if !unifiedFolderEnabled(settings, option.ID) {
			continue
		}
		if option.ID == "starred" {
			if !hasUnifiedFolderAccount(accounts, settings, option.ID) {
				continue
			}
		} else if !seenRole[option.ID] {
			continue
		}
		folders = append(folders, models.Folder{
			ID:       option.ID,
			Name:     option.Name,
			Icon:     option.Icon,
			Role:     option.ID,
			Unread:   unreadByRole[option.ID],
			IsSystem: true,
		})
	}
	return folders
}

func hasUnifiedFolderAccount(accounts []models.Account, settings map[string]string, folderID string) bool {
	for _, account := range accounts {
		if !account.IsDeleting && account.EmailSyncEnabled && unifiedFolderAccountEnabled(settings, folderID, account.ID) {
			return true
		}
	}
	return false
}

func collectUnifiedFolders(folders []models.Folder, accountID string, settings map[string]string, unreadByRole map[string]int, seenRole map[string]bool) {
	for _, folder := range folders {
		role := unifiedFolderIDFromRole(folder.Role)
		if role != "" && role != "custom" && unifiedFolderAccountEnabled(settings, role, accountID) {
			seenRole[role] = true
			unreadByRole[role] += folder.Unread
		}
		collectUnifiedFolders(folder.Children, accountID, settings, unreadByRole, seenRole)
	}
}

func unifiedFolderIDFromRole(role string) string {
	if role == "junk" {
		return "spam"
	}
	return role
}

func unifiedSidebarLabels(accounts []models.Account) []models.Label {
	labels := make([]models.Label, 0)
	seen := make(map[string]bool)
	for _, account := range accounts {
		if account.IsDeleting || !account.EmailSyncEnabled {
			continue
		}
		for _, label := range account.Labels {
			name := strings.TrimSpace(label.Name)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			label.Name = name
			labels = append(labels, label)
		}
	}
	sort.SliceStable(labels, func(i, j int) bool {
		return strings.ToLower(labels[i].Name) < strings.ToLower(labels[j].Name)
	})
	return labels
}

func sidebarTagBaseFolder(activeFolder string) string {
	folder := strings.TrimSpace(activeFolder)
	if folder == "" || folder == "scheduled" {
		return "inbox"
	}
	return folder
}

func sidebarTagFilterURL(folderID string, label models.Label, accountID string) templ.SafeURL {
	folderID = sidebarTagBaseFolder(folderID)
	values := url.Values{}
	values.Set("tag", strings.TrimSpace(label.Name))
	if strings.TrimSpace(accountID) != "" {
		values.Set("tag_account_id", strings.TrimSpace(accountID))
		if providerID := strings.TrimSpace(label.ProviderID); providerID != "" {
			values.Set("tag_provider_id", providerID)
			if providerType := strings.TrimSpace(label.ProviderType); providerType != "" {
				values.Set("tag_provider_type", providerType)
			}
		}
	}
	return templ.URL("/folder/" + url.PathEscape(folderID) + "?" + values.Encode())
}

func sidebarTagActive(activeFolder string, folderID string, filters models.EmailFilters, label models.Label, accountID string) bool {
	if strings.TrimSpace(activeFolder) != sidebarTagBaseFolder(folderID) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(filters.Tag), strings.TrimSpace(label.Name)) {
		return false
	}
	return strings.TrimSpace(filters.TagAccountID) == strings.TrimSpace(accountID)
}

func sidebarAccountHasActiveTag(accountID string, filters models.EmailFilters) bool {
	return strings.TrimSpace(filters.Tag) != "" && strings.TrimSpace(filters.TagAccountID) == strings.TrimSpace(accountID)
}

func sidebarUnifiedHasActiveTag(filters models.EmailFilters) bool {
	return strings.TrimSpace(filters.Tag) != "" && strings.TrimSpace(filters.TagAccountID) == ""
}

func sidebarTagGroupActive(filters models.EmailFilters, accountID string) bool {
	if strings.TrimSpace(accountID) == "" {
		return sidebarUnifiedHasActiveTag(filters)
	}
	return sidebarAccountHasActiveTag(accountID, filters)
}

// themeClass is the page's light or dark class. Express has no dark mode, so it
// is always light; the saved choice still applies to the other styles.
func themeClass(settings map[string]string) string {
	if themeStyle(settings) == "express" {
		return ""
	}
	if uiSettingGet(settings, "theme", "dark") == "dark" {
		return "dark"
	}
	return ""
}

func themeStyle(settings map[string]string) string {
	return uiSettingGet(settings, "theme_style", "classic")
}

// themeStyleLabel is a style's name as Settings shows it.
func themeStyleLabel(style string) string {
	switch style {
	case "minimal":
		return "Minimal"
	case "express":
		return "Express"
	default:
		return "Classic"
	}
}

// themeColorsCSS is a style's changed colors as CSS, or "" if it has none.
func themeColorsCSS(settings map[string]string, style string) string {
	return themecolors.FromSettings(settings, style).CSS(style)
}

// themeColorsChanged reports whether a style has changed colors to discard.
func themeColorsChanged(settings map[string]string, style string) bool {
	return !themecolors.FromSettings(settings, style).Empty()
}

func senderDisplay(contact models.Contact, mode string) string {
	switch mode {
	case "email":
		if contact.Email != "" {
			return contact.Email
		}
		return contact.Name
	case "both":
		if contact.Name == "" || contact.Email == "" || contact.Name == contact.Email {
			if contact.Name != "" {
				return contact.Name
			}
			return contact.Email
		}
		return fmt.Sprintf("%s <%s>", contact.Name, contact.Email)
	default:
		if contact.Name != "" {
			return contact.Name
		}
		return contact.Email
	}
}

// senderDisplayPrimary and senderDisplayAddressSuffix split the "both" form of
// senderDisplay so small screens can drop the address and keep the name.
func senderDisplayPrimary(contact models.Contact, mode string) string {
	if senderDisplayAddressSuffix(contact, mode) != "" {
		return contact.Name
	}
	return senderDisplay(contact, mode)
}

func senderDisplayAddressSuffix(contact models.Contact, mode string) string {
	if mode != "both" || contact.Name == "" || contact.Email == "" || contact.Name == contact.Email {
		return ""
	}
	return fmt.Sprintf(" <%s>", contact.Email)
}

func contactsDisplay(contacts []models.Contact, mode string) string {
	if len(contacts) == 0 {
		return ""
	}
	if len(contacts) == 1 {
		return senderDisplay(contacts[0], mode)
	}
	return fmt.Sprintf("%s +%d", senderDisplay(contacts[0], mode), len(contacts)-1)
}

func mailListAccountDisplay(accounts []models.Account, accountID string) string {
	for _, account := range accounts {
		if account.ID != accountID {
			continue
		}
		if strings.TrimSpace(account.Name) != "" {
			return account.Name
		}
		if strings.TrimSpace(account.Email) != "" {
			return account.Email
		}
		return account.ID
	}
	return accountID
}

func contactAvatarListFallback(isRead bool) string {
	if isRead {
		return "bg-muted text-muted-foreground"
	}
	return "bg-gradient-to-b from-amber-700/80 to-amber-900/80 text-amber-100"
}

func contactAvatarThreadFallback(isCurrent bool) string {
	if isCurrent {
		return "bg-gradient-to-b from-amber-700/70 to-amber-900/70 text-amber-100"
	}
	return "bg-ink/[0.06] text-ink/40"
}

func senderDisplaySettingLabel(mode string) string {
	switch mode {
	case "email":
		return "Only email"
	case "both":
		return "Name and email"
	default:
		return "Only name"
	}
}

func defaultComposeViewSettingLabel(view string) string {
	switch view {
	case "pane":
		return "Inline, one pane"
	case "full":
		return "Inline, expanded"
	default:
		return "Dialog"
	}
}

func mailPaneLayout(value string) string {
	if value == "stacked" {
		return "stacked"
	}
	return "side"
}

// PaneLayout is "side" or "stacked" for one app section. Unlike the panel
// sizes, contacts and calendar never borrow mail's layout.
func PaneLayout(settings map[string]string, section string) string {
	if section == "contacts" || section == "calendar" {
		return mailPaneLayout(settings[section+"_pane_layout"])
	}
	return mailPaneLayout(settings["mail_pane_layout"])
}

func mailPaneLayoutClass(value string) string {
	if mailPaneLayout(value) == "stacked" {
		return "flex flex-1 min-w-0 flex-col"
	}
	return "flex flex-1 min-w-0"
}

func mailPaneLayoutSettingLabel(value string) string {
	if mailPaneLayout(value) == "stacked" {
		return "Stacked"
	}
	return "Side by side"
}

func translationProviderSettingLabel(value string) string {
	switch translationProviderSettingValue(value) {
	case "google_web_basic":
		return "Google Web Translate (Basic)"
	default:
		if strings.TrimSpace(value) == "" {
			return "Google Web Translate (Basic)"
		}
		return strings.TrimSpace(value)
	}
}

func translationProviderSettingValue(value string) string {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_") {
	case "", "google", "google_web", "google_web_basic", "google_translate":
		return "google_web_basic"
	default:
		return strings.TrimSpace(value)
	}
}

func translationLanguageSettingLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ar":
		return "Arabic"
	case "cs":
		return "Czech"
	case "de":
		return "German"
	case "en", "":
		return "English"
	case "es":
		return "Spanish"
	case "fr":
		return "French"
	case "it":
		return "Italian"
	case "ja":
		return "Japanese"
	case "ko":
		return "Korean"
	case "nl":
		return "Dutch"
	case "pl":
		return "Polish"
	case "pt":
		return "Portuguese"
	case "ru":
		return "Russian"
	case "uk":
		return "Ukrainian"
	case "zh-cn", "zh":
		return "Chinese"
	default:
		return value
	}
}

type translationLanguageOption struct {
	Value string
	Label string
}

func translationLanguageOptions() []translationLanguageOption {
	return []translationLanguageOption{
		{Value: "en", Label: "English"},
		{Value: "cs", Label: "Czech"},
		{Value: "de", Label: "German"},
		{Value: "es", Label: "Spanish"},
		{Value: "fr", Label: "French"},
		{Value: "it", Label: "Italian"},
		{Value: "nl", Label: "Dutch"},
		{Value: "pl", Label: "Polish"},
		{Value: "pt", Label: "Portuguese"},
		{Value: "uk", Label: "Ukrainian"},
		{Value: "zh-CN", Label: "Chinese"},
		{Value: "ja", Label: "Japanese"},
		{Value: "ko", Label: "Korean"},
	}
}

func notificationModeSettingLabel(mode string) string {
	switch mode {
	case "web_push":
		return "Web Push"
	case "browser_tab":
		return "Browser tab"
	case "off":
		return "Off"
	default:
		return "Auto"
	}
}

func composeAutosaveDebounceLabel(value string) string {
	switch value {
	case "3":
		return "3 seconds"
	case "10":
		return "10 seconds"
	case "15":
		return "15 seconds"
	case "1":
		return "1 second"
	default:
		return "5 seconds"
	}
}

func composeAutosaveConditionsLabel(settings map[string]string) string {
	value := uiSettingGet(settings, "compose_autosave_conditions", "chars,attachment")
	if value == "" {
		return "No conditions"
	}
	count := 0
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) != "" {
			count++
		}
	}
	if count == 1 {
		return "1 condition"
	}
	return fmt.Sprintf("%d conditions", count)
}

func signatureTotal(data []models.AccountSignatureData) int {
	if len(data) == 0 {
		return 0
	}
	return len(data[0].Signatures)
}

func signatureAssignmentCount(data []models.AccountSignatureData) int {
	count := 0
	for _, item := range data {
		if item.Settings.NewEnabled && item.Settings.NewSignatureID != "" {
			count++
		}
		if item.Settings.ReplyEnabled && item.Settings.ReplySignatureID != "" {
			count++
		}
		if item.Settings.ForwardEnabled && item.Settings.ForwardSignatureID != "" {
			count++
		}
	}
	return count
}

func signatureName(signatures []models.Signature, id string) string {
	for _, sig := range signatures {
		if sig.ID == id {
			return sig.Name
		}
	}
	return "No signature"
}

func mailListViewMode(mode string) string {
	if mode == "table" {
		return "table"
	}
	return "cards"
}

func mailListViewIndicatorStyle(mode string) string {
	if mailListViewMode(mode) == "table" {
		return "transform: translateX(calc(100% + 2px));"
	}
	return "transform: translateX(0);"
}

func mailListNavigationMode(mode string) string {
	if mode == "pagination" {
		return "pagination"
	}
	return "infinite"
}

func mailListNavigationSettingLabel(value string) string {
	if mailListNavigationMode(value) == "pagination" {
		return "Pagination"
	}
	return "Infinite scroll"
}

func mailListPaginationPageSize() int {
	return 50
}

func mailListPaginationTotalPages(totalCount int, pageSize int) int {
	if totalCount <= 0 || pageSize <= 0 {
		return 1
	}
	return (totalCount + pageSize - 1) / pageSize
}

func mailListPaginationPage(windowStart int, pageSize int) int {
	if windowStart < 0 || pageSize <= 0 {
		return 1
	}
	return (windowStart / pageSize) + 1
}

func mailListPaginationRangeLabel(windowStart int, itemCount int, totalCount int) string {
	if totalCount <= 0 || itemCount <= 0 {
		return "No messages"
	}
	start := windowStart + 1
	end := windowStart + itemCount
	if end > totalCount {
		end = totalCount
	}
	return fmt.Sprintf("%d-%d of %d", start, end, totalCount)
}

func autoMarkReadSettingLabel(value string) string {
	switch value {
	case "0":
		return "Immediately"
	case "5":
		return "After 5 seconds"
	case "10":
		return "After 10 seconds"
	case "2":
		return "After 2 seconds"
	case "never":
		return "Never"
	default:
		return "Immediately"
	}
}

// GroupConversations reports whether mail lists show each conversation
// once, opening into all its messages, rather than each message on its own.
func GroupConversations(settings map[string]string) bool {
	return uiSettingGet(settings, "group_conversations", "true") != "false"
}

// ThreadNewestFirst reports whether the reading pane lists a thread's messages
// newest first, like the message list, rather than in reading order.
func ThreadNewestFirst(settings map[string]string) bool {
	return uiSettingGet(settings, "thread_order", "newest") != "oldest"
}

func threadOrderSettingLabel(value string) string {
	if value == "oldest" {
		return "Oldest first"
	}
	return "Newest first"
}

// EmailBodyStyle is how message bodies look until one is switched in the
// reading pane: "theme" follows the app's color mode, "dark" and "light"
// recolor the message, and "original" shows it as sent.
func EmailBodyStyle(settings map[string]string) string {
	switch value := uiSettingGet(settings, "email_body_style", "theme"); value {
	case "dark", "light", "original":
		return value
	}
	return "theme"
}

func emailBodyStyleSettingLabel(value string) string {
	switch value {
	case "dark":
		return "Dark"
	case "light":
		return "Light"
	case "original":
		return "Original"
	}
	return "Follow theme"
}

func accountColorStyle(color string) string {
	return "background-color: " + accountColorValue(color)
}

func accountColorValue(color string) string {
	color = strings.TrimSpace(color)
	if len(color) == 6 {
		color = "#" + color
	}
	if len(color) != 7 || color[0] != '#' {
		return "#8b5cf6"
	}
	for _, r := range color[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return "#8b5cf6"
		}
	}
	return strings.ToLower(color)
}

func accountColorOptions() []string {
	return []string{
		"#ef4444", "#f97316", "#facc15", "#84cc16", "#22c55e", "#14b8a6",
		"#06b6d4", "#0ea5e9", "#2563eb", "#4f46e5", "#7c3aed", "#d946ef",
		"#ec4899", "#fb7185", "#7f1d1d", "#14532d", "#083344", "#111827",
	}
}

func accountColorSelected(current, option string) bool {
	return accountColorValue(current) == accountColorValue(option)
}

func calendarSourceName(source models.CalendarSource) string {
	if name := strings.TrimSpace(source.Name); name != "" {
		return name
	}
	if source.IsPrimary {
		return "Primary calendar"
	}
	return "Untitled calendar"
}

func calendarSourceDetails(source models.CalendarSource) string {
	details := make([]string, 0, 2)
	if timeZone := strings.TrimSpace(source.TimeZone); timeZone != "" {
		details = append(details, timeZone)
	}
	if accessRole := strings.TrimSpace(source.AccessRole); accessRole != "" {
		details = append(details, calendarSourceAccessLabel(accessRole))
	}
	return strings.Join(details, " · ")
}

func calendarSourceAccessLabel(accessRole string) string {
	switch strings.ToLower(strings.TrimSpace(accessRole)) {
	case "owner":
		return "Owner"
	case "writer":
		return "Can edit"
	case "reader":
		return "Read only"
	case "freebusyreader":
		return "Availability only"
	default:
		return accessRole
	}
}

func accountMarkerStyle(accounts []models.Account) string {
	colors := make([]string, 0, len(accounts))
	seen := map[string]bool{}
	for _, account := range accounts {
		color := account.Color
		if color == "" {
			color = "#8b5cf6"
		}
		if seen[color] {
			continue
		}
		seen[color] = true
		colors = append(colors, color)
	}
	if len(colors) == 0 {
		return "background-color: #8b5cf6"
	}
	if len(colors) > 3 {
		rand.Shuffle(len(colors), func(i, j int) {
			colors[i], colors[j] = colors[j], colors[i]
		})
		colors = colors[:3]
	}
	if len(colors) == 1 {
		return "background-color: " + colors[0]
	}
	step := 360 / len(colors)
	style := "background: conic-gradient("
	for i, color := range colors {
		if i > 0 {
			style += ", "
		}
		start := i * step
		end := (i + 1) * step
		if i == len(colors)-1 {
			end = 360
		}
		style += fmt.Sprintf("%s %ddeg %ddeg", color, start, end)
	}
	return style + ")"
}

func sidebarFolderHref(folderID, accountID string) templ.SafeURL {
	if accountID == "" {
		return templ.URL(fmt.Sprintf("/?folder=%s", folderID))
	}
	return templ.URL(fmt.Sprintf("/?folder=%s&account=%s", folderID, accountID))
}
