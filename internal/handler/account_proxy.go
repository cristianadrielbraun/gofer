package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// accountProxyContext marks ctx so connections made with it go through the
// proxy of the owner's account id, or, while an account is being added and
// id is empty, the proxy a new account starts with: the default.
//
// Most connections find their account's proxy on their own, from the
// account's configuration or from the credentials they carry. This is for
// work that signs in with what someone typed into a form.
func (h *Handler) accountProxyContext(ctx context.Context, owner, id string) (context.Context, error) {
	var (
		proxy *netguard.Proxy
		err   error
	)
	switch {
	case id == "" && h.userAccounts != nil:
		proxy, err = h.userAccounts.ResolveProxyChoice(ctx, storage.AccountProxyDefault)
	case id == "" && h.accountStore != nil:
		proxy, err = h.accountStore.ResolveProxyChoice(ctx, storage.AccountProxyDefault)
	case h.userAccounts != nil:
		proxy, err = h.userAccounts.AccountProxyForUser(ctx, owner, id)
	case h.accountStore != nil:
		proxy, err = h.accountStore.AccountProxy(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	return netguard.WithProxy(ctx, proxy), nil
}
