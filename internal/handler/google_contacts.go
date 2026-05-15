package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
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
	ResourceName   string        `json:"resourceName,omitempty"`
	Etag           string        `json:"etag,omitempty"`
	Names          []googleName  `json:"names,omitempty"`
	EmailAddresses []googleEmail `json:"emailAddresses"`
}

type googleName struct {
	DisplayName string `json:"displayName,omitempty"`
	GivenName   string `json:"givenName,omitempty"`
	FamilyName  string `json:"familyName,omitempty"`
}

type googleEmail struct {
	Value string `json:"value,omitempty"`
}

type googleAPIError struct {
	Status int
	Body   string
}

func (e googleAPIError) Error() string {
	return fmt.Sprintf("people api returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

type gmailContactSyncAccount struct {
	ID    string
	Email string
}

func (h *Handler) handleSyncGmailContacts(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || !h.auth.HasGoogleOAuth() {
		htmlStatus(w, http.StatusBadRequest, "Google OAuth is not configured.")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid sync request.")
		return
	}

	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := strings.TrimSpace(r.FormValue("account_id"))
	accounts, err := h.gmailContactSyncAccounts(ctx, userID, accountID)
	if err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not find Gmail accounts.")
		return
	}
	if len(accounts) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Connect a Gmail account before syncing Gmail contacts.")
		return
	}

	totalImported := 0
	failures := make([]string, 0)
	for _, account := range accounts {
		token, err := h.auth.GetOAuthTokenForAccount(ctx, account.ID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: reconnect Gmail to grant contact access", account.Email))
			continue
		}

		imported, err := h.syncGooglePeopleConnections(ctx, userID, account.ID, token)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", account.Email, err.Error()))
			continue
		}
		totalImported += imported
	}

	if len(failures) == len(accounts) {
		htmlStatus(w, http.StatusBadGateway, "Gmail contact sync failed: "+strings.Join(failures, "; "))
		return
	}

	_ = h.db.LogContactActivity(ctx, userID, "provider_contacts_synced", "", "Gmail contacts synced", totalImported)
	if len(failures) > 0 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Gmail contacts partially synced: %d imported or updated. Failed: %s", totalImported, strings.Join(failures, "; ")))
		return
	}
	if len(accounts) == 1 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Gmail contacts synced for %s: %d imported or updated.", accounts[0].Email, totalImported))
		return
	}
	htmlStatus(w, http.StatusOK, fmt.Sprintf("Gmail contacts synced across %d accounts: %d imported or updated.", len(accounts), totalImported))
}

