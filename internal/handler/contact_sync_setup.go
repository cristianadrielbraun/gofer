package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func contactSyncSetupRequired(requested models.Contact, previous *models.Contact) bool {
	if !requested.GoferSyncEnabled {
		return false
	}
	if previous == nil || !previous.GoferSyncEnabled {
		return true
	}
	existing := make(map[string]bool, len(previous.SaveTargets))
	for _, target := range previous.SaveTargets {
		existing[strings.TrimSpace(target)] = true
	}
	for _, target := range requested.SaveTargets {
		target = strings.TrimSpace(target)
		if target != "" && target != "local" && !existing[target] {
			return true
		}
	}
	return false
}

func (h *Handler) handleContactSyncSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	contact, err := h.db.GetContact(ctx, h.userID(ctx), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		http.Error(w, "Could not load contact sync setup", http.StatusInternalServerError)
		return
	}
	if contact == nil {
		http.NotFound(w, r)
		return
	}
	setup := models.ContactSyncSetup{Contact: *contact, Phase: "searching", SearchMode: "automatic"}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.ContactSyncSetupDialog(setup).Render(ctx, w); err != nil {
		http.Error(w, "Could not render contact sync setup", http.StatusInternalServerError)
	}
}

func (h *Handler) handleContactSyncSetupFindings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	contact, err := h.db.GetContact(ctx, h.userID(ctx), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		http.Error(w, "Could not load contact sync setup", http.StatusInternalServerError)
		return
	}
	if contact == nil {
		http.NotFound(w, r)
		return
	}
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = "automatic"
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	setup := h.discoverContactSyncSetup(ctx, h.userID(ctx), *contact, mode, query, accountID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if accountID != "" {
		if len(setup.Locations) == 0 {
			http.Error(w, "Sync location could not be found", http.StatusNotFound)
			return
		}
		if err := views.ContactSyncSetupLocation(setup.Contact.ID, setup.Locations[0]).Render(ctx, w); err != nil {
			http.Error(w, "Could not render contact sync findings", http.StatusInternalServerError)
		}
		return
	}
	if err := views.ContactSyncSetupDiscover(setup).Render(ctx, w); err != nil {
		http.Error(w, "Could not render contact sync findings", http.StatusInternalServerError)
	}
}

func (h *Handler) handlePreviewContactSyncSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	contactID := strings.TrimSpace(r.PathValue("id"))
	contact, err := h.db.GetContact(ctx, userID, contactID)
	if err != nil || contact == nil {
		http.Error(w, "Contact could not be loaded", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid setup choices", http.StatusBadRequest)
		return
	}
	accounts, err := h.contactTargetAccountSet(ctx, userID, contact.SaveTargets)
	if err != nil {
		http.Error(w, "Sync locations could not be loaded", http.StatusBadRequest)
		return
	}
	for accountID, account := range accounts {
		key := strings.TrimSpace(r.FormValue("candidate_" + accountID))
		if key == "" || key == "none" {
			continue
		}
		candidate, source, err := h.loadContactSyncSetupCandidate(ctx, userID, account, key)
		if err != nil {
			http.Error(w, fmt.Sprintf("Could not use the selected contact from %s: %v", account.Email, err), http.StatusBadGateway)
			return
		}
		if err := h.db.ReplaceSyncedContactFieldsForProfile(ctx, userID, contactID, accountID, candidate); err != nil {
			http.Error(w, "Could not compare the selected contact", http.StatusInternalServerError)
			return
		}
		source.ContactID = contactID
		source.UserID = userID
		if err := h.db.UpsertContactSource(ctx, source); err != nil {
			http.Error(w, "Could not attach the selected contact", http.StatusInternalServerError)
			return
		}
	}
	profile, err := h.db.GetContactProfile(ctx, userID, contactID)
	if err != nil || profile == nil {
		http.Error(w, "Could not prepare conflict review", http.StatusInternalServerError)
		return
	}
	setup := models.ContactSyncSetup{Contact: *contact, Phase: "resolve", ConflictFields: profile.Fields}
	accountIDs := make([]string, 0, len(accounts))
	for accountID := range accounts {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	for _, accountID := range accountIDs {
		account := accounts[accountID]
		setup.Locations = append(setup.Locations, models.ContactSyncSetupLocation{AccountID: account.ID, Label: account.Email, Provider: account.Provider})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.ContactSyncSetupResolve(setup).Render(ctx, w); err != nil {
		http.Error(w, "Could not render conflict review", http.StatusInternalServerError)
	}
}

func (h *Handler) handleConfirmContactSyncSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	contactID := strings.TrimSpace(r.PathValue("id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid conflict choices", http.StatusBadRequest)
		return
	}
	selectedFields := map[string]string{}
	for name, values := range r.Form {
		if !strings.HasPrefix(name, "preferred_") || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
			continue
		}
		selectedFields[strings.TrimPrefix(name, "preferred_")] = strings.TrimSpace(values[0])
	}
	if err := h.db.InitializeContactCanonicalFields(ctx, userID, contactID, selectedFields); err != nil {
		http.Error(w, "Could not initialize synchronized contact values: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.SetContactProfileSyncEnabled(ctx, userID, contactID, true); err != nil {
		http.Error(w, "Could not enable Gofer Sync", http.StatusBadRequest)
		return
	}
	contact, err := h.db.GetContact(ctx, userID, contactID)
	if err != nil || contact == nil {
		http.Error(w, "Could not load enabled contact", http.StatusInternalServerError)
		return
	}
	syncQueued := h.scheduleContactAccountSync(ctx, userID, *contact, nil)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":                  true,
		"contact_id":          contactID,
		"location":            "/contacts?contact=" + contactID,
		"contact_sync_queued": syncQueued,
	})
}

