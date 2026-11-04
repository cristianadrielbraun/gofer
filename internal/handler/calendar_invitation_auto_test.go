package handler

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
	"github.com/emersion/go-msgauth/dkim"
)

// Asked for, an invitation someone else sent on can be added, but it never
// changes an event already in the calendar.
func TestForwardedMailInvitation(t *testing.T) {
	raw := invitationTestMail("friend@example.org", "REQUEST", invitationTestEvent("0", ""))
	if _, err := readMailInvitation(raw, "invite-uid", "guest@example.com", false); err == nil {
		t.Fatal("forwarded invitation read as the organizer's")
	}
	invitation, err := readMailInvitation(raw, "invite-uid", "guest@example.com", true)
	if err != nil || invitation.organizer != "host@example.com" || invitation.forwardedBy != "friend@example.org" {
		t.Fatalf("forwarded: %+v %v", invitation, err)
	}
	own := invitationTestMail("friend@example.org", "REQUEST", strings.Replace(invitationTestEvent("0", ""), "ORGANIZER;CN=Host:mailto:host@example.com", "ORGANIZER:mailto:guest@example.com", 1))
	if _, err := readMailInvitation(own, "invite-uid", "guest@example.com", true); err == nil {
		t.Fatal("own meeting forwarded back accepted")
	}
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: "https://dav.example.com/cal/", AccessRole: "owner"}
	credentials := calendarCredentials{baseURL: source.RemoteID}
	var refusal calendarInvitationRefusal
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation, storage.CalendarEvent{ID: "cached"}); !errors.As(err, &refusal) {
		t.Fatalf("forwarded change: %v", err)
	}

	sources := []storage.CalendarSource{{ID: "source", AccountID: "account", Provider: "caldav", Name: "Work", AccessRole: "owner"}}
	cached := []storage.CalendarEvent{{ID: "cached", SourceProvider: "caldav"}}
	item := mailCalendarEvent{UID: "invite-uid", Method: "REQUEST", Organizer: "host@example.com", From: "friend@example.org", Invited: true}
	if offer := mailCalendarInvitationOffer(context.Background(), item, nil, sources, "account", "guest@example.com"); offer == nil || offer.SourceID != "source" || !strings.HasPrefix(offer.Note, "Forwarded by friend@example.org.") {
		t.Fatalf("forwarded offer: %+v", offer)
	}
	item.Modified = &time.Time{}
	if offer := mailCalendarInvitationOffer(context.Background(), item, cached, sources, "account", "guest@example.com"); offer != nil {
		t.Fatalf("forwarded update offered: %+v", offer)
	}
	item.Method = "CANCEL"
	if offer := mailCalendarInvitationOffer(context.Background(), item, cached, sources, "account", "guest@example.com"); offer != nil {
		t.Fatalf("forwarded cancellation offered: %+v", offer)
	}
	item.From = "host@example.com"
	if offer := mailCalendarInvitationOffer(context.Background(), item, cached, sources, "account", "guest@example.com"); offer == nil || offer.Label != "Remove from calendar" {
		t.Fatalf("organizer's cancellation: %+v", offer)
	}
}

func signedInvitationTestMail(t *testing.T, h *Handler, raw []byte, domain string) []byte {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	h.calendarIncomingLookupTXT = func(context.Context, string) ([]string, error) {
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}, nil
	}
	var output bytes.Buffer
	if err := dkim.Sign(&output, bytes.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "test", Signer: key}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

