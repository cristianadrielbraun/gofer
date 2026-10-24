package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"github.com/cristianadrielbraun/gofer/internal/views"
	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-message/mail"
)

type mailCalendarEvent struct {
	UID, Method, Summary, Location, Organizer, OrganizerName, Reply, JoinURL string
	From                                                                     string // The email's sender.
	Start, End, Recurrence                                                   *time.Time
	Modified                                                                 *time.Time // LAST-MODIFIED, or else DTSTAMP
	StartDate, EndDate, WallStart, WallEnd, Zone                             string
	AllDay, Recurring, Invited, HasRecurrence, Cancelled                     bool
}

func mustMailID(value string) int64 {
	id, _ := strconv.ParseInt(value, 10, 64)
	return id
}

func mailCalendarCard(item mailCalendarEvent, location *time.Location) views.MailCalendarEventData {
	data := views.MailCalendarEventData{Summary: item.Summary, Location: item.Location, Organizer: item.Organizer, OrganizerName: item.OrganizerName, Status: item.Reply, JoinURL: item.JoinURL, Kind: "Event"}
	if data.Summary == "" {
		data.Summary = "Untitled event"
	}
	switch item.Method {
	case "REQUEST":
		data.Kind = "Invitation"
	case "REPLY":
		data.Kind = "Invitation response"
	case "CANCEL":
		data.Kind = "Cancelled event"
		data.JoinURL = ""
	}
	if item.Cancelled {
		data.Kind, data.JoinURL = "Cancelled event", ""
	}
	if item.AllDay && item.StartDate != "" {
		if start, err := time.Parse("2006-01-02", item.StartDate); err == nil {
			data.Date = start.Format("Mon, Jan 2, 2006")
			if end, err := time.Parse("2006-01-02", item.EndDate); err == nil && end.AddDate(0, 0, -1).After(start) {
				data.Date += " - " + end.AddDate(0, 0, -1).Format("Mon, Jan 2, 2006")
			}
			data.Time = "All day"
		}
	} else if item.Start != nil {
		start := item.Start.In(location)
		data.Date = start.Format("Mon, Jan 2, 2006")
		data.Time = start.Format("15:04")
		if item.End != nil {
			end := item.End.In(location)
			if end.Format("2006-01-02") != start.Format("2006-01-02") {
				data.Time += " - " + end.Format("Mon, Jan 2, 15:04")
			} else {
				data.Time += " - " + end.Format("15:04")
			}
		}
		data.Time += " (" + location.String() + ")"
	} else {
		data.Time = item.WallStart
		if item.WallEnd != "" {
			data.Time += " - " + item.WallEnd
		}
	}
	return data
}

func mailCalendarTime(prop *ical.Prop, fallback *time.Location) (*time.Time, string) {
	if prop == nil {
		return nil, ""
	}
	copy := *prop
	copy.Params = make(ical.Params)
	for name, values := range prop.Params {
		copy.Params[name] = append([]string(nil), values...)
	}
	if prop.ValueType() == ical.ValueDate || len(prop.Value) == 8 {
		copy.Params.Set("VALUE", "DATE")
		copy.Params.Del("TZID")
		at, err := copy.DateTime(time.UTC)
		if err != nil {
			return nil, ""
		}
		return &at, ""
	}
	zone := prop.Params.Get("TZID")
	if zone != "" {
		location := outlookCalendarTimeLocation(zone)
		if zone == "GMT Standard Time" {
			location, _ = time.LoadLocation("Europe/London")
		}
		// An unknown timezone must remain wall time with its original label.
		if location == nil || (location == time.UTC && zone != "UTC" && zone != "Etc/UTC") {
			if wall, err := time.Parse("20060102T150405", prop.Value); err == nil {
				return nil, wall.Format("Mon, Jan 2, 2006 15:04") + " (" + zone + ")"
			}
			return nil, ""
		}
		copy.Params.Set("TZID", location.String())
	}
	at, err := copy.DateTime(fallback)
	if err != nil {
		return nil, ""
	}
	return &at, ""
}

