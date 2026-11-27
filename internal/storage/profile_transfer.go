package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// ProfileImportPendingKey holds the accounts a profile import created that
// still wait for their password or sign-in. Each entry records which
// services to turn back on once the account is reconnected.
const ProfileImportPendingKey = "profile_import_pending"

// RemoteContentSenderSettingPrefix starts the settings that allow remote
// content from one sender. The rest of the key is a hash of the address.
const RemoteContentSenderSettingPrefix = "remote_content_sender_allow_"

// The logins an imported account can be waiting for. Gmail and Outlook
// accounts wait for a new sign-in; other accounts wait for their mail
// password, and for separate contacts or calendar passwords when they had
// them.
const (
	ImportedLoginMail     = "mail"
	ImportedLoginContacts = "contacts"
	ImportedLoginCalendar = "calendar"
	ImportedLoginSignIn   = "signin"
)

// ImportedAccountPending is what an imported account still needs, and what
// it turns on as each login is entered.
type ImportedAccountPending struct {
	Email    bool `json:"email"`
	Contacts bool `json:"contacts"`
	// Calendars are the remote IDs of the calendars to select again.
	Calendars []string `json:"calendars,omitempty"`
	// Needs are the logins still missing, in the order they are asked for.
	Needs []string `json:"needs,omitempty"`
	// SMTPSeparate is set when sending has its own login, entered with the
	// mail one.
	SMTPSeparate bool `json:"smtp_separate,omitempty"`
	// EmailLogin, ContactsLogin and CalendarLogin name the login each
	// service waits for. Contacts and calendars use the mail login unless
	// they had their own.
	EmailLogin    string `json:"email_login,omitempty"`
	ContactsLogin string `json:"contacts_login,omitempty"`
	CalendarLogin string `json:"calendar_login,omitempty"`
}

// Missing returns the logins the account still waits for.
func (p ImportedAccountPending) Missing() []string {
	if len(p.Needs) == 0 {
		return []string{p.emailLogin()}
	}
	return append([]string(nil), p.Needs...)
}

func (p ImportedAccountPending) emailLogin() string {
	if p.EmailLogin != "" {
		return p.EmailLogin
	}
	return ImportedLoginMail
}

func (p ImportedAccountPending) contactsLogin() string {
	if p.ContactsLogin != "" {
		return p.ContactsLogin
	}
	return p.emailLogin()
}

func (p ImportedAccountPending) calendarLogin() string {
	if p.CalendarLogin != "" {
		return p.CalendarLogin
	}
	return p.emailLogin()
}

// ImportedServices are the services to turn on once a login is entered.
type ImportedServices struct {
	Email     bool
	Contacts  bool
	Calendars []string
}

func (db *DB) ImportedAccountsPending(ctx context.Context, userID string) (map[string]ImportedAccountPending, error) {
	value, err := db.GetSetting(ctx, userID, ProfileImportPendingKey)
	if err != nil {
		return nil, err
	}
	return decodeImportedAccountsPending(value), nil
}

func decodeImportedAccountsPending(value string) map[string]ImportedAccountPending {
	pending := map[string]ImportedAccountPending{}
	if strings.TrimSpace(value) == "" {
		return pending
	}
	if err := json.Unmarshal([]byte(value), &pending); err != nil {
		return map[string]ImportedAccountPending{}
	}
	return pending
}

// SetImportedAccountPending records an imported account as waiting for its
// credentials.
func (db *DB) SetImportedAccountPending(ctx context.Context, userID, accountID string, pending ImportedAccountPending) error {
	_, err := db.updateImportedAccountsPending(ctx, userID, func(all map[string]ImportedAccountPending) {
		all[accountID] = pending
	})
	return err
}

// ResolveImportedAccountLogin records that an imported account has one of
// the logins it was waiting for, and returns the services that login turns
// on. The account stops waiting once it has them all. waiting is false when
// the account was not waiting for that login.
func (db *DB) ResolveImportedAccountLogin(ctx context.Context, userID, accountID, login string) (services ImportedServices, remaining []string, waiting bool, err error) {
	_, err = db.updateImportedAccountsPending(ctx, userID, func(all map[string]ImportedAccountPending) {
		pending, found := all[accountID]
		if !found {
			return
		}
		missing := pending.Missing()
		index := -1
		for i, need := range missing {
			if need == login {
				index = i
			}
		}
		if index < 0 {
			remaining = missing
			return
		}
		waiting = true
		remaining = append(missing[:index:index], missing[index+1:]...)
		services.Email = pending.Email && pending.emailLogin() == login
		services.Contacts = pending.Contacts && pending.contactsLogin() == login
		if pending.calendarLogin() == login {
			services.Calendars = pending.Calendars
		}
		if len(remaining) == 0 {
			delete(all, accountID)
			return
		}
		pending.Needs = remaining
		all[accountID] = pending
	})
	return services, remaining, waiting, err
}

