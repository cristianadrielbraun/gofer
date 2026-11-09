package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Calendar subscriptions are added, changed and removed from the Calendar
// sidebar. They live in the user's Local folders account; a new address is
// read once before it is saved, so a wrong one is reported at once.

const calendarSubscriptionDefaultColor = "#0ea5e9"

// calendarSubscriptionTestTimeout bounds reading a new address before it is
// saved.
const calendarSubscriptionTestTimeout = 45 * time.Second

var errCalendarSubscriptionRefresh = errors.New("Choose how often the calendar is refreshed.")

type calendarSubscriptionRequest struct {
	URL            string `json:"url"`
	Name           string `json:"name"`
	Color          string `json:"color"`
	RefreshMinutes int    `json:"refresh_minutes"`
}

// readCalendarSubscription reads and checks a subscription's settings.
func readCalendarSubscription(w http.ResponseWriter, r *http.Request) (storage.CalendarSubscription, error) {
	var request calendarSubscriptionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return storage.CalendarSubscription{}, errors.New("The subscription could not be read.")
	}
	address, err := normalizeCalendarFeedURL(request.URL)
	if err != nil {
		return storage.CalendarSubscription{}, err
	}
	if request.RefreshMinutes == 0 {
		request.RefreshMinutes = storage.CalendarSubscriptionDefaultRefresh
	}
	if !storage.ValidCalendarSubscriptionRefresh(request.RefreshMinutes) {
		return storage.CalendarSubscription{}, errCalendarSubscriptionRefresh
	}
	name := strings.TrimSpace(request.Name)
	if err := checkCollectionName(name); err != nil {
		return storage.CalendarSubscription{}, err
	}
	color := calendarFeedColor(request.Color)
	if strings.TrimSpace(request.Color) != "" && color == "" {
		return storage.CalendarSubscription{}, errors.New("Choose a color for the calendar.")
	}
	return storage.CalendarSubscription{URL: address, Name: name, Color: color, RefreshMinutes: request.RefreshMinutes}, nil
}

// testCalendarSubscription reads a feed before it is saved, and fills in the
// name and color it suggests where none were chosen.
func (h *Handler) testCalendarSubscription(ctx context.Context, owner string, subscription *storage.CalendarSubscription) error {
	ctx, cancel := context.WithTimeout(ctx, calendarSubscriptionTestTimeout)
	defer cancel()
	ctx, err := h.defaultProxyContext(ctx, owner)
	if err != nil {
		return err
	}
	data, err := fetchCalendarFeed(ctx, subscription.URL)
	if err != nil {
		return err
	}
	feed, err := decodeCalendarFeed(data)
	if err != nil {
		return err
	}
	var location *time.Location
	_ = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		location = viewsCalendarLocation(db.GetUISettings(ctx, owner))
		return nil
	})
	if location == nil {
		location = time.UTC
	}
	start, end := calendarBackgroundWindow(time.Now(), location)
	if _, err := calendarFeedEvents(feed, start, end, location); err != nil {
		return err
	}
	if subscription.Name == "" {
		subscription.Name = feed.name
	}
	if subscription.Name == "" {
		if u, err := url.Parse(subscription.URL); err == nil {
			subscription.Name = u.Hostname()
		}
	}
	if subscription.Color == "" {
		subscription.Color = feed.color
	}
	if subscription.Color == "" {
		subscription.Color = calendarSubscriptionDefaultColor
	}
	return nil
}

// calendarSubscriptionAccount is the owner's Local folders account, which
// holds their subscriptions, made now if they have none yet.
func (h *Handler) calendarSubscriptionAccount(ctx context.Context, owner string) (string, error) {
	if err := h.ensureLocalFolders(ctx, owner); err != nil {
		return "", err
	}
	var id string
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		id, err = db.LocalFoldersAccountIDForUser(ctx, owner)
		return err
	})
	if err == nil && id == "" {
		err = errors.New("Local folders are not available.")
	}
	return id, err
}

// calendarSubscription is one of the owner's subscriptions.
func (h *Handler) calendarSubscription(ctx context.Context, owner, sourceID string) (storage.CalendarSource, error) {
	var found *storage.CalendarSource
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(ctx, owner)
		for i := range sources {
			if sources[i].ID == sourceID && sources[i].Provider == storage.CalendarSourceProviderICS {
				found = &sources[i]
			}
		}
		return err
	})
	if err == nil && found == nil {
		err = sql.ErrNoRows
	}
	if err != nil {
		return storage.CalendarSource{}, err
	}
	return *found, nil
}

