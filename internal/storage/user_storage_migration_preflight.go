package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

var ErrUserStorageMigrationSource = errors.New("shared database cannot be migrated to per-user storage")

const (
	migrationCentral       = "central-copy"
	migrationUsers         = "central-copy-local-stub"
	migrationLocal         = "local-copy"
	migrationPreferences   = "split-explicit-owner-contract"
	migrationOAuth         = "central-transform"
	migrationGenerated     = "generated"
	migrationSearch        = "local-rebuild-compare"
	migrationSearchShadow  = "local-rebuild-shadow"
	migrationLegacySenders = "attributable-legacy-transform"
	migrationSchema        = "metadata-initialize-validate"
	migrationSequence      = "metadata-preserve-high-water"
	migrationPlanner       = "metadata-rebuild-planner"
)

type migrationTableRule struct{ destination, owner, columns string }
type migrationLink struct {
	child, parent string
	from, to      []string
}

// Table inventory and row counts contain no credential or mailbox content.
type UserStorageMigrationTable struct {
	Name        string                          `json:"name"`
	Destination string                          `json:"destination"`
	Columns     []string                        `json:"columns"`
	References  []UserStorageMigrationReference `json:"references,omitempty"`
	Rows        int64                           `json:"rows"`
}

type UserStorageMigrationReference struct {
	Columns       []string `json:"columns"`
	Table         string   `json:"table"`
	TargetColumns []string `json:"target_columns"`
	Required      bool     `json:"required"`
}

type UserStorageMigrationPreflight struct {
	SchemaVersion       int                         `json:"schema_version"`
	TargetSchemaVersion int                         `json:"target_schema_version"`
	Owners              int64                       `json:"owners"`
	Accounts            int64                       `json:"accounts"`
	Tables              []UserStorageMigrationTable `json:"tables"`
}