func (db *DB) updateImportedAccountsPending(ctx context.Context, userID string, change func(map[string]ImportedAccountPending)) (map[string]ImportedAccountPending, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var value string
	err = tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id = ? AND key = ?`, userID, ProfileImportPendingKey).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	all := decodeImportedAccountsPending(value)
	change(all)
	// Accounts deleted while they waited leave nothing behind.
	for id := range all {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0)`, id, userID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			delete(all, id)
		}
	}
	if len(all) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_settings WHERE user_id = ? AND key = ?`, userID, ProfileImportPendingKey); err != nil {
			return nil, err
		}
	} else {
		encoded, err := json.Marshal(all)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO app_settings (key, user_id, value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, ProfileImportPendingKey, userID, string(encoded)); err != nil {
			return nil, err
		}
	}
	return all, tx.Commit()
}

func (db *DB) attachImportedAccountsPending(ctx context.Context, userID string, accounts []models.Account) {
	if len(accounts) == 0 {
		return
	}
	pending, err := db.ImportedAccountsPending(ctx, userID)
	if err != nil || len(pending) == 0 {
		return
	}
	for i := range accounts {
		if waiting, found := pending[accounts[i].ID]; found {
			accounts[i].ReconnectPending = true
			accounts[i].ReconnectNeeds = waiting.Missing()
		}
	}
}

// ListSettingsWithPrefix returns the user's settings whose keys start with
// prefix.
func (db *DB) ListSettingsWithPrefix(ctx context.Context, userID, prefix string) (map[string]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT key, value FROM app_settings WHERE user_id = ? AND substr(key, 1, ?) = ?`, userID, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	settings := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		settings[key] = value
	}
	return settings, rows.Err()
}

// SetSettings saves several settings at once.
func (db *DB) SetSettings(ctx context.Context, userID string, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range settings {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO app_settings (key, user_id, value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, key, userID, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddImportedCalendarSources adds calendars from a profile to an account that
// has none yet. They start unselected, since the account has no credentials
// to sync them with; discovery later keeps their selection and hidden state.
func (db *DB) AddImportedCalendarSources(ctx context.Context, userID, accountID, provider string, sources []CalendarSource) error {
	if len(sources) == 0 {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, source := range sources {
		remoteID := strings.TrimSpace(source.RemoteID)
		if remoteID == "" {
			continue
		}
		id := uuid.NewString()
		result, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_sources (
				id, user_id, account_id, provider, remote_id, name, description,
				timezone, color, access_role, is_primary, is_selected, is_hidden, is_deleted, chosen_color
			)
			SELECT ?, ?, a.id, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?
			FROM accounts a
			WHERE a.id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
			ON CONFLICT(account_id, remote_id) DO NOTHING`,
			id, userID, provider, remoteID,
			strings.TrimSpace(source.Name), strings.TrimSpace(source.Description),
			strings.TrimSpace(source.TimeZone), strings.TrimSpace(source.Color), strings.TrimSpace(source.AccessRole),
			calendarBoolInt(source.IsPrimary), calendarBoolInt(source.IsHidden), importedCalendarColor(source.ChosenColor),
			accountID, userID)
		if err != nil {
			return fmt.Errorf("add imported calendar %q: %w", remoteID, err)
		}
		if added, err := result.RowsAffected(); err != nil {
			return err
		} else if added == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO calendar_sync_state (source_id) VALUES (?) ON CONFLICT(source_id) DO NOTHING`, id); err != nil {
			return fmt.Errorf("create calendar sync state %q: %w", id, err)
		}
	}
	return tx.Commit()
}

// importedCalendarColor is a profile's color picked in Gofer for a calendar,
// or empty when it isn't #rrggbb.
func importedCalendarColor(color string) string {
	color = strings.ToLower(strings.TrimSpace(color))
	if !calendarHexColor(color) {
		return ""
	}
	return color
}
