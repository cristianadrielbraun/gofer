package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var outlookGraphBaseURL = "https://graph.microsoft.com/v1.0"

const outlookContactPhotoMaxBytes = 1024 * 1024

type outlookContactsResponse struct {
	Contacts []outlookContact `json:"value"`
	NextLink string           `json:"@odata.nextLink"`
}

type outlookContact struct {
	ID             string                `json:"id,omitempty"`
	ChangeKey      string                `json:"changeKey,omitempty"`
	ODataEtag      string                `json:"@odata.etag,omitempty"`
	DisplayName    string                `json:"displayName,omitempty"`
	GivenName      string                `json:"givenName,omitempty"`
	Surname        string                `json:"surname,omitempty"`
	EmailAddresses []outlookEmailAddress `json:"emailAddresses,omitempty"`
	BusinessPhones []string              `json:"businessPhones,omitempty"`
	HomePhones     []string              `json:"homePhones,omitempty"`
	MobilePhone    string                `json:"mobilePhone,omitempty"`
	CompanyName    string                `json:"companyName,omitempty"`
	JobTitle       string                `json:"jobTitle,omitempty"`
	PersonalNotes  string                `json:"personalNotes,omitempty"`
}

type outlookContactPayload struct {
	DisplayName    string                `json:"displayName"`
	EmailAddresses []outlookEmailAddress `json:"emailAddresses"`
	BusinessPhones []string              `json:"businessPhones"`
	HomePhones     []string              `json:"homePhones"`
	MobilePhone    string                `json:"mobilePhone"`
	CompanyName    string                `json:"companyName"`
	JobTitle       string                `json:"jobTitle"`
	PersonalNotes  string                `json:"personalNotes"`
}

type outlookEmailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
}

type outlookAPIError struct {
	Status    int
	Body      string
	RetryAt   time.Time
	RequestID string
}

func (e outlookAPIError) Error() string {
	return fmt.Sprintf("graph contacts api returned %d: %s", e.Status, sanitizeProviderErrorBody(e.Body))
}

func (e outlookAPIError) RetryAfter() (time.Time, bool) {
	if e.RetryAt.IsZero() {
		return time.Time{}, false
	}
	return e.RetryAt, true
}

func (h *Handler) syncOutlookContacts(ctx context.Context, userID, accountID, accessToken string) (int, error) {
	endpoint := outlookGraphBaseURL + "/me/contacts?" + outlookContactListQuery().Encode()
	imported := 0

	for endpoint != "" {
		var page outlookContactsResponse
		if err := h.doOutlookJSON(ctx, http.MethodGet, endpoint, accessToken, nil, &page); err != nil {
			return imported, err
		}
		for _, remote := range page.Contacts {
			contact := outlookContactFromGraph(remote)
			if photoURL, err := h.fetchOutlookContactPhotoDataURL(ctx, accessToken, remote.ID); err == nil {
				contact.AvatarURL = photoURL
			}
			if strings.TrimSpace(contact.Email) == "" {
				continue
			}
			contactID, _, err := h.db.UpsertSyncedContactFromContact(ctx, userID, accountID, contact)
			if err != nil {
				return imported, err
			}
			if contactID != "" && strings.TrimSpace(remote.ID) != "" {
				if err := h.db.UpsertContactSource(ctx, storage.ContactSource{
					ContactID: contactID,
					UserID:    userID,
					Provider:  providers.ProviderOutlook,
					AccountID: accountID,
					RemoteID:  remote.ID,
					Etag:      outlookContactVersion(remote),
				}); err != nil {
					return imported, err
				}
			}
			imported++
		}
		endpoint = page.NextLink
	}
	return imported, nil
}

