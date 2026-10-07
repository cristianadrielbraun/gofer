package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleUserContactSyncSetup(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	var snapshot *config.UserContactEditSnapshot
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		var err error
		snapshot, err = h.userAccounts.SnapshotContactSetup(ctx, owner, id)
		if err != nil {
			return err
		}
		return h.userAccounts.ValidateContactSetup(ctx, snapshot)
	})
	if err != nil {
		userContactEditError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.ContactSyncSetupDialog(models.ContactSyncSetup{Contact: snapshot.Contact(), Phase: "searching", SearchMode: "automatic"}).Render(r.Context(), w); err != nil {
		http.Error(w, "Could not render contact sync setup", http.StatusInternalServerError)
	}
}

type userContactPreviewProviderError struct{ err error }

func (e userContactPreviewProviderError) Error() string { return e.err.Error() }
func (e userContactPreviewProviderError) Unwrap() error { return e.err }

func (h *Handler) selectedUserContactValue(ctx context.Context, owner string, snapshot *config.UserContactPreviewSnapshot, selection storage.ContactSetupSelection, service *config.AccountServiceSnapshot) (storage.ContactSetupCandidateValue, error) {
	value := storage.ContactSetupCandidateValue{AccountID: selection.AccountID, RemoteID: selection.RemoteID}
	if stored := snapshot.StoredContact(selection.AccountID); stored != nil {
		value.Contact = *stored
		return value, nil
	}
	if h.userCredentials == nil {
		return value, userContactPreviewProviderError{errors.New("mailbox OAuth is unavailable")}
	}
	if (selection.Provider == "gmail" && !validOwnedGoogleContactID(selection.RemoteID)) || (selection.Provider == "outlook" && !validOwnedGraphContactID(selection.RemoteID)) {
		return value, storage.ErrContactSetupInvalid
	}
	err := h.userIMAP.RunAccountService(ctx, owner, selection.AccountID, mail.AccountServiceContacts, 2*time.Minute, func(ctx context.Context) error {
		pull := &userContactPull{h: h, snapshot: service, preview: snapshot, credentials: h.userCredentials.ContactsAccount(owner, selection.AccountID)}
		switch selection.Provider {
		case "gmail":
			var person googlePerson
			query := url.Values{"personFields": {googleContactPersonFields()}}
			if err := pull.getJSON(ctx, googlePeopleAPIBaseURL+"/"+selection.RemoteID+"?"+query.Encode(), &person); err != nil {
				return userContactPreviewProviderError{err}
			}
			if person.ResourceName != selection.RemoteID {
				return userContactPreviewProviderError{errors.New("contact provider returned another remote identity")}
			}
			value.Contact, value.Etag = googleContactFromPerson(person), person.Etag
		case "outlook":
			var remote outlookContact
			query := outlookContactListQuery()
			query.Del("$top")
			if err := pull.getJSON(ctx, outlookGraphBaseURL+"/me/contacts/"+url.PathEscape(selection.RemoteID)+"?"+query.Encode(), &remote); err != nil {
				return userContactPreviewProviderError{err}
			}
			if remote.ID != selection.RemoteID {
				return userContactPreviewProviderError{errors.New("contact provider returned another remote identity")}
			}
			value.Contact, value.Etag = outlookContactFromGraph(remote), outlookContactVersion(remote)
		default:
			return storage.ErrContactSetupInvalid
		}
		return nil
	})
	return value, err
}

func (h *Handler) handleUserPreviewContactSyncSetup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid setup choices", http.StatusBadRequest)
		return
	}
	keys := map[string]string{}
	for name, values := range r.Form {
		if strings.HasPrefix(name, "candidate_") && len(values) > 0 {
			keys[strings.TrimPrefix(name, "candidate_")] = values[0]
		}
	}
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	var result storage.ContactPreviewResult
	var services []*config.AccountServiceSnapshot
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactPreview(ctx, owner, id, keys)
		if err != nil {
			return err
		}
		services = snapshot.Services()
		byAccount := map[string]*config.AccountServiceSnapshot{}
		for _, service := range services {
			byAccount[service.AccountID()] = service
		}
		var values []storage.ContactSetupCandidateValue
		for _, selection := range snapshot.Selections() {
			value, err := h.selectedUserContactValue(ctx, owner, snapshot, selection, byAccount[selection.AccountID])
			if err != nil {
				return err
			}
			values = append(values, value)
		}
		result, err = h.userAccounts.PublishContactPreview(ctx, snapshot, values)
		return err
	})
	if err != nil && result.Contact.ID == "" {
		var provider userContactPreviewProviderError
		if errors.As(err, &provider) && !errors.Is(err, storage.ErrContactEditChanged) && !errors.Is(err, config.ErrAccountServicesChanged) && !errors.Is(err, storage.ErrAccountRoute) && !errors.Is(err, storage.ErrUserStoreOwner) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "Could not use the selected contact: "+err.Error(), http.StatusBadGateway)
		} else {
			userContactEditError(w, r, err)
		}
		return
	}
	setup := models.ContactSyncSetup{Contact: result.Contact, Phase: "resolve", ConflictFields: result.Fields}
	for _, service := range services {
		setup.Locations = append(setup.Locations, models.ContactSyncSetupLocation{AccountID: service.AccountID(), Label: service.Identity().EmailAddress, Provider: service.ContactConfig().Provider})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.ContactSyncSetupResolve(setup).Render(r.Context(), w); err != nil {
		http.Error(w, "Could not render conflict review", http.StatusInternalServerError)
	}
}
