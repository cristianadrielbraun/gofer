package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// loadAccountIdentitiesData gathers what the identities dialog of an account
// shows, or writes the error response and returns false.
func (h *Handler) loadAccountIdentitiesData(w http.ResponseWriter, r *http.Request) (models.AccountIdentitiesData, bool) {
	ctx := r.Context()
	accountID := r.PathValue("id")
	account, err := h.ownedAccount(ctx, accountID)
	if err != nil {
		log.Printf("identities account=%s: %v", accountID, err)
		http.Error(w, "failed to load account", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	if account == nil {
		http.NotFound(w, r)
		return models.AccountIdentitiesData{}, false
	}
	userID := h.userID(ctx)
	identities, err := h.db.ListAccountIdentities(ctx, userID, accountID)
	if err != nil {
		http.Error(w, "failed to load identities", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	signatures, err := h.db.ListSignatures(ctx, userID)
	if err != nil {
		http.Error(w, "failed to load signatures", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	domains, err := h.db.CatchAllIdentityDomains(ctx, userID, accountID)
	if err != nil {
		http.Error(w, "failed to load identities", http.StatusInternalServerError)
		return models.AccountIdentitiesData{}, false
	}
	data := models.AccountIdentitiesData{
		Account: *account, Identities: identities, Signatures: signatures,
		ProviderListsIdentities: mail.ProviderListsIdentities(account.Provider),
		CatchAllDomains:         domains,
		AccountSendsAnyAddress:  account.Provider != providers.ProviderGmail && (account.Provider != providers.ProviderJMAP || len(domains) > 0),
		// Gofer could keep no copy in an Outlook account's Sent Items.
		CanUseOwnSMTP: account.Provider != providers.ProviderOutlook,
		Unusable:      map[string]bool{},
	}
	data.CanAddIdentities = data.AccountSendsAnyAddress || data.CanUseOwnSMTP
	for _, identity := range identities {
		if identity.Source == models.IdentitySourceManual && !identity.HasOwnSMTP() && !storage.IdentityAddressAllowed(account.Provider, domains, identity.Email) {
			data.Unusable[identity.ID] = true
		}
	}
	return data, true
}

// handleManageAccountIdentities renders the identities dialog of an account.
func (h *Handler) handleManageAccountIdentities(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html")
	views.AccountIdentitiesDialog(data).Render(r.Context(), w)
}

// handleAccountIdentitiesPanel renders the inside of the identities dialog,
// with the identity named by edit in the form, or an empty form.
func (h *Handler) handleAccountIdentitiesPanel(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	if id := strings.TrimSpace(r.URL.Query().Get("edit")); id != "" {
		index := slices.IndexFunc(data.Identities, func(identity models.Identity) bool { return identity.ID == id })
		if index < 0 {
			http.NotFound(w, r)
			return
		}
		data.Editing = data.Identities[index]
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

func (h *Handler) renderAccountIdentitiesPanel(w http.ResponseWriter, r *http.Request, data models.AccountIdentitiesData) {
	w.Header().Set("Content-Type", "text/html")
	views.AccountIdentitiesPanel(data).Render(r.Context(), w)
}

// handleSaveAccountIdentity adds an identity, or changes the one named by id.
// A refused change shows its reason with the form as it was sent.
func (h *Handler) handleSaveAccountIdentity(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	identity, password, formErr := identityFromForm(r)
	userID := h.userID(r.Context())
	var current models.Identity
	if identity.ID != "" {
		// Provider identities only take a signature; the form shows the rest
		// read-only and does not send it.
		var err error
		current, err = h.db.GetAccountIdentity(r.Context(), userID, identity.AccountID, identity.ID)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "failed to load identity", http.StatusInternalServerError)
			return
		}
		if current.Source == models.IdentitySourceProvider {
			identity.Name, identity.Email, identity.ReplyTo = current.Name, current.Email, current.ReplyTo
		}
	}
	saveErr := formErr
	if saveErr == nil && identity.HasOwnSMTP() && password == "" && !keepsSMTPPassword(current, identity) {
		saveErr = errIdentitySMTPPasswordMissing
	}
	if saveErr == nil {
		var saved models.Identity
		saved, saveErr = h.db.SaveAccountIdentityGuarded(r.Context(), userID, identity, h.signatureGuard)
		if saveErr == nil && saved.HasOwnSMTP() && password != "" {
			saveErr = h.accountStore.SetIdentitySMTPPassword(r.Context(), userID, saved.AccountID, saved.ID, password, h.signatureGuard)
		}
	}
	if errors.Is(saveErr, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	if saveErr != nil {
		if errors.Is(saveErr, storage.ErrProviderIdentity) {
			data.Error = "Only the signature of an identity from the provider can be changed here."
		} else if errors.Is(saveErr, storage.ErrIdentityAddressNotAllowed) {
			data.Error = "The provider does not let this account send as " + strings.TrimSpace(identity.Email) + ". Send it through another SMTP server instead."
		} else if errors.Is(saveErr, storage.ErrIdentitySMTPNotSupported) {
			data.Error = "This identity can't send through its own server."
		} else {
			data.Error = saveErr.Error()
		}
		data.Editing = identity
		if identity.ID != "" {
			if index := slices.IndexFunc(data.Identities, func(i models.Identity) bool { return i.ID == identity.ID }); index >= 0 {
				data.Editing.Source = data.Identities[index].Source
			}
		}
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

// handleDeleteAccountIdentity removes a manual identity.
func (h *Handler) handleDeleteAccountIdentity(w http.ResponseWriter, r *http.Request) {
	err := h.db.DeleteAccountIdentityGuarded(r.Context(), h.userID(r.Context()), r.PathValue("id"), r.PathValue("identity"), h.signatureGuard)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	if errors.Is(err, storage.ErrProviderIdentity) {
		data.Error = "An identity from the provider can only be removed at the provider."
	} else if err != nil {
		log.Printf("delete identity account=%s: %v", r.PathValue("id"), err)
		data.Error = "The identity could not be deleted."
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

// handleMoveAccountIdentity moves an identity one place up or down.
func (h *Handler) handleMoveAccountIdentity(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	ids := make([]string, len(data.Identities))
	for i, identity := range data.Identities {
		ids[i] = identity.ID
	}
	from := slices.Index(ids, r.PathValue("identity"))
	if from < 0 {
		http.NotFound(w, r)
		return
	}
	to := from - 1
	if r.FormValue("direction") == "down" {
		to = from + 1
	}
	if to >= 0 && to < len(ids) {
		ids[from], ids[to] = ids[to], ids[from]
		if err := h.db.SetAccountIdentityOrderGuarded(r.Context(), h.userID(r.Context()), data.Account.ID, ids, h.signatureGuard); err != nil {
			log.Printf("order identities account=%s: %v", data.Account.ID, err)
			data.Error = "The identities could not be reordered."
		} else if data, ok = h.loadAccountIdentitiesData(w, r); !ok {
			return
		}
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}

// handleRefreshAccountIdentities asks the account's provider again for the
// addresses it may send from, then shows the dialog's list.
func (h *Handler) handleRefreshAccountIdentities(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if !h.requireOwnedAccount(w, r, accountID) {
		return
	}
	var err error
	if h.syncer == nil {
		err = errors.New("mail sync is not running")
	} else {
		err = h.syncer.RefreshProviderIdentities(r.Context(), accountID)
	}
	h.refreshedAccountIdentitiesPanel(err)(w, r)
}

// handleUserRefreshAccountIdentities is handleRefreshAccountIdentities for an
// owner whose mail lives in their own store.
func (h *Handler) handleUserRefreshAccountIdentities(w http.ResponseWriter, r *http.Request) {
	err := h.userIMAP.RefreshProviderIdentities(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	h.userSignatureHandler(func(h *Handler) http.HandlerFunc { return h.refreshedAccountIdentitiesPanel(err) })(w, r)
}

// refreshedAccountIdentitiesPanel shows the identities after asking the
// provider, with why that failed if it did.
func (h *Handler) refreshedAccountIdentitiesPanel(refreshErr error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, ok := h.loadAccountIdentitiesData(w, r)
		if !ok {
			return
		}
		switch {
		case refreshErr == nil:
		case errors.Is(refreshErr, mail.ErrProviderIdentitiesUnsupported):
			data.RefreshError = "This provider does not list the addresses it can send from."
		default:
			log.Printf("refresh identities account=%s: %v", data.Account.ID, refreshErr)
			data.RefreshError = "The provider could not be asked for its addresses. Try again later."
		}
		h.renderAccountIdentitiesPanel(w, r, data)
	}
}

var errIdentitySMTPPasswordMissing = errors.New("enter the password for the SMTP server")

// identityFromForm reads an identity and its SMTP server's password from the
// identities form. The server's settings count only when the form sends
// through another server.
func identityFromForm(r *http.Request) (models.Identity, string, error) {
	identity := models.Identity{
		ID:          strings.TrimSpace(r.FormValue("id")),
		AccountID:   r.PathValue("id"),
		Name:        r.FormValue("name"),
		Email:       r.FormValue("email"),
		ReplyTo:     r.FormValue("reply_to"),
		SignatureID: r.FormValue("signature_id"),
	}
	if r.FormValue("send_through") != "smtp" {
		return identity, "", nil
	}
	identity.SMTPHost = r.FormValue("smtp_host")
	identity.SMTPTLSMode = r.FormValue("smtp_tls_mode")
	identity.SMTPUsername = r.FormValue("smtp_username")
	var err error
	if text := strings.TrimSpace(r.FormValue("smtp_port")); text != "" {
		if identity.SMTPPort, err = strconv.Atoi(text); err != nil {
			err = errors.New("the SMTP port must be a number")
		}
	}
	if err == nil && strings.TrimSpace(identity.SMTPHost) == "" {
		err = errors.New("enter the SMTP server")
	}
	return identity, r.FormValue("smtp_password"), err
}

// keepsSMTPPassword reports whether saving identity keeps the password stored
// for current: it has one, for the same server and login.
func keepsSMTPPassword(current, identity models.Identity) bool {
	return current.HasSMTPPassword && strings.EqualFold(strings.TrimSpace(current.SMTPHost), strings.TrimSpace(identity.SMTPHost)) &&
		strings.TrimSpace(current.SMTPUsername) == strings.TrimSpace(identity.SMTPUsername)
}

// identitySMTPTest is a connection test of the SMTP server in the
// identities form, prepared from the account's store.
type identitySMTPTest struct {
	cfg      *models.AccountConfig
	password string
}

// prepareIdentitySMTPTest reads the server to test from the form. An empty
// password field tests with the one stored for the identity, as long as the
// server and login are unchanged.
func prepareIdentitySMTPTest(r *http.Request, accounts *config.AccountStore, db *storage.DB, userID string) (identitySMTPTest, error) {
	identity, password, err := identityFromForm(r)
	if err == nil {
		err = storage.NormalizeIdentitySMTP(&identity)
	}
	if err != nil {
		return identitySMTPTest{}, err
	}
	if !identity.HasOwnSMTP() {
		return identitySMTPTest{}, errors.New("enter the SMTP server")
	}
	if identity.Email == "" {
		identity.Email = identity.SMTPUsername
	}
	if password == "" && identity.ID != "" {
		current, err := db.GetAccountIdentity(r.Context(), userID, identity.AccountID, identity.ID)
		if err != nil {
			return identitySMTPTest{}, err
		}
		if keepsSMTPPassword(current, identity) {
			if password, err = accounts.DecryptIdentitySMTPPassword(r.Context(), identity.AccountID, identity.ID); err != nil {
				return identitySMTPTest{}, err
			}
		}
	}
	if password == "" {
		return identitySMTPTest{}, errIdentitySMTPPasswordMissing
	}
	cfg, err := accounts.IdentitySMTPConfig(r.Context(), identity)
	if err != nil {
		return identitySMTPTest{}, err
	}
	return identitySMTPTest{cfg: cfg, password: password}, nil
}

// run connects and logs in to the server without sending anything.
func (t identitySMTPTest) run(ctx context.Context) models.ConnectionTestResult {
	result := models.ConnectionTestResult{Service: "smtp"}
	err := runAccountConnectionTest(ctx, accountConnectionTestRetryDelay, func() error {
		return smtpclient.TestConnection(ctx, t.cfg, t.password)
	})
	if err != nil {
		result.Error = sanitizeOutgoingErrorText(err.Error())
	} else {
		result.Success, result.Message = true, "Connected and logged in to "+t.cfg.SMTPHost+"."
	}
	return result
}

func renderIdentitySMTPTest(w http.ResponseWriter, r *http.Request, result models.ConnectionTestResult) {
	w.Header().Set("Content-Type", "text/html")
	views.AccountIdentitySMTPTestResult(result).Render(r.Context(), w)
}

// handleTestIdentitySMTP tests the SMTP server in an account's identities
// form.
func (h *Handler) handleTestIdentitySMTP(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAccount(w, r, r.PathValue("id")) {
		return
	}
	test, err := prepareIdentitySMTPTest(r, h.accountStore, h.db, h.userID(r.Context()))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		renderIdentitySMTPTest(w, r, models.ConnectionTestResult{Service: "smtp", Error: err.Error()})
		return
	}
	renderIdentitySMTPTest(w, r, test.run(r.Context()))
}

// handleUserTestIdentitySMTP is handleTestIdentitySMTP for an owner whose
// mail lives in their own store. The server is contacted after the store is
// released.
func (h *Handler) handleUserTestIdentitySMTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	owner := h.userID(r.Context())
	var test identitySMTPTest
	var prepareErr error
	err := h.userAccounts.WithAccountForUser(r.Context(), owner, r.PathValue("id"), func(accounts *config.AccountStore, db *storage.DB) error {
		test, prepareErr = prepareIdentitySMTPTest(r, accounts, db, owner)
		if errors.Is(prepareErr, sql.ErrNoRows) {
			return prepareErr
		}
		return nil
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if prepareErr != nil {
		renderIdentitySMTPTest(w, r, models.ConnectionTestResult{Service: "smtp", Error: prepareErr.Error()})
		return
	}
	renderIdentitySMTPTest(w, r, test.run(r.Context()))
}