// Display parsing does not authenticate mail or apply attendee changes. It also
// accepts Exchange REPLY data without ORGANIZER, unlike the mutation parser.
func mailCalendarEvents(raw []byte, accountEmail string, location *time.Location) (events []mailCalendarEvent) {
	defer func() {
		if recover() != nil {
			events = nil
		}
	}()
	if len(raw) > message.CalendarIncomingMaxSize {
		return nil
	}
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	defer reader.Close()
	var from string
	if senders, err := reader.Header.AddressList("From"); err == nil && len(senders) == 1 {
		from = strings.ToLower(senders[0].Address)
	}
	seen := make(map[string]bool)
	for count := 0; count < 64 && len(events) < 8; count++ {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		var kind, filename string
		switch header := part.Header.(type) {
		case *mail.InlineHeader:
			kind, _, _ = header.ContentType()
		case *mail.AttachmentHeader:
			kind, _, _ = header.ContentType()
			filename, _ = header.Filename()
		}
		if kind != "text/calendar" && kind != "application/ics" && kind != "application/icalendar" && !strings.HasSuffix(strings.ToLower(filename), ".ics") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part.Body, (256<<10)+1))
		if err != nil || len(data) > 256<<10 {
			continue
		}
		decoder := ical.NewDecoder(bytes.NewReader(data))
		cal, err := decoder.Decode()
		if err != nil {
			continue
		}
		if _, err := decoder.Decode(); err != io.EOF {
			continue
		}
		method, _ := cal.Props.Text("METHOD")
		for _, event := range cal.Events() {
			if len(events) >= 8 {
				break
			}
			props := event.Props
			uid, err := props.Text("UID")
			if err != nil || uid == "" || len(uid) > 1024 || len(props["UID"]) != 1 {
				continue
			}
			item := mailCalendarEvent{UID: uid, Method: strings.ToUpper(method), From: from}
			status, _ := props.Text("STATUS")
			item.Cancelled = item.Method == "CANCEL" || strings.EqualFold(status, "CANCELLED")
			item.Summary, _ = props.Text("SUMMARY")
			item.Location, _ = props.Text("LOCATION")
			for _, name := range []string{"LAST-MODIFIED", "DTSTAMP"} {
				if prop := props.Get(name); prop != nil && item.Modified == nil {
					if at, err := prop.DateTime(time.UTC); err == nil {
						item.Modified = &at
					}
				}
			}
			if organizer := props.Get("ORGANIZER"); organizer != nil {
				item.Organizer = calendarReplyAddress(organizer.Value)
				item.OrganizerName = organizer.Params.Get("CN")
			}
			start := props.Get("DTSTART")
			if start != nil {
				item.AllDay = start.ValueType() == ical.ValueDate || len(start.Value) == 8
				item.Zone = start.Params.Get("TZID")
				item.Start, item.WallStart = mailCalendarTime(start, location)
				item.End, item.WallEnd = mailCalendarTime(props.Get("DTEND"), location)
				if item.End == nil && item.WallEnd == "" && item.Start != nil {
					if duration := props.Get("DURATION"); duration != nil {
						if value, err := duration.Duration(); err == nil && value >= 0 {
							end := item.Start.Add(value)
							item.End = &end
						}
					} else if item.AllDay {
						end := item.Start.AddDate(0, 0, 1)
						item.End = &end
					}
				}
				if item.AllDay && item.Start != nil {
					item.StartDate = item.Start.Format("2006-01-02")
					if item.End != nil {
						item.EndDate = item.End.Format("2006-01-02")
					}
				}
			}
			item.Recurrence, _ = mailCalendarTime(props.Get("RECURRENCE-ID"), location)
			item.HasRecurrence = props.Get("RECURRENCE-ID") != nil
			item.Recurring = props.Get("RRULE") != nil
			key := uid + "\x00" + item.Method
			if recurrence := props.Get("RECURRENCE-ID"); recurrence != nil {
				key += "\x00" + recurrence.Value
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			for _, attendee := range props["ATTENDEE"] {
				address := calendarReplyAddress(attendee.Value)
				if address == "" {
					continue
				}
				if strings.EqualFold(address, accountEmail) {
					item.Invited = true
				}
				if item.Method == "REPLY" && item.Reply == "" {
					name := attendee.Params.Get("CN")
					if name == "" {
						name = address
					}
					switch attendee.Params.Get("PARTSTAT") {
					case "ACCEPTED":
						item.Reply = name + " accepted"
					case "TENTATIVE":
						item.Reply = name + " replied Maybe"
					case "DECLINED":
						item.Reply = name + " declined"
					}
				}
			}
			description, _ := props.Text("DESCRIPTION")
			item.JoinURL = calendar.MeetingJoinURLWithDescription("{}", item.Location+"\n"+description)
			if item.JoinURL == "" {
				for _, name := range []string{"X-GOOGLE-CONFERENCE", "X-MICROSOFT-SKYPETEAMSMEETINGURL", "URL"} {
					if prop := props.Get(name); prop != nil {
						item.JoinURL = calendar.MeetingJoinURLWithDescription("{}", prop.Value)
						if item.JoinURL != "" {
							break
						}
					}
				}
			}
			events = append(events, item)
		}
	}
	return events
}