// Gofer adds by itself only what its organizer sent, in mail their domain
// signed; the rest waits on the card.
func TestImportMailInvitationsOnlyFromSigningOrganizers(t *testing.T) {
	h := &Handler{}
	offer := func(item mailCalendarEvent) (*views.MailCalendarInvitation, error) {
		return &views.MailCalendarInvitation{Label: "Add to calendar", UID: item.UID, SourceID: "source"}, nil
	}
	run := func(raw []byte) ([]string, error) {
		var applied []string
		err := h.importMailInvitations(t.Context(), raw, mailCalendarEvents(raw, "guest@example.com", time.UTC), "guest@example.com", offer, func(invitation mailInvitation, uid, sourceID string) error {
			applied = append(applied, uid+" "+sourceID+" "+invitation.method)
			return nil
		})
		return applied, err
	}

	raw := invitationTestMail("host@example.com", "REQUEST", invitationTestEvent("0", ""))
	applied, err := run(signedInvitationTestMail(t, h, raw, "example.com"))
	if err != nil || len(applied) != 1 || applied[0] != "invite-uid source REQUEST" {
		t.Fatalf("signed: %v %v", applied, err)
	}
	if applied, err := run(raw); !errors.Is(err, message.ErrCalendarReplyAuthentication) || len(applied) != 0 {
		t.Fatalf("unsigned: %v %v", applied, err)
	}
	if applied, err := run(signedInvitationTestMail(t, h, raw, "example.net")); !errors.Is(err, message.ErrCalendarReplyAuthentication) || len(applied) != 0 {
		t.Fatalf("signed by another domain: %v %v", applied, err)
	}
	forwarded := signedInvitationTestMail(t, h, invitationTestMail("friend@example.org", "REQUEST", invitationTestEvent("0", "")), "example.org")
	var refusal calendarInvitationRefusal
	if applied, err := run(forwarded); !errors.As(err, &refusal) || len(applied) != 0 {
		t.Fatalf("forwarded: %v %v", applied, err)
	}
	// Nothing to do: the card offers nothing.
	nothing := func(mailCalendarEvent) (*views.MailCalendarInvitation, error) { return nil, nil }
	if err := h.importMailInvitations(t.Context(), raw, mailCalendarEvents(raw, "guest@example.com", time.UTC), "guest@example.com", nothing, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCalendarInvitationOutcome(t *testing.T) {
	refusal := calendarInvitationRefusal("this account isn't invited")
	for name, test := range map[string]struct {
		err      error
		attempts int
		want     string
	}{
		"done":              {nil, 0, "complete"},
		"refused":           {refusal, 0, "ignored"},
		"unsigned":          {message.ErrCalendarReplyAuthentication, 0, "ignored"},
		"refused twice":     {errors.Join(refusal, refusal), 0, "ignored"},
		"refused, offline":  {errors.Join(refusal, errors.New("offline")), 0, "retry"},
		"signature pending": {message.ErrCalendarReplyAuthenticationTemporary, 0, "retry"},
		"tried enough":      {errors.New("offline"), calendarInvitationMaxAttempts - 1, "ignored"},
	} {
		if state, _ := calendarInvitationOutcome(test.err, test.attempts); state != test.want {
			t.Errorf("%s: %s, want %s", name, state, test.want)
		}
	}
}

// Outlook names its zones the Windows way; the invitation is read in the
// zone's IANA name, its rules kept.
func TestMailInvitationWithWindowsZone(t *testing.T) {
	zone := "BEGIN:VTIMEZONE\r\nTZID:Central Europe Standard Time\r\nBEGIN:STANDARD\r\nDTSTART:16010101T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nRRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=10\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:16010101T020000\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nRRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=3\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\n"
	event := strings.NewReplacer("DTSTART:20261002T070000Z", "DTSTART;TZID=Central Europe Standard Time:20261111T150000", "DTEND:20261002T080000Z", "DTEND;TZID=Central Europe Standard Time:20261111T153000").Replace(invitationTestEvent("0", ""))
	invitation, err := readMailInvitation(invitationTestMail("host@example.com", "REQUEST", zone+event), "invite-uid", "guest@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := encodeCalendarReply(calendarInvitationResource(invitation))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "DTSTART;TZID=Europe/Budapest:20261111T150000") || !strings.Contains(body, "TZID:Europe/Budapest") || strings.Contains(body, "Central Europe Standard Time") || !strings.Contains(body, "BYMONTH=10") {
		t.Fatalf("zone: %s", body)
	}
}
