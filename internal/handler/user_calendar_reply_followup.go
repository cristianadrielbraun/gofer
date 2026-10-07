package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func replyAuthorityConflict(err error) bool {
	return errors.Is(err, config.ErrAccountServicesChanged) || errors.Is(err, storage.ErrCalendarEventChanged) || errors.Is(err, storage.ErrCalendarSourceChanged) || errors.Is(err, storage.ErrCalendarDiscoveryChanged) || errors.Is(err, sql.ErrNoRows)
}

// Unknown failures after a conditional PUT include lost acknowledgements and
// failed readback. Keep them pending: the next pass reads the desired resource
// before any new PUT, and can never send email through this path.
func replySaveDefinitivelyRejected(err error) bool {
	if errors.Is(err, errCalendarUpdateConflict) {
		return true
	}
	var provider calendarCreateProviderError
	return errors.As(err, &provider) && !calendarCreateUncertain(err)
}

func (h *Handler) finishUserCalendarReply(parent context.Context, owner, account, id string) (bool, error) {
	var changed bool
	var sourceID string
	err := h.userIMAP.RunUserServiceWork(parent, owner, func(ctx context.Context) error {
		return h.userIMAP.RunAccountService(ctx, owner, account, mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
			record, err := h.userAccounts.SnapshotCalendarReplyFollowupRecord(operation, owner, id)
			if err != nil {
				return err
			}
			if record.Send().AccountID != account {
				return storage.ErrAccountRoute
			}
			job := record.Job()
			sourceID = job.SourceID
			unlock, err := h.lockCalendarCreate(operation, storage.CalendarSource{UserID: owner, ID: job.SourceID})
			if err != nil {
				return err
			}
			defer unlock()
			authority, err := h.userAccounts.RestoreCalendarReply(operation, owner, id, false)
			if err != nil {
				if replyAuthorityConflict(err) {
					if markErr := h.userAccounts.MarkCalendarReplyConflict(operation, record); markErr != nil {
						return errors.Join(err, markErr)
					}
					changed = true
				}
				return err
			}
			claim, err := h.userAccounts.StartCalendarReplyFollowup(operation, authority)
			if err != nil {
				return err
			}
			p := &userCalendarRequest{h: h, source: authority.Source(), reply: authority, followup: claim}
			operation, err = p.actionContext(operation, true)
			if err != nil {
				return err
			}
			credentials, err := p.actionCredentials()
			if err != nil {
				return err
			}
			var payload calendarReplyPayload
			state := ""
			if json.Unmarshal([]byte(job.Payload), &payload) != nil {
				state = "conflict"
			} else {
				desired, decodeErr := calendarUpdateDecodeICS([]byte(payload.Calendar))
				endpoint, endpointErr := calDAVResponseEndpoint(authority.Source().Source(), job.ResourceID)
				if decodeErr != nil || endpointErr != nil {
					state = "conflict"
				} else {
					current, headers, readErr := calendarUpdateCalDAVGet(operation, calendarDeleteClient(calDAVHTTPTransport), endpoint, credentials.username, credentials.password)
					switch {
					case readErr != nil:
						if replySaveDefinitivelyRejected(readErr) {
							state = "conflict"
						} else {
							return readErr
						}
					case calDAVResponseResourceMatches(desired, current):
						state = "complete"
					case headers.Get("ETag") != job.Version:
						state = "conflict"
					default:
						target := calendarResponseTarget{Event: payload.Target, Scope: payload.Scope, Endpoint: endpoint, HTTPETag: job.Version, SelfEmail: payload.SelfEmail, CalDAV: &calDAVResponseTarget{Username: credentials.username, Password: credentials.password, ScheduleTag: headers.Get("Schedule-Tag")}}
						_, saveErr := putCalDAVResponse(operation, target, desired, job.Response)
						if saveErr == nil {
							state = "complete"
						} else if replySaveDefinitivelyRejected(saveErr) {
							state = "conflict"
						} else {
							return saveErr
						}
					}
				}
			}
			if err := h.userAccounts.FinishCalendarReplyFollowup(operation, claim, state); err != nil {
				return err
			}
			changed = true
			return nil
		})
	})
	if changed {
		h.userIMAP.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: owner, AccountID: account, Payload: map[string]any{"source_id": sourceID}})
	}
	return changed, err
}

// Called from the existing bounded Calendar account worker. Replies do not
// obtain another mail gate and never hold a local store while waiting on HTTP.
func (h *Handler) runUserCalendarReplyFollowups(ctx context.Context, owner, account string) error {
	var jobs []storage.CalendarReplyJob
	if err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		jobs, err = db.ListAccountCalendarReplyFollowups(ctx, owner, account, 5)
		return err
	}); err != nil {
		return err
	}
	var failures error
	for _, job := range jobs {
		_, err := h.finishUserCalendarReply(ctx, owner, account, job.ID)
		failures = errors.Join(failures, err)
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
	}
	return failures
}