func (h *Handler) readMailCalendarEvents(ctx context.Context, emailID string) ([]mailCalendarEvent, string, error) {
	items, accountID, _, _, err := h.readMailCalendarMessage(ctx, emailID)
	return items, accountID, err
}

// readMailCalendarMessage returns an email's calendar events, account, raw
// message and the account's address.
func (h *Handler) readMailCalendarMessage(ctx context.Context, emailID string) ([]mailCalendarEvent, string, []byte, string, error) {
	email, err := h.db.GetEmailByIDForUser(ctx, emailID, h.userID(ctx))
	if err != nil || email == nil {
		return nil, "", nil, "", fmt.Errorf("email unavailable")
	}
	info, err := h.db.GetMessageStorageInfoForUser(ctx, mustMailID(emailID), h.userID(ctx))
	if err != nil || info == nil {
		return nil, "", nil, "", fmt.Errorf("email unavailable")
	}
	var raw []byte
	if info.RawPath != "" {
		file, err := store.OpenMessageFile(info.RawPath)
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(file, message.CalendarIncomingMaxSize+1))
			file.Close()
			if err != nil {
				raw = nil
			}
		}
	}
	if raw == nil {
		fetchInfo, err := h.db.GetMessageFetchInfoForUser(ctx, mustMailID(emailID), h.userID(ctx))
		if err != nil || fetchInfo == nil {
			return nil, email.AccountID, nil, "", err
		}
		raw, err = h.fetchBodyRemote(ctx, mustMailID(emailID), fetchInfo)
		if err != nil {
			return nil, email.AccountID, nil, "", err
		}
		if len(raw) <= message.CalendarIncomingMaxSize && h.blobStore != nil {
			if path, err := h.blobStore.StoreRaw(ctx, email.AccountID, mustMailID(emailID), raw); err == nil {
				_, _ = h.db.Write().ExecContext(ctx, `UPDATE messages SET raw_path=? WHERE id=? AND account_id=?`, path, mustMailID(emailID), email.AccountID)
			}
		}
	}
	var address string
	_ = h.db.Read().QueryRowContext(ctx, `SELECT email_address FROM accounts WHERE id=? AND user_id=? AND is_deleting=0`, email.AccountID, h.userID(ctx)).Scan(&address)
	location := time.UTC
	if zone, err := time.LoadLocation(h.db.GetUISettings(ctx, h.userID(ctx))["timezone"]); err == nil {
		location = zone
	}
	return mailCalendarEvents(raw, address, location), email.AccountID, raw, address, nil
}

func (h *Handler) matchMailCalendarEvent(ctx context.Context, accountID string, item mailCalendarEvent) (storage.CalendarEvent, bool) {
	if item.HasRecurrence && item.Recurrence == nil {
		return storage.CalendarEvent{}, false
	}
	events, err := h.db.ListMailCalendarEvents(ctx, h.userID(ctx), accountID, item.UID)
	if err != nil {
		return storage.CalendarEvent{}, false
	}
	return matchMailCalendarEvents(events, item)
}

func (h *Handler) handleMailCalendarFooter(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	h.renderMailCalendarFooter(ctx, w, r.PathValue("id"), r.URL.Query().Get("refresh") == "1", "", "")
}