func (h *Handler) gmailContactSyncAccounts(ctx context.Context, userID, accountID string) ([]gmailContactSyncAccount, error) {
	if accountID != "" {
		var account gmailContactSyncAccount
		err := h.db.Read().QueryRowContext(ctx, `
		SELECT id, email_address
		FROM accounts
		WHERE id = ? AND user_id = ? AND provider = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID, providers.ProviderGmail).Scan(&account.ID, &account.Email)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []gmailContactSyncAccount{account}, nil
	}

	rows, err := h.db.Read().QueryContext(ctx, `
		SELECT id, email_address
		FROM accounts
		WHERE user_id = ? AND provider = ? AND COALESCE(is_deleting, 0) = 0
		ORDER BY email_address COLLATE NOCASE`, userID, providers.ProviderGmail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []gmailContactSyncAccount
	for rows.Next() {
		var account gmailContactSyncAccount
		if err := rows.Scan(&account.ID, &account.Email); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (h *Handler) syncGooglePeopleConnections(ctx context.Context, userID, accountID, accessToken string) (int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	pageToken := ""
	imported := 0

	for {
		values := url.Values{}
		values.Set("personFields", "names,emailAddresses,metadata")
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
			name := googlePersonName(person)
			for _, email := range person.EmailAddresses {
				value := strings.TrimSpace(email.Value)
				if value == "" {
					continue
				}
				contactID, _, err := h.db.UpsertSyncedContact(ctx, userID, accountID, name, value)
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

func (h *Handler) syncContactToGmailTargets(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) error {
	if h.auth == nil || !h.auth.HasGoogleOAuth() || contact.ID == "" || contact.Email == "" {
		return nil
	}

	desired, err := h.gmailTargetAccountSet(ctx, userID, contact.SaveTargets)
	if err != nil {
		return err
	}
	previousTargets := map[string]bool{}
	if previous != nil {
		previousTargets, err = h.gmailTargetAccountSet(ctx, userID, previous.SaveTargets)
		if err != nil {
			return err
		}
	}

	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderGmail)
	if err != nil {
		return err
	}
	handledRemoved := make(map[string]bool)
	for _, source := range sources {
		if !desired[source.AccountID] {
			handledRemoved[source.AccountID] = true
			if strings.TrimSpace(source.RemoteID) != "" {
				token, err := h.auth.GetOAuthTokenForAccount(ctx, source.AccountID)
				if err != nil {
					return err
				}
				if err := h.deleteGoogleContactByResourceAndEmail(ctx, token, source.RemoteID, contact.Email); err != nil {
					return err
				}
			}
			if err := h.db.DeleteContactSource(ctx, userID, contact.ID, providers.ProviderGmail, source.AccountID); err != nil {
				return err
			}
		}
	}
	for accountID := range previousTargets {
		if desired[accountID] || handledRemoved[accountID] {
			continue
		}
		token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if err := h.deleteGoogleContactsByEmail(ctx, token, contact.Email); err != nil {
			return err
		}
	}

	for accountID := range desired {
		if err := h.pushContactToGmailAccount(ctx, userID, contact, accountID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) scheduleContactGmailSync(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) bool {
	if !h.contactNeedsGmailSync(ctx, userID, contact, previous) {
		return false
	}
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		logSyncResult := func(eventType, message string) {
			logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer logCancel()
			_ = h.db.LogContactActivity(logCtx, userID, eventType, contact.Email, message, 1)
		}
		if err := h.syncContactToGmailTargets(bg, userID, contact, previous); err != nil {
			logSyncResult("gmail_contact_sync_failed", "Gmail contact sync failed: "+err.Error())
			return
		}
		logSyncResult("gmail_contact_synced", "Gmail contact synced")
	}()
	return true
}

func (h *Handler) contactNeedsGmailSync(ctx context.Context, userID string, contact models.Contact, previous *models.Contact) bool {
	if h.auth == nil || !h.auth.HasGoogleOAuth() || contact.ID == "" || contact.Email == "" {
		return false
	}
	currentTargets, err := h.gmailTargetAccountSet(ctx, userID, contact.SaveTargets)
	if err == nil && len(currentTargets) > 0 {
		return true
	}
	if previous != nil {
		previousTargets, err := h.gmailTargetAccountSet(ctx, userID, previous.SaveTargets)
		if err == nil && len(previousTargets) > 0 {
			return true
		}
	}
	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderGmail)
	return err == nil && len(sources) > 0
}

func (h *Handler) gmailTargetAccountSet(ctx context.Context, userID string, targets []string) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, target := range targets {
		accountID, ok := strings.CutPrefix(strings.TrimSpace(target), "account:")
		if !ok || accountID == "" {
			continue
		}
		accounts, err := h.gmailContactSyncAccounts(ctx, userID, accountID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 1 {
			out[accountID] = true
		}
	}
	return out, nil
}

func (h *Handler) deleteContactFromGmail(ctx context.Context, userID string, contact models.Contact) error {
	if h.auth == nil || !h.auth.HasGoogleOAuth() || contact.ID == "" {
		return nil
	}
	sources, err := h.db.GetContactSources(ctx, userID, contact.ID, providers.ProviderGmail)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if strings.TrimSpace(source.RemoteID) == "" {
			if err := h.db.DeleteContactSource(ctx, userID, contact.ID, providers.ProviderGmail, source.AccountID); err != nil {
				return err
			}
			continue
		}
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
		if err := h.db.DeleteContactSource(ctx, userID, contact.ID, providers.ProviderGmail, source.AccountID); err != nil {
			return err
		}
	}
	return nil
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
	endpoint := "https://people.googleapis.com/v1/people:createContact?personFields=names,emailAddresses,metadata"
	return h.writeGoogleContact(ctx, http.MethodPost, endpoint, accessToken, googlePersonFromContact(contact, "", ""))
}

func (h *Handler) updateGoogleContact(ctx context.Context, accessToken, remoteID, etag string, contact models.Contact) (googlePerson, error) {
	endpoint := "https://people.googleapis.com/v1/" + strings.TrimSpace(remoteID) + ":updateContact?updatePersonFields=names,emailAddresses&personFields=names,emailAddresses,metadata"
	return h.writeGoogleContact(ctx, http.MethodPatch, endpoint, accessToken, googlePersonFromContact(contact, remoteID, etag))
}

func (h *Handler) getGoogleContact(ctx context.Context, accessToken, remoteID string) (googlePerson, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://people.googleapis.com/v1/"+strings.TrimSpace(remoteID)+"?personFields=names,emailAddresses,metadata", nil)
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
	values.Set("readMask", "names,emailAddresses,metadata")
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
	person := googlePerson{ResourceName: strings.TrimSpace(remoteID), Etag: strings.TrimSpace(etag), EmailAddresses: []googleEmail{{Value: strings.TrimSpace(contact.Email)}}}
	name := strings.TrimSpace(contact.Name)
	if name != "" && !strings.EqualFold(name, strings.TrimSpace(contact.Email)) {
		person.Names = []googleName{{GivenName: name}}
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
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<div class="rounded-md border border-border bg-background px-3 py-2 text-xs text-muted-foreground">` + html.EscapeString(message) + `</div>`))
}
