package views

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

type accountGroupsContextKey struct{}
type uiSettingsContextKey struct{}

// WithAccountGroups attaches the user's connected-account groups to ctx so
// SettingsAccountsTab can render nested groups, including empty ones, without
// changing existing view signatures.
func WithAccountGroups(ctx context.Context, groups []models.AccountGroup) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, accountGroupsContextKey{}, slices.Clone(groups))
}

func AccountGroupsFromContext(ctx context.Context) []models.AccountGroup {
	if ctx == nil {
		return nil
	}
	groups, _ := ctx.Value(accountGroupsContextKey{}).([]models.AccountGroup)
	return groups
}

// WithUISettings attaches UI settings to ctx for views whose signature does not
// carry uiSettings directly (such as CalendarSidebarContent).
func WithUISettings(ctx context.Context, settings map[string]string) context.Context {
	if ctx == nil || len(settings) == 0 {
		return ctx
	}
	return context.WithValue(ctx, uiSettingsContextKey{}, settings)
}

func uiSettingsFromContext(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	settings, _ := ctx.Value(uiSettingsContextKey{}).(map[string]string)
	return settings
}

// AccountGroupDialogData holds the form state for creating or editing a group.
type AccountGroupDialogData struct {
	Group    models.AccountGroup
	Parents  []models.AccountGroup
	Error    string
	IsCreate bool
}

// MoveAccountGroupDialogData holds the form state for moving an account to a group.
type MoveAccountGroupDialogData struct {
	Account models.Account
	Groups  []models.AccountGroup
	Error   string
}

// AddExistingAccountsDialogData holds the form state for multi-selecting
// existing connected accounts from the group tree to move into Group.
type AddExistingAccountsDialogData struct {
	Group    models.AccountGroup
	Groups   []models.AccountGroup
	Accounts []models.Account
	Selected map[string]bool
	Error    string
}

type groupAccountPickerNode struct {
	IsGroup         bool
	Group           models.AccountGroup
	Account         models.Account
	AlreadyInTarget bool
	Checked         bool
	SelectableCount int
	TotalAccounts   int
	Children        []groupAccountPickerNode
}

type groupAccountPickerTree struct {
	Nodes           []groupAccountPickerNode
	HasGroups       bool
	TotalAccounts   int
	SelectableCount int
	SelectedCount   int
}

func buildGroupAccountPickerTree(data AddExistingAccountsDialogData) groupAccountPickerTree {
	connected := make([]models.Account, 0, len(data.Accounts))
	for _, a := range data.Accounts {
		if !accountIsLocalFolders(a) && !a.IsDeleting {
			connected = append(connected, a)
		}
	}
	raw := buildSettingsAccountTree(data.Groups, connected)

	var convert func(items []settingsAccountTreeItem) ([]groupAccountPickerNode, int, int, int)
	convert = func(items []settingsAccountTreeItem) ([]groupAccountPickerNode, int, int, int) {
		var (
			out        []groupAccountPickerNode
			total      int
			selectable int
			selected   int
		)
		for _, item := range items {
			if !item.IsGroup {
				already := item.Account.GroupID == data.Group.ID
				checked := already || data.Selected[item.Account.ID]
				sel := 1
				picked := 0
				if already {
					sel = 0
				} else if checked {
					picked = 1
				}
				out = append(out, groupAccountPickerNode{
					Account:         item.Account,
					AlreadyInTarget: already,
					Checked:         checked,
					SelectableCount: sel,
					TotalAccounts:   1,
				})
				total++
				selectable += sel
				selected += picked
				continue
			}
			children, childTotal, childSelectable, childSelected := convert(item.Children)
			if childTotal == 0 {
				continue
			}
			groupChecked := (childSelectable == 0 && childTotal > 0) || (childSelectable > 0 && childSelected == childSelectable)
			out = append(out, groupAccountPickerNode{
				IsGroup:         true,
				Group:           item.Group,
				Checked:         groupChecked,
				SelectableCount: childSelectable,
				TotalAccounts:   childTotal,
				Children:        children,
			})
			total += childTotal
			selectable += childSelectable
			selected += childSelected
		}
		return out, total, selectable, selected
	}

	nodes, total, selectable, selected := convert(raw)
	hasGroups := false
	for _, n := range nodes {
		if n.IsGroup {
			hasGroups = true
			break
		}
	}
	return groupAccountPickerTree{
		Nodes:           nodes,
		HasGroups:       hasGroups,
		TotalAccounts:   total,
		SelectableCount: selectable,
		SelectedCount:   selected,
	}
}

