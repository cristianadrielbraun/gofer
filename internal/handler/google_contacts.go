package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type googlePeopleResponse struct {
	Connections   []googlePerson `json:"connections"`
	NextPageToken string         `json:"nextPageToken"`
}

type googleSearchContactsResponse struct {
	Results []googleSearchContactResult `json:"results"`
}

type googleSearchContactResult struct {
	Person googlePerson `json:"person"`
}

type googlePerson struct {
	ResourceName   string               `json:"resourceName,omitempty"`
	Etag           string               `json:"etag,omitempty"`
	Names          []googleName         `json:"names,omitempty"`
	EmailAddresses []googleEmail        `json:"emailAddresses"`
	PhoneNumbers   []googlePhoneNumber  `json:"phoneNumbers,omitempty"`
	Organizations  []googleOrganization `json:"organizations,omitempty"`
	Biographies    []googleBiography    `json:"biographies,omitempty"`
}

type googleName struct {
	DisplayName string `json:"displayName,omitempty"`
	GivenName   string `json:"givenName,omitempty"`
	FamilyName  string `json:"familyName,omitempty"`
}

type googleEmail struct {
	Value string `json:"value,omitempty"`
	Type  string `json:"type,omitempty"`
}

type googlePhoneNumber struct {
	Value string `json:"value,omitempty"`
	Type  string `json:"type,omitempty"`
}

type googleOrganization struct {
	Name  string `json:"name,omitempty"`
	Title string `json:"title,omitempty"`
}

type googleBiography struct {
	Value string `json:"value,omitempty"`
}

type googleAPIError struct {
	Status int
	Body   string
}

func (e googleAPIError) Error() string {
	return fmt.Sprintf("people api returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

type contactSyncAccount struct {
	ID       string
	Email    string
	Provider string
}

func builtinAccountContactSync(provider string) bool {
	switch provider {
	case providers.ProviderGmail, providers.ProviderOutlook:
		return true
	default:
		return false
	}
}

func (h *Handler) accountSupportsContactSync(ctx context.Context, userID string, account contactSyncAccount) bool {
	if builtinAccountContactSync(account.Provider) {
		var enabled int
		err := h.db.Read().QueryRowContext(ctx, `SELECT enabled FROM account_contact_sync_configs WHERE account_id = ? AND user_id = ?`, account.ID, userID).Scan(&enabled)
		return err == sql.ErrNoRows || (err == nil && enabled == 1)
	}
	cfg, err := h.accountStore.GetContactSyncConfig(ctx, userID, account.ID)
	return err == nil && cfg.Enabled && cfg.Provider == providers.ProviderCardDAV
}

func (h *Handler) handleSyncAccountContacts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid sync request.")
		return
	}

	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := strings.TrimSpace(r.FormValue("account_id"))
	accounts, err := h.contactSyncAccounts(ctx, userID, accountID)
	if err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not find contact-sync accounts.")
		return
	}
	if len(accounts) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Connect an account with contact sync before syncing contacts.")
		return
	}

	totalImported := 0
	failures := make([]string, 0)
	for _, account := range accounts {
		imported, err := h.syncContactAccount(ctx, userID, account)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", account.Email, err.Error()))
			continue
		}
		totalImported += imported
	}

	if len(failures) == len(accounts) {
		htmlStatus(w, http.StatusBadGateway, "Contact sync failed: "+strings.Join(failures, "; "))
		return
	}

	_ = h.db.LogContactActivity(ctx, userID, "provider_contacts_synced", "", "Account contacts synced", totalImported)
	if len(failures) > 0 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Contacts partially synced: %d imported or updated. Failed: %s", totalImported, strings.Join(failures, "; ")))
		return
	}
	if len(accounts) == 1 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Contacts synced for %s: %d imported or updated.", accounts[0].Email, totalImported))
		return
	}
	htmlStatus(w, http.StatusOK, fmt.Sprintf("Contacts synced across %d accounts: %d imported or updated.", len(accounts), totalImported))
}

func (h *Handler) handleSyncGmailContacts(w http.ResponseWriter, r *http.Request) {
	h.handleSyncAccountContacts(w, r)
}

