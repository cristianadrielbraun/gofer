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

// ErrOwnOutboundProxiesOff means the administrator doesn't let people save
// proxies for their own accounts.
var ErrOwnOutboundProxiesOff = errors.New("your Gofer administrator doesn't let people add their own proxies")

// OutboundProxyInput is a proxy as someone entered it.
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
	// OwnerUserID is the person saving a proxy for their own accounts, or ""
	// for an administrator's proxy. A person's proxy can't be the default
	// and must have a public address.
	OwnerUserID string
}

// SaveOutboundProxy validates and saves a proxy, and returns its id. Only
// "http" and "socks5" proxies are accepted. A new administrator's proxy is
// for everyone.
func (s *AccountStore) SaveOutboundProxy(ctx context.Context, input OutboundProxyInput, createdBy string) (string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return "", errors.New("proxy name is required")
	}
	if len(name) > 100 {
		return "", errors.New("proxy name can be at most 100 characters")
	}
	owner := strings.TrimSpace(input.OwnerUserID)
	if owner != "" {
		allowed, err := s.policyDB.PeopleMayAddOutboundProxies(ctx)
		if err != nil {
			return "", err
		}
		if !allowed {
			return "", ErrOwnOutboundProxiesOff
		}
	}
	id := strings.TrimSpace(input.ID)
	existing := storage.OutboundProxy{Everyone: true}
	if id != "" {
		var err error
		if existing, err = s.policyDB.GetOutboundProxy(ctx, id); err != nil {
			return "", err
		}
		if existing.OwnerUserID != owner {
			return "", storage.ErrOutboundProxyNotFound
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
	if owner != "" {
		// Checked again on every connection; this tells the person now.
		approved, err := s.policyDB.IsPrivateTargetAllowed(ctx, "proxy", proxy.Host, proxy.Port)
		if err != nil {
			return "", fmt.Errorf("check proxy private target exception: %w", err)
		}
		if !approved {
			private, err := netguard.IsPrivateHost(ctx, proxy.Host)
			if err != nil {
				return "", fmt.Errorf("look up proxy host %s: %w", proxy.Host, err)
			}
			if private {
				return "", fmt.Errorf("proxy %s:%d is on a private network; a proxy for your own accounts needs a public address, or an administrator's Proxy private target exception for that host and port", proxy.Host, proxy.Port)
			}
		}
	}

	row := storage.OutboundProxy{ID: id, Name: name, Type: proxy.Type, Host: proxy.Host, Port: proxy.Port,
		Username: proxy.Username, IsDefault: input.IsDefault && owner == "", OwnerUserID: owner, Everyone: existing.Everyone}
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

// ListOutboundProxies returns the administrator's proxies. Their passwords
// stay encrypted.
func (s *AccountStore) ListOutboundProxies(ctx context.Context) ([]storage.OutboundProxy, error) {
	return s.policyDB.ListOutboundProxies(ctx)
}

// ListOwnOutboundProxies returns the proxies a person saved for their own
// accounts.
func (s *AccountStore) ListOwnOutboundProxies(ctx context.Context, userID string) ([]storage.OutboundProxy, error) {
	return s.policyDB.ListOwnOutboundProxies(ctx, userID)
}

// OutboundProxiesForUser returns the proxies a person's accounts may use.
func (s *AccountStore) OutboundProxiesForUser(ctx context.Context, userID string) ([]storage.OutboundProxy, error) {
	return s.policyDB.OutboundProxiesForUser(ctx, userID)
}

// PeopleMayAddOutboundProxies reports whether people may save proxies for
// their own accounts.
func (s *AccountStore) PeopleMayAddOutboundProxies(ctx context.Context) (bool, error) {
	return s.policyDB.PeopleMayAddOutboundProxies(ctx)
}

// DeleteOutboundProxy removes a proxy of ownerUserID's, or of the
// administrator's when it is "". Accounts that chose it connect as on the
// default afterwards.
func (s *AccountStore) DeleteOutboundProxy(ctx context.Context, ownerUserID, id string) error {
	return s.policyDB.DeleteOutboundProxy(ctx, ownerUserID, id)
}

// AccountProxy returns the proxy the account connects through, or nil for a
// direct connection.
func (s *AccountStore) AccountProxy(ctx context.Context, accountID string) (*netguard.Proxy, error) {
	choice, err := s.db.GetAccountProxyChoice(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("read account proxy: %w", err)
	}
	owner, err := s.db.GetAccountUserID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("read account owner: %w", err)
	}
	return s.ResolveProxyChoice(ctx, owner, choice)
}

// ResolveProxyChoice returns the proxy a choice of owner's means: none for
// storage.AccountProxyDirect, the saved proxy for its id, and otherwise the
// default proxy, or none when there is no default. A proxy that was deleted,
// or that owner may no longer use, falls back to the default, and a default
// owner may not use means none. An empty owner is work for nobody in
// particular, which may use any of the administrator's proxies.
//
// A proxy that can't be read is an error, never a direct connection: an
// account set to use a proxy must not quietly connect without it.
func (s *AccountStore) ResolveProxyChoice(ctx context.Context, owner, choice string) (*netguard.Proxy, error) {
	choice = strings.TrimSpace(choice)
	if choice == storage.AccountProxyDirect {
		return nil, nil
	}
	if choice != storage.AccountProxyDefault {
		saved, err := s.policyDB.GetOutboundProxy(ctx, choice)
		if err != nil && !errors.Is(err, storage.ErrOutboundProxyNotFound) {
			return nil, fmt.Errorf("read proxy: %w", err)
		}
		if err == nil {
			available, err := s.policyDB.OutboundProxyAvailableTo(ctx, saved, owner)
			if err != nil {
				return nil, fmt.Errorf("check proxy: %w", err)
			}
			if available {
				return s.openOutboundProxy(ctx, saved)
			}
		}
	}
	saved, ok, err := s.policyDB.DefaultOutboundProxy(ctx)
	if err != nil {
		return nil, fmt.Errorf("read default proxy: %w", err)
	}
	if !ok {
		return nil, nil
	}
	available, err := s.policyDB.OutboundProxyAvailableTo(ctx, saved, owner)
	if err != nil {
		return nil, fmt.Errorf("check default proxy: %w", err)
	}
	if !available {
		return nil, nil
	}
	return s.openOutboundProxy(ctx, saved)
}

// openOutboundProxy reads a saved proxy to connect through. A person's own
// proxy may only be reached on a private address that an administrator
// approved with a Proxy private target exception.
func (s *AccountStore) openOutboundProxy(ctx context.Context, saved storage.OutboundProxy) (*netguard.Proxy, error) {
	proxy := &netguard.Proxy{Type: saved.Type, Host: saved.Host, Port: saved.Port, Username: saved.Username}
	if saved.OwnerUserID != "" {
		approved, err := s.policyDB.IsPrivateTargetAllowed(ctx, "proxy", saved.Host, saved.Port)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: check private target exception: %w", saved.Name, err)
		}
		proxy.PublicOnly = !approved
	}
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
// storage.AccountProxyDefault, storage.AccountProxyDirect, or the id of a
// proxy the owner may use.
func (s *AccountStore) SetAccountProxy(ctx context.Context, userID, accountID, choice string) error {
	choice = strings.TrimSpace(choice)
	if err := s.checkProxyChoice(ctx, userID, choice); err != nil {
		return err
	}
	return s.db.SetAccountProxyChoice(ctx, userID, accountID, choice)
}

// checkProxyChoice refuses a proxy choice the owner may not make: the id of a
// proxy that doesn't exist or isn't available to them.
func (s *AccountStore) checkProxyChoice(ctx context.Context, owner, choice string) error {
	if choice == storage.AccountProxyDefault || choice == storage.AccountProxyDirect {
		return nil
	}
	saved, err := s.policyDB.GetOutboundProxy(ctx, choice)
	if err != nil {
		return err
	}
	available, err := s.policyDB.OutboundProxyAvailableTo(ctx, saved, owner)
	if err != nil {
		return err
	}
	if !available {
		return storage.ErrOutboundProxyNotFound
	}
	return nil
}

// ProxyChoice returns the proxy a choice of owner's means, refusing one they
// may not make, for work done before their account exists.
func (s *AccountStore) ProxyChoice(ctx context.Context, owner, choice string) (*netguard.Proxy, error) {
	choice = strings.TrimSpace(choice)
	if err := s.checkProxyChoice(ctx, owner, choice); err != nil {
		return nil, err
	}
	return s.ResolveProxyChoice(ctx, owner, choice)
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

// ResolveProxyChoice is AccountStore.ResolveProxyChoice with the central
// saved proxies, for an account that doesn't exist yet.
func (s *UserAccountStore) ResolveProxyChoice(ctx context.Context, owner, choice string) (*netguard.Proxy, error) {
	return s.base.ResolveProxyChoice(ctx, owner, choice)
}

// DefaultOutboundProxyName is the name of the default proxy as it applies to
// owner, or "" when their accounts on the default connect directly.
func (s *AccountStore) DefaultOutboundProxyName(ctx context.Context, owner string) (string, error) {
	saved, ok, err := s.policyDB.DefaultOutboundProxy(ctx)
	if err != nil || !ok {
		return "", err
	}
	available, err := s.policyDB.OutboundProxyAvailableTo(ctx, saved, owner)
	if err != nil || !available {
		return "", err
	}
	return saved.Name, nil
}

// Proxies is the account store of the central saved proxies, for work on
// them that belongs to no one account.
func (s *UserAccountStore) Proxies() *AccountStore {
	return s.base
}
