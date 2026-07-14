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

	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

type AccountStore struct {
	db  *storage.DB
	aes cipher.AEAD
}

const accountDeletionBatchSize int64 = 5000

type AccountDeletionProgress struct {
	Step                  string
	RowsAffected          int64
	TotalStepRowsAffected int64
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
		`SELECT encrypted_password FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 0`, accountID,
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
		`SELECT encrypted_smtp_password FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 0`, accountID,
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
		 FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 0`, accountID,
	).Scan(&cfg.Provider, &cfg.ProviderAccountID,
		&cfg.IMAPHost, &cfg.IMAPPort, &cfg.IMAPTLSMode,
		&cfg.SMTPHost, &cfg.SMTPPort, &cfg.SMTPTLSMode,
		&cfg.Username, &cfg.AuthMethod, &cfg.SmtpUsername)
	if err != nil {
		return nil, fmt.Errorf("query account config: %w", err)
	}
	if !accountProviderUsesGraphMail(cfg.Provider) {
		cfg.IMAPAllowPlaintext, err = s.db.IsPlaintextTransportAllowed(ctx, "imap", cfg.IMAPHost, cfg.IMAPPort)
		if err != nil {
			return nil, fmt.Errorf("account %s: check IMAP security exception: %w", accountID, err)
		}
		cfg.IMAPTLSMode, err = mailtransport.RequireTLSModeWithPlaintext("IMAP", cfg.IMAPTLSMode, cfg.IMAPAllowPlaintext)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", accountID, err)
		}
		cfg.SMTPAllowPlaintext, err = s.db.IsPlaintextTransportAllowed(ctx, "smtp", cfg.SMTPHost, cfg.SMTPPort)
		if err != nil {
			return nil, fmt.Errorf("account %s: check SMTP security exception: %w", accountID, err)
		}
		cfg.SMTPTLSMode, err = mailtransport.RequireTLSModeWithPlaintext("SMTP", cfg.SMTPTLSMode, cfg.SMTPAllowPlaintext)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", accountID, err)
		}
		if (cfg.IMAPTLSMode == mailtransport.TLSModePlaintext || cfg.SMTPTLSMode == mailtransport.TLSModePlaintext) && !strings.EqualFold(strings.TrimSpace(cfg.AuthMethod), "plain") {
			return nil, fmt.Errorf("account %s: OAuth authentication is not allowed over a plaintext connection", accountID)
		}
	}
	return &cfg, nil
}

func (s *AccountStore) GetEditData(ctx context.Context, accountID string) (*models.EditAccountData, error) {
	var data models.EditAccountData
	data.AccountID = accountID
	var emailSyncEnabled int
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT provider, provider_account_id, email_address, display_name,
		        imap_host, imap_port, imap_tls_mode,
		        smtp_host, smtp_port, smtp_tls_mode, COALESCE(email_sync_enabled, 1),
		        username, auth_method, COALESCE(smtp_username, '')
		 FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 0`, accountID,
	).Scan(&data.Provider, &data.ProviderAccountID, &data.EmailAddress, &data.DisplayName,
		&data.IMAPHost, &data.IMAPPort, &data.IMAPTLSMode,
		&data.SMTPHost, &data.SMTPPort, &data.SMTPTLSMode, &emailSyncEnabled,
		&data.Username, &data.AuthMethod, &data.SmtpUsername)
	if err != nil {
		return nil, fmt.Errorf("query account edit data: %w", err)
	}
	data.EmailSyncEnabled = emailSyncEnabled == 1
	data.SameSmtpAuth = data.SmtpUsername == "" || data.SmtpUsername == data.Username
	userID, err := s.db.GetAccountUserID(ctx, accountID)
	if err == nil && userID != "" {
		data.Signatures, _ = s.db.ListSignatures(ctx, userID)
		data.SignatureSettings, _ = s.db.GetAccountSignatureSettings(ctx, userID, accountID)
		data.ContactSync, _ = s.GetContactSyncConfig(ctx, userID, accountID)
		if isBuiltinContactProvider(data.Provider) && data.ContactSync.Provider != data.Provider {
			data.ContactSync.Provider = data.Provider
			data.ContactSync.Enabled = true
		}
	}
	return &data, nil
}

