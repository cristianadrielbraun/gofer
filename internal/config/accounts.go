package config

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type AccountStore struct {
	db  *storage.DB
	aes cipher.AEAD
}

func NewAccountStore(db *storage.DB, secretKey []byte) (*AccountStore, error) {
	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &AccountStore{db: db, aes: gcm}, nil
}

func (s *AccountStore) encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aes.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return s.aes.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (s *AccountStore) decrypt(ciphertext []byte) (string, error) {
	nonceSize := s.aes.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := s.aes.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

func (s *AccountStore) DecryptPassword(ctx context.Context, accountID string) (string, error) {
	var encrypted []byte
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT encrypted_password FROM accounts WHERE id = ?`, accountID,
	).Scan(&encrypted)
	if err != nil {
		return "", fmt.Errorf("query password: %w", err)
	}
	if encrypted == nil {
		return "", nil
	}
	return s.decrypt(encrypted)
}

func (s *AccountStore) DecryptSmtpPassword(ctx context.Context, accountID string) (string, error) {
	var encrypted []byte
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT encrypted_smtp_password FROM accounts WHERE id = ?`, accountID,
	).Scan(&encrypted)
	if err != nil {
		return "", fmt.Errorf("query smtp password: %w", err)
	}
	if encrypted == nil {
		return "", nil
	}
	return s.decrypt(encrypted)
}

func (s *AccountStore) GetConfig(ctx context.Context, accountID string) (*models.AccountConfig, error) {
	var cfg models.AccountConfig
	cfg.AccountID = accountID
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT provider, provider_account_id,
		        imap_host, imap_port, imap_tls_mode,
		        smtp_host, smtp_port, smtp_tls_mode,
		        username, auth_method, smtp_username
		 FROM accounts WHERE id = ?`, accountID,
	).Scan(&cfg.Provider, &cfg.ProviderAccountID,
		&cfg.IMAPHost, &cfg.IMAPPort, &cfg.IMAPTLSMode,
		&cfg.SMTPHost, &cfg.SMTPPort, &cfg.SMTPTLSMode,
		&cfg.Username, &cfg.AuthMethod, &cfg.SmtpUsername)
	if err != nil {
		return nil, fmt.Errorf("query account config: %w", err)
	}
	return &cfg, nil
}

func (s *AccountStore) GetEditData(ctx context.Context, accountID string) (*models.EditAccountData, error) {
	var data models.EditAccountData
	data.AccountID = accountID
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT provider, provider_account_id, email_address, display_name,
		        imap_host, imap_port, imap_tls_mode,
		        smtp_host, smtp_port, smtp_tls_mode,
		        username, auth_method, COALESCE(smtp_username, '')
		 FROM accounts WHERE id = ?`, accountID,
	).Scan(&data.Provider, &data.ProviderAccountID, &data.EmailAddress, &data.DisplayName,
		&data.IMAPHost, &data.IMAPPort, &data.IMAPTLSMode,
		&data.SMTPHost, &data.SMTPPort, &data.SMTPTLSMode,
		&data.Username, &data.AuthMethod, &data.SmtpUsername)
	if err != nil {
		return nil, fmt.Errorf("query account edit data: %w", err)
	}
	data.SameSmtpAuth = data.SmtpUsername == "" || data.SmtpUsername == data.Username
	userID, err := s.db.GetAccountUserID(ctx, accountID)
	if err == nil && userID != "" {
		data.Signatures, _ = s.db.ListSignatures(ctx, userID)
		data.SignatureSettings, _ = s.db.GetAccountSignatureSettings(ctx, userID, accountID)
	}
	return &data, nil
}

