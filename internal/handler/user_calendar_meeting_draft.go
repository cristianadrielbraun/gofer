package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

func (p *userCalendarRequest) attachMeetingDraft(ctx context.Context, source storage.CalendarSource, draft *calendar.EventDraft, target, provider string) error {
	if p == nil || draft == nil || !p.write || p.provider() != provider || (p.create == nil) == (p.event == nil) {
		return storage.ErrCalendarCreateConflict
	}
	if p.create != nil {
		if target != "create:"+p.create.RequestID() || draft.RequestID != p.create.RequestID() {
			return storage.ErrCalendarCreateConflict
		}
	} else if provider != "gmail" || target != "event:"+p.event.Event().ID || calendarEventIsSeries(p.event.Event()) {
		return storage.ErrCalendarEventChanged
	}
	original := p.actionSource()
	if source.ID != original.ID || source.UserID != original.UserID || source.AccountID != original.AccountID || source.Provider != original.Provider || source.RemoteID != original.RemoteID {
		return storage.ErrCalendarSourceChanged
	}
	guards, err := p.meetingWriteGuards(ctx)
	if err != nil {
		return err
	}
	selected := p.source
	if selected == nil {
		selected, err = p.h.userAccounts.SnapshotCalendarSource(ctx, source.UserID, source.ID)
		if err != nil {
			return err
		}
	}
	id := draft.GoogleMeetDraftID
	if provider == "outlook" {
		id = draft.TeamsDraftID
	}
	d, err := p.h.userAccounts.SnapshotCalendarMeetingDraft(ctx, selected, id, guards...)
	if err != nil {
		return err
	}
	if provider == "gmail" && !calendarGoogleMeetConfirmed(json.RawMessage(d.Google().ConferenceJSON)) || provider == "outlook" && !calendarTeamsLinkConfirmed(json.RawMessage(d.Teams().MeetingJSON)) {
		return fmt.Errorf("meeting link is not ready")
	}
	next, err := p.h.userAccounts.BindCalendarMeetingDraft(ctx, d, p.create, p.event, guards...)
	if err != nil {
		return err
	}
	p.meeting = next
	if provider == "gmail" {
		draft.GoogleMeetConference = json.RawMessage(next.Google().ConferenceJSON)
	} else {
		draft.TeamsRemoteID = next.Teams().RemoteID
		draft.TeamsConference = json.RawMessage(next.Teams().MeetingJSON)
	}
	return p.validate(ctx)
}

func (p *userCalendarRequest) finishTeamsCreate(ctx context.Context, id string, completed *config.UserCalendarCreateClaim) error {
	if p == nil || p.source == nil || completed == nil || completed != p.create || p.provider() != "outlook" {
		return storage.ErrCalendarCreateConflict
	}
	guards, err := p.meetingWriteGuards(ctx)
	if err != nil {
		return err
	}
	d := p.meeting
	if d == nil {
		d, err = p.h.userAccounts.SnapshotCalendarMeetingDraft(ctx, p.source, id, guards...)
		if err != nil {
			return err
		}
	} else if d.Teams().DraftID != id {
		return storage.ErrCalendarCreateConflict
	}
	next, err := p.h.userAccounts.FinishCalendarTeamsCreate(ctx, d, completed, guards...)
	if err == nil {
		p.meeting = next
	}
	return err
}