func (s *AccountStore) GetContactSyncConfig(ctx context.Context, userID, accountID string) (models.ContactSyncConfig, error) {
	cfg := models.ContactSyncConfig{AccountID: accountID, UserID: userID, Provider: "carddav"}
	var enabled int
	var hasPassword int
	var lastSuccess, updatedAt sql.NullString
	err := s.db.Read().QueryRowContext(ctx, `
		SELECT account_id, user_id, provider, enabled, base_url, addressbook_url, username,
		       CASE WHEN encrypted_password IS NULL THEN 0 ELSE 1 END,
		       last_sync_token, last_error, last_success_at, updated_at
		FROM account_contact_sync_configs
		WHERE user_id = ? AND account_id = ?`, userID, accountID).Scan(
		&cfg.AccountID, &cfg.UserID, &cfg.Provider, &enabled, &cfg.BaseURL, &cfg.AddressBookURL, &cfg.Username,
		&hasPassword, &cfg.LastSyncToken, &cfg.LastError, &lastSuccess, &updatedAt)
	if err == sql.ErrNoRows {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	cfg.Enabled = enabled == 1
	cfg.HasPassword = hasPassword == 1
	if lastSuccess.Valid {
		cfg.LastSuccessAt = lastSuccess.String
	}
	if updatedAt.Valid {
		cfg.UpdatedAt = updatedAt.String
	}
	cfg.AddressBooks, _ = s.listContactAddressBooks(ctx, userID, accountID)
	if len(cfg.AddressBooks) == 0 && strings.TrimSpace(cfg.AddressBookURL) != "" {
		cfg.AddressBooks = []models.ContactAddressBook{{URL: strings.TrimSpace(cfg.AddressBookURL), Default: true, Selected: true, LastSyncToken: cfg.LastSyncToken}}
	}
	return cfg, nil
}

func (s *AccountStore) listContactAddressBooks(ctx context.Context, userID, accountID string) ([]models.ContactAddressBook, error) {
	rows, err := s.db.Read().QueryContext(ctx, `
		SELECT id, name, url, is_default, last_sync_token
		FROM account_contact_address_books
		WHERE user_id = ? AND account_id = ?
		ORDER BY is_default DESC, name COLLATE NOCASE, url`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var books []models.ContactAddressBook
	for rows.Next() {
		var book models.ContactAddressBook
		var isDefault int
		if err := rows.Scan(&book.ID, &book.Name, &book.URL, &isDefault, &book.LastSyncToken); err != nil {
			return nil, err
		}
		book.URL = strings.TrimSpace(book.URL)
		if book.URL == "" {
			continue
		}
		book.Selected = true
		book.Default = isDefault == 1
		books = append(books, book)
	}
	return books, rows.Err()
}

func (s *AccountStore) SaveContactSyncConfig(ctx context.Context, userID, accountID string, cfg models.ContactSyncConfig, password string) error {
	var exists int
	if err := s.db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}

	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "carddav"
	}
	var encrypted []byte
	if password != "" {
		var err error
		encrypted, err = s.encrypt(password)
		if err != nil {
			return err
		}
	} else {
		_ = s.db.Read().QueryRowContext(ctx, `SELECT encrypted_password FROM account_contact_sync_configs WHERE user_id = ? AND account_id = ?`, userID, accountID).Scan(&encrypted)
	}

	books := normalizeContactAddressBooks(cfg.AddressBooks, cfg.AddressBookURL)
	if len(books) > 0 {
		cfg.AddressBookURL = books[0].URL
		for _, book := range books {
			if book.Default {
				cfg.AddressBookURL = book.URL
				break
			}
		}
	}
	existingBooks, _ := s.listContactAddressBooks(ctx, userID, accountID)
	existingByURL := make(map[string]string, len(existingBooks))
	for _, book := range existingBooks {
		if strings.TrimSpace(book.URL) != "" && strings.TrimSpace(book.ID) != "" {
			existingByURL[strings.TrimSpace(book.URL)] = strings.TrimSpace(book.ID)
		}
	}
	for i := range books {
		if books[i].ID == "" {
			books[i].ID = existingByURL[books[i].URL]
		}
	}

	tx, err := s.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled, base_url, addressbook_url, username, encrypted_password)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			user_id = excluded.user_id,
			provider = excluded.provider,
			enabled = excluded.enabled,
			base_url = excluded.base_url,
			addressbook_url = excluded.addressbook_url,
			username = excluded.username,
			encrypted_password = excluded.encrypted_password,
			last_error = '',
			updated_at = CURRENT_TIMESTAMP`,
		accountID, userID, provider, boolInt(cfg.Enabled), strings.TrimSpace(cfg.BaseURL), strings.TrimSpace(cfg.AddressBookURL), strings.TrimSpace(cfg.Username), encrypted)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_contact_address_books WHERE user_id = ? AND account_id = ?`, userID, accountID); err != nil {
		return err
	}
	for _, book := range books {
		bookID := strings.TrimSpace(book.ID)
		if bookID == "" {
			bookID = uuid.NewString()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_contact_address_books (account_id, user_id, id, url, name, is_default)
			VALUES (?, ?, ?, ?, ?, ?)`, accountID, userID, bookID, book.URL, book.Name, boolInt(book.Default)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func normalizeContactAddressBooks(books []models.ContactAddressBook, fallbackURL string) []models.ContactAddressBook {
	seen := make(map[string]bool)
	out := make([]models.ContactAddressBook, 0, len(books)+1)
	for _, book := range books {
		url := strings.TrimSpace(book.URL)
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		book.URL = url
		book.Name = strings.TrimSpace(book.Name)
		book.Selected = true
		out = append(out, book)
	}
	if len(out) == 0 {
		fallbackURL = strings.TrimSpace(fallbackURL)
		if fallbackURL != "" {
			out = append(out, models.ContactAddressBook{URL: fallbackURL, Selected: true, Default: true})
		}
	}
	defaultSet := false
	for i := range out {
		if out[i].Default {
			if defaultSet {
				out[i].Default = false
				continue
			}
			defaultSet = true
		}
	}
	if len(out) > 0 && !defaultSet {
		out[0].Default = true
	}
	return out
}

func (s *AccountStore) DecryptContactSyncPassword(ctx context.Context, userID, accountID string) (string, error) {
	var encrypted []byte
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT encrypted_password FROM account_contact_sync_configs WHERE user_id = ? AND account_id = ?`, userID, accountID,
	).Scan(&encrypted)
	if err != nil {
		return "", err
	}
	if encrypted == nil {
		return "", nil
	}
	return s.decrypt(encrypted)
}

