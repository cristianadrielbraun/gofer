package config

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A proxy's password is bound to the proxy, the way an account's passwords
// are bound to the account, so it doesn't decrypt in another proxy's row.
const passwordKindProxy = "proxy"

// OutboundProxyInput is a proxy as an administrator entered it.
type OutboundProxyInput struct {
	// ID is empty for a new proxy.
	ID       string
	Name     string
	Type     string
	Host     string
	Port     int
	Username string
	// Password nil keeps the saved password of an existing proxy; any other
	// value replaces it, and "" removes it.
	Password  *string
	IsDefault bool
}

// SaveOutboundProxy validates and saves a proxy, and returns its id. Only
// "http" and "socks5" proxies are accepted.
func (s *AccountStore) SaveOutboundProxy(ctx context.Context, input OutboundProxyInput, createdBy string) (string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return "", errors.New("proxy name is required")
	}
	if len(name) > 100 {
		return "", errors.New("proxy name can be at most 100 characters")
	}
	id := strings.TrimSpace(input.ID)
	var existing storage.OutboundProxy
	if id != "" {
		var err error
		if existing, err = s.policyDB.GetOutboundProxy(ctx, id); err != nil {
			return "", err
		}
	} else {
		id = storage.NewOutboundProxyID()
	}

	password := ""
	keepPassword := input.Password == nil && len(existing.EncryptedPassword) > 0
	if input.Password != nil {
		password = *input.Password
	} else if keepPassword {
		var err error
		if password, err = s.decryptPassword("", id, passwordKindProxy, existing.EncryptedPassword); err != nil {
			return "", fmt.Errorf("read saved proxy password: %w", err)
		}
	}
	username := strings.TrimSpace(input.Username)
	if username == "" {
		// A password alone is never sent, so none is kept.
		password = ""
	}
	proxy := netguard.Proxy{Type: strings.ToLower(strings.TrimSpace(input.Type)), Host: strings.TrimSpace(input.Host), Port: input.Port,
		Username: username, Password: password}
	if err := proxy.Validate(); err != nil {
		return "", err
	}

	row := storage.OutboundProxy{ID: id, Name: name, Type: proxy.Type, Host: proxy.Host, Port: proxy.Port,
		Username: proxy.Username, IsDefault: input.IsDefault}
	if password != "" {
		encrypted, err := s.encryptPassword("", id, passwordKindProxy, password)
		if err != nil {
			return "", err
		}
		row.EncryptedPassword = encrypted
	}
	if err := s.policyDB.SaveOutboundProxy(ctx, row, createdBy); err != nil {
		return "", err
	}
	return id, nil
}

// ListOutboundProxies returns the saved proxies. Their passwords stay
// encrypted.
func (s *AccountStore) ListOutboundProxies(ctx context.Context) ([]storage.OutboundProxy, error) {
	return s.policyDB.ListOutboundProxies(ctx)
}

// DeleteOutboundProxy removes a saved proxy. Accounts that chose it connect
// as on the default afterwards.
func (s *AccountStore) DeleteOutboundProxy(ctx context.Context, id string) error {
	return s.policyDB.DeleteOutboundProxy(ctx, id)
}

// AccountProxy returns the proxy the account connects through, or nil for a
// direct connection.
func (s *AccountStore) AccountProxy(ctx context.Context, accountID string) (*netguard.Proxy, error) {
	choice, err := s.db.GetAccountProxyChoice(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("read account proxy: %w", err)
	}
	return s.ResolveProxyChoice(ctx, choice)
}

// ResolveProxyChoice returns the proxy a choice means: none for
// storage.AccountProxyDirect, the saved proxy for its id, and the default
// proxy, or none when there is no default, for storage.AccountProxyDefault
// and for the id of a proxy that was deleted.
//
// A proxy that can't be read is an error, never a direct connection: an
// account set to use a proxy must not quietly connect without it.
func (s *AccountStore) ResolveProxyChoice(ctx context.Context, choice string) (*netguard.Proxy, error) {
	choice = strings.TrimSpace(choice)
	if choice == storage.AccountProxyDirect {
		return nil, nil
	}
	if choice != storage.AccountProxyDefault {
		saved, err := s.policyDB.GetOutboundProxy(ctx, choice)
		if err == nil {
			return s.openOutboundProxy(saved)
		}
		if !errors.Is(err, storage.ErrOutboundProxyNotFound) {
			return nil, fmt.Errorf("read proxy: %w", err)
		}
	}
	saved, ok, err := s.policyDB.DefaultOutboundProxy(ctx)
	if err != nil {
		return nil, fmt.Errorf("read default proxy: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return s.openOutboundProxy(saved)
}

func (s *AccountStore) openOutboundProxy(saved storage.OutboundProxy) (*netguard.Proxy, error) {
	proxy := &netguard.Proxy{Type: saved.Type, Host: saved.Host, Port: saved.Port, Username: saved.Username}
	if len(saved.EncryptedPassword) > 0 {
		password, err := s.decryptPassword("", saved.ID, passwordKindProxy, saved.EncryptedPassword)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: read password: %w", saved.Name, err)
		}
		proxy.Password = password
	}
	if err := proxy.Validate(); err != nil {
		return nil, fmt.Errorf("proxy %q: %w", saved.Name, err)
	}
	return proxy, nil
}

// SetAccountProxy records the proxy choice of the owner's account:
// storage.AccountProxyDefault, storage.AccountProxyDirect, or a saved
// proxy's id.
func (s *AccountStore) SetAccountProxy(ctx context.Context, userID, accountID, choice string) error {
	choice = strings.TrimSpace(choice)
	if choice != storage.AccountProxyDefault && choice != storage.AccountProxyDirect {
		if _, err := s.policyDB.GetOutboundProxy(ctx, choice); err != nil {
			return err
		}
	}
	return s.db.SetAccountProxyChoice(ctx, userID, accountID, choice)
}

// AccountProxyForUser is AccountProxy for an account in its owner's own
// database.
func (s *UserAccountStore) AccountProxyForUser(ctx context.Context, owner, accountID string) (*netguard.Proxy, error) {
	var proxy *netguard.Proxy
	err := s.WithAccountForUser(ctx, owner, accountID, func(local *AccountStore, _ *storage.DB) error {
		var err error
		proxy, err = local.AccountProxy(ctx, accountID)
		return err
	})
	return proxy, err
}