func (h *Handler) discoverContactSyncSetup(ctx context.Context, userID string, contact models.Contact, mode, customQuery, accountFilter string) models.ContactSyncSetup {
	setup := models.ContactSyncSetup{Contact: contact, Phase: "discover", SearchMode: mode, SearchQuery: customQuery}
	queries := []string{contact.Email, contact.Phone, contact.Name}
	switch mode {
	case "email":
		queries = []string{contact.Email}
	case "custom":
		queries = []string{customQuery}
	}
	accounts, err := h.contactTargetAccountSet(ctx, userID, contact.SaveTargets)
	if err != nil {
		return setup
	}
	accountIDs := make([]string, 0, len(accounts))
	for accountID := range accounts {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)
	for _, accountID := range accountIDs {
		if accountFilter != "" && accountID != accountFilter {
			continue
		}
		account := accounts[accountID]
		location := models.ContactSyncSetupLocation{AccountID: account.ID, Label: account.Email, Provider: account.Provider}
		location.Candidates, location.Error = h.searchContactSyncSetupCandidates(ctx, userID, account, contact, queries)
		setup.Locations = append(setup.Locations, location)
	}
	return setup
}

func (h *Handler) searchContactSyncSetupCandidates(ctx context.Context, userID string, account contactSyncAccount, contact models.Contact, queries []string) ([]models.ContactSyncSetupCandidate, string) {
	seen := map[string]bool{}
	var candidates []models.ContactSyncSetupCandidate
	appendCandidate := func(key, remoteID, contactID string, candidate models.Contact) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		matchEmail := contactSyncEmailsMatch(contact, candidate)
		matchPhone := contactSyncPhonesMatch(contact, candidate)
		matchName := contactSyncNamesMatch(contact, candidate)
		candidates = append(candidates, models.ContactSyncSetupCandidate{
			Key: key, RemoteID: remoteID, ContactID: contactID, Name: candidate.Name, Email: candidate.Email,
			Phone: candidate.Phone, Organization: candidate.Organization,
			MatchEmail: matchEmail, MatchPhone: matchPhone, MatchName: matchName,
		})
	}
	switch account.Provider {
	case providers.ProviderGmail:
		if h.auth == nil {
			return nil, "Gmail authorization is unavailable."
		}
		token, err := h.auth.GetOAuthTokenForAccount(ctx, account.ID)
		if err != nil {
			return nil, err.Error()
		}
		for _, query := range queries {
			if strings.TrimSpace(query) == "" {
				continue
			}
			people, err := h.searchGoogleContacts(ctx, token, query)
			if err != nil {
				return candidates, err.Error()
			}
			for _, person := range people {
				appendCandidate("gmail:"+person.ResourceName, person.ResourceName, "", googleContactFromPerson(person))
			}
		}
	case providers.ProviderOutlook:
		if h.auth == nil {
			return nil, "Outlook authorization is unavailable."
		}
		token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, account.ID)
		if err != nil {
			return nil, err.Error()
		}
		for _, query := range queries {
			if strings.TrimSpace(query) == "" {
				continue
			}
			remotes, err := h.searchOutlookContacts(ctx, token, query)
			if err != nil {
				return candidates, err.Error()
			}
			for _, remote := range remotes {
				appendCandidate("outlook:"+remote.ID, remote.ID, "", outlookContactFromGraph(remote))
			}
		}
	default:
		for offset := 0; ; offset += 200 {
			contacts, err := h.db.ListContacts(ctx, userID, models.ContactFilters{}, 200, offset)
			if err != nil {
				return candidates, err.Error()
			}
			for _, candidate := range contacts {
				if !contactSyncCandidateMatchesQueries(candidate, queries) {
					continue
				}
				sources, _ := h.db.GetContactSources(ctx, userID, candidate.ID, account.Provider)
				for _, source := range sources {
					if source.AccountID == account.ID {
						appendCandidate("stored:"+candidate.ID, source.RemoteID, candidate.ID, candidate)
						break
					}
				}
			}
			if len(contacts) < 200 {
				break
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return contactSyncSetupCandidateScore(candidates[i]) > contactSyncSetupCandidateScore(candidates[j])
	})
	return candidates, ""
}

