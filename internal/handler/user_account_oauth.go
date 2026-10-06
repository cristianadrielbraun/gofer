package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserAccountOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", 400)
		return
	}
	provider := strings.TrimSpace(r.FormValue("provider"))
	if _, err := h.userCredentials.AccountAuthorizationURL(provider, ""); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	owner := h.userID(r.Context())
	form := map[string]string{"provider": provider, "email_address": strings.TrimSpace(r.FormValue("email_address")), "display_name": r.FormValue("display_name"), "flow_action": accountOAuthFlowAction(r.FormValue("flow_action"))}
	if form["flow_action"] == "reconnect" {
		// Existing reconnect forms identify a mailbox by owner/provider/email.
		// Resolve once and bind its opaque ID and subject into central flow state.
		var id string
		err := h.userAccounts.WithUser(r.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
			return db.Read().QueryRowContext(r.Context(), `SELECT id FROM accounts WHERE user_id=? AND provider=? AND email_address=? COLLATE NOCASE AND auth_method='oauth2' AND COALESCE(is_deleting,0)=0`, owner, provider, form["email_address"]).Scan(&id)
		})
		if err != nil {
			userAccountError(w, r, err)
			return
		}
		data, err := h.userEditData(r.Context(), owner, id)
		if err != nil {
			userAccountError(w, r, err)
			return
		}
		form["account_id"], form["provider_account_id"] = id, data.ProviderAccountID
	}
	state, err := h.userCredentials.CreateAccountOAuthFlow(r.Context(), owner, auth.GetSessionToken(r), provider, form)
	if err != nil {
		http.Error(w, "could not start account authorization", 503)
		return
	}
	location, err := h.userCredentials.AccountAuthorizationURL(provider, state)
	if err != nil {
		http.Error(w, "mailbox OAuth provider is not configured", 400)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (h *Handler) handleUserAccountOAuthCallback(w http.ResponseWriter, r *http.Request, provider string) {
	owner := h.userID(r.Context())
	flow, err := h.userCredentials.ConsumeAccountOAuthFlow(r.Context(), r.URL.Query().Get("state"), owner, auth.GetSessionToken(r), provider)
	if err != nil {
		code := "oauth_invalid_state"
		if errors.Is(err, mailauth.ErrAccountOAuthFlowExpired) {
			code = "oauth_expired_state"
		}
		if errors.Is(err, mailauth.ErrAccountOAuthFlowUserMismatch) || errors.Is(err, mailauth.ErrAccountOAuthFlowSessionMismatch) || errors.Is(err, mailauth.ErrAccountOAuthFlowProviderMismatch) {
			code = "oauth_session_mismatch"
		}
		http.Redirect(w, r, "/settings/accounts?error="+code, http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Redirect(w, r, "/settings/accounts?error=oauth_no_code", http.StatusSeeOther)
		return
	}
	failure := "create_failed"
	err = h.userCredentials.WithMailboxSetup(r.Context(), owner, func(ctx context.Context) error {
		complete := func() error {
			failure = "oauth_exchange_failed"
			req, token, err := h.userCredentials.ExchangeMailboxCode(ctx, provider, code)
			if err != nil {
				return err
			}
			failure = "oauth_email_mismatch"
			if email := strings.TrimSpace(flow.FormData["email_address"]); email != "" && !strings.EqualFold(email, strings.TrimSpace(req.EmailAddress)) {
				return errors.New("authorized mailbox email does not match requested email")
			}
			id := flow.FormData["account_id"]
			failure = "oauth_identity_mismatch"
			if id != "" && req.ProviderAccountID != flow.FormData["provider_account_id"] {
				return errors.New("authorized provider identity does not match reconnect mailbox")
			}
			if name := strings.TrimSpace(flow.FormData["display_name"]); name != "" {
				req.DisplayName = name
			}
			failure = "create_failed"
			if id == "" {
				id, err = h.userAccounts.FindOAuthAccount(ctx, owner, provider, req.ProviderAccountID, req.EmailAddress)
				if err != nil {
					return err
				}
			}
			created := id == ""
			if created {
				account, err := h.userAccounts.CreateAccount(ctx, owner, req)
				if err != nil {
					var pending *config.PendingAccountCreationError
					if errors.As(err, &pending) {
						w.Header().Set("X-Gofer-Account-ID", pending.AccountID)
					}
					return err
				}
				id = account.ID
			}
			w.Header().Set("X-Gofer-Account-ID", id)
			return h.userStorage.WithAccountActivityForUser(ctx, owner, id, func() error {
				// Missing credentials fail closed in provider workers. If this save
				// fails, the persisted mailbox is recoverable by authorizing the same
				// identity again; neither success nor worker startup is reported.
				failure = "oauth_store_failed"
				var expires *time.Time
				if !token.Expiry.IsZero() {
					expires = &token.Expiry
				}
				scopes, _ := token.Extra("scope").(string)
				oauthProvider := providers.OAuthGoogle
				if provider == providers.ProviderOutlook {
					oauthProvider = providers.OAuthMicrosoft
				}
				if err := h.userCredentials.UpsertForUser(ctx, owner, id, oauthProvider, req.ProviderAccountID, token.AccessToken, token.RefreshToken, token.TokenType, expires, scopes); err != nil {
					return err
				}
				failure = "oauth_metadata_failed"
				if err := h.userAccounts.UpdateOAuthMetadata(ctx, owner, id, req); err != nil {
					return err
				}
				failure = "oauth_sync_failed"
				if created {
					return h.userAccountHooks.Created(ctx, id)
				}
				return h.userAccountHooks.Updated(ctx, id)
			})
		}
		if id := flow.FormData["account_id"]; id != "" {
			// Deletion drains the entire reconnect, but provider HTTP holds no
			// user DB lease and another owner can use a one-slot cache meanwhile.
			return h.userStorage.WithAccountActivityForUser(ctx, owner, id, complete)
		}
		return complete()
	})
	if err != nil {
		log.Printf("routed mailbox OAuth callback: %v", err)
		http.Redirect(w, r, "/settings/accounts?error="+failure, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, accountOAuthSuccessRedirect(flow.FormData), http.StatusSeeOther)
}