func (h *Handler) contactSyncAccounts(ctx context.Context, userID, accountID string) ([]contactSyncAccount, error) {
	if accountID != "" {
		var account contactSyncAccount
		err := h.db.Read().QueryRowContext(ctx, `
		SELECT a.id, a.email_address,
		       CASE WHEN a.provider IN ('gmail', 'outlook') THEN a.provider ELSE COALESCE(acc.provider, '') END AS contact_provider
		FROM accounts a
		LEFT JOIN account_contact_sync_configs acc ON acc.account_id = a.id AND acc.user_id = a.user_id
		WHERE a.id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0`, accountID, userID).Scan(&account.ID, &account.Email, &account.Provider)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !h.accountSupportsContactSync(ctx, userID, account) {
			return nil, nil
		}
		return []contactSyncAccount{account}, nil
	}

	rows, err := h.db.Read().QueryContext(ctx, `
		SELECT a.id, a.email_address,
		       CASE WHEN a.provider IN ('gmail', 'outlook') THEN a.provider ELSE COALESCE(acc.provider, '') END AS contact_provider
		FROM accounts a
		LEFT JOIN account_contact_sync_configs acc ON acc.account_id = a.id AND acc.user_id = a.user_id
		WHERE a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		ORDER BY a.email_address COLLATE NOCASE`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []contactSyncAccount
	for rows.Next() {
		var account contactSyncAccount
		if err := rows.Scan(&account.ID, &account.Email, &account.Provider); err != nil {
			return nil, err
		}
		if h.accountSupportsContactSync(ctx, userID, account) {
			accounts = append(accounts, account)
		}
	}
	return accounts, rows.Err()
}

func (h *Handler) pullContactAccount(ctx context.Context, userID string, account contactSyncAccount) (int, error) {
	switch account.Provider {
	case providers.ProviderGmail:
		if h.auth == nil || !h.auth.HasGoogleOAuth() {
			return 0, fmt.Errorf("Google OAuth is not configured")
		}
		token, err := h.auth.GetOAuthTokenForAccount(ctx, account.ID)
		if err != nil {
			return 0, fmt.Errorf("reconnect Gmail to grant contact access: %w", err)
		}
		return h.syncGooglePeopleConnections(ctx, userID, account.ID, token)
	case providers.ProviderOutlook:
		if h.auth == nil || !h.auth.HasMicrosoftOAuth() {
			return 0, fmt.Errorf("Microsoft OAuth is not configured")
		}
		token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, account.ID)
		if err != nil {
			return 0, fmt.Errorf("reconnect Outlook to grant contact access: %w", err)
		}
		return h.syncOutlookContacts(ctx, userID, account.ID, token)
	case providers.ProviderCardDAV:
		return h.syncCardDAVContacts(ctx, userID, account)
	default:
		return 0, fmt.Errorf("contact sync is not configured for this account")
	}
}