// UserStorageMigrationInventory returns an independent, sorted policy snapshot.
// Optional SQLite planner tables are included even if not created by Gofer.
func UserStorageMigrationInventory() []UserStorageMigrationTable {
	result := make([]UserStorageMigrationTable, 0, len(userStorageMigrationTables))
	for name, rule := range userStorageMigrationTables {
		table := UserStorageMigrationTable{Name: name, Destination: rule.destination, Columns: strings.Split(rule.columns, ",")}
		for _, group := range []struct {
			links    []migrationLink
			required bool
		}{{userStorageMigrationLinks, true}, {userStorageMigrationOptionalLinks, false}} {
			for _, link := range group.links {
				if link.child == name {
					table.References = append(table.References, UserStorageMigrationReference{Columns: append([]string(nil), link.from...), Table: link.parent, TargetColumns: append([]string(nil), link.to...), Required: group.required})
				}
			}
		}
		result = append(result, table)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (r migrationTableRule) ownerSQL(schema, alias string) string {
	prefix := ""
	if schema != "" {
		prefix = quoteStoreIdentifier(schema) + "."
	}
	return strings.ReplaceAll(strings.ReplaceAll(r.owner, "@", prefix), "r.", alias+".")
}

// InspectUserStorageMigration is offline and read-only. The same exclusive
// runtime lock used by the server/operator stays held for the entire inspection.
// It neither upgrades the source nor creates destination files or a manifest.
func InspectUserStorageMigration(ctx context.Context, sourcePath string) (UserStorageMigrationPreflight, error) {
	if err := ctx.Err(); err != nil {
		return UserStorageMigrationPreflight{}, err
	}
	if err := requireExistingDatabase(sourcePath); err != nil {
		return UserStorageMigrationPreflight{}, err
	}
	lock, err := runtimeguard.Acquire(sourcePath)
	if err != nil {
		return UserStorageMigrationPreflight{}, err
	}
	defer lock.Close()
	source, err := openUserStorageMigrationSource(ctx, sourcePath)
	if err != nil {
		return UserStorageMigrationPreflight{}, err
	}
	defer source.Close()
	return inspectUserStorageMigration(ctx, source)
}

func migrationSharedVersionSupported(version int) bool {
	return version == 105 || version == 106 || version == 107
}

// These recognized shared schemas differ only in the explicitly handled push
// revision and response-claim additions. Older schemas need a separate reviewed
// upgrade path; never run application migrations against the original source.
func openUserStorageMigrationSource(ctx context.Context, path string) (*DB, error) {
	if err := requireExistingDatabase(path); err != nil {
		return nil, err
	}
	read, err := openReadOnlyDB(path)
	if err != nil {
		return nil, err
	}
	read.SetMaxOpenConns(4)
	write, err := openReadOnlyDB(path)
	if err != nil {
		read.Close()
		return nil, err
	}
	write.SetMaxOpenConns(1)
	source := &DB{read: read, write: write, path: path}
	var version int
	if err := read.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		source.Close()
		return nil, err
	}
	if !migrationSharedVersionSupported(version) {
		source.Close()
		return nil, migrationSourceError("schema_version", "source version is not a recognized shared schema")
	}
	return source, nil
}

// The eventual copier calls this under its already-held source lock and keeps
// that lock through verification/publication. It must not inspect, release the
// lock, then copy a source that another process could have changed.
func inspectUserStorageMigration(ctx context.Context, source *DB) (report UserStorageMigrationPreflight, err error) {
	tx, err := source.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&report.SchemaVersion); err != nil {
		return report, err
	}
	if !migrationSharedVersionSupported(report.SchemaVersion) {
		return report, ErrUserStorageMigrationSource
	}
	report.TargetSchemaVersion = CurrentSchemaVersion
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
	if err != nil {
		return report, err
	}
	present := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return report, err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	if present["gofer_user_store"] {
		return report, migrationSourceError("gofer_user_store", "source is already a user store")
	}
	if err := requireSharedMigrationTables(ctx, present, report.SchemaVersion); err != nil {
		return report, err
	}
	for _, policy := range UserStorageMigrationInventory() {
		if !present[policy.Name] {
			continue
		}
		if err := migrationSourceColumns(ctx, tx, &policy, report.SchemaVersion); err != nil {
			return report, err
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteStoreIdentifier(policy.Name)).Scan(&policy.Rows); err != nil {
			return report, err
		}
		if policy.Destination == migrationGenerated && policy.Rows != 0 {
			return report, migrationSourceError(policy.Name, "source contains a mixed storage layout")
		}
		report.Tables = append(report.Tables, policy)
	}
	for name := range present {
		if _, known := userStorageMigrationTables[name]; !known {
			return report, migrationSourceError(name, "table has no migration policy")
		}
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return report, err
	}
	if integrity != "ok" {
		return report, migrationSourceError("sqlite", "source integrity check failed")
	}
	if err := migrationSourceForeignKeys(ctx, tx); err != nil {
		return report, err
	}
	for _, table := range report.Tables {
		rule := userStorageMigrationTables[table.Name]
		if rule.owner == "" || rule.destination == migrationUsers {
			continue
		}
		owner := rule.ownerSQL("", "r")
		allowed := `u.user_type='webmail' AND u.is_admin=0`
		if rule.destination == migrationPreferences {
			allowed = `((u.user_type='webmail' AND u.is_admin=0) OR u.user_type='management')`
		}
		if err := migrationRejectRows(ctx, tx, table.Name, `NOT EXISTS(SELECT 1 FROM users u WHERE u.id=(`+owner+`) AND `+allowed+`)`, "row has no valid storage owner"); err != nil {
			return report, err
		}
	}
	for _, link := range userStorageMigrationLinks {
		if !present[link.child] {
			continue
		}
		if err := migrationCheckLink(ctx, tx, link, present, true); err != nil {
			return report, err
		}
	}
	for _, link := range userStorageMigrationOptionalLinks {
		if present[link.child] {
			if err := migrationCheckLink(ctx, tx, link, present, false); err != nil {
				return report, err
			}
		}
	}
	if err := migrationRejectRows(ctx, tx, "message_search", `NOT EXISTS(SELECT 1 FROM messages m WHERE m.id=r.rowid AND m.account_id IS r.account_id)`, "search row does not match its original message"); err != nil {
		return report, err
	}
	// Existing upload namespaces trim owner IDs, while store paths use the exact
	// identity. Never migrate an identity that could alias another owner's files.
	rows, err = tx.QueryContext(ctx, `SELECT id FROM users WHERE user_type='webmail' AND is_admin=0`)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			rows.Close()
			return report, err
		}
		if owner == "" || strings.TrimSpace(owner) != owner {
			rows.Close()
			return report, migrationSourceError("users", "owner identity conflicts with the existing upload namespace")
		}
		report.Owners++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&report.Accounts); err != nil {
		return report, err
	}
	if err := migrationSourceSequences(ctx, tx, present); err != nil {
		return report, err
	}
	if err := migrationSourcePayloads(ctx, tx); err != nil {
		return report, err
	}
	return report, tx.Commit()
}

func migrationSourceError(table, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrUserStorageMigrationSource, table, reason)
}

func migrationRejectRows(ctx context.Context, tx *sql.Tx, table, condition, reason string) error {
	var invalid bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+quoteStoreIdentifier(table)+" r WHERE "+condition+")").Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return migrationSourceError(table, reason)
	}
	return nil
}