func (s *AccountStore) CreateAccount(ctx context.Context, userID string, req *models.CreateAccountRequest) (*models.Account, error) {
	encrypted, err := s.encrypt(req.Password)
	if err != nil {
		return nil, fmt.Errorf("encrypt password: %w", err)
	}

	if req.IMAPPort == 0 {
		req.IMAPPort = 993
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = 465
	}
	if req.IMAPTLSMode == "" {
		req.IMAPTLSMode = "tls"
	}
	if req.SMTPTLSMode == "" {
		req.SMTPTLSMode = "tls"
	}
	if req.AuthMethod == "" {
		req.AuthMethod = "plain"
	}
	if req.Provider == "" {
		req.Provider = "imap"
	}

	var encryptedSmtpPw []byte
	if req.SmtpPassword != "" {
		encryptedSmtpPw, err = s.encrypt(req.SmtpPassword)
		if err != nil {
			return nil, fmt.Errorf("encrypt smtp password: %w", err)
		}
	}

	id := generateAccountID(req.EmailAddress)
	initials := extractInitials(req.DisplayName)
	color := generateColor(id)

	_, err = s.db.Write().ExecContext(ctx,
		`INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, color, initials,
		  imap_host, imap_port, imap_tls_mode,
		  smtp_host, smtp_port, smtp_tls_mode,
		  username, encrypted_password, auth_method,
		  smtp_username, encrypted_smtp_password)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, req.Provider, req.ProviderAccountID, req.EmailAddress, req.DisplayName, color, initials,
		req.IMAPHost, req.IMAPPort, req.IMAPTLSMode,
		req.SMTPHost, req.SMTPPort, req.SMTPTLSMode,
		req.Username, encrypted, req.AuthMethod,
		req.SmtpUsername, encryptedSmtpPw)
	if err != nil {
		return nil, fmt.Errorf("insert account: %w", err)
	}

	return &models.Account{
		ID:       id,
		Email:    req.EmailAddress,
		Name:     req.DisplayName,
		Color:    color,
		Initials: initials,
	}, nil
}

func (s *AccountStore) UpdateAccount(ctx context.Context, accountID string, req *models.CreateAccountRequest) error {
	setClauses := []string{}
	args := []any{}

	if req.EmailAddress != "" {
		setClauses = append(setClauses, "email_address = ?")
		args = append(args, req.EmailAddress)
	}
	if req.DisplayName != "" {
		setClauses = append(setClauses, "display_name = ?")
		args = append(args, req.DisplayName)
	}
	if req.Provider != "" {
		setClauses = append(setClauses, "provider = ?")
		args = append(args, req.Provider)
	}
	if req.ProviderAccountID != "" {
		setClauses = append(setClauses, "provider_account_id = ?")
		args = append(args, req.ProviderAccountID)
	}
	if req.IMAPHost != "" {
		setClauses = append(setClauses, "imap_host = ?")
		args = append(args, req.IMAPHost)
	}
	if req.IMAPPort != 0 {
		setClauses = append(setClauses, "imap_port = ?")
		args = append(args, req.IMAPPort)
	}
	if req.IMAPTLSMode != "" {
		setClauses = append(setClauses, "imap_tls_mode = ?")
		args = append(args, req.IMAPTLSMode)
	}
	if req.SMTPHost != "" {
		setClauses = append(setClauses, "smtp_host = ?")
		args = append(args, req.SMTPHost)
	}
	if req.SMTPPort != 0 {
		setClauses = append(setClauses, "smtp_port = ?")
		args = append(args, req.SMTPPort)
	}
	if req.SMTPTLSMode != "" {
		setClauses = append(setClauses, "smtp_tls_mode = ?")
		args = append(args, req.SMTPTLSMode)
	}
	if req.Username != "" {
		setClauses = append(setClauses, "username = ?")
		args = append(args, req.Username)
	}
	if req.Password != "" {
		encrypted, err := s.encrypt(req.Password)
		if err != nil {
			return fmt.Errorf("encrypt password: %w", err)
		}
		setClauses = append(setClauses, "encrypted_password = ?")
		args = append(args, encrypted)
	}
	if req.AuthMethod != "" {
		setClauses = append(setClauses, "auth_method = ?")
		args = append(args, req.AuthMethod)
	}
	if req.SmtpUsername != "" {
		setClauses = append(setClauses, "smtp_username = ?")
		args = append(args, req.SmtpUsername)
	}
	if req.SmtpPassword != "" {
		encrypted, err := s.encrypt(req.SmtpPassword)
		if err != nil {
			return fmt.Errorf("encrypt smtp password: %w", err)
		}
		setClauses = append(setClauses, "encrypted_smtp_password = ?")
		args = append(args, encrypted)
	}

	if len(setClauses) == 0 {
		return nil
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, accountID)

	query := fmt.Sprintf("UPDATE accounts SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	_, err := s.db.Write().ExecContext(ctx, query, args...)
	return err
}

func (s *AccountStore) FindProviderAccountID(ctx context.Context, userID, provider, providerAccountID, email string) (string, error) {
	var id string
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT id FROM accounts
		 WHERE user_id = ? AND COALESCE(is_deleting, 0) = 0
		   AND (
		     (provider = ? AND provider_account_id = ? AND provider_account_id != '')
		     OR (email_address = ? AND imap_host = 'imap.gmail.com' AND auth_method = 'oauth2')
		   )
		 LIMIT 1`,
		userID, provider, providerAccountID, email,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *AccountStore) UpdateAccountColor(ctx context.Context, userID, accountID, color string) error {
	res, err := s.db.Write().ExecContext(ctx,
		`UPDATE accounts SET color = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`,
		color, accountID, userID)
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

func (s *AccountStore) DeleteAccount(ctx context.Context, accountID string) error {
	_, err := s.db.Write().ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, accountID)
	return err
}

func (s *AccountStore) MarkAccountDeleting(ctx context.Context, accountID string) error {
	_, err := s.db.Write().ExecContext(ctx, `UPDATE accounts SET is_deleting = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, accountID)
	return err
}

func (s *AccountStore) GetAccountByID(ctx context.Context, accountID string) (*models.Account, error) {
	var a models.Account
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT id, provider, email_address, display_name, color, initials FROM accounts WHERE id = ?`, accountID,
	).Scan(&a.ID, &a.Provider, &a.Email, &a.Name, &a.Color, &a.Initials)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *AccountStore) GetFirstAccountID(ctx context.Context, userID string) string {
	var id string
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE user_id = ? AND COALESCE(is_deleting, 0) = 0 ORDER BY id LIMIT 1`, userID).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

func generateAccountID(email string) string {
	return fmt.Sprintf("acc_%s", strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '_'
	}, email))
}

func extractInitials(name string) string {
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		return strings.ToUpper(firstRune(parts[0]) + firstRune(parts[1]))
	}
	runes := []rune(name)
	if len(runes) >= 2 {
		return strings.ToUpper(string(runes[:2]))
	}
	return strings.ToUpper(name)
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

func generateColor(id string) string {
	colors := []string{"#3b82f6", "#8b5cf6", "#ec4899", "#f97316", "#14b8a6", "#6366f1"}
	h := 0
	for _, c := range id {
		h = h*31 + int(c)
	}
	return colors[abs(h)%len(colors)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
