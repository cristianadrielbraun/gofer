package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// oauthCalendarCreateRequest is where and what to send to make a calendar
// named name on a Google or Microsoft calendar.
func oauthCalendarCreateRequest(provider, name string) (string, map[string]any) {
	if provider == providers.ProviderOutlook {
		return outlookGraphBaseURL + "/me/calendars", map[string]any{"name": name}
	}
	return googleCalendarAPIBaseURL + "/calendars", map[string]any{"summary": name}
}

// oauthCalendarCreateError explains a provider's refusal to make a calendar.
// A refused grant asks for a reconnect, which brings the permission to.
func oauthCalendarCreateError(provider string, err error) error {
	status := 0
	var created calendarCreateProviderError
	var google googleAPIError
	var outlook outlookAPIError
	switch {
	case errors.As(err, &created):
		status = created.Status
	case errors.As(err, &google):
		status = google.Status
	case errors.As(err, &outlook):
		status = outlook.Status
	}
	name := "Google"
	if provider == providers.ProviderOutlook {
		name = "Microsoft"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%s did not let this account create calendars. Reconnect the account from its actions menu, then try again", name)
	}
	return fmt.Errorf("%s did not create the calendar: %w", name, err)
}

// createOAuthCalendar makes a calendar on a shared-store Google or Microsoft
// account and returns its id.
func (h *Handler) createOAuthCalendar(ctx context.Context, accountID, provider, name string) (string, error) {
	if h.mailCredentials() == nil {
		return "", errors.New("mailbox OAuth is not configured for this Gofer instance")
	}
	token, err := h.mailCredentials().GetCalendarCreateTokenForAccount(ctx, accountID, provider)
	if err != nil {
		return "", err
	}
	endpoint, payload := oauthCalendarCreateRequest(provider, name)
	var created struct {
		ID string `json:"id"`
	}
	if err := calendarCreateJSON(ctx, http.MethodPost, endpoint, token, payload, &created); err != nil {
		return "", oauthCalendarCreateError(provider, err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", errors.New("the calendar service did not confirm the new calendar")
	}
	return created.ID, nil
}

// createOAuthCalendar makes a calendar on the request account's Google or
// Microsoft calendar and returns its id. It signs in with a grant for making
// calendars, which discovery's read grant is not.
func (p *userCalendarRequest) createOAuthCalendar(ctx context.Context, name string) (string, error) {
	if err := p.ready(ctx); err != nil {
		return "", err
	}
	if p.h.userCredentials == nil {
		return "", errors.New("Mailbox OAuth is not configured for this Gofer instance.")
	}
	service := p.service()
	credentials := p.h.userCredentials.CalendarCreateAccount(service.OwnerID(), service.AccountID())
	authorization, err := credentials.ServiceAuthorization(ctx, false)
	if err != nil {
		return "", p.recordRetry(ctx, err)
	}
	endpoint, payload := oauthCalendarCreateRequest(p.provider(), name)
	var created struct {
		ID string `json:"id"`
	}
	post := func() error {
		return userContactProviderJSON(ctx, p.provider(), http.MethodPost, endpoint, authorization.Token(), payload, &created, func() error { return p.ready(ctx) })
	}
	err = post()
	if contactAPIUnauthorized(err) {
		// A 401 means nothing was made, so trying once more with a fresh
		// token can't make two.
		if authorization, err = p.h.userCredentials.RefreshServiceAuthorization(ctx, authorization); err != nil {
			return "", p.recordRetry(ctx, err)
		}
		if err := p.ready(ctx); err != nil {
			return "", err
		}
		err = post()
	}
	if err != nil {
		return "", p.recordRetry(ctx, oauthCalendarCreateError(p.provider(), err))
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", errors.New("the calendar service did not confirm the new calendar")
	}
	return created.ID, nil
}

// createJMAPCalendar makes a calendar on the request account's JMAP server
// and returns the address its calendar source has.
func (p *userCalendarRequest) createJMAPCalendar(ctx context.Context, name string) (string, error) {
	cfg, password, err := p.jmapConnection(ctx)
	if err != nil {
		return "", err
	}
	if err := p.ready(ctx); err != nil {
		return "", err
	}
	id, err := mail.CreateJMAPCalendar(ctx, cfg, password, name)
	if err != nil {
		return "", p.recordRetry(ctx, err)
	}
	return jmapCalendarDAVCollection(id), nil
}

// selectCreatedCalendar ticks the calendar discovery just made.
func selectCreatedCalendar(sources []storage.CalendarSource, create *davCreate) {
	if create == nil || create.Created == "" {
		return
	}
	for i := range sources {
		if sources[i].RemoteID == create.Created || davSameCollection(sources[i].RemoteID, create.Created) {
			sources[i].IsSelected = true
		}
	}
}

// calendarCreateAccess is whether the account may make calendars: Google
// grants from before Gofer asked for it need a reconnect first. Accounts
// that don't sign in with OAuth have no say here.
func calendarCreateAccess(provider string, allowed func() bool) string {
	if provider != providers.ProviderGmail && provider != providers.ProviderOutlook {
		return ""
	}
	if allowed() {
		return models.CalendarCreateAllowed
	}
	return models.CalendarCreateNeedsReconnect
}

// markCalendarCreateAccess fills in whether a shared-store account may make
// calendars.
func (h *Handler) markCalendarCreateAccess(ctx context.Context, data *models.EditAccountData) {
	if data == nil {
		return
	}
	data.CalendarSync.CreateAccess = calendarCreateAccess(data.Provider, func() bool {
		return h.mailCredentials() != nil && h.mailCredentials().CalendarCreateAuthorized(ctx, data.AccountID, data.Provider)
	})
}

// markUserCalendarCreateAccess fills in whether a per-user account may make
// calendars.
func (h *Handler) markUserCalendarCreateAccess(ctx context.Context, owner string, data *models.EditAccountData) {
	if data == nil {
		return
	}
	data.CalendarSync.CreateAccess = calendarCreateAccess(data.Provider, func() bool {
		return h.userCredentials != nil && h.userCredentials.CalendarCreateAuthorizedForUser(ctx, owner, data.AccountID)
	})
}