func (p *userCalendarRequest) meetingWriteGuards(ctx context.Context) ([]func() error, error) {
	if p == nil || !p.write {
		return nil, storage.ErrCalendarCreateConflict
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}
	if p.provider() == storage.CalendarSourceProviderCalDAV {
		return nil, storage.ErrCalendarSourceChanged
	}
	if p.authorization == nil || p.h.userCredentials == nil {
		return nil, mailauth.ErrMailboxAuthorizationChanged
	}
	authorization, service := p.authorization, p.service()
	return []func() error{func() error {
		return p.h.userCredentials.ValidateCalendarAuthorization(ctx, authorization, service.OwnerID(), service.AccountID(), true)
	}}, nil
}
func (p *userCalendarRequest) publishMeetingDraft(ctx context.Context, kind storage.CalendarMeetingDraftTransition, value string) error {
	guards, err := p.meetingWriteGuards(ctx)
	if err != nil {
		return err
	}
	next, err := p.h.userAccounts.PublishCalendarMeetingDraft(ctx, p.meeting, kind, value, guards...)
	if err == nil {
		p.meeting = next
	}
	return err
}
func (h *Handler) setCalendarTeamsDraftRemote(ctx context.Context, d storage.CalendarTeamsDraft, id string) error {
	if p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); owned {
		if p.h != h || p.meeting == nil || p.meeting.Teams() != d {
			return storage.ErrCalendarCreateConflict
		}
		return p.publishMeetingDraft(ctx, storage.CalendarMeetingDraftRemote, id)
	}
	return h.db.SetCalendarTeamsDraftRemote(ctx, d, id)
}
func (h *Handler) completeCalendarTeamsDraft(ctx context.Context, d storage.CalendarTeamsDraft, meeting string) error {
	if p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); owned {
		if p.h != h || p.meeting == nil || p.meeting.Teams() != d {
			return storage.ErrCalendarCreateConflict
		}
		return p.publishMeetingDraft(ctx, storage.CalendarMeetingDraftConference, meeting)
	}
	return h.db.CompleteCalendarTeamsDraft(ctx, d, meeting)
}

func (h *Handler) handleUserCalendarGoogleMeetDraft(w http.ResponseWriter, r *http.Request) {
	h.userCalendarMeetingDraftRequest(w, r, "gmail", false)
}
func (h *Handler) handleUserCalendarTeamsDraft(w http.ResponseWriter, r *http.Request) {
	h.userCalendarMeetingDraftRequest(w, r, "outlook", false)
}
func (h *Handler) handleUserCalendarTeamsDraftDiscard(w http.ResponseWriter, r *http.Request) {
	h.userCalendarMeetingDraftRequest(w, r, "outlook", true)
}