// refreshCalendarSubscription reads a subscription now, in the background.
func (h *Handler) refreshCalendarSubscription(ctx context.Context, owner, sourceID string) {
	if h.userStorage != nil {
		source, err := h.calendarSubscription(ctx, owner, sourceID)
		if err == nil {
			h.wakeUserCalendarAccount(ctx, owner, source.AccountID)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), calendarSourceTimeout+10*time.Second)
		defer cancel()
		source, err := h.calendarSubscription(ctx, owner, sourceID)
		if err != nil {
			return
		}
		start, end := calendarBackgroundWindow(time.Now(), viewsCalendarLocation(h.db.GetUISettings(ctx, owner)))
		if _, err := h.syncCalendarSource(ctx, source, start, end, false, map[string]calendarCredentials{}); err != nil && ctx.Err() == nil {
			log.Printf("calendar subscription %s: %v", sourceID, err)
		}
	}()
}

// handleCalendarSubscriptionAdd subscribes to a calendar feed.
func (h *Handler) handleCalendarSubscriptionAdd(w http.ResponseWriter, r *http.Request) {
	ctx, owner := r.Context(), h.userID(r.Context())
	subscription, err := readCalendarSubscription(w, r)
	if err != nil {
		http.Error(w, serverText(ctx, err.Error()), http.StatusBadRequest)
		return
	}
	if err := h.testCalendarSubscription(ctx, owner, &subscription); err != nil {
		http.Error(w, serverText(ctx, err.Error()), http.StatusBadGateway)
		return
	}
	accountID, err := h.calendarSubscriptionAccount(ctx, owner)
	if err != nil {
		h.calendarSubscriptionError(w, r, err)
		return
	}
	var source storage.CalendarSource
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		source, err = db.AddCalendarSubscription(ctx, owner, accountID, subscription)
		return err
	})
	if err != nil {
		h.calendarSubscriptionError(w, r, err)
		return
	}
	h.refreshCalendarSubscription(ctx, owner, source.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": source.ID, "name": source.Name})
}

// handleCalendarSubscriptionChange changes a subscription, or with DELETE
// removes it with its events.
func (h *Handler) handleCalendarSubscriptionChange(w http.ResponseWriter, r *http.Request) {
	ctx, owner := r.Context(), h.userID(r.Context())
	sourceID := strings.TrimSpace(r.PathValue("id"))
	current, err := h.calendarSubscription(ctx, owner, sourceID)
	if err != nil {
		h.calendarSubscriptionError(w, r, err)
		return
	}
	if r.Method == http.MethodDelete {
		err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
			return db.RemoveCalendarSubscription(ctx, owner, sourceID)
		})
		if err != nil {
			h.calendarSubscriptionError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	subscription, err := readCalendarSubscription(w, r)
	if err != nil {
		http.Error(w, serverText(ctx, err.Error()), http.StatusBadRequest)
		return
	}
	if subscription.URL != current.RemoteID {
		if err := h.testCalendarSubscription(ctx, owner, &subscription); err != nil {
			http.Error(w, serverText(ctx, err.Error()), http.StatusBadGateway)
			return
		}
	}
	if subscription.Name == "" {
		subscription.Name = current.Name
	}
	if subscription.Color == "" {
		subscription.Color = current.Color
	}
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		return db.UpdateCalendarSubscription(ctx, owner, sourceID, subscription)
	})
	if err != nil {
		h.calendarSubscriptionError(w, r, err)
		return
	}
	if subscription.URL != current.RemoteID {
		h.refreshCalendarSubscription(ctx, owner, sourceID)
	} else if subscription.RefreshMinutes != current.RefreshMinutes {
		h.rescheduleCalendarSubscription(ctx, owner, current, subscription.RefreshMinutes)
	}
	w.WriteHeader(http.StatusNoContent)
}

// rescheduleCalendarSubscription moves a subscription's next refresh to its
// new interval after the last one.
func (h *Handler) rescheduleCalendarSubscription(ctx context.Context, owner string, source storage.CalendarSource, minutes int) {
	next := time.Now()
	if source.LastSuccessAt != nil {
		next = source.LastSuccessAt.Add(time.Duration(minutes) * time.Minute)
	}
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		return db.ScheduleCalendarSync(ctx, owner, source.ID, next)
	})
	if err != nil {
		log.Printf("calendar subscription %s: reschedule: %v", source.ID, err)
		return
	}
	if h.userStorage != nil && !next.After(time.Now()) {
		h.wakeUserCalendarAccount(ctx, owner, source.AccountID)
	}
}

func (h *Handler) calendarSubscriptionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, serverText(r.Context(), "This calendar subscription no longer exists. Refresh the page."), http.StatusNotFound)
	case errors.Is(err, storage.ErrCalendarSubscriptionExists):
		http.Error(w, serverText(r.Context(), err.Error()), http.StatusConflict)
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, storage.ErrUserStoreOwner):
		userAccountError(w, r, err)
	default:
		log.Printf("calendar subscription: %v", err)
		http.Error(w, serverText(r.Context(), "The calendar subscription could not be saved."), http.StatusInternalServerError)
	}
}