// renderMailCalendarFooter shows an email's calendar events; note, when set,
// replaces what the card for event uid says.
func (h *Handler) renderMailCalendarFooter(ctx context.Context, w http.ResponseWriter, emailID string, refresh bool, uid, note string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	items, accountID, _, address, err := h.readMailCalendarMessage(ctx, emailID)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if refresh && len(items) > 0 {
		start := time.Now()
		if items[0].Start != nil {
			start = *items[0].Start
		}
		month := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location())
		_, _ = h.syncCalendarScopedWindow(ctx, h.userID(ctx), month.AddDate(0, 0, -7), month.AddDate(0, 1, 7), "", accountID)
	}
	location := time.UTC
	if zone, err := time.LoadLocation(h.db.GetUISettings(ctx, h.userID(ctx))["timezone"]); err == nil {
		location = zone
	}
	sources, _ := h.db.ListSelectedCalendarSources(ctx, h.userID(ctx))
	var cards []views.MailCalendarEventData
	for _, item := range items {
		event, matched := h.matchMailCalendarEvent(ctx, accountID, item)
		card := mailCalendarEventCard(item, event, matched, location, emailID, func(event storage.CalendarEvent) bool {
			_, err := h.calendarResponseAccess(ctx, event)
			return err == nil
		})
		candidates, _ := h.db.ListMailCalendarEvents(ctx, h.userID(ctx), accountID, item.UID)
		mailCalendarInvitationCard(&card, item, candidates, sources, accountID, address, location)
		if note != "" && item.UID == uid {
			card.Note, card.Invitation = note, nil
		}
		cards = append(cards, card)
	}
	_ = views.MailCalendarFooter(emailID, cards).Render(ctx, w)
}

// mailCalendarInvitationCard adds the CalDAV add/update/remove offer to a card.
// An invitation with changes not in the calendar yet shows the email's details,
// not the calendar's older ones.
func mailCalendarInvitationCard(card *views.MailCalendarEventData, item mailCalendarEvent, candidates []storage.CalendarEvent, sources []storage.CalendarSource, accountID, address string, location *time.Location) {
	card.Invitation = mailCalendarInvitationOffer(item, candidates, sources, accountID, address)
	if card.Invitation != nil && card.Invitation.SourceID != "" {
		card.Note = "" // Instead of "could not be matched to an event".
	}
	if card.Invitation != nil && card.Invitation.SourceID == "" && item.Method == "REQUEST" {
		latest := mailCalendarCard(item, location)
		card.Summary, card.Date, card.Time, card.Location = latest.Summary, latest.Date, latest.Time, latest.Location
	}
}

// handleMailCalendarInvitation adds an email's invitation to a CalDAV
// calendar, or updates or removes the event it is for, then shows the card.
func (h *Handler) handleMailCalendarInvitation(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		http.Error(w, "The request could not be read.", http.StatusBadRequest)
		return
	}
	uid, sourceID := r.PostForm.Get("uid"), r.PostForm.Get("source_id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	emailID := r.PathValue("id")
	fail := func(message string) { h.renderMailCalendarFooter(ctx, w, emailID, false, uid, message) }
	_, accountID, raw, address, err := h.readMailCalendarMessage(ctx, emailID)
	if err != nil || uid == "" || len(uid) > 1024 {
		fail("The invitation could not be read.")
		return
	}
	// Asked for, a forwarded invitation can be added too.
	invitation, err := readMailInvitation(raw, uid, address, true)
	if err == nil {
		err = h.applyMailInvitation(ctx, accountID, invitation, uid, sourceID)
	}
	if err != nil {
		fail(mailCalendarInvitationFailure(err))
		return
	}
	h.renderMailCalendarFooter(ctx, w, emailID, true, "", "")
}

// mailInvitationProblem is a failure already worded for the email's card.
type mailInvitationProblem string

func (e mailInvitationProblem) Error() string { return string(e) }

// applyMailInvitation adds the invitation for uid to calendar sourceID, or
// updates or removes the event it is for in its own calendar.
func (h *Handler) applyMailInvitation(ctx context.Context, accountID string, invitation mailInvitation, uid, sourceID string) error {
	userID := h.userID(ctx)
	candidates, err := h.db.ListMailCalendarEvents(ctx, userID, accountID, uid)
	if err != nil {
		return mailInvitationProblem("The calendar could not be read. Try again.")
	}
	sources, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		return mailInvitationProblem("The calendar could not be read. Try again.")
	}
	source, existing, err := mailCalendarInvitationTarget(candidates, sources, accountID, sourceID)
	if err != nil {
		return err
	}
	if !h.calendarWriteAuthorized(ctx, source) {
		return mailInvitationProblem("This account can't write to its calendar. Check it in Accounts.")
	}
	unlock, err := h.lockCalendarCreate(ctx, source)
	if err != nil {
		return mailInvitationProblem("Calendar is still refreshing. Try again.")
	}
	defer unlock()
	credentials := h.calendarUpdateCredentials(ctx, source)
	if credentials.err != nil {
		return credentials.err
	}
	ctx = withJMAPCalendarDAV(ctx, source, credentials)
	_, err = applyCalDAVInvitation(ctx, source, credentials, invitation, existing)
	return err
}

