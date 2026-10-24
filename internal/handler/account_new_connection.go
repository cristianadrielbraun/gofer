package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	mailautodiscover "github.com/cristianadrielbraun/gofer/internal/mail/autodiscover"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// accountConnectionTestResultHeader tells the add-account wizard whether every
// service connected, so it creates the account only after a passing test.
const accountConnectionTestResultHeader = "X-Gofer-Connection-Test"

// handleTestNewAccount tests the IMAP and SMTP settings typed into the
// add-account wizard before the account exists. Nothing is saved: the
// password is used for this test only. The servers go through the same
// private-network and unencrypted-connection rules as a saved account.
func (h *Handler) handleTestNewAccount(w http.ResponseWriter, r *http.Request) {
	req, ok := parseTypedAccountTest(w, r)
	if !ok {
		return
	}
	if req.Password == "" {
		http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
		return
	}
	h.writeTypedAccountTest(w, r, req)
}

// handleTestAccountChanges tests the settings typed into the edit-account
// wizard before they are saved. A password left blank keeps the saved one, so
// the test uses it too. Nothing is saved.
func (h *Handler) handleTestAccountChanges(w http.ResponseWriter, r *http.Request) {
	req, ok := parseTypedAccountTest(w, r)
	if !ok {
		return
	}
	password, smtpPassword, found, err := h.savedAccountPasswords(r, r.PathValue("id"))
	switch {
	case err != nil:
		userAccountError(w, r, err)
		return
	case !found:
		http.NotFound(w, r)
		return
	}
	if req.Password == "" {
		req.Password = password
	}
	if req.SmtpUsername != "" && req.SmtpPassword == "" {
		req.SmtpPassword = smtpPassword
	}
	h.writeTypedAccountTest(w, r, req)
}