func (h *Handler) syncGooglePeopleConnections(ctx context.Context, userID, accountID, accessToken string) (int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	pageToken := ""
	imported := 0

	for {
		values := url.Values{}
		values.Set("personFields", googleContactPersonFields())
		values.Set("pageSize", "1000")
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://people.googleapis.com/v1/people/me/connections?"+values.Encode(), nil)
		if err != nil {
			return imported, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := client.Do(req)
		if err != nil {
			return imported, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return imported, fmt.Errorf("people api returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var people googlePeopleResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&people)
		resp.Body.Close()
		if decodeErr != nil {
			return imported, decodeErr
		}

		for _, person := range people.Connections {
			contact := googleContactFromPerson(person)
			if strings.TrimSpace(contact.Email) == "" {
				continue
			}
			contactID, _, err := h.db.UpsertSyncedContactFromContact(ctx, userID, accountID, contact)
			if err != nil {
				return imported, err
			}
			if contactID != "" && strings.TrimSpace(person.ResourceName) != "" {
				if err := h.db.UpsertContactSource(ctx, storage.ContactSource{
					ContactID: contactID,
					UserID:    userID,
					Provider:  providers.ProviderGmail,
					AccountID: accountID,
					RemoteID:  person.ResourceName,
					Etag:      person.Etag,
				}); err != nil {
					return imported, err
				}
			}
			imported++
		}

		pageToken = people.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return imported, nil
}

func googlePersonName(person googlePerson) string {
	for _, name := range person.Names {
		if strings.TrimSpace(name.DisplayName) != "" {
			return strings.TrimSpace(name.DisplayName)
		}
		parts := strings.TrimSpace(strings.TrimSpace(name.GivenName) + " " + strings.TrimSpace(name.FamilyName))
		if parts != "" {
			return parts
		}
	}
	return ""
}

func googleContactPersonFields() string {
	return "names,emailAddresses,phoneNumbers,organizations,biographies,metadata"
}

func googleContactFromPerson(person googlePerson) models.Contact {
	contact := models.Contact{Name: googlePersonName(person)}
	for _, email := range normalizedGoogleEmailValues(person.EmailAddresses) {
		if contact.Email == "" {
			contact.Email = email.Value
			contact.EmailLabel = googleContactLabel(email.Type, "primary")
		} else {
			contact.AdditionalEmails = append(contact.AdditionalEmails, email.Value)
			contact.AdditionalEmailLabels = append(contact.AdditionalEmailLabels, googleContactLabel(email.Type, "alternate"))
		}
	}
	for _, phone := range normalizedGooglePhoneValues(person.PhoneNumbers) {
		if contact.Phone == "" {
			contact.Phone = phone.Value
			contact.PhoneLabel = googleContactLabel(phone.Type, "primary")
		} else {
			contact.AdditionalPhones = append(contact.AdditionalPhones, phone.Value)
			contact.AdditionalPhoneLabels = append(contact.AdditionalPhoneLabels, googleContactLabel(phone.Type, "alternate"))
		}
	}
	for _, org := range person.Organizations {
		if value := strings.TrimSpace(org.Name); value != "" && contact.Organization == "" {
			contact.Organization = value
		}
		if value := strings.TrimSpace(org.Title); value != "" && contact.Title == "" {
			contact.Title = value
		}
		if contact.Organization != "" && contact.Title != "" {
			break
		}
	}
	for _, bio := range person.Biographies {
		if value := strings.TrimSpace(bio.Value); value != "" {
			contact.Notes = value
			break
		}
	}
	return contact
}

func normalizedGoogleEmailValues(values []googleEmail) []googleEmail {
	out := make([]googleEmail, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		out = appendGoogleEmailValue(out, value, seen)
	}
	return out
}

func normalizedGooglePhoneValues(values []googlePhoneNumber) []googlePhoneNumber {
	out := make([]googlePhoneNumber, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		out = appendGooglePhoneValue(out, value, seen)
	}
	return out
}

func appendGoogleEmailValue(values []googleEmail, value googleEmail, seen map[string]bool) []googleEmail {
	value.Value = strings.TrimSpace(value.Value)
	value.Type = googleContactLabel(value.Type, "")
	if value.Value == "" {
		return values
	}
	key := strings.ToLower(value.Value)
	if seen[key] {
		return values
	}
	seen[key] = true
	return append(values, value)
}

func appendGooglePhoneValue(values []googlePhoneNumber, value googlePhoneNumber, seen map[string]bool) []googlePhoneNumber {
	value.Value = strings.TrimSpace(value.Value)
	value.Type = googleContactLabel(value.Type, "")
	if value.Value == "" {
		return values
	}
	key := strings.ToLower(value.Value)
	if seen[key] {
		return values
	}
	seen[key] = true
	return append(values, value)
}

func googleContactLabel(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func (h *Handler) syncContactToAccountTargets(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) error {
	if contact.ID == "" || contact.Email == "" {
		return nil
	}

	desired, err := h.contactTargetAccountSet(ctx, userID, contact.SaveTargets)
	if err != nil {
		return err
	}
	previousTargets := map[string]contactSyncAccount{}
	if previous != nil {
		previousTargets, err = h.contactTargetAccountSet(ctx, userID, previous.SaveTargets)
		if err != nil {
			return err
		}
	}

	handledRemoved := make(map[string]bool)
	if err := h.removeUnwantedContactSources(ctx, userID, contact, providers.ProviderGmail, desired, handledRemoved); err != nil {
		return err
	}
	if err := h.removeUnwantedContactSources(ctx, userID, contact, providers.ProviderOutlook, desired, handledRemoved); err != nil {
		return err
	}
	if err := h.removeUnwantedContactSources(ctx, userID, contact, providers.ProviderCardDAV, desired, handledRemoved); err != nil {
		return err
	}
	for accountID := range previousTargets {
		if _, ok := desired[accountID]; ok || handledRemoved[accountID] {
			continue
		}
		switch previousTargets[accountID].Provider {
		case providers.ProviderGmail:
			token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
			if err != nil {
				return err
			}
			if err := h.deleteGoogleContactsByEmail(ctx, token, contact.Email); err != nil {
				return err
			}
		case providers.ProviderOutlook:
			token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, accountID)
			if err != nil {
				return err
			}
			if err := h.deleteOutlookContactsByEmail(ctx, token, contact.Email); err != nil {
				return err
			}
		case providers.ProviderCardDAV:
			if err := h.deleteCardDAVContactsByEmail(ctx, userID, accountID, contact.Email); err != nil {
				return err
			}
		}
	}

	for accountID, account := range desired {
		switch account.Provider {
		case providers.ProviderGmail:
			if err := h.pushContactToGmailAccount(ctx, userID, contact, accountID); err != nil {
				return err
			}
		case providers.ProviderOutlook:
			if err := h.pushContactToOutlookAccount(ctx, userID, contact, accountID); err != nil {
				return err
			}
		case providers.ProviderCardDAV:
			if err := h.pushContactToCardDAVAccount(ctx, userID, contact, accountID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("contact sync is not configured for this account")
		}
	}
	return nil
}

func (h *Handler) scheduleContactAccountSync(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) bool {
	if !h.contactNeedsAccountSync(ctx, userID, contact, previous) {
		return false
	}
	if _, err := h.db.EnqueueContactSyncOperation(ctx, userID, contact, previous); err != nil {
		log.Printf("contacts sync: enqueue %s: %v", contact.ID, err)
		return false
	}
	h.signalContactSyncOperationWorker()
	return true
}

func (h *Handler) scheduleContactGmailSync(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) bool {
	return h.scheduleContactAccountSync(ctx, userID, contact, previous)
}

func (h *Handler) signalContactSyncOperationWorker() {
	if h == nil || h.contactSyncQueue == nil {
		return
	}
	select {
	case h.contactSyncQueue <- struct{}{}:
	default:
	}
}

func (h *Handler) startContactSyncOperationWorker(ctx context.Context) {
	if h == nil || h.contactSyncQueue == nil {
		return
	}
	h.signalContactSyncOperationWorker()
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.contactSyncQueue:
				h.processContactSyncOperations(ctx)
			case <-ticker.C:
				h.processContactSyncOperations(ctx)
			}
		}
	}()
}

func (h *Handler) processContactSyncOperations(ctx context.Context) {
	for {
		ops, err := h.db.ClaimContactSyncOperations(ctx, 5, 5*time.Minute)
		if err != nil {
			log.Printf("contacts sync operations: claim failed: %v", err)
			return
		}
		if len(ops) == 0 {
			return
		}
		for _, op := range ops {
			h.processContactSyncOperation(ctx, op)
		}
	}
}

func (h *Handler) processContactSyncOperation(parent context.Context, op storage.ContactSyncOperation) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()

	contact := op.Payload.Contact
	previous := op.Payload.Previous
	logSyncResult := func(eventType, message string) {
		logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer logCancel()
		_ = h.db.LogContactActivity(logCtx, op.UserID, eventType, contact.Email, message, 1)
	}
	if err := h.syncContactToAccountTargets(ctx, op.UserID, contact, previous); err != nil {
		retry := op.AttemptCount < 3
		if markErr := h.db.MarkContactSyncOperationError(context.Background(), op.ID, err.Error(), retry); markErr != nil {
			log.Printf("contacts sync operation %s: mark error: %v", op.ID, markErr)
		}
		logSyncResult("contact_sync_failed", "Contact sync failed: "+err.Error())
		if retry {
			h.signalContactSyncOperationWorker()
		}
		return
	}
	if err := h.db.MarkContactSyncOperationSuccess(context.Background(), op.ID); err != nil {
		log.Printf("contacts sync operation %s: mark success: %v", op.ID, err)
	}
	logSyncResult("contact_synced", "Contact synced")
}

func (h *Handler) contactNeedsAccountSync(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) bool {
	if contact.ID == "" || contact.Email == "" {
		return false
	}
	currentTargets, err := h.contactTargetAccountSet(ctx, userID, contact.SaveTargets)
	if err == nil && len(currentTargets) > 0 {
		return true
	}
	if previous != nil {
		previousTargets, err := h.contactTargetAccountSet(ctx, userID, previous.SaveTargets)
		if err == nil && len(previousTargets) > 0 {
			return true
		}
	}
	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderGmail)
	if err == nil && len(sources) > 0 {
		return true
	}
	sources, err = h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderOutlook)
	if err == nil && len(sources) > 0 {
		return true
	}
	sources, err = h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderCardDAV)
	return err == nil && len(sources) > 0
}

