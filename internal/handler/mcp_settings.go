package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Settings > Advanced > AI access: the tokens AI clients sign in to the MCP
// server with.

func (h *Handler) registerMCPSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings/mcp", h.handleMCPSettingsPanel)
	mux.HandleFunc("POST /api/settings/mcp/tokens", h.handleCreateMCPToken)
	mux.HandleFunc("POST /api/settings/mcp/tokens/{id}/revoke", h.handleRevokeMCPToken)
	mux.HandleFunc("POST /api/settings/mcp/grants/{id}/revoke", h.handleRevokeMCPGrant)
}

func (h *Handler) mcpSettingsData(r *http.Request) (views.MCPSettingsData, error) {
	data := views.MCPSettingsData{Enabled: h.mcpEnabled(), Scopes: MCPScopes}
	if !data.Enabled {
		return data, nil
	}
	data.Endpoint = strings.TrimRight(h.auth.Config().BaseURL, "/") + httpguard.MCPPath
	userID := h.userID(r.Context())
	mailboxes, err := h.mcpMailboxChoices(r.Context(), userID)
	if err != nil {
		return data, err
	}
	data.Mailboxes = mailboxes
	tokens, err := h.db.ListMCPTokens(r.Context(), userID)
	if err != nil {
		return data, err
	}
	now := time.Now()
	for _, t := range tokens {
		// Tokens given to OAuth apps are shown by app (Grants).
		if t.Kind != "personal" {
			continue
		}
		data.Tokens = append(data.Tokens, views.MCPTokenRow{
			ID: t.ID, Name: t.Name, Scopes: t.Scopes, Mailboxes: h.mcpMailboxLabels(r.Context(), userID, t.Mailboxes), Kind: t.Kind,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
			Revoked: t.RevokedAt != nil, Expired: t.RevokedAt == nil && !t.Active(now),
		})
	}
	grants, err := h.db.ListOAuthGrants(r.Context(), h.userID(r.Context()))
	if err != nil {
		return data, err
	}
	for _, g := range grants {
		data.Grants = append(data.Grants, views.MCPGrantRow{ID: g.ID, ClientName: g.ClientName, Scopes: g.Scopes, Mailboxes: h.mcpMailboxLabels(r.Context(), userID, g.Mailboxes), CreatedAt: g.CreatedAt, LastUsedAt: g.LastUsedAt})
	}
	return data, nil
}

func (h *Handler) handleRevokeMCPGrant(w http.ResponseWriter, r *http.Request) {
	if !h.mcpEnabled() {
		http.NotFound(w, r)
		return
	}
	err := h.db.RevokeOAuthGrant(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, loadErr := h.mcpSettingsData(r)
	if err != nil || loadErr != nil {
		log.Printf("mcp settings: revoke app: %v %v", err, loadErr)
		data.Error = i18n.T(r.Context(), "settings.mcp.revoke_failed")
	}
	h.renderMCPSettings(w, r, data)
}

func (h *Handler) renderMCPSettings(w http.ResponseWriter, r *http.Request, data views.MCPSettingsData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.MCPSettingsPanel(data).Render(r.Context(), w)
}

func (h *Handler) handleMCPSettingsPanel(w http.ResponseWriter, r *http.Request) {
	data, err := h.mcpSettingsData(r)
	if err != nil {
		log.Printf("mcp settings: %v", err)
		data.Error = i18n.T(r.Context(), "settings.mcp.load_failed")
	}
	h.renderMCPSettings(w, r, data)
}

func (h *Handler) handleCreateMCPToken(w http.ResponseWriter, r *http.Request) {
	if !h.mcpEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	scopes := validMCPScopes(r.Form["scope"])
	name := strings.TrimSpace(r.FormValue("name"))
	var created, failure string
	switch {
	case name == "":
		failure = i18n.T(r.Context(), "settings.mcp.name_required")
	case len(name) > 80:
		failure = i18n.T(r.Context(), "settings.mcp.name_too_long")
	case len(scopes) == 0:
		failure = i18n.T(r.Context(), "settings.mcp.scope_required")
	default:
		var expires *time.Time
		if days, err := strconv.Atoi(r.FormValue("expires_days")); err == nil && days > 0 && days <= 3650 {
			at := time.Now().UTC().AddDate(0, 0, days)
			expires = &at
		}
		mailboxes, mailboxErr := h.mcpChosenMailboxes(r.Context(), h.userID(r.Context()), mcpFormAllMailboxes(r.Form), r.Form["mailbox"])
		if mailboxErr != nil {
			failure = i18n.T(r.Context(), "settings.mcp.mailbox_required")
			break
		}
		_, secret, err := h.db.CreateMCPToken(r.Context(), storage.MCPTokenInput{UserID: h.userID(r.Context()), Name: name, Scopes: scopes, Mailboxes: mailboxes, ExpiresAt: expires})
		if err != nil {
			log.Printf("mcp settings: create token: %v", err)
			failure = i18n.T(r.Context(), "settings.mcp.create_failed")
		} else {
			created = secret
		}
	}
	data, err := h.mcpSettingsData(r)
	if err != nil {
		log.Printf("mcp settings: %v", err)
	}
	data.NewToken, data.Error = created, failure
	h.renderMCPSettings(w, r, data)
}

func (h *Handler) handleRevokeMCPToken(w http.ResponseWriter, r *http.Request) {
	if !h.mcpEnabled() {
		http.NotFound(w, r)
		return
	}
	err := h.db.RevokeMCPToken(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, loadErr := h.mcpSettingsData(r)
	if err != nil || loadErr != nil {
		log.Printf("mcp settings: revoke: %v %v", err, loadErr)
		data.Error = i18n.T(r.Context(), "settings.mcp.revoke_failed")
	}
	h.renderMCPSettings(w, r, data)
}