func parseTypedAccountTest(w http.ResponseWriter, r *http.Request) (models.CreateAccountRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return models.CreateAccountRequest{}, false
	}
	req := accountRequestFromForm(r)
	if req.Provider == providers.ProviderJMAP {
		if !providers.JMAPEnabled() {
			http.Error(w, providers.ErrJMAPDisabled.Error(), http.StatusBadRequest)
			return req, false
		}
		if req.EmailAddress == "" {
			http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
			return req, false
		}
		if req.Username == "" {
			req.Username = req.EmailAddress
		}
		return req, true
	}
	if req.EmailAddress == "" || req.IMAPHost == "" || req.SMTPHost == "" || req.Username == "" {
		http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
		return req, false
	}
	// OAuth mailboxes connect through the provider's authorization flow instead.
	if (req.Provider != "" && req.Provider != "imap") || (req.AuthMethod != "" && req.AuthMethod != "plain") {
		http.Error(w, "this account connection method can't be tested here", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// savedAccountPasswords reads the saved passwords of an account the current
// person owns and that signs in with a password.
func (h *Handler) savedAccountPasswords(r *http.Request, id string) (password, smtpPassword string, found bool, err error) {
	read := func(store *config.AccountStore) error {
		data, err := store.GetEditData(r.Context(), id)
		if err != nil || data == nil || (data.Provider != "imap" && data.Provider != providers.ProviderJMAP) || data.AuthMethod != "plain" {
			return err
		}
		if password, err = store.DecryptPassword(r.Context(), id); err != nil {
			return err
		}
		if data.SmtpUsername != "" {
			if smtpPassword, err = store.DecryptSmtpPassword(r.Context(), id); err != nil {
				return err
			}
		}
		found = true
		return nil
	}
	if h.userAccounts != nil {
		err = h.userAccounts.WithAccountForUser(r.Context(), h.userID(r.Context()), id, func(store *config.AccountStore, _ *storage.DB) error {
			return read(store)
		})
		return password, smtpPassword, found, err
	}
	if account, ownErr := h.ownedAccount(r.Context(), id); ownErr != nil || account == nil {
		return "", "", false, nil
	}
	err = read(h.accountStore)
	return password, smtpPassword, found, err
}

func (h *Handler) writeTypedAccountTest(w http.ResponseWriter, r *http.Request, req models.CreateAccountRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if req.Provider == providers.ProviderJMAP {
		h.writeJMAPAccountTest(ctx, w, r, req)
		return
	}
	cfg := &models.AccountConfig{
		Provider: "imap", AuthMethod: "plain", Username: req.Username, SmtpUsername: req.SmtpUsername,
		IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort,
		SMTPHost: req.SMTPHost, SMTPPort: req.SMTPPort,
	}
	smtpPassword := req.Password
	if req.SmtpUsername != "" {
		smtpPassword = req.SmtpPassword
	}
	results := []models.ConnectionTestResult{
		h.testNewAccountService(ctx, "imap", cfg, req.IMAPTLSMode, req.Password),
		h.testNewAccountService(ctx, "smtp", cfg, req.SMTPTLSMode, smtpPassword),
	}
	writeAccountTestResults(w, r, results)
}

// jmapSessionURLHeader carries the session URL a JMAP connection test found,
// for the wizard to save with the account.
const jmapSessionURLHeader = "X-Gofer-JMAP-Session-URL"

// writeJMAPAccountTest discovers the session URL from the server address
// typed into the wizard, or from the email's domain when it is blank, and
// signs in there. A session URL already known, when editing, is tried as is.
func (h *Handler) writeJMAPAccountTest(ctx context.Context, w http.ResponseWriter, r *http.Request, req models.CreateAccountRequest) {
	result := models.ConnectionTestResult{Service: "jmap"}
	if req.Password == "" {
		http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
		return
	}
	server := strings.TrimSpace(r.FormValue("jmap_server"))
	if server == "" {
		server = req.JMAPSessionURL
	}
	candidates, err := mailautodiscover.JMAPSessionCandidates(ctx, req.EmailAddress, server, nil)
	if err == nil {
		var sessionURL string
		var session *jmap.Session
		sessionURL, session, err = mail.DiscoverJMAPSession(ctx, candidates, req.Username, req.Password, h.jmapEndpointPolicy)
		if err == nil {
			w.Header().Set(jmapSessionURLHeader, sessionURL)
			result.Success = true
			result.Message = fmt.Sprintf("Connected to %s (mail account %s)", sessionURL, session.PrimaryAccount(jmap.CapabilityMail))
			result.Warning = mail.JMAPSendingWarning(session)
		}
	}
	if err != nil {
		result.Message = server
		if result.Message == "" {
			result.Message = req.EmailAddress
		}
		result.Error = err.Error()
	}
	writeAccountTestResults(w, r, []models.ConnectionTestResult{result})
}

// prepareJMAPAccount checks a new JMAP account's form and fills in its session
// URL. The wizard sends the URL its connection test found; without one, as
// when the test was skipped, discovery runs here as it does in the test.
func (h *Handler) prepareJMAPAccount(r *http.Request, req *models.CreateAccountRequest) error {
	if !providers.JMAPEnabled() {
		return providers.ErrJMAPDisabled
	}
	if req.Username == "" {
		req.Username = req.EmailAddress
	}
	if req.EmailAddress == "" || req.Password == "" {
		return errors.New("All required fields must be filled in")
	}
	if strings.TrimSpace(req.JMAPSessionURL) != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	candidates, err := mailautodiscover.JMAPSessionCandidates(ctx, req.EmailAddress, strings.TrimSpace(r.FormValue("jmap_server")), nil)
	if err != nil {
		return err
	}
	sessionURL, _, err := mail.DiscoverJMAPSession(ctx, candidates, req.Username, req.Password, h.jmapEndpointPolicy)
	if err != nil {
		return err
	}
	req.JMAPSessionURL = sessionURL
	return nil
}

// resolveJMAPAccountChanges checks the edited settings of a JMAP account. The
// server field keeps a session URL as entered; a host, or nothing, is looked
// up with the typed password, or the saved one when it was left blank.
func (h *Handler) resolveJMAPAccountChanges(r *http.Request, accountID string, req *models.CreateAccountRequest) error {
	if !providers.JMAPEnabled() {
		return providers.ErrJMAPDisabled
	}
	if req.EmailAddress == "" {
		return errors.New("Email address is required")
	}
	if req.Username == "" {
		req.Username = req.EmailAddress
	}
	server := strings.TrimSpace(req.JMAPSessionURL)
	if strings.Contains(server, "://") {
		return nil
	}
	password := req.Password
	if password == "" {
		saved, _, found, err := h.savedAccountPasswords(r, accountID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("account not found")
		}
		password = saved
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	candidates, err := mailautodiscover.JMAPSessionCandidates(ctx, req.EmailAddress, server, nil)
	if err != nil {
		return err
	}
	sessionURL, _, err := mail.DiscoverJMAPSession(ctx, candidates, req.Username, password, h.jmapEndpointPolicy)
	if err != nil {
		return err
	}
	req.JMAPSessionURL = sessionURL
	return nil
}

// jmapEndpointPolicy reads the mail security exceptions for one JMAP
// endpoint, as they apply when a saved account connects.
func (h *Handler) jmapEndpointPolicy(ctx context.Context, endpoint jmap.Endpoint) (allowPlaintext, allowPrivate bool, err error) {
	if endpoint.Scheme == "http" {
		if allowPlaintext, err = h.db.IsPlaintextTransportAllowed(ctx, "http", endpoint.Host, endpoint.Port); err != nil {
			return false, false, fmt.Errorf("check HTTP security exception: %w", err)
		}
	}
	if allowPrivate, err = h.db.IsPrivateTargetAllowed(ctx, endpoint.Scheme, endpoint.Host, endpoint.Port); err != nil {
		return false, false, fmt.Errorf("check %s private target exception: %w", strings.ToUpper(endpoint.Scheme), err)
	}
	return allowPlaintext, allowPrivate, nil
}

// writeAccountTestResults renders connection test results for the account
// wizards, with a header saying whether every service connected.
func writeAccountTestResults(w http.ResponseWriter, r *http.Request, results []models.ConnectionTestResult) {
	outcome := "ok"
	for _, result := range results {
		if !result.Success {
			outcome = "failed"
		}
	}
	w.Header().Set(accountConnectionTestResultHeader, outcome)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.ConnectionTestResults(results, "", "add").Render(r.Context(), w)
}

func (h *Handler) testNewAccountService(ctx context.Context, protocol string, cfg *models.AccountConfig, tlsMode, password string) models.ConnectionTestResult {
	host, port := cfg.IMAPHost, cfg.IMAPPort
	if protocol == "smtp" {
		host, port = cfg.SMTPHost, cfg.SMTPPort
	}
	result := models.ConnectionTestResult{Service: protocol, Message: fmt.Sprintf("%s:%d (%s)", host, port, tlsMode)}
	err := h.prepareNewAccountTransport(ctx, protocol, cfg, tlsMode)
	if err == nil {
		err = runAccountConnectionTest(ctx, accountConnectionTestRetryDelay, func() error {
			if protocol == "smtp" {
				return smtpclient.TestConnection(ctx, cfg, password)
			}
			return imap.TestConnection(ctx, cfg, password)
		})
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Success = true
	result.Message = "Connection successful"
	return result
}

// prepareNewAccountTransport applies the instance's mail security exceptions
// to one service, exactly as they apply when a saved account connects.
func (h *Handler) prepareNewAccountTransport(ctx context.Context, protocol string, cfg *models.AccountConfig, tlsMode string) error {
	host, port := cfg.IMAPHost, cfg.IMAPPort
	if protocol == "smtp" {
		host, port = cfg.SMTPHost, cfg.SMTPPort
	}
	allowPlaintext, err := h.db.IsPlaintextTransportAllowed(ctx, protocol, host, port)
	if err != nil {
		return fmt.Errorf("check %s security exception: %w", strings.ToUpper(protocol), err)
	}
	mode, err := mailtransport.RequireTLSModeWithPlaintext(protocol, tlsMode, allowPlaintext)
	if err != nil {
		return err
	}
	allowPrivate, err := h.db.IsPrivateTargetAllowed(ctx, protocol, host, port)
	if err != nil {
		return fmt.Errorf("check %s private target exception: %w", strings.ToUpper(protocol), err)
	}
	if protocol == "smtp" {
		cfg.SMTPTLSMode, cfg.SMTPAllowPlaintext, cfg.SMTPAllowPrivate = mode, allowPlaintext, allowPrivate
	} else {
		cfg.IMAPTLSMode, cfg.IMAPAllowPlaintext, cfg.IMAPAllowPrivate = mode, allowPlaintext, allowPrivate
	}
	return nil
}