func (h *Handler) contactTargetAccountSet(ctx context.Context, userID string, targets []string) (map[string]contactSyncAccount, error) {
	out := make(map[string]contactSyncAccount)
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if bookID, ok := strings.CutPrefix(target, "book:"); ok {
			book, err := h.db.GetContactAddressBook(ctx, userID, bookID)
			if err != nil || book.AccountID == "" {
				continue
			}
			accounts, err := h.contactSyncAccounts(ctx, userID, book.AccountID)
			if err != nil {
				return nil, err
			}
			if len(accounts) == 1 {
				out[book.AccountID] = accounts[0]
			}
			continue
		}
		accountID, ok := strings.CutPrefix(target, "account:")
		if !ok || accountID == "" {
			continue
		}
		accounts, err := h.contactSyncAccounts(ctx, userID, accountID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 1 {
			out[accountID] = accounts[0]
		}
	}
	return out, nil
}

func (h *Handler) gmailTargetAccountSet(ctx context.Context, userID string, targets []string) (map[string]bool, error) {
	accounts, err := h.contactTargetAccountSet(ctx, userID, targets)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for accountID, account := range accounts {
		if account.Provider == providers.ProviderGmail {
			out[accountID] = true
		}
	}
	return out, nil
}

func (h *Handler) deleteContactFromAccounts(ctx context.Context, userID string, contact models.Contact) error {
	if contact.ID == "" {
		return nil
	}
	if err := h.deleteContactSourcesFromProvider(ctx, userID, contact, providers.ProviderGmail); err != nil {
		return err
	}
	if err := h.deleteContactSourcesFromProvider(ctx, userID, contact, providers.ProviderOutlook); err != nil {
		return err
	}
	return h.deleteContactSourcesFromProvider(ctx, userID, contact, providers.ProviderCardDAV)
}