func mailCalendarInvitationFailure(err error) string {
	var problem mailInvitationProblem
	if errors.As(err, &problem) {
		return string(problem)
	}
	switch {
	case errors.Is(err, errCalendarUpdateConflict):
		return "The event changed in your calendar. Refresh and try again."
	case errors.Is(err, errCalendarUpdateUnsupported):
		return err.Error()
	}
	var provider calendarCreateProviderError
	if errors.As(err, &provider) && provider.Status == http.StatusPreconditionFailed {
		return "The event changed in your calendar. Refresh and try again."
	}
	var refusal calendarInvitationRefusal
	if errors.As(err, &refusal) {
		return "Gofer can't do this: " + refusal.Error() + "."
	}
	return "The calendar didn't accept the change. Try again."
}

func matchMailCalendarEvents(events []storage.CalendarEvent, item mailCalendarEvent) (storage.CalendarEvent, bool) {
	if item.HasRecurrence && item.Recurrence == nil {
		return storage.CalendarEvent{}, false
	}
	var matches []storage.CalendarEvent
	for _, event := range events {
		if item.Organizer != "" && !strings.EqualFold(item.Organizer, event.OrganizerEmail) {
			continue
		}
		if item.Recurrence != nil {
			if item.AllDay {
				if !event.AllDay || event.StartDate != item.Recurrence.Format("2006-01-02") {
					continue
				}
			} else if event.StartAt == nil || !event.StartAt.Equal(*item.Recurrence) {
				continue
			}
		} else if len(events) > 1 {
			if item.AllDay {
				if event.StartDate != item.StartDate {
					continue
				}
			} else if item.Start == nil || event.StartAt == nil || !event.StartAt.Equal(*item.Start) {
				continue
			}
		}
		matches = append(matches, event)
	}
	if len(matches) != 1 {
		return storage.CalendarEvent{}, false
	}
	return matches[0], true
}

func mailCalendarEventCard(item mailCalendarEvent, event storage.CalendarEvent, matched bool, location *time.Location, mailID string, authorized func(storage.CalendarEvent) bool) views.MailCalendarEventData {
	card := mailCalendarCard(item, location)
	if matched {
		card.EventID = event.ID
		if card.Organizer == "" {
			card.Organizer, card.OrganizerName = event.OrganizerEmail, event.OrganizerName
		}
		if card.Date == "" && card.Time == "" {
			current := item
			current.Start, current.End, current.AllDay, current.StartDate, current.EndDate = event.StartAt, event.EndAt, event.AllDay, event.StartDate, event.EndDate
			info := mailCalendarCard(current, location)
			card.Date, card.Time = info.Date, info.Time
		}
		if item.Method == "REQUEST" {
			current := item
			current.Summary, current.Location, current.Start, current.End = event.Summary, event.Location, event.StartAt, event.EndAt
			current.AllDay, current.StartDate, current.EndDate = event.AllDay, event.StartDate, event.EndDate
			current.Cancelled = item.Cancelled || event.Status == "cancelled"
			current.Organizer, current.OrganizerName = event.OrganizerEmail, event.OrganizerName
			current.WallStart, current.WallEnd = "", ""
			card = mailCalendarCard(current, location)
			card.EventID = event.ID
			if link := calendar.MeetingJoinURLWithDescription(event.OnlineMeetingJSON, event.Description); link != "" && !current.Cancelled {
				card.JoinURL = link
			}
		}
	}
	if item.Method == "REQUEST" && item.Invited && !item.Cancelled && (!matched || event.Status != "cancelled") {
		card.Note = "This invitation could not be matched to an event in this account's selected calendars. Refresh to try again."
		if matched && item.Organizer == "" {
			card.Note = "This invitation is missing organizer details."
		} else if matched && event.ResponseStatus == "organizer" {
			card.Note = "You organize this event."
		}
		if matched && item.Organizer != "" && event.ResponseStatus != "organizer" {
			if authorized(event) {
				data := calendarResponseData(event)
				data.Ready = false
				data.MailMessageID = mailID
				if item.Recurring && item.Recurrence == nil {
					data.Scope = "series"
				}
				card.Response, card.Note = &data, ""
			} else {
				card.Note = "Responding requires Calendar write access for this account."
			}
		}
	}
	return card
}