func (s *AccountStore) MarkContactSyncSuccess(ctx context.Context, userID, accountID, syncToken string) error {
	_, err := s.db.Write().ExecContext(ctx, `
		UPDATE account_contact_sync_configs
		SET last_sync_token = ?, last_success_at = CURRENT_TIMESTAMP, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND account_id = ?`, strings.TrimSpace(syncToken), userID, accountID)
	return err
}

func (s *AccountStore) MarkContactAddressBookSyncSuccess(ctx context.Context, userID, accountID, addressBookURL, syncToken string) error {
	_, err := s.db.Write().ExecContext(ctx, `
		UPDATE account_contact_address_books
		SET last_sync_token = ?, last_success_at = CURRENT_TIMESTAMP, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND account_id = ? AND url = ?`, strings.TrimSpace(syncToken), userID, accountID, strings.TrimSpace(addressBookURL))
	return err
}

func (s *AccountStore) MarkContactSyncError(ctx context.Context, userID, accountID, message string) error {
	_, err := s.db.Write().ExecContext(ctx, `
		UPDATE account_contact_sync_configs
		SET last_error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND account_id = ?`, strings.TrimSpace(message), userID, accountID)
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func accountProviderUsesGraphMail(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "outlook")
}

func (s *AccountStore) normalizeAccountMailTLSModes(ctx context.Context, req *models.CreateAccountRequest, fillDefaults bool) error {
	if accountProviderUsesGraphMail(req.Provider) {
		return nil
	}
	if fillDefaults && strings.TrimSpace(req.IMAPTLSMode) == "" {
		req.IMAPTLSMode = mailtransport.TLSModeImplicit
	}
	if fillDefaults && strings.TrimSpace(req.SMTPTLSMode) == "" {
		req.SMTPTLSMode = mailtransport.TLSModeImplicit
	}
	if strings.TrimSpace(req.IMAPTLSMode) != "" {
		allowPlaintext, err := s.db.IsPlaintextTransportAllowed(ctx, "imap", req.IMAPHost, req.IMAPPort)
		if err != nil {
			return fmt.Errorf("check IMAP security exception: %w", err)
		}
		mode, err := mailtransport.RequireTLSModeWithPlaintext("IMAP", req.IMAPTLSMode, allowPlaintext)
		if err != nil {
			return err
		}
		req.IMAPTLSMode = mode
	}
	if strings.TrimSpace(req.SMTPTLSMode) != "" {
		allowPlaintext, err := s.db.IsPlaintextTransportAllowed(ctx, "smtp", req.SMTPHost, req.SMTPPort)
		if err != nil {
			return fmt.Errorf("check SMTP security exception: %w", err)
		}
		mode, err := mailtransport.RequireTLSModeWithPlaintext("SMTP", req.SMTPTLSMode, allowPlaintext)
		if err != nil {
			return err
		}
		req.SMTPTLSMode = mode
	}
	if (req.IMAPTLSMode == mailtransport.TLSModePlaintext || req.SMTPTLSMode == mailtransport.TLSModePlaintext) && !strings.EqualFold(strings.TrimSpace(req.AuthMethod), "plain") {
		return fmt.Errorf("OAuth authentication is not allowed over a plaintext connection")
	}
	return nil
}