func (h *Handler) fetchOutlookContactPhotoDataURL(ctx context.Context, accessToken, remoteID string) (string, error) {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return "", nil
	}
	endpoint := outlookGraphBaseURL + "/me/contacts/" + url.PathEscape(remoteID) + "/photo/$value"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", newOutlookAPIError(resp, body)
	}
	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if contentType == "" {
		contentType = "image/jpeg"
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return "", fmt.Errorf("graph contact photo returned non-image content type %q", contentType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, outlookContactPhotoMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > outlookContactPhotoMaxBytes {
		return "", fmt.Errorf("graph contact photo exceeds %d bytes", outlookContactPhotoMaxBytes)
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func outlookContactListQuery() url.Values {
	values := url.Values{}
	values.Set("$top", "500")
	values.Set("$select", "id,changeKey,displayName,givenName,surname,emailAddresses,businessPhones,homePhones,mobilePhone,companyName,jobTitle,personalNotes")
	return values
}

func outlookContactFromGraph(remote outlookContact) models.Contact {
	contact := models.Contact{Name: outlookContactDisplayName(remote)}
	for _, email := range normalizedOutlookEmailValues(remote.EmailAddresses) {
		if contact.Email == "" {
			contact.Email = email.Address
			contact.EmailLabel = "primary"
		} else {
			contact.AdditionalEmails = append(contact.AdditionalEmails, email.Address)
			contact.AdditionalEmailLabels = append(contact.AdditionalEmailLabels, "alternate")
		}
	}
	appendPhone := func(value, label string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if contact.Phone == "" {
			contact.Phone = value
			contact.PhoneLabel = label
			return
		}
		if sameOutlookContactValue(value, contact.Phone) {
			return
		}
		for _, existing := range contact.AdditionalPhones {
			if sameOutlookContactValue(value, existing) {
				return
			}
		}
		contact.AdditionalPhones = append(contact.AdditionalPhones, value)
		contact.AdditionalPhoneLabels = append(contact.AdditionalPhoneLabels, label)
	}
	appendPhone(remote.MobilePhone, "mobile")
	for _, phone := range remote.BusinessPhones {
		appendPhone(phone, "work")
	}
	for _, phone := range remote.HomePhones {
		appendPhone(phone, "home")
	}
	contact.Organization = strings.TrimSpace(remote.CompanyName)
	contact.Title = strings.TrimSpace(remote.JobTitle)
	contact.Notes = strings.TrimSpace(remote.PersonalNotes)
	return contact
}

func outlookContactDisplayName(remote outlookContact) string {
	for _, value := range []string{
		remote.DisplayName,
		strings.TrimSpace(strings.TrimSpace(remote.GivenName) + " " + strings.TrimSpace(remote.Surname)),
	} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func normalizedOutlookEmailValues(values []outlookEmailAddress) []outlookEmailAddress {
	out := make([]outlookEmailAddress, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value.Address = strings.TrimSpace(value.Address)
		value.Name = strings.TrimSpace(value.Name)
		if value.Address == "" {
			continue
		}
		key := strings.ToLower(value.Address)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func sameOutlookContactValue(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func (h *Handler) pushContactToOutlookAccount(ctx context.Context, userID string, contact models.Contact, accountID string) error {
	token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	source, err := h.db.GetContactSource(ctx, userID, contact.ID, providers.ProviderOutlook, accountID)
	if err != nil {
		return err
	}
	if source == nil || strings.TrimSpace(source.RemoteID) == "" {
		matches, err := h.searchOutlookContactsByEmail(ctx, token, contact.Email)
		if err != nil {
			return fmt.Errorf("preflight Outlook contact: %w", err)
		}
		if len(matches) > 1 {
			return fmt.Errorf("Outlook has multiple contacts with %s; choose the copy to use before enabling Gofer Sync", contact.Email)
		}
		if len(matches) == 1 && strings.TrimSpace(matches[0].ID) != "" {
			source = &storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderOutlook, AccountID: accountID, RemoteID: matches[0].ID, Etag: outlookContactVersion(matches[0])}
			if err := h.db.UpsertContactSource(ctx, *source); err != nil {
				return err
			}
		} else {
			remote, err := h.createOutlookContact(ctx, token, contact)
			if err != nil {
				return err
			}
			if strings.TrimSpace(remote.ID) == "" {
				return fmt.Errorf("graph contacts api did not return a contact id")
			}
			return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderOutlook, AccountID: accountID, RemoteID: remote.ID, Etag: outlookContactVersion(remote)})
		}
	}

	remote, err := h.updateOutlookContact(ctx, token, source.RemoteID, contact)
	if err != nil {
		var apiErr outlookAPIError
		if isOutlookNotFound(err, &apiErr) {
			remote, err := h.createOutlookContact(ctx, token, contact)
			if err != nil {
				return err
			}
			if strings.TrimSpace(remote.ID) == "" {
				return fmt.Errorf("graph contacts api did not return a contact id")
			}
			return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderOutlook, AccountID: accountID, RemoteID: remote.ID, Etag: outlookContactVersion(remote)})
		}
		return err
	}
	remoteID := strings.TrimSpace(remote.ID)
	if remoteID == "" {
		remoteID = source.RemoteID
	}
	return h.db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contact.ID, UserID: userID, Provider: providers.ProviderOutlook, AccountID: accountID, RemoteID: remoteID, Etag: outlookContactVersion(remote)})
}

func (h *Handler) createOutlookContact(ctx context.Context, accessToken string, contact models.Contact) (outlookContact, error) {
	var remote outlookContact
	err := h.doOutlookJSON(ctx, http.MethodPost, outlookGraphBaseURL+"/me/contacts", accessToken, outlookContactPayloadFromContact(contact), &remote)
	return remote, err
}

func (h *Handler) updateOutlookContact(ctx context.Context, accessToken, remoteID string, contact models.Contact) (outlookContact, error) {
	var remote outlookContact
	err := h.doOutlookJSON(ctx, http.MethodPatch, outlookGraphBaseURL+"/me/contacts/"+url.PathEscape(strings.TrimSpace(remoteID)), accessToken, outlookContactPayloadFromContact(contact), &remote)
	return remote, err
}