func (h *Handler) userCalendarMeetingDraftRequest(w http.ResponseWriter, r *http.Request, provider string, discard bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	fail := func(status int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.URL.RawQuery != "" || r.ParseForm() != nil {
		fail(400, "Could not read the meeting request.")
		return
	}
	for name, values := range r.PostForm {
		if len(values) != 1 || (name != "source_id" && name != "draft_id" && (name != "event_id" || provider != "gmail")) {
			fail(400, "Invalid meeting request.")
			return
		}
	}
	id, err := uuid.Parse(r.PostForm.Get("draft_id"))
	if err != nil || id == uuid.Nil || len(r.PostForm.Get("draft_id")) > 64 || r.PostForm.Get("source_id") == "" || len(r.PostForm.Get("source_id")) > 4096 || len(r.PostForm.Get("event_id")) > 128 {
		fail(400, "Invalid meeting request.")
		return
	}
	owner := h.userID(r.Context())
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	status := http.StatusOK
	var response map[string]any
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		source, err := h.userAccounts.SnapshotCalendarSource(ctx, owner, r.PostForm.Get("source_id"))
		if err != nil {
			return err
		}
		if source.Source().Provider != provider {
			return userCalendarFormFailure{404, "This calendar is unavailable."}
		}
		if !calendarSourceWritable(source.Source()) || !h.userCalendarWriteAuthorized(ctx, source.Source()) {
			return userCalendarFormFailure{403, "Reconnect this account from Accounts to grant Calendar write access."}
		}
		p := &userCalendarRequest{h: h, source: source}
		if eventID := r.PostForm.Get("event_id"); eventID != "" {
			event, err := h.userAccounts.SnapshotCalendarEvent(ctx, owner, eventID)
			if err != nil {
				return err
			}
			if event.Source().ID != source.Source().ID {
				return userCalendarFormFailure{404, "This event is unavailable."}
			}
			edit := &userCalendarRequest{h: h, event: event}
			if err := edit.mutationAccess(ctx, false); err != nil {
				return err
			}
			if calendarStoredGoogleMeet(event.Event()) || calendarEventIsSeries(event.Event()) {
				return userCalendarFormFailure{403, "A Meet conference cannot be added to this event."}
			}
			p.meetingEvent = event
		}
		return h.userIMAP.RunAccountService(ctx, owner, source.Source().AccountID, mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, source.Source())
			if err != nil {
				return err
			}
			defer unlock()
			operation, err = p.actionContext(operation, true)
			if err != nil {
				return err
			}
			guards, err := p.meetingWriteGuards(operation)
			if err != nil {
				return err
			}
			p.meeting, err = h.userAccounts.BeginCalendarMeetingDraft(operation, source, id.String(), guards...)
			if err != nil {
				return err
			}
			response = map[string]any{"draft_id": id.String(), "source_id": source.Source().ID}
			if provider == "outlook" {
				if discard {
					if err := p.publishMeetingDraft(operation, storage.CalendarMeetingDraftAbandon, ""); err != nil {
						return err
					}
					status = http.StatusNoContent
					return nil
				}
				d := p.meeting.Teams()
				if d.State != "active" {
					return userCalendarFormFailure{409, "This Teams draft is no longer available. Toggle Teams off and on to prepare another link."}
				}
				ready, err := h.prepareCalendarTeamsDraft(operation, source.Source(), &d, p.token)
				if err != nil {
					return userCalendarFormFailure{502, "Could not prepare the Teams link. Retry, or toggle Teams off and on."}
				}
				if !ready {
					status = http.StatusAccepted
					response["pending"] = true
				} else {
					response["join_url"] = calendar.MeetingJoinURL(d.MeetingJSON)
				}
			} else {
				d := p.meeting.Google()
				if d.UsedBy != "" {
					return userCalendarFormFailure{409, "This Meet link has already been used. Toggle Google Meet off and on to generate a new one."}
				}
				if d.ConferenceJSON == "" {
					supported, err := googleCalendarMeetSupported(operation, p.token, source.Source().RemoteID)
					if err != nil || !supported {
						return userCalendarFormFailure{502, calendarGoogleMeetUnavailableMessage}
					}
					conference, err := prepareGoogleMeetDraft(operation, source.Source(), d, p.token)
					if err != nil {
						return userCalendarFormFailure{502, "Could not prepare the Google Meet link. Retry, or toggle Google Meet off and on."}
					}
					if len(conference) == 0 {
						status = http.StatusAccepted
						response["pending"] = true
						return nil
					}
					if err := p.publishMeetingDraft(operation, storage.CalendarMeetingDraftConference, string(conference)); err != nil {
						return err
					}
					d = p.meeting.Google()
				}
				// Cleanup never loses a retained ready link. Failed/ambiguous cleanup stays
				// durable for bounded background recovery and repeats identity preflight.
				cleanupCtx, finish := context.WithTimeout(operation, 3*time.Second)
				if cleanupGoogleMeetDraft(cleanupCtx, source.Source(), d, p.token) == nil {
					_ = p.publishMeetingDraft(operation, storage.CalendarMeetingDraftCleaned, "")
				}
				finish()
				response["join_url"] = calendar.MeetingJoinURL(d.ConferenceJSON)
			}
			return p.validate(operation)
		})
	})
	if err != nil {
		var form userCalendarFormFailure
		switch {
		case errors.As(err, &form):
			fail(form.status, form.message)
		case errors.Is(err, storage.ErrCalendarCreateConflict), errors.Is(err, storage.ErrCalendarSourceChanged), errors.Is(err, config.ErrAccountServicesChanged), errors.Is(err, mailauth.ErrMailboxAuthorizationChanged):
			fail(409, "Calendar configuration or meeting details changed. Reopen the event.")
		default:
			if errors.Is(err, sql.ErrNoRows) {
				fail(404, "This calendar or event is unavailable.")
			} else {
				fail(503, "Calendar is unavailable. Retry this same meeting request.")
			}
		}
		return
	}
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(response)
	}
}