func isBuiltinContactProvider(provider string) bool {
	switch strings.TrimSpace(provider) {
	case "gmail", "outlook":
		return true
	default:
		return false
	}
}

func (s *AccountStore) CreateAccount(ctx context.Context, userID string, req *models.CreateAccountRequest) (*models.Account, error) {
	if req.Provider == "" {
		req.Provider = "imap"
	}
	if accountProviderUsesGraphMail(req.Provider) {
		req.IMAPHost = ""
		req.IMAPPort = 0
		req.IMAPTLSMode = ""
		req.SMTPHost = ""
		req.SMTPPort = 0
		req.SMTPTLSMode = ""
		req.Username = ""
		req.SmtpUsername = ""
		req.SmtpPassword = ""
		req.AuthMethod = "oauth2"
	} else {
		if req.IMAPPort == 0 {
			req.IMAPPort = 993
		}
		if req.SMTPPort == 0 {
			req.SMTPPort = 465
		}
		if req.AuthMethod == "" {
			req.AuthMethod = "plain"
		}
	}
	if err := s.normalizeAccountMailTLSModes(ctx, req, true); err != nil {
		return nil, err
	}

	id := generateAccountID(req.EmailAddress)
	if err := s.purgeDeletingAccountForCreate(ctx, userID, id); err != nil {
		return nil, fmt.Errorf("purge deleting account: %w", err)
	}

	encrypted, err := s.encrypt(req.Password)
	if err != nil {
		return nil, fmt.Errorf("encrypt password: %w", err)
	}

	var encryptedSmtpPw []byte
	if req.SmtpPassword != "" {
		encryptedSmtpPw, err = s.encrypt(req.SmtpPassword)
		if err != nil {
			return nil, fmt.Errorf("encrypt smtp password: %w", err)
		}
	}

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
	if err := s.normalizeAccountMailTLSModes(ctx, req, false); err != nil {
		return err
	}
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
	if accountProviderUsesGraphMail(req.Provider) {
		setClauses = append(setClauses,
			"imap_host = ?", "imap_port = ?", "imap_tls_mode = ?",
			"smtp_host = ?", "smtp_port = ?", "smtp_tls_mode = ?",
			"username = ?", "smtp_username = ?", "auth_method = ?",
		)
		args = append(args, "", 0, "", "", 0, "", "", "", "oauth2")
	} else {
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
		if req.AuthMethod != "" {
			setClauses = append(setClauses, "auth_method = ?")
			args = append(args, req.AuthMethod)
		}
		if req.SmtpUsername != "" {
			setClauses = append(setClauses, "smtp_username = ?")
			args = append(args, req.SmtpUsername)
		}
	}
	if req.Password != "" {
		encrypted, err := s.encrypt(req.Password)
		if err != nil {
			return fmt.Errorf("encrypt password: %w", err)
		}
		setClauses = append(setClauses, "encrypted_password = ?")
		args = append(args, encrypted)
	}
	if !accountProviderUsesGraphMail(req.Provider) && req.SmtpPassword != "" {
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

	query := fmt.Sprintf("UPDATE accounts SET %s WHERE id = ? AND COALESCE(is_deleting, 0) = 0", strings.Join(setClauses, ", "))
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
		     OR (provider = ? AND email_address = ? AND auth_method = 'oauth2')
		     OR (email_address = ? AND auth_method = 'oauth2' AND ? = 'gmail' AND imap_host = 'imap.gmail.com')
		   )
		 LIMIT 1`,
		userID, provider, providerAccountID, provider, email, email, provider,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *AccountStore) FindDeletingAccountIDByEmail(ctx context.Context, userID, email string) (string, error) {
	id := generateAccountID(email)
	var accountID string
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 1`,
		id, userID,
	).Scan(&accountID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return accountID, nil
}

