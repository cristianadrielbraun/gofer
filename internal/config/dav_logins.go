package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

// CalDAV and CardDAV requests carry no account, only the username and
// password they sign in with. Each time one of an account's passwords is read
// for them, it is bound to the account's proxy with every username the
// account may sign in with, so the request takes the account's route whatever
// context it is made with (see netguard.BindBasicProxy).

// BindDAVLogin routes CalDAV and CardDAV requests that sign in with password,
// as one of the account's usernames or as one of extra, through the
// account's proxy. An account whose proxy can't be read is an error.
func (s *AccountStore) BindDAVLogin(ctx context.Context, accountID, password string, extra ...string) error {
	if password == "" {
		return nil
	}
	proxy, err := s.AccountProxy(ctx, accountID)
	if err != nil {
		return err
	}
	var username, email, contacts, calendar string
	if err := s.db.Read().QueryRowContext(ctx, `
		SELECT a.username, a.email_address, COALESCE(c.username, ''), COALESCE(d.username, '')
		FROM accounts a
		LEFT JOIN account_contact_sync_configs c ON c.account_id = a.id
		LEFT JOIN account_caldav_configs d ON d.account_id = a.id
		WHERE a.id = ?`, accountID).Scan(&username, &email, &contacts, &calendar); err != nil {
		return fmt.Errorf("read account sign-in names: %w", err)
	}
	bindDAVLogins(proxy, password, append([]string{username, email, contacts, calendar}, extra...))
	return nil
}

func bindDAVLogins(proxy *netguard.Proxy, password string, usernames []string) {
	if password == "" {
		return
	}
	for _, name := range usernames {
		netguard.BindBasicProxy(name, password, proxy)
		if trimmed := strings.TrimSpace(name); trimmed != name {
			netguard.BindBasicProxy(trimmed, password, proxy)
		}
	}
}

// BindDAVLogin is AccountStore.BindDAVLogin for an account copied into a
// snapshot, with the proxy read when the snapshot was taken.
func (s *AccountServiceSnapshot) BindDAVLogin(password string, extra ...string) error {
	if password == "" {
		return nil
	}
	if s.proxyErr != nil {
		return s.proxyErr
	}
	d := s.data
	bindDAVLogins(s.proxy, password, append([]string{d.Username, d.EmailAddress, d.ContactSync.Username, d.CalendarSync.CalDAVUsername}, extra...))
	return nil
}