func (h *Handler) deleteContactSourcesFromProvider(ctx context.Context, userID string, contact models.Contact, provider string) error {
	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, provider)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if strings.TrimSpace(source.RemoteID) == "" {
			if err := h.db.DeleteContactSource(ctx, userID, contact.ID, provider, source.AccountID); err != nil {
				return err
			}
			continue
		}
		switch provider {
		case providers.ProviderGmail:
			token, err := h.auth.GetOAuthTokenForAccount(ctx, source.AccountID)
			if err != nil {
				return err
			}
			if err := h.deleteGoogleContact(ctx, token, source.RemoteID); err != nil {
				var apiErr googleAPIError
				if !isGoogleNotFound(err, &apiErr) {
					return err
				}
			}
		case providers.ProviderOutlook:
			token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, source.AccountID)
			if err != nil {
				return err
			}
			if err := h.deleteOutlookContact(ctx, token, source.RemoteID); err != nil {
				var apiErr outlookAPIError
				if !isOutlookNotFound(err, &apiErr) {
					return err
				}
			}
		case providers.ProviderCardDAV:
			if err := h.deleteCardDAVContact(ctx, userID, source); err != nil {
				return err
			}
			if err := h.db.DeleteContactSourceByRemoteID(ctx, userID, provider, source.AccountID, source.RemoteID); err != nil {
				return err
			}
			continue
		}
		if err := h.db.DeleteContactSource(ctx, userID, contact.ID, provider, source.AccountID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) deleteContactFromGmail(ctx context.Context, userID string, contact models.Contact) error {
	return h.deleteContactFromAccounts(ctx, userID, contact)
}

