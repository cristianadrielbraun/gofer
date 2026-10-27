package config

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var ErrAccountServicesChanged = errors.New("account service configuration changed during provider work")
var ErrContactServicesNotConfigured = errors.New("contact service is not configured")

// AccountServiceSnapshot copies endpoint and encrypted credential state, without
// retaining a database lease. The private owner and repository binding cannot
// be supplied by the browser. Provider results must use a guarded publication.
type AccountServiceSnapshot struct {
	repository                                                *UserAccountStore
	owner, id                                                 string
	data                                                      models.EditAccountData
	password, contactPassword, calendarPassword, smtpPassword []byte
	connection, contacts, calendar, smtp                      [32]byte
}

func (s *AccountServiceSnapshot) OwnerID() string   { return s.owner }
func (s *AccountServiceSnapshot) AccountID() string { return s.id }

type AccountServiceIdentity struct {
	Provider, ProviderAccountID, EmailAddress, DisplayName, Username, AuthMethod, IMAPHost, SMTPHost string
	JMAPSessionURL                                                                                   string
}

func (s *AccountServiceSnapshot) Identity() AccountServiceIdentity {
	d := s.data
	return AccountServiceIdentity{d.Provider, d.ProviderAccountID, d.EmailAddress, d.DisplayName, d.Username, d.AuthMethod, d.IMAPHost, d.SMTPHost, d.JMAPSessionURL}
}

// SMTP readiness is copied with the Calendar authority so DAV email preflight
// never consults shared account rows or adopts changed sending configuration.
func (s *AccountServiceSnapshot) SMTPConfigured() bool {
	return strings.TrimSpace(s.data.SMTPHost) != "" && s.data.SMTPPort > 0
}

// SendsEmail reports whether the account can send the email a calendar reply
// needs: over SMTP, or over JMAP for a JMAP account signed in with a password.
func (s *AccountServiceSnapshot) SendsEmail() bool {
	if s.data.Provider == providers.ProviderJMAP {
		return s.data.AuthMethod == "plain" && providers.JMAPEnabled()
	}
	return s.SMTPConfigured()
}

// EmailTransport is how the account sends calendar email: JMAP accounts over
// JMAP, others over SMTP.
func (s *AccountServiceSnapshot) EmailTransport() string {
	if s.data.Provider == providers.ProviderJMAP {
		return storage.OutgoingTransportJMAP
	}
	return storage.OutgoingTransportSMTP
}

// passwordMailbox reports whether the account is an IMAP or JMAP mailbox
// signed in with a password, the accounts whose calendars come from CalDAV.
func (s *AccountServiceSnapshot) passwordMailbox() bool {
	return (s.data.Provider == providers.ProviderIMAP || s.data.Provider == providers.ProviderJMAP) && s.data.AuthMethod == "plain"
}

