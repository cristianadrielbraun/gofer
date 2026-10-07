package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleUserContactSyncSetupFindings(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	mode, query, filter := strings.TrimSpace(r.URL.Query().Get("mode")), strings.TrimSpace(r.URL.Query().Get("query")), strings.TrimSpace(r.URL.Query().Get("account_id"))
	if mode == "" {
		mode = "automatic"
	}
	var setup models.ContactSyncSetup
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactSetup(ctx, owner, id)
		if err != nil {
			return err
		}
		setup = models.ContactSyncSetup{Contact: snapshot.Contact(), Phase: "discover", SearchMode: mode, SearchQuery: query}
		queries := []string{setup.Contact.Email, setup.Contact.Phone, setup.Contact.Name}
		switch mode {
		case "email":
			queries = []string{setup.Contact.Email}
		case "custom":
			queries = []string{query}
		}
		services := snapshot.Services()
		if filter != "" {
			found := false
			for _, service := range services {
				found = found || service.AccountID() == filter
			}
			if !found {
				return sql.ErrNoRows
			}
		}
		for _, service := range services {
			if filter != "" && service.AccountID() != filter {
				continue
			}
			location := models.ContactSyncSetupLocation{AccountID: service.AccountID(), Label: service.Identity().EmailAddress, Provider: service.ContactConfig().Provider}
			searchErr := h.userIMAP.RunAccountService(ctx, owner, service.AccountID(), mail.AccountServiceContacts, 2*time.Minute, func(ctx context.Context) error {
				var err error
				location.Candidates, err = h.searchUserContactSetup(ctx, snapshot, service, queries)
				return err
			})
			// Provider failures can be shown per location, but stale authority or a
			// stopped root must never render copied owner data as current findings.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := h.userAccounts.ValidateContactSetup(ctx, snapshot); err != nil {
				return err
			}
			if searchErr != nil {
				location.Error = searchErr.Error()
			}
			setup.Locations = append(setup.Locations, location)
		}
		return h.userAccounts.ValidateContactSetup(ctx, snapshot)
	})
	if err != nil {
		userContactEditError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if filter != "" {
		if err := views.ContactSyncSetupLocation(setup.Contact.ID, setup.Locations[0]).Render(r.Context(), w); err != nil {
			http.Error(w, "Could not render contact sync findings", 500)
		}
		return
	}
	if err := views.ContactSyncSetupDiscover(setup).Render(r.Context(), w); err != nil {
		http.Error(w, "Could not render contact sync findings", 500)
	}
}

func (h *Handler) searchUserContactSetup(ctx context.Context, setup *config.UserContactEditSnapshot, service *config.AccountServiceSnapshot, queries []string) (candidates []models.ContactSyncSetupCandidate, err error) {
	contact := setup.Contact()
	seen := map[string]bool{}
	appendCandidate := func(key, remote, profile string, candidate models.Contact) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, models.ContactSyncSetupCandidate{Key: key, RemoteID: remote, ContactID: profile, Name: candidate.Name, Email: candidate.Email, Phone: candidate.Phone, Organization: candidate.Organization,
			MatchEmail: contactSyncEmailsMatch(contact, candidate), MatchPhone: contactSyncPhonesMatch(contact, candidate), MatchName: contactSyncNamesMatch(contact, candidate)})
	}
	defer func() {
		sort.SliceStable(candidates, func(i, j int) bool {
			return contactSyncSetupCandidateScore(candidates[i]) > contactSyncSetupCandidateScore(candidates[j])
		})
	}()
	provider := service.ContactConfig().Provider
	if provider != providers.ProviderGmail && provider != providers.ProviderOutlook {
		for after := ""; ; {
			page, err := h.userAccounts.ContactSetupCandidates(ctx, setup, service.AccountID(), after)
			if err != nil {
				return candidates, err
			}
			for _, stored := range page {
				if contactSyncCandidateMatchesQueries(stored.Contact, queries) {
					appendCandidate("stored:"+stored.Contact.ID, stored.RemoteID, stored.Contact.ID, stored.Contact)
				}
			}
			if len(page) < 200 {
				return candidates, nil
			}
			after = page[len(page)-1].Contact.ID
		}
	}
	if h.userCredentials == nil {
		return nil, errors.New("mailbox OAuth is unavailable")
	}
	pull := &userContactPull{h: h, snapshot: service, setup: setup, credentials: h.userCredentials.ContactsAccount(service.OwnerID(), service.AccountID())}
	for _, query := range queries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		if provider == providers.ProviderGmail {
			var page googleSearchContactsResponse
			values := url.Values{"query": {query}, "readMask": {googleContactPersonFields()}, "pageSize": {"10"}}
			if err := pull.getJSON(ctx, googlePeopleAPIBaseURL+"/people:searchContacts?"+values.Encode(), &page); err != nil {
				return candidates, err
			}
			for _, item := range page.Results {
				if !validOwnedGoogleContactID(item.Person.ResourceName) {
					return candidates, errors.New("invalid returned Google contact identity")
				}
				appendCandidate("gmail:"+item.Person.ResourceName, item.Person.ResourceName, "", googleContactFromPerson(item.Person))
			}
		} else {
			matches, err := pull.searchOutlookSetup(ctx, query)
			if err != nil {
				return candidates, err
			}
			for _, remote := range matches {
				appendCandidate("outlook:"+remote.ID, remote.ID, "", outlookContactFromGraph(remote))
			}
		}
	}
	return candidates, nil
}

func (p *userContactPull) searchOutlookSetup(ctx context.Context, query string) ([]outlookContact, error) {
	values := outlookContactListQuery()
	values.Set("$top", "100")
	endpoint := outlookGraphBaseURL + "/me/contacts?" + values.Encode()
	seen := map[string]bool{}
	var matches []outlookContact
	for endpoint != "" && len(matches) < 10 {
		if err := validUserContactGraphPage(endpoint); err != nil {
			return nil, err
		}
		if seen[endpoint] {
			return nil, errors.New("contact provider repeated a page URL")
		}
		seen[endpoint] = true
		var page outlookContactsResponse
		if err := p.getJSON(ctx, endpoint, &page); err != nil {
			return nil, err
		}
		for _, remote := range page.Contacts {
			if !outlookContactMatchesSetupQuery(remote, query) {
				continue
			}
			if !validOwnedGraphContactID(remote.ID) {
				return nil, errors.New("invalid returned Graph contact identity")
			}
			matches = append(matches, remote)
			if len(matches) == 10 {
				break
			}
		}
		endpoint = page.NextLink
	}
	return matches, nil
}

func outlookContactMatchesSetupQuery(remote outlookContact, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return false
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(remote.DisplayName)), query) {
		return true
	}
	for _, email := range remote.EmailAddresses {
		if strings.Contains(strings.ToLower(strings.TrimSpace(email.Address)), query) {
			return true
		}
	}
	phoneQuery := normalizedContactSyncPhone(query)
	if len(phoneQuery) < 7 {
		return false
	}
	phones := append(append([]string{}, remote.BusinessPhones...), remote.HomePhones...)
	phones = append(phones, remote.MobilePhone)
	for _, phone := range phones {
		if strings.Contains(normalizedContactSyncPhone(phone), phoneQuery) {
			return true
		}
	}
	return false
}
