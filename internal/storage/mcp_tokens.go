package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MCPTokenPrefix starts every personal access token for the MCP server, so a
// leaked one is easy to recognize and scan for.
const MCPTokenPrefix = "gofer_mcp_"

// ErrMCPTokenInvalid is a token that is unknown, revoked or expired.
var ErrMCPTokenInvalid = errors.New("invalid MCP token")

// MCPToken is a token an AI client signs in to the MCP server with. Only its
// hash is stored; the token itself is shown once, when it is made.
type MCPToken struct {
	ID       string
	UserID   string
	Name     string
	Scopes   []string
	ClientID string
	// GrantID is the OAuth grant the token belongs to, "" for a personal
	// token.
	GrantID string
	// Mailboxes are the mailboxes the token may reach (MCP mailbox keys);
	// none means every mailbox, those added later included.
	Mailboxes []string
	// Kind is "personal" for one made in Settings, "oauth" for one an OAuth
	// client was given.
	Kind       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Active reports whether the token can still be used at.
func (t MCPToken) Active(at time.Time) bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || at.Before(*t.ExpiresAt))
}

// ensureMCPTokenSchema adds the table of MCP tokens. Like the shared mailbox
// schema, it checks before it changes anything and runs on every start.
func ensureMCPTokenSchema(db *sql.DB) error {
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'users'`).Scan(&users); err != nil {
		return fmt.Errorf("check users table: %w", err)
	}
	if users == 0 {
		return nil
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_tokens (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL UNIQUE,
			scopes TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'oauth')),
			client_id TEXT NOT NULL DEFAULT '',
			-- The OAuth grant an access token was issued under (mcp_oauth.go).
			grant_id TEXT NOT NULL DEFAULT '',
			-- The mailboxes the token may reach, one key a line; '' for all.
			mailboxes TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at DATETIME,
			last_used_at DATETIME,
			revoked_at DATETIME
		)`); err != nil {
		return fmt.Errorf("create mcp_tokens: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_mcp_tokens_user ON mcp_tokens(user_id, created_at)`); err != nil {
		return fmt.Errorf("index mcp_tokens: %w", err)
	}
	return ensureMCPOAuthSchema(db)
}

func init() {
	userStorageMigrationTables["mcp_tokens"] = migrationTableRule{migrationCentral, ``,
		"id,user_id,name,token_hash,scopes,kind,client_id,grant_id,mailboxes,created_at,expires_at,last_used_at,revoked_at"}
	userStorageMigrationLinks = append(userStorageMigrationLinks,
		migrationLink{"mcp_tokens", "users", []string{"user_id"}, []string{"id"}})
}

// HashMCPToken is how a token is stored and looked up.
func HashMCPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewMCPTokenSecret makes the text of a new token.
func NewMCPTokenSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return MCPTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// MCPTokenInput describes a token to make.
type MCPTokenInput struct {
	UserID    string
	Name      string
	Scopes    []string
	Kind      string
	ClientID  string
	ExpiresAt *time.Time
	GrantID   string
	Mailboxes []string
	// Secret is the token's text; one is made when it is empty.
	Secret string
}

// CreateMCPToken stores a new token and returns it with its text, which is
// not kept and cannot be read again.
func (db *DB) CreateMCPToken(ctx context.Context, in MCPTokenInput) (MCPToken, string, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return MCPToken{}, "", fmt.Errorf("a token needs a user")
	}
	secret := in.Secret
	if secret == "" {
		var err error
		if secret, err = NewMCPTokenSecret(); err != nil {
			return MCPToken{}, "", err
		}
	}
	kind := in.Kind
	if kind == "" {
		kind = "personal"
	}
	token := MCPToken{
		ID: uuid.NewString(), UserID: in.UserID, Name: strings.TrimSpace(in.Name), Scopes: in.Scopes,
		Kind: kind, ClientID: in.ClientID, GrantID: in.GrantID, Mailboxes: in.Mailboxes, CreatedAt: time.Now().UTC(), ExpiresAt: in.ExpiresAt,
	}
	var expires any
	if in.ExpiresAt != nil {
		expires = in.ExpiresAt.UTC()
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO mcp_tokens (id, user_id, name, token_hash, scopes, kind, client_id, grant_id, mailboxes, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		token.ID, token.UserID, token.Name, HashMCPToken(secret), strings.Join(in.Scopes, " "), kind, in.ClientID, in.GrantID, joinMailboxKeys(in.Mailboxes), token.CreatedAt, expires); err != nil {
		return MCPToken{}, "", fmt.Errorf("save MCP token: %w", err)
	}
	return token, secret, nil
}