// JMAPConfig is a JMAP account's connection from the copied session URL, with
// the central transport exceptions for its endpoint, as SMTPConfig is for SMTP.
func (s *AccountServiceSnapshot) JMAPConfig(ctx context.Context) (*models.AccountConfig, error) {
	if err := s.repository.checkServiceSnapshot(s); err != nil {
		return nil, err
	}
	if err := s.checkMailAllowlist(ctx); err != nil {
		return nil, err
	}
	d := s.data
	if d.Provider != providers.ProviderJMAP || d.AuthMethod != "plain" {
		return nil, errors.New("the account does not send over JMAP")
	}
	if !providers.JMAPEnabled() {
		return nil, providers.ErrJMAPDisabled
	}
	cfg := &models.AccountConfig{AccountID: s.id, Provider: d.Provider, ProviderAccountID: d.ProviderAccountID,
		Username: d.Username, AuthMethod: d.AuthMethod, JMAPSessionURL: d.JMAPSessionURL}
	if err := s.repository.base.applyJMAPTransportPolicy(ctx, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Read the central transport exception for the copied endpoint, without
// reacquiring account configuration or retaining a user-store lease.
func (s *AccountServiceSnapshot) SMTPConfig(ctx context.Context) (*models.AccountConfig, error) {
	if err := s.repository.checkServiceSnapshot(s); err != nil {
		return nil, err
	}
	if err := s.checkMailAllowlist(ctx); err != nil {
		return nil, err
	}
	d := s.data
	cfg := &models.AccountConfig{AccountID: s.id, Provider: d.Provider, ProviderAccountID: d.ProviderAccountID,
		SMTPHost: d.SMTPHost, SMTPPort: d.SMTPPort, SMTPTLSMode: d.SMTPTLSMode,
		Username: d.Username, AuthMethod: d.AuthMethod, SmtpUsername: d.SmtpUsername}
	var err error
	cfg.SMTPAllowPlaintext, err = s.repository.routing.System().IsPlaintextTransportAllowed(ctx, "smtp", cfg.SMTPHost, cfg.SMTPPort)
	if err != nil {
		return nil, err
	}
	cfg.SMTPTLSMode, err = mailtransport.RequireTLSModeWithPlaintext("SMTP", cfg.SMTPTLSMode, cfg.SMTPAllowPlaintext)
	if err != nil {
		return nil, err
	}
	if cfg.SMTPTLSMode == mailtransport.TLSModePlaintext && !strings.EqualFold(strings.TrimSpace(cfg.AuthMethod), "plain") {
		return nil, errors.New("SMTP OAuth authentication is not allowed over a plaintext connection")
	}
	cfg.SMTPAllowPrivate, err = s.repository.routing.System().IsPrivateTargetAllowed(ctx, "smtp", cfg.SMTPHost, cfg.SMTPPort)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *AccountServiceSnapshot) SMTPPassword() (string, error) {
	if s.data.SmtpUsername != "" && len(s.smtpPassword) > 0 {
		password, err := s.repository.base.decryptPassword(s.owner, s.id, passwordKindSMTP, s.smtpPassword)
		if err != nil {
			return "", err
		}
		if password != "" {
			return password, nil
		}
	}
	return s.MailboxPassword()
}

func (s *AccountServiceSnapshot) ContactConfig() models.ContactSyncConfig {
	cfg := s.data.ContactSync
	cfg.AddressBooks = append([]models.ContactAddressBook(nil), cfg.AddressBooks...)
	return cfg
}
func (s *AccountServiceSnapshot) CalDAVSettings() (baseURL, username string, useAccountCredentials bool) {
	cfg := s.data.CalendarSync
	return cfg.CalDAVBaseURL, cfg.CalDAVUsername, cfg.CalDAVUseAccountCredentials
}

// ContactSyncPassword preserves the saved CardDAV-then-mailbox fallback. It
// decrypts a copied ciphertext only; no local store is opened during HTTP.
func (s *AccountServiceSnapshot) ContactSyncPassword(submitted string) (string, error) {
	if strings.TrimSpace(submitted) != "" {
		return submitted, nil
	}
	if len(s.contactPassword) > 0 {
		password, err := s.repository.base.decryptPassword(s.owner, s.id, passwordKindCardDAV, s.contactPassword)
		if err == nil && password != "" {
			return password, nil
		}
	}
	return s.MailboxPassword()
}
func (s *AccountServiceSnapshot) MailboxPassword() (string, error) {
	if len(s.password) == 0 {
		return "", nil
	}
	return s.repository.base.decryptPassword(s.owner, s.id, passwordKindIMAP, s.password)
}
func (s *AccountServiceSnapshot) CalDAVPassword(submitted string, useAccountCredentials bool) (string, error) {
	if useAccountCredentials {
		return s.MailboxPassword()
	}
	if strings.TrimSpace(submitted) != "" {
		return submitted, nil
	}
	if len(s.calendarPassword) == 0 {
		return "", nil
	}
	return s.repository.base.decryptPassword(s.owner, s.id, passwordKindCalDAV, s.calendarPassword)
}

func (s *UserAccountStore) SnapshotServices(ctx context.Context, owner, id string) (snapshot *AccountServiceSnapshot, err error) {
	err = s.WithAccountForUser(ctx, owner, id, func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		snapshot, err = s.serviceSnapshot(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func serviceStateHash(value any) ([32]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func (s *UserAccountStore) serviceSnapshot(ctx context.Context, tx *sql.Tx, owner, id string) (*AccountServiceSnapshot, error) {
	snapshot := &AccountServiceSnapshot{repository: s, owner: owner, id: id}
	data := &snapshot.data
	data.AccountID = id
	var emailEnabled int
	err := tx.QueryRowContext(ctx, `SELECT a.provider,a.provider_account_id,a.email_address,a.display_name,
 a.username,a.auth_method,a.imap_host,a.smtp_host,a.smtp_port,a.encrypted_password,COALESCE(a.email_sync_enabled,1),
 a.smtp_tls_mode,COALESCE(a.smtp_username,''),a.encrypted_smtp_password,COALESCE(a.jmap_session_url,'')
 FROM accounts a JOIN users u ON u.id=a.user_id
 WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0
 AND u.status='active' AND u.deletion_pending=0`, id, owner).Scan(
		&data.Provider, &data.ProviderAccountID, &data.EmailAddress, &data.DisplayName,
		&data.Username, &data.AuthMethod, &data.IMAPHost, &data.SMTPHost, &data.SMTPPort, &snapshot.password, &emailEnabled,
		&data.SMTPTLSMode, &data.SmtpUsername, &snapshot.smtpPassword, &data.JMAPSessionURL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrAccountRoute
	}
	if err != nil {
		return nil, err
	}
	data.EmailSyncEnabled = emailEnabled == 1
	// Display metadata and mail-only sync controls are not service identity.
	snapshot.connection, err = serviceStateHash([]any{data.Provider, data.ProviderAccountID, data.EmailAddress,
		data.Username, data.AuthMethod, data.IMAPHost, data.SMTPHost, data.SMTPPort, snapshot.password, data.JMAPSessionURL})
	if err != nil {
		return nil, err
	}

	snapshot.smtp, err = serviceStateHash([]any{snapshot.connection, data.SMTPTLSMode, data.SmtpUsername, snapshot.smtpPassword})
	if err != nil {
		return nil, err
	}

	cfg := &data.ContactSync
	*cfg = models.ContactSyncConfig{AccountID: id, UserID: owner, Provider: "carddav"}
	var enabled int
	var lastSuccess, updated sql.NullString
	contactPresent := true
	err = tx.QueryRowContext(ctx, `SELECT provider,enabled,base_url,addressbook_url,username,encrypted_password,
 last_sync_token,last_error,last_success_at,updated_at FROM account_contact_sync_configs
 WHERE account_id=? AND user_id=?`, id, owner).Scan(&cfg.Provider, &enabled, &cfg.BaseURL, &cfg.AddressBookURL,
		&cfg.Username, &snapshot.contactPassword, &cfg.LastSyncToken, &cfg.LastError, &lastSuccess, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		contactPresent = false
	} else if err != nil {
		return nil, err
	}
	cfg.Enabled = enabled == 1
	cfg.HasPassword = len(snapshot.contactPassword) > 0
	cfg.LastSuccessAt, cfg.UpdatedAt = lastSuccess.String, updated.String
	rows, err := tx.QueryContext(ctx, `SELECT id,name,url,is_default,last_sync_token FROM account_contact_address_books
 WHERE account_id=? AND user_id=? ORDER BY is_default DESC,name COLLATE NOCASE,url`, id, owner)
	if err != nil {
		return nil, err
	}
	var bookIdentity []any
	for rows.Next() {
		var book models.ContactAddressBook
		var isDefault int
		if err := rows.Scan(&book.ID, &book.Name, &book.URL, &isDefault, &book.LastSyncToken); err != nil {
			rows.Close()
			return nil, err
		}
		book.Selected, book.Default = true, isDefault == 1
		cfg.AddressBooks = append(cfg.AddressBooks, book)
		bookIdentity = append(bookIdentity, []any{book.ID, book.Name, book.URL, book.Default, book.LastSyncToken})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Ignore status-only timestamps/errors, but include durable DAV cursors and
	// exact book membership. A previous endpoint test cannot reset newer state.
	snapshot.contacts, err = serviceStateHash([]any{contactPresent, cfg.Provider, cfg.Enabled, cfg.BaseURL,
		cfg.AddressBookURL, cfg.Username, snapshot.contactPassword, cfg.LastSyncToken, bookIdentity})
	if err != nil {
		return nil, err
	}
	if len(cfg.AddressBooks) == 0 && strings.TrimSpace(cfg.AddressBookURL) != "" {
		cfg.AddressBooks = []models.ContactAddressBook{{URL: strings.TrimSpace(cfg.AddressBookURL), Default: true, Selected: true, LastSyncToken: cfg.LastSyncToken}}
	}

	if isBuiltinContactProvider(data.Provider) {
		cfg.Provider = data.Provider
		if !contactPresent {
			cfg.Enabled = true
		}
	}

	cal := &data.CalendarSync
	*cal = models.CalendarSyncConfig{AccountID: id, UserID: owner, Provider: data.Provider, CalDAVUseAccountCredentials: true}
	var useAccount int
	calendarPresent := true
	err = tx.QueryRowContext(ctx, `SELECT base_url,username,encrypted_password,use_account_credentials
 FROM account_caldav_configs WHERE account_id=? AND user_id=?`, id, owner).Scan(
		&cal.CalDAVBaseURL, &cal.CalDAVUsername, &snapshot.calendarPassword, &useAccount)
	if errors.Is(err, sql.ErrNoRows) {
		calendarPresent = false
	} else if err != nil {
		return nil, err
	}
	if calendarPresent {
		cal.CalDAVUseAccountCredentials = useAccount == 1
	}
	cal.CalDAVHasPassword = len(snapshot.calendarPassword) > 0
	snapshot.calendar, err = serviceStateHash([]any{calendarPresent, cal.CalDAVBaseURL, cal.CalDAVUsername,
		snapshot.calendarPassword, cal.CalDAVUseAccountCredentials})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) serviceGuard(ctx context.Context, snapshot *AccountServiceSnapshot, contacts bool) func(*sql.Tx) error {
	return s.serviceGuardWithSMTP(ctx, snapshot, contacts, false)
}

func (s *UserAccountStore) serviceGuardWithSMTP(ctx context.Context, snapshot *AccountServiceSnapshot, contacts, smtp bool) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		// The local writer may have been busy after routing authorized the call.
		// Recheck central lifecycle after that wait, before any local publication.
		if err := s.routing.ValidateUser(ctx, snapshot.owner); err != nil {
			return err
		}
		state, err := s.routing.AccountStateForUser(ctx, snapshot.owner, snapshot.id)
		if err != nil {
			return err
		}
		if state != storage.AccountActive {
			return storage.ErrAccountRoute
		}
		current, err := s.serviceSnapshot(ctx, tx, snapshot.owner, snapshot.id)
		if err != nil {
			return err
		}
		if current.connection != snapshot.connection || (contacts && current.contacts != snapshot.contacts) || (!contacts && current.calendar != snapshot.calendar) || (smtp && current.smtp != snapshot.smtp) {
			return ErrAccountServicesChanged
		}
		return nil
	}
}
func (s *UserAccountStore) checkServiceSnapshot(snapshot *AccountServiceSnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.owner == "" || snapshot.id == "" {
		return storage.ErrAccountRoute
	}
	return nil
}

// PublishInboundContacts releases the store after the guarded local commit;
// fetching a provider page precedes this call and holds no database lease.
func (s *UserAccountStore) PublishInboundContacts(ctx context.Context, snapshot *AccountServiceSnapshot, inputs []storage.InboundContact) (results []storage.InboundContactResult, err error) {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return nil, err
	}
	cfg := snapshot.ContactConfig()
	if !cfg.Enabled {
		return nil, storage.ErrContactPublication
	}
	err = s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		var publishErr error
		results, publishErr = db.PublishInboundContacts(ctx, snapshot.owner, snapshot.id, cfg.Provider, inputs, s.serviceGuard(ctx, snapshot, true))
		return publishErr
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// PublishInboundContactBook returns a fresh opaque snapshot from the same
// transaction as the DAV cursor. The next book cannot recapture a changed
// endpoint/identity and accidentally publish results fetched with old settings.
func (s *UserAccountStore) PublishInboundContactBook(ctx context.Context, snapshot *AccountServiceSnapshot, bookID string, page storage.InboundContactBookPage) (results []storage.InboundContactResult, next *AccountServiceSnapshot, err error) {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return nil, nil, err
	}
	cfg := snapshot.ContactConfig()
	if !cfg.Enabled || cfg.Provider != "carddav" {
		return nil, nil, storage.ErrContactPublication
	}
	var book models.ContactAddressBook
	found := false
	for _, candidate := range cfg.AddressBooks {
		if candidate.ID == bookID {
			book, found = candidate, true
			break
		}
	}
	if !found {
		return nil, nil, storage.ErrContactPublication
	}
	err = s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		var publishErr error
		results, publishErr = db.PublishInboundContactBook(ctx, snapshot.owner, snapshot.id, book, page, s.serviceGuard(ctx, snapshot, true), func(tx *sql.Tx) error {
			var snapshotErr error
			next, snapshotErr = s.serviceSnapshot(ctx, tx, snapshot.owner, snapshot.id)
			return snapshotErr
		})
		return publishErr
	})
	if err != nil {
		return nil, nil, err
	}
	return results, next, nil
}

// PublishInboundJMAPContacts commits a JMAP contacts pull and returns the
// snapshot holding its new state, as PublishInboundContactBook does for a book.
func (s *UserAccountStore) PublishInboundJMAPContacts(ctx context.Context, snapshot *AccountServiceSnapshot, page storage.InboundJMAPContactPage) (results []storage.InboundContactResult, next *AccountServiceSnapshot, err error) {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return nil, nil, err
	}
	cfg := snapshot.ContactConfig()
	if !cfg.Enabled || cfg.Provider != providers.ProviderJMAP {
		return nil, nil, storage.ErrContactPublication
	}
	err = s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		var publishErr error
		results, publishErr = db.PublishInboundJMAPContacts(ctx, snapshot.owner, snapshot.id, page, s.serviceGuard(ctx, snapshot, true), func(tx *sql.Tx) error {
			var snapshotErr error
			next, snapshotErr = s.serviceSnapshot(ctx, tx, snapshot.owner, snapshot.id)
			return snapshotErr
		})
		return publishErr
	})
	if err != nil {
		return nil, nil, err
	}
	return results, next, nil
}

