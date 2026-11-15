package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// OAuth 2.1 for the MCP server: the clients that registered themselves
// (RFC 7591), the one-use codes the authorization step gives them, and the
// grants a user approved. A grant holds a refresh token, turned over on each
// use, and the access tokens issued under it are MCP tokens of kind "oauth"
// (mcp_tokens.go). Only hashes of codes and tokens are kept.

var (
	// ErrOAuthInvalidGrant is a code or refresh token that is unknown,
	// used, expired, revoked, or presented by another client.
	ErrOAuthInvalidGrant = errors.New("invalid grant")
	// ErrOAuthUnknownClient is a client_id never registered.
	ErrOAuthUnknownClient = errors.New("unknown client")
)

// OAuth lifetimes.
const (
	OAuthCodeLifetime    = 10 * time.Minute
	OAuthAccessLifetime  = time.Hour
	OAuthRefreshLifetime = 90 * 24 * time.Hour
)

func ensureMCPOAuthSchema(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			redirect_uris TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_oauth_codes (
			code_hash TEXT PRIMARY KEY,
			client_id TEXT NOT NULL REFERENCES mcp_oauth_clients(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			redirect_uri TEXT NOT NULL,
			scopes TEXT NOT NULL DEFAULT '',
			code_challenge TEXT NOT NULL,
			resource TEXT NOT NULL DEFAULT '',
			expires_at DATETIME NOT NULL,
			used_at DATETIME,
			-- The grant the code was traded for, revoked if it comes again.
			grant_id TEXT NOT NULL DEFAULT '',
			mailboxes TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_oauth_grants (
			id TEXT PRIMARY KEY,
			client_id TEXT NOT NULL REFERENCES mcp_oauth_clients(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			scopes TEXT NOT NULL DEFAULT '',
			-- The mailboxes the user let it reach, one key a line; '' for all.
			mailboxes TEXT NOT NULL DEFAULT '',
			refresh_hash TEXT NOT NULL UNIQUE,
			refresh_expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_used_at DATETIME,
			revoked_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_oauth_grants_user ON mcp_oauth_grants(user_id, created_at)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("mcp oauth schema: %w", err)
		}
	}
	for _, c := range []struct{ table, column string }{
		{"mcp_tokens", "grant_id"}, {"mcp_tokens", "mailboxes"},
		{"mcp_oauth_codes", "grant_id"}, {"mcp_oauth_codes", "mailboxes"}, {"mcp_oauth_grants", "mailboxes"},
	} {
		var present int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	return nil
}

func init() {
	userStorageMigrationTables["mcp_oauth_clients"] = migrationTableRule{migrationCentral, ``, "id,name,redirect_uris,created_at"}
	userStorageMigrationTables["mcp_oauth_codes"] = migrationTableRule{migrationCentral, ``,
		"code_hash,client_id,user_id,redirect_uri,scopes,code_challenge,resource,expires_at,used_at,grant_id,mailboxes"}
	userStorageMigrationTables["mcp_oauth_grants"] = migrationTableRule{migrationCentral, ``,
		"id,client_id,user_id,scopes,mailboxes,refresh_hash,refresh_expires_at,created_at,last_used_at,revoked_at"}
	userStorageMigrationLinks = append(userStorageMigrationLinks,
		migrationLink{"mcp_oauth_codes", "mcp_oauth_clients", []string{"client_id"}, []string{"id"}},
		migrationLink{"mcp_oauth_codes", "users", []string{"user_id"}, []string{"id"}},
		migrationLink{"mcp_oauth_grants", "mcp_oauth_clients", []string{"client_id"}, []string{"id"}},
		migrationLink{"mcp_oauth_grants", "users", []string{"user_id"}, []string{"id"}},
	)
}

// OAuthClient is a client that registered itself.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// RegisterOAuthClient records a new public client (it has no secret; PKCE
// proves it instead).
func (db *DB) RegisterOAuthClient(ctx context.Context, name string, redirectURIs []string) (OAuthClient, error) {
	client := OAuthClient{ID: "gofer-client-" + uuid.NewString(), Name: strings.TrimSpace(name), RedirectURIs: redirectURIs}
	if len(client.Name) > 100 {
		client.Name = client.Name[:100]
	}
	_, err := db.Write().ExecContext(ctx, `INSERT INTO mcp_oauth_clients (id, name, redirect_uris) VALUES (?, ?, ?)`,
		client.ID, client.Name, strings.Join(redirectURIs, "\n"))
	return client, err
}

// GetOAuthClient returns a registered client, or ErrOAuthUnknownClient.
func (db *DB) GetOAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	var client OAuthClient
	var uris string
	err := db.Read().QueryRowContext(ctx, `SELECT id, name, redirect_uris FROM mcp_oauth_clients WHERE id = ?`, id).Scan(&client.ID, &client.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return client, ErrOAuthUnknownClient
	}
	client.RedirectURIs = strings.Split(uris, "\n")
	return client, err
}

// OAuthCode is what an authorization code stands for.
type OAuthCode struct {
	ClientID      string
	UserID        string
	RedirectURI   string
	Scopes        []string
	CodeChallenge string
	Resource      string
	// Mailboxes the user let the client reach; none for all.
	Mailboxes []string
	// secret is the code's text once it was consumed.
	secret string
}

// CreateOAuthCode stores a new one-use code and returns its text.
func (db *DB) CreateOAuthCode(ctx context.Context, code OAuthCode) (string, error) {
	secret, err := NewMCPTokenSecret()
	if err != nil {
		return "", err
	}
	secret = "gofer_code_" + strings.TrimPrefix(secret, MCPTokenPrefix)
	_, err = db.Write().ExecContext(ctx, `
		INSERT INTO mcp_oauth_codes (code_hash, client_id, user_id, redirect_uri, scopes, code_challenge, resource, expires_at, mailboxes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		HashMCPToken(secret), code.ClientID, code.UserID, code.RedirectURI, strings.Join(code.Scopes, " "), code.CodeChallenge, code.Resource,
		time.Now().UTC().Add(OAuthCodeLifetime), joinMailboxKeys(code.Mailboxes))
	return secret, err
}

// ConsumeOAuthCode uses up a code presented by clientID. A code used twice
// revokes what its first use gave, as RFC 6749 section 4.1.2 asks.
func (db *DB) ConsumeOAuthCode(ctx context.Context, secret, clientID string, at time.Time) (OAuthCode, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return OAuthCode{}, err
	}
	defer tx.Rollback()
	var code OAuthCode
	var scopes, grantID, mailboxes string
	var expires time.Time
	var used sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT client_id, user_id, redirect_uri, scopes, code_challenge, resource, expires_at, used_at, grant_id, mailboxes
		FROM mcp_oauth_codes WHERE code_hash = ?`, HashMCPToken(secret)).
		Scan(&code.ClientID, &code.UserID, &code.RedirectURI, &scopes, &code.CodeChallenge, &code.Resource, &expires, &used, &grantID, &mailboxes)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthCode{}, ErrOAuthInvalidGrant
	}
	if err != nil {
		return OAuthCode{}, err
	}
	if used.Valid {
		if grantID != "" {
			_, _ = tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND revoked_at IS NULL`, grantID)
			_, _ = tx.ExecContext(ctx, `UPDATE mcp_tokens SET revoked_at = CURRENT_TIMESTAMP WHERE grant_id = ? AND revoked_at IS NULL`, grantID)
		}
		_ = tx.Commit()
		return OAuthCode{}, ErrOAuthInvalidGrant
	}
	if code.ClientID != clientID || !at.Before(expires) {
		return OAuthCode{}, ErrOAuthInvalidGrant
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_oauth_codes SET used_at = ? WHERE code_hash = ?`, at.UTC(), HashMCPToken(secret)); err != nil {
		return OAuthCode{}, err
	}
	code.Scopes = strings.Fields(scopes)
	code.Mailboxes = splitMailboxKeys(mailboxes)
	code.secret = secret
	// Old codes go once they can no longer be used.
	_, _ = tx.ExecContext(ctx, `DELETE FROM mcp_oauth_codes WHERE expires_at < ?`, at.UTC().Add(-time.Hour))
	return code, tx.Commit()
}

// OAuthTokens are what the token endpoint answers.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Scopes       []string
	GrantID      string
}

func (db *DB) issueOAuthAccess(ctx context.Context, grantID, clientID, clientName, userID string, scopes, mailboxes []string, at time.Time) (string, error) {
	expires := at.UTC().Add(OAuthAccessLifetime)
	_, secret, err := db.CreateMCPToken(ctx, MCPTokenInput{UserID: userID, Name: clientName, Scopes: scopes, Mailboxes: mailboxes, Kind: "oauth", ClientID: clientID, GrantID: grantID, ExpiresAt: &expires})
	return secret, err
}

// CreateOAuthGrantFromCode records the grant a consumed code is traded for,
// and remembers it on the code, so that the code coming again revokes it.
func (db *DB) CreateOAuthGrantFromCode(ctx context.Context, client OAuthClient, code OAuthCode, at time.Time) (OAuthTokens, error) {
	tokens, err := db.CreateOAuthGrant(ctx, client, code.UserID, code.Scopes, code.Mailboxes, at)
	if err != nil {
		return tokens, err
	}
	if code.secret != "" {
		_, _ = db.Write().ExecContext(ctx, `UPDATE mcp_oauth_codes SET grant_id = ? WHERE code_hash = ?`, tokens.GrantID, HashMCPToken(code.secret))
	}
	return tokens, nil
}

// CreateOAuthGrant records a user's approval and issues its first tokens.
func (db *DB) CreateOAuthGrant(ctx context.Context, client OAuthClient, userID string, scopes, mailboxes []string, at time.Time) (OAuthTokens, error) {
	refresh, err := NewMCPTokenSecret()
	if err != nil {
		return OAuthTokens{}, err
	}
	refresh = "gofer_refresh_" + strings.TrimPrefix(refresh, MCPTokenPrefix)
	grantID := uuid.NewString()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO mcp_oauth_grants (id, client_id, user_id, scopes, mailboxes, refresh_hash, refresh_expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		grantID, client.ID, userID, strings.Join(scopes, " "), joinMailboxKeys(mailboxes), HashMCPToken(refresh), at.UTC().Add(OAuthRefreshLifetime), at.UTC()); err != nil {
		return OAuthTokens{}, err
	}
	access, err := db.issueOAuthAccess(ctx, grantID, client.ID, client.Name, userID, scopes, mailboxes, at)
	if err != nil {
		return OAuthTokens{}, err
	}
	return OAuthTokens{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(OAuthAccessLifetime.Seconds()), Scopes: scopes, GrantID: grantID}, nil
}

// RefreshOAuthGrant trades a refresh token for a new access token and a new
// refresh token; the old refresh token stops working.
func (db *DB) RefreshOAuthGrant(ctx context.Context, refresh, clientID string, at time.Time) (OAuthTokens, error) {
	var grantID, userID, scopes, mailboxes string
	var expires time.Time
	var revoked sql.NullTime
	err := db.Read().QueryRowContext(ctx, `
		SELECT id, user_id, scopes, mailboxes, refresh_expires_at, revoked_at FROM mcp_oauth_grants
		WHERE refresh_hash = ? AND client_id = ?`, HashMCPToken(refresh), clientID).Scan(&grantID, &userID, &scopes, &mailboxes, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthTokens{}, ErrOAuthInvalidGrant
	}
	if err != nil {
		return OAuthTokens{}, err
	}
	if revoked.Valid || !at.Before(expires) {
		return OAuthTokens{}, ErrOAuthInvalidGrant
	}
	client, err := db.GetOAuthClient(ctx, clientID)
	if err != nil {
		return OAuthTokens{}, err
	}
	next, err := NewMCPTokenSecret()
	if err != nil {
		return OAuthTokens{}, err
	}
	next = "gofer_refresh_" + strings.TrimPrefix(next, MCPTokenPrefix)
	result, err := db.Write().ExecContext(ctx, `
		UPDATE mcp_oauth_grants SET refresh_hash = ?, refresh_expires_at = ?, last_used_at = ?
		WHERE id = ? AND refresh_hash = ? AND revoked_at IS NULL`,
		HashMCPToken(next), at.UTC().Add(OAuthRefreshLifetime), at.UTC(), grantID, HashMCPToken(refresh))
	if err != nil {
		return OAuthTokens{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return OAuthTokens{}, ErrOAuthInvalidGrant
	}
	list := strings.Fields(scopes)
	access, err := db.issueOAuthAccess(ctx, grantID, client.ID, client.Name, userID, list, splitMailboxKeys(mailboxes), at)
	if err != nil {
		return OAuthTokens{}, err
	}
	// Access tokens of the grant that have expired go.
	_, _ = db.Write().ExecContext(ctx, `DELETE FROM mcp_tokens WHERE grant_id = ? AND expires_at < ?`, grantID, at.UTC().Add(-24*time.Hour))
	return OAuthTokens{AccessToken: access, RefreshToken: next, ExpiresIn: int(OAuthAccessLifetime.Seconds()), Scopes: list, GrantID: grantID}, nil
}

// RevokeOAuthToken ends the grant an access or refresh token belongs to, as
// presented by clientID (RFC 7009). An unknown token is not an error.
func (db *DB) RevokeOAuthToken(ctx context.Context, token, clientID string) error {
	hash := HashMCPToken(token)
	var grantID string
	err := db.Read().QueryRowContext(ctx, `SELECT id FROM mcp_oauth_grants WHERE refresh_hash = ? AND client_id = ?`, hash, clientID).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.Read().QueryRowContext(ctx, `SELECT grant_id FROM mcp_tokens WHERE token_hash = ? AND client_id = ? AND grant_id != ''`, hash, clientID).Scan(&grantID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return db.revokeOAuthGrant(ctx, grantID)
}

func (db *DB) revokeOAuthGrant(ctx context.Context, grantID string) error {
	if _, err := db.Write().ExecContext(ctx, `UPDATE mcp_oauth_grants SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND revoked_at IS NULL`, grantID); err != nil {
		return err
	}
	_, err := db.Write().ExecContext(ctx, `UPDATE mcp_tokens SET revoked_at = CURRENT_TIMESTAMP WHERE grant_id = ? AND revoked_at IS NULL`, grantID)
	return err
}

// OAuthGrant is an app a user let in.
type OAuthGrant struct {
	ID         string
	ClientName string
	Scopes     []string
	Mailboxes  []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// ListOAuthGrants returns the user's apps still let in, newest first.
func (db *DB) ListOAuthGrants(ctx context.Context, userID string) ([]OAuthGrant, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT g.id, c.name, g.scopes, g.mailboxes, g.created_at,
		       (SELECT MAX(t.last_used_at) FROM mcp_tokens t WHERE t.grant_id = g.id)
		FROM mcp_oauth_grants g JOIN mcp_oauth_clients c ON c.id = g.client_id
		WHERE g.user_id = ? AND g.revoked_at IS NULL AND g.refresh_expires_at > ?
		ORDER BY g.created_at DESC`, userID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthGrant
	for rows.Next() {
		var g OAuthGrant
		var scopes, mailboxes string
		var used sql.NullString
		if err := rows.Scan(&g.ID, &g.ClientName, &scopes, &mailboxes, &g.CreatedAt, &used); err != nil {
			return nil, err
		}
		g.Scopes = strings.Fields(scopes)
		g.Mailboxes = splitMailboxKeys(mailboxes)
		if used.Valid {
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
				if t, err := time.Parse(layout, used.String); err == nil {
					g.LastUsedAt = &t
					break
				}
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeOAuthGrant ends one of the user's grants.
func (db *DB) RevokeOAuthGrant(ctx context.Context, userID, grantID string) error {
	var owner string
	if err := db.Read().QueryRowContext(ctx, `SELECT user_id FROM mcp_oauth_grants WHERE id = ?`, grantID).Scan(&owner); err != nil || owner != userID {
		return sql.ErrNoRows
	}
	return db.revokeOAuthGrant(ctx, grantID)
}
