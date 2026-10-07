package handler

import "net/http"

// Only a request actually dispatched to the provider makes a response
// uncertain. Accepted or unknown writes retain the durable reservation even
// when a later readback fails with an otherwise definitive HTTP status.
type userCalendarResponseAttempt struct {
	dispatched bool
	uncertain  bool
}

func calendarResponseMutatingRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func (a *userCalendarResponseAttempt) started(r *http.Request) {
	if a != nil && calendarResponseMutatingRequest(r) {
		a.dispatched, a.uncertain = true, true
	}
}

func (a *userCalendarResponseAttempt) received(r *http.Request, status int) {
	if a == nil || !calendarResponseMutatingRequest(r) {
		return
	}
	// A definitive rejection, including the single 401 replay, permits release
	// only if no accepted/unknown write precedes it. An RSVP operation sends at
	// most one action except that rejected 401 replay; every new dispatch resets
	// uncertainty before transport starts. Read requests never change this bit.
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout {
		a.uncertain = false
	}
}