func (h *Handler) pushContactToGmailAccount(ctx context.Context, userID string, contact models.Contact, accountID string) error {
	token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	source, err := h.db.GetContactSource(ctx, userID, contact.ID, providers.ProviderGmail, accountID)
	if err != nil {
		return err
	}
	if source == nil || strings.TrimSpace(source.RemoteID) == "" {
		person, err := h.createGoogleContact(ctx, token, contact)
		if err != nil {
			return err
		}
		if strings.TrimSpace(person.ResourceName) == "" {
			return fmt.Errorf("people api did not return a contact resource name")
		}
		return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderGmail, AccountID: accountID, RemoteID: person.ResourceName, Etag: person.Etag})
	}

	etag := strings.TrimSpace(source.Etag)
	if etag == "" {
		person, err := h.getGoogleContact(ctx, token, source.RemoteID)
		if err != nil {
			var apiErr googleAPIError
			if isGoogleNotFound(err, &apiErr) {
				person, err := h.createGoogleContact(ctx, token, contact)
				if err != nil {
					return err
				}
				if strings.TrimSpace(person.ResourceName) == "" {
					return fmt.Errorf("people api did not return a contact resource name")
				}
				return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderGmail, AccountID: accountID, RemoteID: person.ResourceName, Etag: person.Etag})
			}
			return err
		}
		etag = person.Etag
	}

	person, err := h.updateGoogleContact(ctx, token, source.RemoteID, etag, contact)
	if err != nil {
		var apiErr googleAPIError
		if isGoogleNotFound(err, &apiErr) {
			person, err := h.createGoogleContact(ctx, token, contact)
			if err != nil {
				return err
			}
			if strings.TrimSpace(person.ResourceName) == "" {
				return fmt.Errorf("people api did not return a contact resource name")
			}
			return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderGmail, AccountID: accountID, RemoteID: person.ResourceName, Etag: person.Etag})
		}
		if apiErr.Status == http.StatusBadRequest || apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusPreconditionFailed {
			latest, getErr := h.getGoogleContact(ctx, token, source.RemoteID)
			if getErr != nil {
				return err
			}
			person, err = h.updateGoogleContact(ctx, token, source.RemoteID, latest.Etag, contact)
		}
		if err != nil {
			return err
		}
	}
	remoteID := strings.TrimSpace(person.ResourceName)
	if remoteID == "" {
		remoteID = source.RemoteID
	}
	return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderGmail, AccountID: accountID, RemoteID: remoteID, Etag: person.Etag})
}

func (h *Handler) createGoogleContact(ctx context.Context, accessToken string, contact models.Contact) (googlePerson, error) {
	endpoint := "https://people.googleapis.com/v1/people:createContact?personFields=" + url.QueryEscape(googleContactPersonFields())
	return h.writeGoogleContact(ctx, http.MethodPost, endpoint, accessToken, googlePersonFromContact(contact, "", ""))
}

func (h *Handler) updateGoogleContact(ctx context.Context, accessToken, remoteID, etag string, contact models.Contact) (googlePerson, error) {
	updateFields := "names,emailAddresses,phoneNumbers,organizations,biographies"
	endpoint := "https://people.googleapis.com/v1/" + strings.TrimSpace(remoteID) + ":updateContact?updatePersonFields=" + url.QueryEscape(updateFields) + "&personFields=" + url.QueryEscape(googleContactPersonFields())
	return h.writeGoogleContact(ctx, http.MethodPatch, endpoint, accessToken, googlePersonFromContact(contact, remoteID, etag))
}

