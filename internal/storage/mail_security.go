package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

func normalizeMailSecurityHost(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

func normalizePrivateTargetHost(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	return normalizeMailSecurityHost(value)
}

func (db *DB) AddHTTPDiscoveryException(ctx context.Context, domain, createdBy string) error {
	domain = normalizeMailSecurityHost(domain)
	if domain == "" {
		return fmt.Errorf("discovery domain is required")
	}
	_, err := db.write.ExecContext(ctx, `
		INSERT INTO mail_security_exceptions (id, kind, host, created_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(kind, protocol, host, port) DO NOTHING`,
		uuid.NewString(), models.MailSecurityExceptionHTTPDiscovery, domain, strings.TrimSpace(createdBy))
	return err
}

func (db *DB) AddPlaintextTransportException(ctx context.Context, protocol, host string, port int, createdBy string) error {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	host = normalizeMailSecurityHost(host)
	if protocol != "imap" && protocol != "smtp" {
		return fmt.Errorf("protocol must be IMAP or SMTP")
	}
	if host == "" {
		return fmt.Errorf("server host is required")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("server port must be between 1 and 65535")
	}
	_, err := db.write.ExecContext(ctx, `
		INSERT INTO mail_security_exceptions (id, kind, protocol, host, port, created_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, protocol, host, port) DO NOTHING`,
		uuid.NewString(), models.MailSecurityExceptionPlaintextTransport, protocol, host, port, strings.TrimSpace(createdBy))
	return err
}

func (db *DB) AddPrivateTargetException(ctx context.Context, protocol, host string, port int, createdBy string) error {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	host = normalizePrivateTargetHost(host)
	if protocol != "http" && protocol != "https" && protocol != "imap" && protocol != "smtp" {
		return fmt.Errorf("protocol must be HTTP, HTTPS, IMAP, or SMTP")
	}
	if host == "" {
		return fmt.Errorf("target host is required")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("target port must be between 1 and 65535")
	}
	_, err := db.write.ExecContext(ctx, `
		INSERT INTO mail_security_exceptions (id, kind, protocol, host, port, created_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, protocol, host, port) DO NOTHING`,
		uuid.NewString(), models.MailSecurityExceptionPrivateTarget, protocol, host, port, strings.TrimSpace(createdBy))
	return err
}

func (db *DB) DeleteMailSecurityException(ctx context.Context, id string) error {
	_, err := db.write.ExecContext(ctx, `DELETE FROM mail_security_exceptions WHERE id = ?`, strings.TrimSpace(id))
	return err
}

func (db *DB) IsHTTPDiscoveryAllowed(ctx context.Context, domain string) (bool, error) {
	var allowed int
	err := db.read.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mail_security_exceptions
			WHERE kind = ? AND host = ?
		)`, models.MailSecurityExceptionHTTPDiscovery, normalizeMailSecurityHost(domain)).Scan(&allowed)
	return allowed == 1, err
}

func (db *DB) IsPlaintextTransportAllowed(ctx context.Context, protocol, host string, port int) (bool, error) {
	var allowed int
	err := db.read.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mail_security_exceptions
			WHERE kind = ? AND protocol = ? AND host = ? AND port = ?
		)`, models.MailSecurityExceptionPlaintextTransport, strings.ToLower(strings.TrimSpace(protocol)), normalizeMailSecurityHost(host), port).Scan(&allowed)
	return allowed == 1, err
}

func (db *DB) IsPrivateTargetAllowed(ctx context.Context, protocol, host string, port int) (bool, error) {
	var allowed int
	err := db.read.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mail_security_exceptions
			WHERE kind = ? AND protocol = ? AND host = ? AND port = ?
		)`, models.MailSecurityExceptionPrivateTarget, strings.ToLower(strings.TrimSpace(protocol)), normalizePrivateTargetHost(host), port).Scan(&allowed)
	return allowed == 1, err
}

func (db *DB) GetMailSecurityException(ctx context.Context, id string) (*models.MailSecurityException, error) {
	var item models.MailSecurityException
	err := db.read.QueryRowContext(ctx, `
		SELECT id, kind, protocol, host, port, created_by, created_at
		FROM mail_security_exceptions WHERE id = ?`, strings.TrimSpace(id)).Scan(
		&item.ID, &item.Kind, &item.Protocol, &item.Host, &item.Port, &item.CreatedBy, &item.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := db.loadMailSecurityExceptionAccounts(ctx, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (db *DB) ListMailSecurityExceptions(ctx context.Context) ([]models.MailSecurityException, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT id, kind, protocol, host, port, created_by, created_at
		FROM mail_security_exceptions
		ORDER BY kind, protocol, host, port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.MailSecurityException
	for rows.Next() {
		var item models.MailSecurityException
		if err := rows.Scan(&item.ID, &item.Kind, &item.Protocol, &item.Host, &item.Port, &item.CreatedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range items {
		if err := db.loadMailSecurityExceptionAccounts(ctx, &items[i]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (db *DB) loadMailSecurityExceptionAccounts(ctx context.Context, item *models.MailSecurityException) error {
	if item == nil || item.Kind != models.MailSecurityExceptionPlaintextTransport {
		return nil
	}
	columnPrefix := "imap"
	if item.Protocol == "smtp" {
		columnPrefix = "smtp"
	}
	query := fmt.Sprintf(`
		SELECT id, email_address
		FROM accounts
		WHERE COALESCE(is_deleting, 0) = 0
		  AND LOWER(TRIM(%[1]s_host)) = ?
		  AND %[1]s_port = ?
		  AND LOWER(TRIM(%[1]s_tls_mode)) = 'plaintext'
		ORDER BY email_address COLLATE NOCASE`, columnPrefix)
	rows, err := db.read.QueryContext(ctx, query, item.Host, item.Port)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var account models.MailSecurityExceptionAccount
		if err := rows.Scan(&account.ID, &account.Email); err != nil {
			return err
		}
		item.Accounts = append(item.Accounts, account)
	}
	return rows.Err()
}
