package storage

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

const maxAccountGroupName = 100

var (
	ErrAccountGroupNotFound  = errors.New("account group not found")
	ErrAccountGroupNameEmpty = errors.New("account group name is empty")
	ErrAccountGroupNameTaken = errors.New("another group already has this name")
	ErrAccountGroupCycle     = errors.New("a group cannot be placed inside itself")
)

const accountGroupsTableSQL = `
CREATE TABLE IF NOT EXISTS account_groups (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES account_groups(id) ON DELETE SET NULL,
    name TEXT NOT NULL CHECK (name <> ''),
    sort_order INTEGER NOT NULL DEFAULT 0,
    proxy_id TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_account_groups_user_parent
    ON account_groups(user_id, parent_id, sort_order, id);
`

// migrateV155ToV156 adds nested groups of connected accounts and each group's
// proxy choice. Accounts start ungrouped at the top level.
func migrateV155ToV156(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 156 {
		return nil
	}
	if _, err := tx.Exec(accountGroupsTableSQL); err != nil {
		return fmt.Errorf("create account_groups: %w", err)
	}
	exists, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "accounts", "group_id")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN group_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		// Early account tables did not record an owner to index groups by.
		hasOwner, err := columnExistsTx(tx, "accounts", "user_id")
		if err != nil {
			return err
		}
		if hasOwner {
			if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_accounts_user_group ON accounts(user_id, group_id)`); err != nil {
				return err
			}
		}
		bgPresent, err := columnExistsTx(tx, "accounts", "background_sync_enabled")
		if err != nil {
			return err
		}
		if !bgPresent {
			if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN background_sync_enabled INTEGER NOT NULL DEFAULT 1 CHECK (background_sync_enabled IN (0, 1))`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 156); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizeAccountGroupName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > maxAccountGroupName {
		name = string([]rune(name)[:maxAccountGroupName])
	}
	return name
}