func (h *Handler) getGoogleContact(ctx context.Context, accessToken, remoteID string) (googlePerson, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://people.googleapis.com/v1/"+strings.TrimSpace(remoteID)+"?personFields="+url.QueryEscape(googleContactPersonFields()), nil)
	if err != nil {
		return googlePerson{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return googlePerson{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return googlePerson{}, googleAPIError{Status: resp.StatusCode, Body: string(body)}
	}
	var person googlePerson
	if err := json.NewDecoder(resp.Body).Decode(&person); err != nil {
		return googlePerson{}, err
	}
	return person, nil
}

func (h *Handler) deleteGoogleContact(ctx context.Context, accessToken, remoteID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://people.googleapis.com/v1/"+strings.TrimSpace(remoteID)+":deleteContact", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return googleAPIError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func (h *Handler) deleteGoogleContactByResourceAndEmail(ctx context.Context, accessToken, remoteID, email string) error {
	if strings.TrimSpace(remoteID) != "" {
		if err := h.deleteGoogleContact(ctx, accessToken, remoteID); err != nil {
			var apiErr googleAPIError
			if !isGoogleNotFound(err, &apiErr) {
				return err
			}
		}
	}
	return h.deleteGoogleContactsByEmail(ctx, accessToken, email)
}

func (h *Handler) deleteGoogleContactsByEmail(ctx context.Context, accessToken, email string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil
	}
	matches, err := h.searchGoogleContactsByEmail(ctx, accessToken, email)
	if err != nil {
		return err
	}
	for _, person := range matches {
		if strings.TrimSpace(person.ResourceName) == "" {
			continue
		}
		if err := h.deleteGoogleContact(ctx, accessToken, person.ResourceName); err != nil {
			var apiErr googleAPIError
			if !isGoogleNotFound(err, &apiErr) {
				return err
			}
		}
	}
	return nil
}

func (h *Handler) searchGoogleContactsByEmail(ctx context.Context, accessToken, email string) ([]googlePerson, error) {
	values := url.Values{}
	values.Set("query", email)
	values.Set("readMask", googleContactPersonFields())
	values.Set("pageSize", "10")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://people.googleapis.com/v1/people:searchContacts?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, googleAPIError{Status: resp.StatusCode, Body: string(body)}
	}
	var result googleSearchContactsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	matches := make([]googlePerson, 0, len(result.Results))
	for _, item := range result.Results {
		for _, candidate := range item.Person.EmailAddresses {
			if strings.EqualFold(strings.TrimSpace(candidate.Value), email) {
				matches = append(matches, item.Person)
				break
			}
		}
	}
	return matches, nil
}

func (h *Handler) writeGoogleContact(ctx context.Context, method, endpoint, accessToken string, person googlePerson) (googlePerson, error) {
	body, err := json.Marshal(person)
	if err != nil {
		return googlePerson{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return googlePerson{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return googlePerson{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return googlePerson{}, googleAPIError{Status: resp.StatusCode, Body: string(body)}
	}
	var out googlePerson
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return googlePerson{}, err
	}
	return out, nil
}

func googlePersonFromContact(contact models.Contact, remoteID, etag string) googlePerson {
	person := googlePerson{ResourceName: strings.TrimSpace(remoteID), Etag: strings.TrimSpace(etag)}
	for _, email := range append([]string{contact.Email}, contact.AdditionalEmails...) {
		i := len(person.EmailAddresses) - 1
		email = strings.TrimSpace(email)
		if email != "" {
			label := contact.EmailLabel
			if i >= 0 && i < len(contact.AdditionalEmailLabels) {
				label = contact.AdditionalEmailLabels[i]
			}
			person.EmailAddresses = append(person.EmailAddresses, googleEmail{Value: email, Type: googleContactLabel(label, "")})
		}
	}
	name := strings.TrimSpace(contact.Name)
	if name != "" && !strings.EqualFold(name, strings.TrimSpace(contact.Email)) {
		person.Names = []googleName{{GivenName: name}}
	}
	for _, phone := range append([]string{contact.Phone}, contact.AdditionalPhones...) {
		i := len(person.PhoneNumbers) - 1
		phone = strings.TrimSpace(phone)
		if phone != "" {
			label := contact.PhoneLabel
			if i >= 0 && i < len(contact.AdditionalPhoneLabels) {
				label = contact.AdditionalPhoneLabels[i]
			}
			person.PhoneNumbers = append(person.PhoneNumbers, googlePhoneNumber{Value: phone, Type: googleContactLabel(label, "")})
		}
	}
	org := googleOrganization{Name: strings.TrimSpace(contact.Organization), Title: strings.TrimSpace(contact.Title)}
	if org.Name != "" || org.Title != "" {
		person.Organizations = []googleOrganization{org}
	}
	if notes := strings.TrimSpace(contact.Notes); notes != "" {
		person.Biographies = []googleBiography{{Value: notes}}
	}
	return person
}

func isGoogleNotFound(err error, apiErr *googleAPIError) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(googleAPIError); ok {
		if apiErr != nil {
			*apiErr = typed
		}
		return typed.Status == http.StatusNotFound || typed.Status == http.StatusGone
	}
	return false
}

func htmlStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status >= 400 {
		w.Header().Set("X-Gofer-Status", "error")
	} else {
		w.Header().Set("X-Gofer-Status", "ok")
	}
	if status >= 400 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<div class="rounded-md border border-border bg-background px-3 py-2 text-xs text-muted-foreground">` + html.EscapeString(message) + `</div>`))
}