func migrationSourceColumns(ctx context.Context, tx *sql.Tx, table *UserStorageMigrationTable, version int) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_xinfo(?) WHERE hidden<>1`, table.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		actual[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if table.Name == "calendar_response_requests" && actual["claim_id"] {
		var kind, defaultValue string
		var required int
		if err := tx.QueryRowContext(ctx, `SELECT type,"notnull",dflt_value FROM pragma_table_xinfo('calendar_response_requests') WHERE name='claim_id'`).Scan(&kind, &required, &defaultValue); err != nil {
			return migrationSourceError(table.Name, "claim representation is incompatible")
		}
		if !strings.EqualFold(kind, "TEXT") || required != 1 || defaultValue != "''" {
			return migrationSourceError(table.Name, "claim representation is incompatible")
		}
	}
	// Older supported databases recorded only version numbers. The application
	// schema upgrader deliberately does not require historical timestamps.
	if table.Name == "schema_version" && len(actual) == 1 && actual["version"] {
		table.Columns = []string{"version"}
		return nil
	}
	missing := ""
	if table.Name == "calendar_response_requests" && version < 107 {
		missing = "claim_id"
	} else if table.Name == "web_push_subscriptions" && version < 106 {
		missing = "revision"
	}
	if missing != "" && !actual[missing] {
		columns := make([]string, 0, len(table.Columns)-1)
		for _, column := range table.Columns {
			if column != missing {
				columns = append(columns, column)
			}
		}
		table.Columns = columns
	}
	if len(actual) != len(table.Columns) {
		return migrationSourceError(table.Name, "columns have no complete migration policy")
	}
	for _, column := range table.Columns {
		if !actual[column] {
			return migrationSourceError(table.Name, "columns have no complete migration policy")
		}
	}
	return nil
}

func requireSharedMigrationTables(ctx context.Context, present map[string]bool, version int) error {
	// Use Gofer's actual embedded schema to identify required shared tables. New
	// source tables cannot silently disappear merely because an old policy omitted
	// them. This disposable native SQLite connection has no filesystem data.
	memory, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer memory.Close()
	memory.SetMaxOpenConns(1)
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	if _, err := memory.ExecContext(ctx, string(schema)); err != nil {
		return err
	}
	rows, err := memory.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !present[name] {
			if name == "web_push_subscriptions" && version == 105 {
				continue // Same recognized historical absence as migrateV105ToV106.
			}
			return migrationSourceError(name, "required shared table is missing")
		}
	}
	return rows.Err()
}

func migrationSourceForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return migrationSourceError("sqlite", "source contains a foreign-key violation")
	}
	return rows.Err()
}

func migrationCheckLink(ctx context.Context, tx *sql.Tx, link migrationLink, present map[string]bool, required bool) error {
	if !present[link.parent] {
		return migrationSourceError(link.child, "reference target table is missing")
	}
	var join, nonnull []string
	for i, from := range link.from {
		join = append(join, "p."+quoteStoreIdentifier(link.to[i])+"=r."+quoteStoreIdentifier(from))
		nonnull = append(nonnull, "r."+quoteStoreIdentifier(from)+" IS NOT NULL")
	}
	match := strings.Join(join, " AND ")
	if required {
		if err := migrationRejectRows(ctx, tx, link.child, strings.Join(nonnull, " AND ")+` AND NOT EXISTS(SELECT 1 FROM `+quoteStoreIdentifier(link.parent)+` p WHERE `+match+`)`, "required relationship is missing"); err != nil {
			return err
		}
	}
	child, parent := userStorageMigrationTables[link.child], userStorageMigrationTables[link.parent]
	if child.owner == "" || parent.owner == "" {
		return nil
	}
	return migrationRejectRows(ctx, tx, link.child, `EXISTS(SELECT 1 FROM `+quoteStoreIdentifier(link.parent)+` p WHERE `+match+` AND (`+child.ownerSQL("", "r")+`) IS NOT (`+parent.ownerSQL("", "p")+`))`, "relationship crosses storage owners")
}

func migrationSourceSequences(ctx context.Context, tx *sql.Tx, present map[string]bool) error {
	if !present["sqlite_sequence"] {
		return nil
	}
	if err := migrationRejectRows(ctx, tx, "sqlite_sequence", `typeof(r.seq)<>'integer' OR r.seq<0`, "invalid AUTOINCREMENT high-water mark"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_sequence`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, known := userStorageMigrationSequences[name]; !known || !present[name] {
			return migrationSourceError("sqlite_sequence", "sequence has no supported destination")
		}
	}
	return rows.Err()
}