func uniqueLiftedGroupName(name string, taken map[string]bool) string {
	name = normalizeAccountGroupName(name)
	if !taken[strings.ToLower(name)] {
		return name
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		base := name
		if maxBase := maxAccountGroupName - utf8.RuneCountInString(suffix); utf8.RuneCountInString(base) > maxBase {
			base = strings.TrimSpace(string([]rune(base)[:maxBase]))
		}
		candidate := base + suffix
		if !taken[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

func nullableGroupID(id string) sql.NullString {
	id = strings.TrimSpace(id)
	return sql.NullString{String: id, Valid: id != ""}
}

// ListAccountGroups returns the owner's account groups in tree order, with each
// group's nesting depth filled in.
func (db *DB) ListAccountGroups(ctx context.Context, userID string) ([]models.AccountGroup, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT id, COALESCE(parent_id, ''), name, sort_order, proxy_id
		FROM account_groups
		WHERE user_id = ?
		ORDER BY sort_order, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var raw []models.AccountGroup
	byID := map[string]*models.AccountGroup{}
	for rows.Next() {
		var g models.AccountGroup
		if err := rows.Scan(&g.ID, &g.ParentID, &g.Name, &g.SortOrder, &g.ProxyID); err != nil {
			return nil, err
		}
		raw = append(raw, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	for i := range raw {
		byID[raw[i].ID] = &raw[i]
	}
	children := map[string][]models.AccountGroup{}
	for _, g := range raw {
		parent := g.ParentID
		if parent != "" && !validGroupAncestor(byID, g.ID, parent) {
			parent = ""
			g.ParentID = ""
		}
		children[parent] = append(children[parent], g)
	}
	for parent := range children {
		slices.SortFunc(children[parent], func(a, b models.AccountGroup) int {
			if c := cmp.Compare(a.SortOrder, b.SortOrder); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
	}
	out := make([]models.AccountGroup, 0, len(raw))
	visited := make(map[string]bool, len(raw))
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, g := range children[parent] {
			if visited[g.ID] {
				continue
			}
			visited[g.ID] = true
			g.Depth = depth
			out = append(out, g)
			walk(g.ID, depth+1)
		}
	}
	walk("", 0)
	return out, nil
}

func validGroupAncestor(byID map[string]*models.AccountGroup, selfID, parentID string) bool {
	seen := map[string]bool{selfID: true}
	cur := parentID
	for cur != "" {
		if seen[cur] {
			return false
		}
		seen[cur] = true
		g, ok := byID[cur]
		if !ok {
			return false
		}
		cur = g.ParentID
	}
	return true
}

// GetAccountGroup returns one of the owner's account groups, or
// ErrAccountGroupNotFound when no group of theirs has that id.
func (db *DB) GetAccountGroup(ctx context.Context, userID, groupID string) (models.AccountGroup, error) {
	var g models.AccountGroup
	err := db.read.QueryRowContext(ctx, `
		SELECT id, COALESCE(parent_id, ''), name, sort_order, proxy_id
		FROM account_groups
		WHERE id = ? AND user_id = ?`, strings.TrimSpace(groupID), userID,
	).Scan(&g.ID, &g.ParentID, &g.Name, &g.SortOrder, &g.ProxyID)
	if errors.Is(err, sql.ErrNoRows) {
		return models.AccountGroup{}, ErrAccountGroupNotFound
	}
	return g, err
}

// AccountGroupExists reports whether groupID belongs to userID.
func (db *DB) AccountGroupExists(ctx context.Context, userID, groupID string) (bool, error) {
	var exists bool
	err := db.read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_groups WHERE id = ? AND user_id = ?)`, strings.TrimSpace(groupID), userID).Scan(&exists)
	return exists, err
}

// NextAccountSortOrder returns the next sort_order at the end of groupID (or
// at the top level when groupID is "").
func (db *DB) NextAccountSortOrder(ctx context.Context, userID, groupID string) (int, error) {
	var next int
	err := db.read.QueryRowContext(ctx, `
		SELECT MAX(v) + 1 FROM (
			SELECT -1 AS v
			UNION ALL
			SELECT COALESCE(MAX(sort_order), -1) FROM accounts WHERE user_id = ? AND group_id = ? AND COALESCE(is_deleting, 0) = 0
			UNION ALL
			SELECT COALESCE(MAX(sort_order), -1) FROM account_groups WHERE user_id = ? AND COALESCE(parent_id, '') = ?
		)`, userID, groupID, userID, groupID).Scan(&next)
	return next, err
}

func nextAccountContainerOrderTx(ctx context.Context, tx *sql.Tx, userID, parentGroupID string) (int, error) {
	var next int
	err := tx.QueryRowContext(ctx, `
		SELECT MAX(v) + 1 FROM (
			SELECT -1 AS v
			UNION ALL
			SELECT COALESCE(MAX(sort_order), -1) FROM accounts WHERE user_id = ? AND group_id = ? AND COALESCE(is_deleting, 0) = 0
			UNION ALL
			SELECT COALESCE(MAX(sort_order), -1) FROM account_groups WHERE user_id = ? AND COALESCE(parent_id, '') = ?
		)`, userID, parentGroupID, userID, parentGroupID).Scan(&next)
	return next, err
}

// CreateAccountGroup creates a group for userID at the end of parentID (or at
// the top level when parentID is ""). The caller has checked proxyID when set.
func (db *DB) CreateAccountGroup(ctx context.Context, userID, parentID, name, proxyID string) (models.AccountGroup, error) {
	name = normalizeAccountGroupName(name)
	if name == "" {
		return models.AccountGroup{}, ErrAccountGroupNameEmpty
	}
	parentID = strings.TrimSpace(parentID)
	proxyID = strings.TrimSpace(proxyID)

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return models.AccountGroup{}, err
	}
	defer tx.Rollback()

	if parentID != "" {
		var ok int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_groups WHERE id = ? AND user_id = ?`, parentID, userID).Scan(&ok); err != nil {
			return models.AccountGroup{}, err
		}
		if ok == 0 {
			return models.AccountGroup{}, ErrAccountGroupNotFound
		}
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_groups
		WHERE user_id = ? AND COALESCE(parent_id, '') = ? AND name = ? COLLATE NOCASE`,
		userID, parentID, name).Scan(&taken); err != nil {
		return models.AccountGroup{}, err
	}
	if taken > 0 {
		return models.AccountGroup{}, ErrAccountGroupNameTaken
	}
	order, err := nextAccountContainerOrderTx(ctx, tx, userID, parentID)
	if err != nil {
		return models.AccountGroup{}, err
	}
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_groups (id, user_id, parent_id, name, sort_order, proxy_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, nullableGroupID(parentID), name, order, proxyID); err != nil {
		return models.AccountGroup{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.AccountGroup{}, err
	}
	return models.AccountGroup{
		ID:        id,
		ParentID:  parentID,
		Name:      name,
		SortOrder: order,
		ProxyID:   proxyID,
	}, nil
}

// UpdateAccountGroup renames one of userID's groups or moves it into parentID
// ("" for the top level). It refuses moving a group into itself or one of its
// own descendants, and reports whether its parent changed.
func (db *DB) UpdateAccountGroup(ctx context.Context, userID, groupID, name, parentID string) (models.AccountGroup, bool, error) {
	name = normalizeAccountGroupName(name)
	if name == "" {
		return models.AccountGroup{}, false, ErrAccountGroupNameEmpty
	}
	groupID = strings.TrimSpace(groupID)
	parentID = strings.TrimSpace(parentID)

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return models.AccountGroup{}, false, err
	}
	defer tx.Rollback()

	var current models.AccountGroup
	err = tx.QueryRowContext(ctx, `
		SELECT id, COALESCE(parent_id, ''), sort_order, proxy_id
		FROM account_groups WHERE id = ? AND user_id = ?`, groupID, userID,
	).Scan(&current.ID, &current.ParentID, &current.SortOrder, &current.ProxyID)
	if errors.Is(err, sql.ErrNoRows) {
		return models.AccountGroup{}, false, ErrAccountGroupNotFound
	}
	if err != nil {
		return models.AccountGroup{}, false, err
	}

	if parentID != "" {
		if parentID == groupID {
			return models.AccountGroup{}, false, ErrAccountGroupCycle
		}
		seen := map[string]bool{groupID: true}
		cur := parentID
		for cur != "" {
			if seen[cur] {
				return models.AccountGroup{}, false, ErrAccountGroupCycle
			}
			seen[cur] = true
			var nextParent string
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(parent_id, '') FROM account_groups WHERE id = ? AND user_id = ?`, cur, userID).Scan(&nextParent)
			if errors.Is(err, sql.ErrNoRows) {
				return models.AccountGroup{}, false, ErrAccountGroupNotFound
			}
			if err != nil {
				return models.AccountGroup{}, false, err
			}
			cur = nextParent
		}
	}

	var taken int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_groups
		WHERE user_id = ? AND COALESCE(parent_id, '') = ? AND name = ? COLLATE NOCASE AND id != ?`,
		userID, parentID, name, groupID).Scan(&taken); err != nil {
		return models.AccountGroup{}, false, err
	}
	if taken > 0 {
		return models.AccountGroup{}, false, ErrAccountGroupNameTaken
	}

	parentChanged := parentID != current.ParentID
	sortOrder := current.SortOrder
	if parentChanged {
		sortOrder, err = nextAccountContainerOrderTx(ctx, tx, userID, parentID)
		if err != nil {
			return models.AccountGroup{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE account_groups
		SET name = ?, parent_id = ?, sort_order = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ?`,
		name, nullableGroupID(parentID), sortOrder, groupID, userID); err != nil {
		return models.AccountGroup{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return models.AccountGroup{}, false, err
	}
	current.Name = name
	current.ParentID = parentID
	current.SortOrder = sortOrder
	return current, parentChanged, nil
}

// DeleteAccountGroup removes one of userID's groups and lifts its direct
// subgroups and accounts up into the deleted group's parent, keeping their
// order. It returns the active accounts that were inside the group's subtree so
// their connections can pick up the new proxy chain.
func (db *DB) DeleteAccountGroup(ctx context.Context, userID, groupID string) ([]string, error) {
	groupID = strings.TrimSpace(groupID)
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var parentID string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(parent_id, '') FROM account_groups WHERE id = ? AND user_id = ?`, groupID, userID).Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountGroupNotFound
	}
	if err != nil {
		return nil, err
	}

	affected, err := descendantAccountIDsTx(ctx, tx, userID, groupID)
	if err != nil {
		return nil, err
	}

	type childEntry struct {
		id        string
		name      string
		sortOrder int
		isGroup   bool
	}
	var lifted []childEntry

	gRows, err := tx.QueryContext(ctx, `SELECT id, name, sort_order FROM account_groups WHERE user_id = ? AND parent_id = ? ORDER BY sort_order, id`, userID, groupID)
	if err != nil {
		return nil, err
	}
	for gRows.Next() {
		var e childEntry
		e.isGroup = true
		if err := gRows.Scan(&e.id, &e.name, &e.sortOrder); err != nil {
			gRows.Close()
			return nil, err
		}
		lifted = append(lifted, e)
	}
	if err := gRows.Close(); err != nil {
		return nil, err
	}

	takenNames := map[string]bool{}
	if len(lifted) > 0 {
		sRows, err := tx.QueryContext(ctx, `SELECT name FROM account_groups WHERE user_id = ? AND COALESCE(parent_id, '') = ? AND id != ?`, userID, parentID, groupID)
		if err != nil {
			return nil, err
		}
		for sRows.Next() {
			var siblingName string
			if err := sRows.Scan(&siblingName); err != nil {
				sRows.Close()
				return nil, err
			}
			takenNames[strings.ToLower(siblingName)] = true
		}
		if err := sRows.Close(); err != nil {
			return nil, err
		}
	}

	aRows, err := tx.QueryContext(ctx, `SELECT id, sort_order FROM accounts WHERE user_id = ? AND group_id = ? ORDER BY sort_order, id`, userID, groupID)
	if err != nil {
		return nil, err
	}
	for aRows.Next() {
		var e childEntry
		if err := aRows.Scan(&e.id, &e.sortOrder); err != nil {
			aRows.Close()
			return nil, err
		}
		lifted = append(lifted, e)
	}
	if err := aRows.Close(); err != nil {
		return nil, err
	}

	slices.SortFunc(lifted, func(a, b childEntry) int {
		if c := cmp.Compare(a.sortOrder, b.sortOrder); c != 0 {
			return c
		}
		if a.isGroup != b.isGroup {
			if !a.isGroup {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.id, b.id)
	})

	base, err := nextAccountContainerOrderTx(ctx, tx, userID, parentID)
	if err != nil {
		return nil, err
	}
	for i, e := range lifted {
		if e.isGroup {
			name := uniqueLiftedGroupName(e.name, takenNames)
			takenNames[strings.ToLower(name)] = true
			if _, err := tx.ExecContext(ctx, `UPDATE account_groups SET name = ?, parent_id = ?, sort_order = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`,
				name, nullableGroupID(parentID), base+i, e.id, userID); err != nil {
				return nil, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET group_id = ?, sort_order = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`,
				parentID, base+i, e.id, userID); err != nil {
				return nil, err
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM account_groups WHERE id = ? AND user_id = ?`, groupID, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}

func descendantAccountIDsTx(ctx context.Context, tx *sql.Tx, userID, groupID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE sub(id) AS (
			SELECT id FROM account_groups WHERE id = ? AND user_id = ?
			UNION
			SELECT g.id FROM account_groups g JOIN sub s ON g.parent_id = s.id WHERE g.user_id = ?
		)
		SELECT a.id FROM accounts a JOIN sub s ON a.group_id = s.id
		WHERE a.user_id = ? AND a.provider != 'local' AND COALESCE(a.is_deleting, 0) = 0
		ORDER BY a.id`, groupID, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AccountGroupAccountIDs lists the non-deleting accounts inside groupID and
// any of its subgroups for userID.
func (db *DB) AccountGroupAccountIDs(ctx context.Context, userID, groupID string) ([]string, error) {
	rows, err := db.read.QueryContext(ctx, `
		WITH RECURSIVE sub(id) AS (
			SELECT id FROM account_groups WHERE id = ? AND user_id = ?
			UNION
			SELECT g.id FROM account_groups g JOIN sub s ON g.parent_id = s.id WHERE g.user_id = ?
		)
		SELECT a.id FROM accounts a JOIN sub s ON a.group_id = s.id
		WHERE a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		ORDER BY a.id`, strings.TrimSpace(groupID), userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetAccountGroupProxyChoice records the proxy choice of one of userID's
// groups. The caller has checked that a proxy id names a saved proxy.
func (db *DB) SetAccountGroupProxyChoice(ctx context.Context, userID, groupID, choice string) error {
	result, err := db.write.ExecContext(ctx, `
		UPDATE account_groups SET proxy_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ?`, strings.TrimSpace(choice), strings.TrimSpace(groupID), userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrAccountGroupNotFound
	}
	return nil
}

// IsBackgroundSyncEnabled reports whether automatic background sync is on for
// an active connected account.
func (db *DB) IsBackgroundSyncEnabled(ctx context.Context, accountID string) bool {
	var enabled int
	err := db.read.QueryRowContext(ctx, `
		SELECT COALESCE(a.background_sync_enabled, 1)
		FROM accounts a JOIN users u ON u.id = a.user_id
		WHERE a.id = ? AND COALESCE(a.is_deleting, 0) = 0 AND a.provider != 'local' AND u.status = 'active'`, accountID).Scan(&enabled)
	return err == nil && enabled == 1
}

// SetAccountBackgroundSyncEnabled turns automatic background sync on or off
// for one of userID's connected accounts. An unknown, deleting or Local
// folders account returns sql.ErrNoRows.
func (db *DB) SetAccountBackgroundSyncEnabled(ctx context.Context, userID, accountID string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	res, err := db.write.ExecContext(ctx, `
		UPDATE accounts SET background_sync_enabled = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ? AND provider != 'local' AND COALESCE(is_deleting, 0) = 0`,
		value, strings.TrimSpace(accountID), userID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAccountGroupBackgroundSyncEnabled turns automatic background sync on or
// off for every connected account in groupID and its subgroups, returning the
// IDs of all non-deleting connected accounts in that subtree. An unknown group
// returns ErrAccountGroupNotFound.
func (db *DB) SetAccountGroupBackgroundSyncEnabled(ctx context.Context, userID, groupID string, enabled bool) ([]string, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, ErrAccountGroupNotFound
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM account_groups WHERE id = ? AND user_id = ?`, groupID, userID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountGroupNotFound
	} else if err != nil {
		return nil, err
	}

	ids, err := descendantAccountIDsTx(ctx, tx, userID, groupID)
	if err != nil {
		return nil, err
	}
	value := 0
	if enabled {
		value = 1
	}
	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx, `
			WITH RECURSIVE sub(id) AS (
				SELECT id FROM account_groups WHERE id = ? AND user_id = ?
				UNION
				SELECT g.id FROM account_groups g JOIN sub s ON g.parent_id = s.id WHERE g.user_id = ?
			)
			UPDATE accounts SET background_sync_enabled = ?, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND provider != 'local' AND COALESCE(is_deleting, 0) = 0
			  AND group_id IN (SELECT id FROM sub)`, groupID, userID, userID, value, userID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// MoveAccountToGroup places one of userID's connected accounts into groupID
// (or at the top level when groupID is ""), at the end of that container.
// Local folders cannot be placed inside a group.
func (db *DB) MoveAccountToGroup(ctx context.Context, userID, accountID, groupID string) error {
	if strings.TrimSpace(accountID) == "" {
		return sql.ErrNoRows
	}
	_, err := db.MoveAccountsToGroup(ctx, userID, groupID, []string{accountID})
	return err
}

// MoveAccountsToGroup places the selected connected accounts of userID into
// groupID (or at the top level when groupID is ""), appending moved accounts
// in the given order. It returns the IDs of accounts whose group_id changed.
func (db *DB) MoveAccountsToGroup(ctx context.Context, userID, groupID string, accountIDs []string) ([]string, error) {
	groupID = strings.TrimSpace(groupID)
	seen := make(map[string]struct{}, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	for _, raw := range accountIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		if groupID != "" {
			exists, err := db.AccountGroupExists(ctx, userID, groupID)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, ErrAccountGroupNotFound
			}
		}
		return nil, nil
	}

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if groupID != "" {
		var ok int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_groups WHERE id = ? AND user_id = ?`, groupID, userID).Scan(&ok); err != nil {
			return nil, err
		}
		if ok == 0 {
			return nil, ErrAccountGroupNotFound
		}
	}

	order, err := nextAccountContainerOrderTx(ctx, tx, userID, groupID)
	if err != nil {
		return nil, err
	}

	moved := make([]string, 0, len(ids))
	for _, accountID := range ids {
		var currentGroup, provider string
		err := tx.QueryRowContext(ctx, `
			SELECT group_id, provider FROM accounts
			WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID,
		).Scan(&currentGroup, &provider)
		if err != nil {
			return nil, err
		}
		if provider == localProvider && groupID != "" {
			return nil, sql.ErrNoRows
		}
		if currentGroup == groupID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE accounts SET group_id = ?, sort_order = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND user_id = ?`, groupID, order, accountID, userID); err != nil {
			return nil, err
		}
		order++
		moved = append(moved, accountID)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return moved, nil
}

// GetGroupProxyChain returns the proxy choices from groupID up through its
// ancestor groups for userID, from nearest group to outermost.
func (db *DB) GetGroupProxyChain(ctx context.Context, userID, groupID string) ([]string, error) {
	return getGroupProxyChainFrom(ctx, db.read, userID, groupID)
}

// GetGroupProxyChainTx is GetGroupProxyChain inside an existing transaction.
func GetGroupProxyChainTx(ctx context.Context, tx *sql.Tx, userID, groupID string) ([]string, error) {
	return getGroupProxyChainFrom(ctx, tx, userID, groupID)
}

func getGroupProxyChainFrom(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID, groupID string) ([]string, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" || userID == "" {
		return nil, nil
	}
	var chain []string
	seen := map[string]bool{}
	cur := groupID
	for cur != "" && !seen[cur] {
		seen[cur] = true
		var proxyID, parentID string
		err := q.QueryRowContext(ctx, `
			SELECT proxy_id, COALESCE(parent_id, '')
			FROM account_groups WHERE id = ? AND user_id = ?`, cur, userID,
		).Scan(&proxyID, &parentID)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		chain = append(chain, proxyID)
		cur = parentID
	}
	return chain, nil
}

// GetAccountProxyChain returns the account's owner and its proxy choice
// followed by its enclosing groups' choices from nearest group to outermost.
func (db *DB) GetAccountProxyChain(ctx context.Context, accountID string) (string, []string, error) {
	var owner sql.NullString
	var proxyID, groupID string
	err := db.read.QueryRowContext(ctx, `SELECT user_id, proxy_id, group_id FROM accounts WHERE id = ?`, accountID).Scan(&owner, &proxyID, &groupID)
	if err != nil {
		return "", nil, err
	}
	chain := []string{proxyID}
	if groupID != "" && owner.String != "" {
		groups, err := getGroupProxyChainFrom(ctx, db.read, owner.String, groupID)
		if err != nil {
			return "", nil, err
		}
		chain = append(chain, groups...)
	}
	return owner.String, chain, nil
}

// attachAccountGroups fills GroupPath on each account and orders the slice in
// depth-first tree order matching the user's groups and sort_order.
func (db *DB) attachAccountGroups(ctx context.Context, userID string, accounts []models.Account) error {
	if len(accounts) == 0 {
		return nil
	}
	groups, err := db.ListAccountGroups(ctx, userID)
	if err != nil {
		return fmt.Errorf("list account groups: %w", err)
	}
	if len(groups) == 0 {
		for i := range accounts {
			accounts[i].GroupID = ""
		}
		return nil
	}
	byID := make(map[string]models.AccountGroup, len(groups))
	childGroups := make(map[string][]models.AccountGroup)
	for _, g := range groups {
		byID[g.ID] = g
		childGroups[g.ParentID] = append(childGroups[g.ParentID], g)
	}

	directAccounts := make(map[string][]int)
	for i := range accounts {
		gid := accounts[i].GroupID
		if gid != "" {
			if _, ok := byID[gid]; !ok {
				gid = ""
				accounts[i].GroupID = ""
			}
		}
		if gid != "" {
			var path []models.AccountGroupRef
			cur := gid
			for cur != "" {
				g, ok := byID[cur]
				if !ok {
					break
				}
				path = append(path, models.AccountGroupRef{ID: g.ID, Name: g.Name, SortOrder: g.SortOrder})
				cur = g.ParentID
			}
			slices.Reverse(path)
			accounts[i].GroupPath = path
		}
		directAccounts[gid] = append(directAccounts[gid], i)
	}

	type treeEntry struct {
		sortOrder int
		isGroup   bool
		id        string
		accIdx    int
	}
	ordered := make([]models.Account, 0, len(accounts))
	var walk func(parent string)
	walk = func(parent string) {
		var entries []treeEntry
		for _, g := range childGroups[parent] {
			entries = append(entries, treeEntry{sortOrder: g.SortOrder, isGroup: true, id: g.ID})
		}
		for _, idx := range directAccounts[parent] {
			entries = append(entries, treeEntry{sortOrder: accounts[idx].SortOrder, isGroup: false, id: accounts[idx].ID, accIdx: idx})
		}
		slices.SortFunc(entries, func(a, b treeEntry) int {
			if c := cmp.Compare(a.sortOrder, b.sortOrder); c != 0 {
				return c
			}
			if a.isGroup != b.isGroup {
				if !a.isGroup {
					return -1
				}
				return 1
			}
			return cmp.Compare(a.id, b.id)
		})
		for _, e := range entries {
			if e.isGroup {
				walk(e.id)
			} else {
				ordered = append(ordered, accounts[e.accIdx])
			}
		}
	}
	walk("")
	copy(accounts, ordered)
	return nil
}