func normalizedContactSyncPhone(value string) string {
	var digits strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			digits.WriteRune(char)
		}
	}
	return digits.String()
}

func normalizedContactSyncName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func contactSyncEmailsMatch(left, right models.Contact) bool {
	leftEmails := append([]string{left.Email}, left.AdditionalEmails...)
	rightEmails := append([]string{right.Email}, right.AdditionalEmails...)
	for _, leftEmail := range leftEmails {
		leftEmail = strings.TrimSpace(leftEmail)
		if leftEmail == "" {
			continue
		}
		for _, rightEmail := range rightEmails {
			if strings.EqualFold(leftEmail, strings.TrimSpace(rightEmail)) {
				return true
			}
		}
	}
	return false
}

func contactSyncPhonesMatch(left, right models.Contact) bool {
	leftPhones := append([]string{left.Phone}, left.AdditionalPhones...)
	rightPhones := append([]string{right.Phone}, right.AdditionalPhones...)
	for _, leftPhone := range leftPhones {
		leftPhone = normalizedContactSyncPhone(leftPhone)
		if len(leftPhone) < 7 {
			continue
		}
		for _, rightPhone := range rightPhones {
			if leftPhone == normalizedContactSyncPhone(rightPhone) {
				return true
			}
		}
	}
	return false
}

func contactSyncNamesMatch(left, right models.Contact) bool {
	leftName := normalizedContactSyncName(left.Name)
	return leftName != "" && leftName == normalizedContactSyncName(right.Name)
}

func contactSyncSetupCandidateScore(candidate models.ContactSyncSetupCandidate) int {
	score := 0
	if candidate.MatchEmail {
		score += 4
	}
	if candidate.MatchPhone {
		score += 2
	}
	if candidate.MatchName {
		score++
	}
	return score
}

func contactSyncCandidateMatchesQueries(candidate models.Contact, queries []string) bool {
	values := append([]string{candidate.Name, candidate.Email, candidate.Phone}, candidate.AdditionalEmails...)
	values = append(values, candidate.AdditionalPhones...)
	for _, query := range queries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		queryLower := strings.ToLower(query)
		queryPhone := normalizedContactSyncPhone(query)
		for _, value := range values {
			if strings.Contains(strings.ToLower(strings.TrimSpace(value)), queryLower) {
				return true
			}
			if len(queryPhone) >= 7 && strings.Contains(normalizedContactSyncPhone(value), queryPhone) {
				return true
			}
		}
	}
	return false
}

func (h *Handler) loadContactSyncSetupCandidate(ctx context.Context, userID string, account contactSyncAccount, key string) (models.Contact, storage.ContactSource, error) {
	switch {
	case account.Provider == providers.ProviderGmail && strings.HasPrefix(key, "gmail:"):
		token, err := h.auth.GetOAuthTokenForAccount(ctx, account.ID)
		if err != nil {
			return models.Contact{}, storage.ContactSource{}, err
		}
		remoteID := strings.TrimPrefix(key, "gmail:")
		person, err := h.getGoogleContact(ctx, token, remoteID)
		return googleContactFromPerson(person), storage.ContactSource{Provider: account.Provider, AccountID: account.ID, RemoteID: remoteID, Etag: person.Etag}, err
	case account.Provider == providers.ProviderOutlook && strings.HasPrefix(key, "outlook:"):
		token, err := h.auth.GetMicrosoftGraphContactsTokenForAccount(ctx, account.ID)
		if err != nil {
			return models.Contact{}, storage.ContactSource{}, err
		}
		remoteID := strings.TrimPrefix(key, "outlook:")
		remote, err := h.getOutlookContact(ctx, token, remoteID)
		return outlookContactFromGraph(remote), storage.ContactSource{Provider: account.Provider, AccountID: account.ID, RemoteID: remoteID, Etag: outlookContactVersion(remote)}, err
	case strings.HasPrefix(key, "stored:"):
		candidateID := strings.TrimPrefix(key, "stored:")
		candidate, err := h.db.GetContact(ctx, userID, candidateID)
		if err != nil || candidate == nil {
			return models.Contact{}, storage.ContactSource{}, fmt.Errorf("stored contact was not found")
		}
		source, err := h.db.GetContactSource(ctx, userID, candidateID, account.Provider, account.ID)
		if err != nil || source == nil {
			return models.Contact{}, storage.ContactSource{}, fmt.Errorf("stored provider contact was not found")
		}
		return *candidate, *source, nil
	default:
		return models.Contact{}, storage.ContactSource{}, fmt.Errorf("invalid contact selection")
	}
}