func accountPickerInputName(alreadyInTarget bool) string {
	if alreadyInTarget {
		return ""
	}
	return "account_id"
}

type settingsAccountTreeItem struct {
	IsGroup  bool
	Group    models.AccountGroup
	Account  models.Account
	Children []settingsAccountTreeItem
}

func buildSettingsAccountTree(groups []models.AccountGroup, accounts []models.Account) []settingsAccountTreeItem {
	groupsByID := make(map[string]models.AccountGroup, len(groups))
	for _, g := range groups {
		groupsByID[g.ID] = g
	}
	// Synthesize group entries from account paths when a caller rendered
	// SettingsAccountsTab without WithAccountGroups (for example in unit tests).
	if len(groupsByID) == 0 {
		for _, a := range accounts {
			var parentID string
			for depth, ref := range a.GroupPath {
				if _, exists := groupsByID[ref.ID]; !exists {
					groupsByID[ref.ID] = models.AccountGroup{
						ID:        ref.ID,
						ParentID:  parentID,
						Name:      ref.Name,
						SortOrder: len(groupsByID),
						Depth:     depth,
					}
				}
				parentID = ref.ID
			}
		}
	}

	childGroups := make(map[string][]models.AccountGroup)
	for _, g := range groupsByID {
		parent := g.ParentID
		if parent != "" {
			if _, ok := groupsByID[parent]; !ok {
				parent = ""
			}
		}
		childGroups[parent] = append(childGroups[parent], g)
	}

	childAccounts := make(map[string][]models.Account)
	for _, a := range accounts {
		parent := a.GroupID
		if accountIsLocalFolders(a) {
			parent = ""
		} else if parent != "" {
			if _, ok := groupsByID[parent]; !ok {
				parent = ""
			}
		}
		childAccounts[parent] = append(childAccounts[parent], a)
	}

	visited := make(map[string]bool, len(groupsByID))
	var build func(parentID string, depth int) []settingsAccountTreeItem
	build = func(parentID string, depth int) []settingsAccountTreeItem {
		type rawEntry struct {
			sortOrder int
			seq       int
			isGroup   bool
			group     models.AccountGroup
			account   models.Account
		}
		var entries []rawEntry
		for _, g := range childGroups[parentID] {
			if visited[g.ID] {
				continue
			}
			entries = append(entries, rawEntry{
				sortOrder: g.SortOrder,
				isGroup:   true,
				group:     g,
			})
		}
		for idx, a := range childAccounts[parentID] {
			entries = append(entries, rawEntry{
				sortOrder: a.SortOrder,
				seq:       idx,
				account:   a,
			})
		}
		slices.SortFunc(entries, func(left, right rawEntry) int {
			if diff := cmp.Compare(left.sortOrder, right.sortOrder); diff != 0 {
				return diff
			}
			if left.isGroup != right.isGroup {
				if !left.isGroup {
					return -1
				}
				return 1
			}
			if left.isGroup {
				return cmp.Compare(left.group.ID, right.group.ID)
			}
			return cmp.Compare(left.seq, right.seq)
		})

		items := make([]settingsAccountTreeItem, 0, len(entries))
		for _, entry := range entries {
			if entry.isGroup {
				visited[entry.group.ID] = true
				g := entry.group
				g.Depth = depth
				items = append(items, settingsAccountTreeItem{
					IsGroup:  true,
					Group:    g,
					Children: build(g.ID, depth+1),
				})
				continue
			}
			items = append(items, settingsAccountTreeItem{
				Account: entry.account,
			})
		}
		return items
	}
	return build("", 0)
}

