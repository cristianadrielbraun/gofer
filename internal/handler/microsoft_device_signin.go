package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/views"
	"golang.org/x/oauth2"
)

// microsoftDeviceSignInPath shows the code to enter at Microsoft (GET) and
// reports whether the user finished (POST).
const microsoftDeviceSignInPath = "/auth/microsoft/mailbox/device"

func microsoftDeviceSignInLocation(state string) string {
	return microsoftDeviceSignInPath + "?state=" + url.QueryEscape(state)
}

type microsoftDeviceSignIns interface {
	MicrosoftDeviceSignIn(ctx context.Context, state, owner, session string) (*mailauth.MicrosoftDeviceSignIn, error)
	PollMicrosoftDeviceSignIn(ctx context.Context, state, owner, session string) (*oauth2.Token, error)
	ConsumeAccountOAuthFlow(ctx context.Context, state, owner, session, provider string) (*mailauth.AccountOAuthFlow, error)
}

func (h *Handler) handleMicrosoftDeviceSignInPage(w http.ResponseWriter, r *http.Request) {
	if h.mailCredentials() == nil || !h.mailCredentials().UsesMicrosoftDeviceCode() {
		http.NotFound(w, r)
		return
	}
	h.renderMicrosoftDeviceSignIn(w, r, h.mailCredentials(), h.userID(r.Context()))
}

func (h *Handler) handleMicrosoftDeviceSignInPoll(w http.ResponseWriter, r *http.Request) {
	if h.mailCredentials() == nil || !h.mailCredentials().UsesMicrosoftDeviceCode() {
		http.NotFound(w, r)
		return
	}
	h.pollMicrosoftDeviceSignIn(w, r, h.mailCredentials(), h.userID(r.Context()), func(ctx context.Context, flow *mailauth.AccountOAuthFlow, token *oauth2.Token) string {
		return h.finishMicrosoftAccount(ctx, flow, token)
	})
}

func (h *Handler) handleUserMicrosoftDeviceSignInPage(w http.ResponseWriter, r *http.Request) {
	if !h.userCredentials.UsesMicrosoftDeviceCode() {
		http.NotFound(w, r)
		return
	}
	h.renderMicrosoftDeviceSignIn(w, r, h.userCredentials, h.userID(r.Context()))
}

func (h *Handler) handleUserMicrosoftDeviceSignInPoll(w http.ResponseWriter, r *http.Request) {
	if !h.userCredentials.UsesMicrosoftDeviceCode() {
		http.NotFound(w, r)
		return
	}
	owner := h.userID(r.Context())
	h.pollMicrosoftDeviceSignIn(w, r, h.userCredentials, owner, func(ctx context.Context, flow *mailauth.AccountOAuthFlow, token *oauth2.Token) string {
		return h.finishUserAccountOAuth(w, ctx, owner, providers.ProviderOutlook, flow, func(ctx context.Context) (*models.CreateAccountRequest, *oauth2.Token, error) {
			return h.userCredentials.MailboxGrant(ctx, providers.ProviderOutlook, token)
		})
	})
}

func (h *Handler) renderMicrosoftDeviceSignIn(w http.ResponseWriter, r *http.Request, signIns microsoftDeviceSignIns, owner string) {
	state := r.URL.Query().Get("state")
	signIn, err := signIns.MicrosoftDeviceSignIn(r.Context(), state, owner, auth.GetSessionToken(r))
	if err != nil {
		http.Redirect(w, r, "/settings/accounts?error="+microsoftDeviceErrorCode(err), http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.MicrosoftDeviceSignInPage(views.MicrosoftDeviceSignInData{
		UserCode:        signIn.UserCode,
		VerificationURI: signIn.VerificationURI,
		State:           state,
		PollPath:        microsoftDeviceSignInPath,
		PollCSRF:        auth.CSRFToken(r.Context(), http.MethodPost, microsoftDeviceSignInPath),
		PollInterval:    signIn.Interval,
		ExpiresAt:       signIn.ExpiresAt,
	}).Render(r.Context(), w); err != nil {
		log.Printf("render microsoft device sign-in: %v", err)
	}
}

// pollMicrosoftDeviceSignIn answers {"status":"pending"} until the sign-in
// ends, then {"status":"done","redirect":...} with where the browser goes next.
func (h *Handler) pollMicrosoftDeviceSignIn(w http.ResponseWriter, r *http.Request, signIns microsoftDeviceSignIns, owner string, finish func(context.Context, *mailauth.AccountOAuthFlow, *oauth2.Token) string) {
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid form data")
		return
	}
	state := r.PostFormValue("state")
	session := auth.GetSessionToken(r)
	respond := func(status, redirect string) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "redirect": redirect})
	}
	token, err := signIns.PollMicrosoftDeviceSignIn(r.Context(), state, owner, session)
	if errors.Is(err, mailauth.ErrMicrosoftDevicePending) {
		respond("pending", "")
		return
	}
	if err != nil {
		log.Printf("microsoft device sign-in: %v", err)
		respond("done", "/settings/accounts?error="+microsoftDeviceErrorCode(err))
		return
	}
	flow, err := signIns.ConsumeAccountOAuthFlow(r.Context(), state, owner, session, providers.ProviderOutlook)
	if err != nil {
		log.Printf("microsoft device sign-in: consume account oauth flow: %v", err)
		respond("done", "/settings/accounts?error="+microsoftDeviceErrorCode(err))
		return
	}
	respond("done", finish(r.Context(), flow, token))
}

func microsoftDeviceErrorCode(err error) string {
	switch {
	case errors.Is(err, mailauth.ErrAccountOAuthFlowNotFound):
		return "oauth_invalid_state"
	case errors.Is(err, mailauth.ErrAccountOAuthFlowExpired), errors.Is(err, mailauth.ErrMicrosoftDeviceExpired):
		return "oauth_expired_state"
	case errors.Is(err, mailauth.ErrAccountOAuthFlowUserMismatch),
		errors.Is(err, mailauth.ErrAccountOAuthFlowSessionMismatch),
		errors.Is(err, mailauth.ErrAccountOAuthFlowProviderMismatch):
		return "oauth_session_mismatch"
	case errors.Is(err, mailauth.ErrMicrosoftDeviceDeclined):
		return "oauth_device_declined"
	default:
		return "oauth_exchange_failed"
	}
}