// SaveContactServices checks the snapshot in the same local writer transaction
// that replaces configuration/address books. Network testing precedes this call.
func (s *UserAccountStore) SaveContactServices(ctx context.Context, snapshot *AccountServiceSnapshot, cfg models.ContactSyncConfig, password string) error {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return err
	}
	cfg.AccountID, cfg.UserID = snapshot.id, snapshot.owner
	cfg.AddressBooks = append([]models.ContactAddressBook(nil), cfg.AddressBooks...)
	addresses := []string{cfg.BaseURL, cfg.AddressBookURL}
	for _, book := range cfg.AddressBooks {
		addresses = append(addresses, book.URL)
	}
	if err := s.checkServiceURLs(ctx, snapshot.owner, addresses...); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(local *AccountStore, _ *storage.DB) error {
		return local.saveContactSyncConfig(ctx, snapshot.owner, snapshot.id, cfg, password, s.serviceGuard(ctx, snapshot, true))
	})
}

// SetContactServicesEnabled preserves credentials, selected books and cursors.
// Recheck the copied state after waiting for the local writer, before toggling.
func (s *UserAccountStore) SetContactServicesEnabled(ctx context.Context, snapshot *AccountServiceSnapshot, enabled bool) error {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.serviceGuard(ctx, snapshot, true)(tx); err != nil {
			return err
		}
		if isBuiltinContactProvider(snapshot.Identity().Provider) {
			_, err = tx.ExecContext(ctx, `INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled)
 VALUES(?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET enabled=excluded.enabled,updated_at=CURRENT_TIMESTAMP`,
				snapshot.id, snapshot.owner, snapshot.Identity().Provider, boolInt(enabled))
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE account_contact_sync_configs SET enabled=?,updated_at=CURRENT_TIMESTAMP
 WHERE account_id=? AND user_id=?`, boolInt(enabled), snapshot.id, snapshot.owner)
			if err == nil {
				var count int64
				count, err = result.RowsAffected()
				if err == nil && count == 0 {
					return ErrContactServicesNotConfigured
				}
			}
		}
		if err != nil {
			return err
		}
		return tx.Commit()
	})
}
func (s *UserAccountStore) SaveCalDAVServices(ctx context.Context, snapshot *AccountServiceSnapshot, baseURL, username, password string, useAccountCredentials bool) error {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return err
	}
	if err := s.checkServiceURLs(ctx, snapshot.owner, baseURL); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(local *AccountStore, _ *storage.DB) error {
		return local.saveCalDAVConfig(ctx, snapshot.owner, snapshot.id, baseURL, username, password, useAccountCredentials, s.serviceGuard(ctx, snapshot, false))
	})
}

// ValidateContactServices rechecks a copied setup result without publishing or
// retaining a writer connection. Saved endpoint testing/discovery must call it
// before returning provider data after an account/configuration edit.
func (s *UserAccountStore) ValidateContactServices(ctx context.Context, snapshot *AccountServiceSnapshot) error {
	if err := s.checkServiceSnapshot(snapshot); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.owner, snapshot.id, func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.serviceGuard(ctx, snapshot, true)(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}