func (h *Handler) deleteOutlookContact(ctx context.Context, accessToken, remoteID string) error {
	err := h.doOutlookJSON(ctx, http.MethodDelete, outlookGraphBaseURL+"/me/contacts/"+url.PathEscape(strings.TrimSpace(remoteID)), accessToken, nil, nil)
	if err != nil {
		var apiErr outlookAPIError
		if isOutlookNotFound(err, &apiErr) {
			return nil
		}
	}
	return err
}

func (h *Handler) deleteOutlookContactByIDAndEmail(ctx context.Context, accessToken, remoteID, email string) error {
	if strings.TrimSpace(remoteID) != "" {
		if err := h.deleteOutlookContact(ctx, accessToken, remoteID); err != nil {
			return err
		}
	}
	return h.deleteOutlookContactsByEmail(ctx, accessToken, email)
}

func (h *Handler) deleteOutlookContactsByEmail(ctx context.Context, accessToken, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	matches, err := h.searchOutlookContactsByEmail(ctx, accessToken, email)
	if err != nil {
		return err
	}
	for _, remote := range matches {
		if strings.TrimSpace(remote.ID) == "" {
			continue
		}
		if err := h.deleteOutlookContact(ctx, accessToken, remote.ID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) searchOutlookContactsByEmail(ctx context.Context, accessToken, email string) ([]outlookContact, error) {
	values := outlookContactListQuery()
	values.Set("$top", "25")
	values.Set("$filter", "emailAddresses/any(a:a/address eq '"+escapeODataString(email)+"')")
	endpoint := outlookGraphBaseURL + "/me/contacts?" + values.Encode()

	var matches []outlookContact
	for endpoint != "" {
		var page outlookContactsResponse
		if err := h.doOutlookJSON(ctx, http.MethodGet, endpoint, accessToken, nil, &page); err != nil {
			return nil, err
		}
		for _, remote := range page.Contacts {
			if outlookContactHasEmail(remote, email) {
				matches = append(matches, remote)
			}
		}
		endpoint = page.NextLink
	}
	return matches, nil
}

func outlookContactHasEmail(remote outlookContact, email string) bool {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return false
	}
	for _, candidate := range remote.EmailAddresses {
		if strings.EqualFold(strings.TrimSpace(candidate.Address), email) {
			return true
		}
	}
	return false
}

func outlookContactPayloadFromContact(contact models.Contact) outlookContactPayload {
	name := strings.TrimSpace(contact.Name)
	if name == "" {
		name = strings.TrimSpace(contact.Email)
	}
	payload := outlookContactPayload{
		DisplayName:    name,
		EmailAddresses: []outlookEmailAddress{},
		BusinessPhones: []string{},
		HomePhones:     []string{},
		MobilePhone:    "",
		CompanyName:    strings.TrimSpace(contact.Organization),
		JobTitle:       strings.TrimSpace(contact.Title),
		PersonalNotes:  strings.TrimSpace(contact.Notes),
	}
	for _, email := range append([]string{contact.Email}, contact.AdditionalEmails...) {
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		payload.EmailAddresses = append(payload.EmailAddresses, outlookEmailAddress{Name: name, Address: email})
	}
	for i, phone := range append([]string{contact.Phone}, contact.AdditionalPhones...) {
		phone = strings.TrimSpace(phone)
		if phone == "" {
			continue
		}
		label := contact.PhoneLabel
		if i > 0 && i-1 < len(contact.AdditionalPhoneLabels) {
			label = contact.AdditionalPhoneLabels[i-1]
		}
		switch normalizeOutlookPhoneLabel(label) {
		case "mobile":
			if payload.MobilePhone == "" {
				payload.MobilePhone = phone
			} else {
				payload.BusinessPhones = append(payload.BusinessPhones, phone)
			}
		case "home":
			payload.HomePhones = append(payload.HomePhones, phone)
		default:
			payload.BusinessPhones = append(payload.BusinessPhones, phone)
		}
	}
	return payload
}

func normalizeOutlookPhoneLabel(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	switch {
	case strings.Contains(label, "mobile"), strings.Contains(label, "cell"):
		return "mobile"
	case strings.Contains(label, "home"):
		return "home"
	default:
		return "work"
	}
}

func outlookContactVersion(remote outlookContact) string {
	if value := strings.TrimSpace(remote.ChangeKey); value != "" {
		return value
	}
	return strings.TrimSpace(remote.ODataEtag)
}

func (h *Handler) doOutlookJSON(ctx context.Context, method, endpoint, accessToken string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return readErr
		}
		return newOutlookAPIError(resp, raw)
	}
	if readErr != nil {
		return readErr
	}
	if out == nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func escapeODataString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func isOutlookNotFound(err error, apiErr *outlookAPIError) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(outlookAPIError); ok {
		if apiErr != nil {
			*apiErr = typed
		}
		return typed.Status == http.StatusNotFound || typed.Status == http.StatusGone
	}
	return false
}