type sidebarAccountTreeNode struct {
	IsGroup  bool
	Group    models.AccountGroupRef
	Account  models.Account
	Children []sidebarAccountTreeNode
}

// buildSidebarAccountTree preserves the already-ordered accounts slice while
// grouping consecutive accounts by their GroupPath hierarchy. Only groups with
// at least one visible account appear in the sidebar.
func buildSidebarAccountTree(accounts []models.Account) []sidebarAccountTreeNode {
	var build func(list []models.Account, depth int) []sidebarAccountTreeNode
	build = func(list []models.Account, depth int) []sidebarAccountTreeNode {
		var nodes []sidebarAccountTreeNode
		groupIdx := make(map[string]int)
		groupBuckets := make(map[string][]models.Account)
		for _, acc := range list {
			if len(acc.GroupPath) <= depth {
				nodes = append(nodes, sidebarAccountTreeNode{
					Account: acc,
				})
				continue
			}
			ref := acc.GroupPath[depth]
			if _, seen := groupIdx[ref.ID]; !seen {
				groupIdx[ref.ID] = len(nodes)
				nodes = append(nodes, sidebarAccountTreeNode{
					IsGroup: true,
					Group:   ref,
				})
			}
			groupBuckets[ref.ID] = append(groupBuckets[ref.ID], acc)
		}
		for id, idx := range groupIdx {
			nodes[idx].Children = build(groupBuckets[id], depth+1)
		}
		return nodes
	}
	return build(accounts, 0)
}

func sidebarAccountGroupCollapsed(uiSettings map[string]string, groupID string, containsActive bool) bool {
	if containsActive || uiSettings == nil || strings.TrimSpace(groupID) == "" {
		return false
	}
	raw := strings.TrimSpace(uiSettings["sidebar_account_group_collapsed"])
	if raw == "" {
		return false
	}
	var state map[string]bool
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return false
	}
	return state[groupID]
}

func sidebarMailNodeHasActiveFolder(node sidebarAccountTreeNode, activeFolder string, filters models.EmailFilters) bool {
	if !node.IsGroup {
		return accountHasActiveFolder(node.Account, activeFolder, filters)
	}
	for _, child := range node.Children {
		if sidebarMailNodeHasActiveFolder(child, activeFolder, filters) {
			return true
		}
	}
	return false
}

func sidebarContactsNodeHasActiveItem(node sidebarAccountTreeNode, filters models.ContactFilters) bool {
	if !node.IsGroup {
		return contactSidebarAccountActive(node.Account, filters)
	}
	for _, child := range node.Children {
		if sidebarContactsNodeHasActiveItem(child, filters) {
			return true
		}
	}
	return false
}

func filterMailSidebarAccounts(accounts []models.Account) []models.Account {
	out := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.EmailSyncEnabled && !account.IsDeleting && !account.Hidden {
			out = append(out, account)
		}
	}
	return out
}

func filterContactsSidebarAccounts(accounts []models.Account) []models.Account {
	out := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		if contactSidebarAccountVisible(account) {
			out = append(out, account)
		}
	}
	return out
}

func sidebarAccountGroupSectionID(prefix, groupID string) string {
	groupID = strings.TrimSpace(groupID)
	if prefix == "" {
		return groupID
	}
	return prefix + ":" + groupID
}

func accountGroupIndentStyle(depth int) string {
	if depth <= 0 {
		return ""
	}
	if depth > 6 {
		depth = 6
	}
	return strings.Repeat("\u00a0\u00a0\u00a0", depth)
}
