package i18n

import (
	"context"
	"sync"
)

// Work done outside a request, such as a push notification, a job's error or
// a background sync, still writes text for one user. ForUser picks that
// user's language: the one their last page used, else their saved choice,
// else English.

type userPrefs struct {
	locale                 Locale
	dateFormat, timeFormat string
}

var (
	seenUsers sync.Map // user ID -> userPrefs

	loaderMu   sync.RWMutex
	prefLoader func(ctx context.Context, userID string) (language, dateFormat, timeFormat string, ok bool)
)

// RememberUser records the language and date settings a user's request was
// rendered with, for the work done later on their behalf.
func RememberUser(userID string, locale Locale, dateFormat, timeFormat string) {
	if userID == "" {
		return
	}
	prefs := userPrefs{locale, dateFormat, timeFormat}
	if current, ok := seenUsers.Load(userID); ok && current.(userPrefs) == prefs {
		return
	}
	seenUsers.Store(userID, prefs)
}

// SetUserPrefsLoader gives ForUser a way to read a user's saved settings,
// for users with no request since Gofer started.
func SetUserPrefsLoader(load func(ctx context.Context, userID string) (language, dateFormat, timeFormat string, ok bool)) {
	loaderMu.Lock()
	prefLoader = load
	loaderMu.Unlock()
}

// ForUser returns ctx set to write in the user's language and date format.
func ForUser(ctx context.Context, userID string) context.Context {
	if cached, ok := seenUsers.Load(userID); ok && userID != "" {
		prefs := cached.(userPrefs)
		return WithDatePrefs(WithLocale(ctx, prefs.locale), prefs.dateFormat, prefs.timeFormat)
	}
	loaderMu.RLock()
	load := prefLoader
	loaderMu.RUnlock()
	locale, dateFormat, timeFormat := English, "", ""
	if load != nil && userID != "" {
		if language, df, tf, ok := load(ctx, userID); ok {
			if saved, ok := FromSetting(language); ok {
				locale = saved
			}
			dateFormat, timeFormat = df, tf
		}
	}
	return WithDatePrefs(WithLocale(ctx, locale), dateFormat, timeFormat)
}