func (s *AccountStore) IsAccountDeleting(ctx context.Context, accountID string) (bool, error) {
	var isDeleting int
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(is_deleting, 0) FROM accounts WHERE id = ?`,
		accountID,
	).Scan(&isDeleting)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isDeleting == 1, nil
}

func (s *AccountStore) ListDeletingAccountIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Read().QueryContext(ctx,
		`SELECT id FROM accounts WHERE COALESCE(is_deleting, 0) = 1 ORDER BY updated_at, id`,
	)
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

func (s *AccountStore) purgeDeletingAccountForCreate(ctx context.Context, userID, accountID string) error {
	var existingID string
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 1`,
		accountID, userID,
	).Scan(&existingID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return s.DeleteAccount(ctx, existingID)
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

func (s *AccountStore) SetEmailSyncEnabled(ctx context.Context, userID, accountID string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	res, err := s.db.Write().ExecContext(ctx,
		`UPDATE accounts SET email_sync_enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`,
		value, accountID, userID)
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

func (s *AccountStore) SetContactSyncEnabled(ctx context.Context, userID, accountID string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	var provider string
	if err := s.db.Read().QueryRowContext(ctx,
		`SELECT provider FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`,
		accountID, userID).Scan(&provider); err != nil {
		return err
	}
	if isBuiltinContactProvider(provider) {
		_, err := s.db.Write().ExecContext(ctx, `
			INSERT INTO account_contact_sync_configs (account_id, user_id, provider, enabled)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET
				user_id = excluded.user_id,
				provider = excluded.provider,
				enabled = excluded.enabled,
				updated_at = CURRENT_TIMESTAMP`, accountID, userID, provider, value)
		return err
	}
	res, err := s.db.Write().ExecContext(ctx,
		`UPDATE account_contact_sync_configs SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE account_id = ? AND user_id = ?`,
		value, accountID, userID)
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
	return s.DeleteAccountWithProgress(ctx, accountID, nil)
}

func (s *AccountStore) DeleteAccountWithProgress(ctx context.Context, accountID string, progress func(AccountDeletionProgress)) error {
	deleting, err := s.IsAccountDeleting(ctx, accountID)
	if err != nil {
		return err
	}
	if !deleting {
		return nil
	}

	statements := []accountDeletionStatement{
		{
			step:  "unlink message thread parents",
			table: "messages",
			sql: `UPDATE messages
			      SET thread_parent_id = NULL
			      WHERE rowid IN (
			          SELECT rowid
			          FROM messages
			          WHERE account_id = ? AND thread_parent_id IS NOT NULL
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete message search",
			table: "message_search",
			sql: `DELETE FROM message_search
			      WHERE rowid IN (SELECT rowid FROM message_search WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete legacy message search docs",
			table: "message_search_docs",
			sql: `DELETE FROM message_search_docs
			      WHERE rowid IN (
			          SELECT msd.rowid
			          FROM message_search_docs msd
			          JOIN messages m ON m.id = msd.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete folder thread state",
			table: "folder_thread_state",
			sql: `DELETE FROM folder_thread_state
			      WHERE rowid IN (SELECT rowid FROM folder_thread_state WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete scheduled sends",
			table: "outgoing_sends",
			sql: `DELETE FROM outgoing_sends
			      WHERE rowid IN (SELECT rowid FROM outgoing_sends WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete message mutations",
			table: "message_mutations",
			sql: `DELETE FROM message_mutations
			      WHERE rowid IN (SELECT rowid FROM message_mutations WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete IMAP draft operations",
			table: "imap_draft_operations",
			sql: `DELETE FROM imap_draft_operations
			      WHERE rowid IN (SELECT rowid FROM imap_draft_operations WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete IMAP draft state",
			table: "imap_draft_states",
			sql: `DELETE FROM imap_draft_states
			      WHERE rowid IN (SELECT rowid FROM imap_draft_states WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete label mutation queue",
			table: "label_mutation_queue",
			sql: `DELETE FROM label_mutation_queue
			      WHERE rowid IN (SELECT rowid FROM label_mutation_queue WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete unresolved references",
			table: "unresolved_references",
			sql: `DELETE FROM unresolved_references
			      WHERE rowid IN (SELECT rowid FROM unresolved_references WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete threads",
			table: "threads",
			sql: `DELETE FROM threads
			      WHERE rowid IN (SELECT rowid FROM threads WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete message folder state by folder",
			table: "message_folder_state",
			sql: `DELETE FROM message_folder_state
			      WHERE rowid IN (
			          SELECT mfs.rowid
			          FROM message_folder_state mfs
			          JOIN folders f ON f.id = mfs.folder_id
			          WHERE f.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete message folder state by message",
			table: "message_folder_state",
			sql: `DELETE FROM message_folder_state
			      WHERE rowid IN (
			          SELECT mfs.rowid
			          FROM message_folder_state mfs
			          JOIN messages m ON m.id = mfs.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete message labels by label",
			table: "message_labels",
			sql: `DELETE FROM message_labels
			      WHERE rowid IN (
			          SELECT ml.rowid
			          FROM message_labels ml
			          JOIN labels l ON l.id = ml.label_id
			          WHERE l.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete message labels by message",
			table: "message_labels",
			sql: `DELETE FROM message_labels
			      WHERE rowid IN (
			          SELECT ml.rowid
			          FROM message_labels ml
			          JOIN messages m ON m.id = ml.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete attachments",
			table: "attachments",
			sql: `DELETE FROM attachments
			      WHERE rowid IN (
			          SELECT a.rowid
			          FROM attachments a
			          JOIN messages m ON m.id = a.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete message recipients",
			table: "message_recipients",
			sql: `DELETE FROM message_recipients
			      WHERE rowid IN (
			          SELECT mr.rowid
			          FROM message_recipients mr
			          JOIN messages m ON m.id = mr.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete message references",
			table: "message_references",
			sql: `DELETE FROM message_references
			      WHERE rowid IN (
			          SELECT mr.rowid
			          FROM message_references mr
			          JOIN messages m ON m.id = mr.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete remote content state",
			table: "remote_content_messages",
			sql: `DELETE FROM remote_content_messages
			      WHERE rowid IN (
			          SELECT rcm.rowid
			          FROM remote_content_messages rcm
			          JOIN messages m ON m.id = rcm.message_id
			          WHERE m.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete messages",
			table: "messages",
			sql: `DELETE FROM messages
			      WHERE rowid IN (SELECT rowid FROM messages WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete sync state",
			table: "sync_state",
			sql: `DELETE FROM sync_state
			      WHERE rowid IN (SELECT rowid FROM sync_state WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete folders",
			table: "folders",
			sql: `DELETE FROM folders
			      WHERE rowid IN (SELECT rowid FROM folders WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete labels",
			table: "labels",
			sql: `DELETE FROM labels
			      WHERE rowid IN (SELECT rowid FROM labels WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete label sync state",
			table: "label_sync_state",
			sql: `DELETE FROM label_sync_state
			      WHERE rowid IN (SELECT rowid FROM label_sync_state WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete gmail watch state",
			table: "gmail_watch_state",
			sql: `DELETE FROM gmail_watch_state
			      WHERE account_id IN (SELECT account_id FROM gmail_watch_state WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete gmail poll state",
			table: "gmail_poll_state",
			sql: `DELETE FROM gmail_poll_state
			      WHERE account_id IN (SELECT account_id FROM gmail_poll_state WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete account contact address books",
			table: "account_contact_address_books",
			sql: `DELETE FROM account_contact_address_books
			      WHERE rowid IN (SELECT rowid FROM account_contact_address_books WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete account contact sync config",
			table: "account_contact_sync_configs",
			sql: `DELETE FROM account_contact_sync_configs
			      WHERE account_id IN (SELECT account_id FROM account_contact_sync_configs WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete account signature settings",
			table: "account_signature_settings",
			sql: `DELETE FROM account_signature_settings
			      WHERE account_id IN (SELECT account_id FROM account_signature_settings WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete contact card groups by card",
			table: "contact_card_groups",
			sql: `DELETE FROM contact_card_groups
			      WHERE rowid IN (
			          SELECT ccg.rowid
			          FROM contact_card_groups ccg
			          JOIN contact_cards cc ON cc.id = ccg.card_id
			          WHERE cc.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete contact card groups by group",
			table: "contact_card_groups",
			sql: `DELETE FROM contact_card_groups
			      WHERE rowid IN (
			          SELECT ccg.rowid
			          FROM contact_card_groups ccg
			          JOIN contact_groups cg ON cg.id = ccg.group_id
			          WHERE cg.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete contact fields",
			table: "contact_fields",
			sql: `DELETE FROM contact_fields
			      WHERE rowid IN (
			          SELECT cf.rowid
			          FROM contact_fields cf
			          JOIN contact_cards cc ON cc.id = cf.card_id
			          WHERE cc.account_id = ?
			          LIMIT ?
			      )`,
		},
		{
			step:  "delete contact conflicts",
			table: "contact_conflicts",
			sql: `DELETE FROM contact_conflicts
			      WHERE rowid IN (SELECT rowid FROM contact_conflicts WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete contact save targets",
			table: "contact_save_targets",
			sql: `DELETE FROM contact_save_targets
			      WHERE rowid IN (SELECT rowid FROM contact_save_targets WHERE target = 'account:' || ? LIMIT ?)`,
		},
		{
			step:  "delete contact sync memberships",
			table: "contact_sync_memberships",
			sql: `DELETE FROM contact_sync_memberships
			      WHERE rowid IN (SELECT rowid FROM contact_sync_memberships WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete contact cards",
			table: "contact_cards",
			sql: `DELETE FROM contact_cards
			      WHERE rowid IN (SELECT rowid FROM contact_cards WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete contact sources",
			table: "contact_sources",
			sql: `DELETE FROM contact_sources
			      WHERE rowid IN (SELECT rowid FROM contact_sources WHERE account_id = ? LIMIT ?)`,
		},
		{
			step:  "delete contact groups",
			table: "contact_groups",
			sql: `DELETE FROM contact_groups
			      WHERE rowid IN (SELECT rowid FROM contact_groups WHERE account_id = ? LIMIT ?)`,
		},
	}
	for _, statement := range statements {
		if err := s.runAccountDeletionStatement(ctx, accountID, statement, progress); err != nil {
			return err
		}
	}

	result, err := s.db.Write().ExecContext(ctx, `DELETE FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 1`, accountID)
	if err != nil {
		return err
	}
	if progress != nil {
		if rows, err := result.RowsAffected(); err == nil && rows > 0 {
			progress(AccountDeletionProgress{Step: "delete account", RowsAffected: rows, TotalStepRowsAffected: rows})
		}
	}
	return nil
}

type accountDeletionStatement struct {
	step  string
	table string
	sql   string
}

func (s *AccountStore) runAccountDeletionStatement(ctx context.Context, accountID string, statement accountDeletionStatement, progress func(AccountDeletionProgress)) error {
	exists, err := s.tableExists(ctx, statement.table)
	if err != nil {
		return fmt.Errorf("check %s table: %w", statement.table, err)
	}
	if !exists {
		return nil
	}

	var total int64
	for {
		result, err := s.db.Write().ExecContext(ctx, statement.sql, accountID, accountDeletionBatchSize)
		if err != nil {
			return fmt.Errorf("%s: %w", statement.step, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("%s rows affected: %w", statement.step, err)
		}
		if rows <= 0 {
			return nil
		}
		total += rows
		if progress != nil {
			progress(AccountDeletionProgress{Step: statement.step, RowsAffected: rows, TotalStepRowsAffected: total})
		}
	}
}

func (s *AccountStore) tableExists(ctx context.Context, table string) (bool, error) {
	var name string
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT name FROM sqlite_schema WHERE type IN ('table', 'view') AND name = ?`, table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *AccountStore) MarkAccountDeleting(ctx context.Context, accountID string) error {
	_, err := s.db.Write().ExecContext(ctx, `UPDATE accounts SET is_deleting = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, accountID)
	return err
}

func (s *AccountStore) AccountDeletionStatus(ctx context.Context, userID, accountID string) (string, error) {
	userID = strings.TrimSpace(userID)
	accountID = strings.TrimSpace(accountID)
	if userID == "" || accountID == "" {
		return "deleted", nil
	}
	var deleting int
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(is_deleting, 0) FROM accounts WHERE id = ? AND user_id = ?`,
		accountID, userID,
	).Scan(&deleting)
	if err == sql.ErrNoRows {
		return "deleted", nil
	}
	if err != nil {
		return "", err
	}
	if deleting == 1 {
		return "deleting", nil
	}
	return "active", nil
}

func (s *AccountStore) GetAccountByID(ctx context.Context, accountID string) (*models.Account, error) {
	return s.getAccountByID(ctx, accountID, "")
}

func (s *AccountStore) GetAccountByIDForUser(ctx context.Context, userID, accountID string) (*models.Account, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(accountID) == "" {
		return nil, nil
	}
	return s.getAccountByID(ctx, accountID, userID)
}

func (s *AccountStore) getAccountByID(ctx context.Context, accountID, userID string) (*models.Account, error) {
	var a models.Account
	var emailSyncEnabled, contactSyncEnabled int
	where := "a.id = ?"
	args := []any{accountID}
	if userID != "" {
		where += " AND a.user_id = ?"
		args = append(args, userID)
	}
	err := s.db.Read().QueryRowContext(ctx,
		`SELECT a.id, a.provider, a.email_address, a.display_name, a.color, a.initials, COALESCE(a.email_sync_enabled, 1),
		        COALESCE(a.email_sync_error, ''), COALESCE(a.email_sync_error_at, ''),
		        CASE WHEN a.provider IN ('gmail', 'outlook') THEN COALESCE(acc.enabled, 1) ELSE COALESCE(acc.enabled, 0) END AS contact_sync_enabled,
		        CASE WHEN a.provider IN ('gmail', 'outlook') THEN a.provider ELSE COALESCE(acc.provider, '') END AS contact_sync_provider
		 FROM accounts a
		 LEFT JOIN account_contact_sync_configs acc ON acc.account_id = a.id AND acc.user_id = a.user_id
		 WHERE `+where+` AND COALESCE(a.is_deleting, 0) = 0`, args...,
	).Scan(&a.ID, &a.Provider, &a.Email, &a.Name, &a.Color, &a.Initials, &emailSyncEnabled, &a.EmailSyncError, &a.EmailSyncErrorAt, &contactSyncEnabled, &a.ContactSyncProvider)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.EmailSyncEnabled = emailSyncEnabled == 1
	a.ContactSyncEnabled = contactSyncEnabled == 1
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