const mcpTokenColumns = `id, user_id, name, scopes, kind, client_id, grant_id, mailboxes, created_at, expires_at, last_used_at, revoked_at`

func scanMCPToken(scan func(...any) error) (MCPToken, error) {
	var t MCPToken
	var scopes, mailboxes string
	var expires, used, revoked sql.NullTime
	if err := scan(&t.ID, &t.UserID, &t.Name, &scopes, &t.Kind, &t.ClientID, &t.GrantID, &mailboxes, &t.CreatedAt, &expires, &used, &revoked); err != nil {
		return t, err
	}
	t.Scopes = strings.Fields(scopes)
	t.Mailboxes = splitMailboxKeys(mailboxes)
	if expires.Valid {
		t.ExpiresAt = &expires.Time
	}
	if used.Valid {
		t.LastUsedAt = &used.Time
	}
	if revoked.Valid {
		t.RevokedAt = &revoked.Time
	}
	return t, nil
}

// VerifyMCPToken returns the active token with this text, and notes that it
// was used. A token that is unknown, revoked or expired, or whose OAuth grant
// was revoked, is ErrMCPTokenInvalid.
func (db *DB) VerifyMCPToken(ctx context.Context, secret string, at time.Time) (MCPToken, error) {
	if !strings.HasPrefix(secret, MCPTokenPrefix) {
		return MCPToken{}, ErrMCPTokenInvalid
	}
	t, err := scanMCPToken(db.Read().QueryRowContext(ctx, `SELECT `+mcpTokenColumns+` FROM mcp_tokens WHERE token_hash = ?`, HashMCPToken(secret)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPToken{}, ErrMCPTokenInvalid
	}
	if err != nil {
		return MCPToken{}, err
	}
	if !t.Active(at) {
		return MCPToken{}, ErrMCPTokenInvalid
	}
	if t.GrantID != "" {
		var revoked int
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_oauth_grants WHERE id = ? AND revoked_at IS NULL`, t.GrantID).Scan(&revoked); err != nil || revoked == 0 {
			return MCPToken{}, ErrMCPTokenInvalid
		}
	}
	// Recorded at most once a minute, so a busy client does not write on
	// every call.
	if t.LastUsedAt == nil || at.Sub(*t.LastUsedAt) > time.Minute {
		_, _ = db.Write().ExecContext(ctx, `UPDATE mcp_tokens SET last_used_at = ? WHERE id = ?`, at.UTC(), t.ID)
	}
	return t, nil
}

// ListMCPTokens returns the user's tokens, newest first, revoked ones too.
func (db *DB) ListMCPTokens(ctx context.Context, userID string) ([]MCPToken, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT `+mcpTokenColumns+` FROM mcp_tokens WHERE user_id = ? ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPToken
	for rows.Next() {
		t, err := scanMCPToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeMCPToken stops one of the user's tokens from working.
func (db *DB) RevokeMCPToken(ctx context.Context, userID, id string) error {
	result, err := db.Write().ExecContext(ctx, `UPDATE mcp_tokens SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Mailbox keys are stored one a line, since a shared mailbox's key holds its
// address and could in principle hold any other separator.
func joinMailboxKeys(keys []string) string { return strings.Join(keys, "\n") }

func splitMailboxKeys(stored string) []string {
	var keys []string
	for _, key := range strings.Split(stored, "\n") {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}
