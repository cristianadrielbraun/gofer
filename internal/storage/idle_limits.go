package storage

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Each IMAP account keeps what Gofer learned about how many folders its
// server lets it watch in real time, so a restart doesn't try the server
// again with more connections than it accepted.
const accountIdleLimitsTableSQL = `CREATE TABLE IF NOT EXISTS account_idle_limits (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    connection_limit INTEGER NOT NULL CHECK (connection_limit >= 1),
    failed_at INTEGER NOT NULL DEFAULT 0,
    failed_time DATETIME,
    stable_at INTEGER NOT NULL DEFAULT 0,
    stable_since DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// GetIdleConnectionLimit returns the account's learned limit, and false when
// nothing was learned yet.
func (db *DB) GetIdleConnectionLimit(ctx context.Context, accountID string) (models.IdleConnectionLimit, bool, error) {
	var limit models.IdleConnectionLimit
	var failed, stable sql.NullTime
	err := db.Read().QueryRowContext(ctx, `SELECT connection_limit, failed_at, failed_time, stable_at, stable_since
		FROM account_idle_limits WHERE account_id = ?`, accountID).Scan(&limit.Limit, &limit.FailedAt, &failed, &limit.StableAt, &stable)
	if errors.Is(err, sql.ErrNoRows) {
		return models.IdleConnectionLimit{}, false, nil
	}
	if err != nil {
		return models.IdleConnectionLimit{}, false, err
	}
	limit.FailedTime = failed.Time
	limit.StableSince = stable.Time
	return limit, true, nil
}

// SaveIdleConnectionLimit keeps the account's learned limit.
func (db *DB) SaveIdleConnectionLimit(ctx context.Context, accountID string, limit models.IdleConnectionLimit) error {
	_, err := db.Write().ExecContext(ctx, `INSERT INTO account_idle_limits
		(account_id, connection_limit, failed_at, failed_time, stable_at, stable_since, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP WHERE EXISTS (SELECT 1 FROM accounts WHERE id = ?)
		ON CONFLICT(account_id) DO UPDATE SET connection_limit = excluded.connection_limit,
			failed_at = excluded.failed_at, failed_time = excluded.failed_time,
			stable_at = excluded.stable_at, stable_since = excluded.stable_since,
			updated_at = excluded.updated_at`,
		accountID, max(limit.Limit, 1), limit.FailedAt, nullTime(limit.FailedTime), limit.StableAt, nullTime(limit.StableSince), accountID)
	return err
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t.UTC(), Valid: !t.IsZero()}
}

// migrateV145ToV146 adds the learned real-time folder limits. Nothing is
// learned yet, so every account starts from the default.
func migrateV145ToV146(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 146 {
		return nil
	}
	if _, err := tx.Exec(accountIdleLimitsTableSQL); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 146); err != nil {
		return err
	}
	return tx.Commit()
}

// IdleFolderRanks orders the account's folders for real-time watching when
// its server allows fewer watches than folders asked for one: Inbox first,
// then the folders as the sidebar shows them, each before the folders inside
// it. A lower rank is watched first.
func (db *DB) IdleFolderRanks(ctx context.Context, owner, accountID string) (map[string]int, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT f.id, COALESCE(f.parent_id, ''), COALESCE(f.role, ''), f.sort_order, folder_order.value
		FROM folders f
		LEFT JOIN app_settings folder_order ON folder_order.user_id = ? AND folder_order.key = 'folder_order:' || f.id
		WHERE f.account_id = ? AND COALESCE(f.discovery_state, 'active') = 'active'
		ORDER BY f.sort_order`, owner, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rankedFolder struct {
		id, parent, role string
		place            sql.NullInt64
	}
	var folders []rankedFolder
	known := map[string]bool{}
	for rows.Next() {
		var f rankedFolder
		var serverOrder sql.NullInt64
		var place sql.NullString
		if err := rows.Scan(&f.id, &f.parent, &f.role, &serverOrder, &place); err != nil {
			return nil, err
		}
		if place.Valid && validOrderPlace(place.String) {
			n, _ := strconv.Atoi(place.String)
			f.place = sql.NullInt64{Int64: int64(n), Valid: true}
		}
		folders = append(folders, f)
		known[f.id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortByUserOrder(folders, func(f rankedFolder) sql.NullInt64 { return f.place })
	children := map[string][]rankedFolder{}
	for _, f := range folders {
		parent := f.parent
		if !known[parent] {
			parent = ""
		}
		children[parent] = append(children[parent], f)
	}
	ranks := make(map[string]int, len(folders))
	next := 1
	var walk func(parent string)
	walk = func(parent string) {
		for _, f := range children[parent] {
			if _, seen := ranks[f.id]; seen {
				continue
			}
			if f.role == "inbox" {
				ranks[f.id] = 0
			} else {
				ranks[f.id] = next
				next++
			}
			walk(f.id)
		}
	}
	walk("")
	return ranks, nil
}
