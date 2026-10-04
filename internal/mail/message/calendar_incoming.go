package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	stdmail "net/mail"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-msgauth/dkim"
	"golang.org/x/net/publicsuffix"
)

const CalendarIncomingMaxSize = 8 << 20

// CalendarIncomingReply is data, never instructions or replacement event
// content. Only one existing attendee's participation status may be applied.
type CalendarIncomingReply struct {
	UID, Organizer, Attendee, Status, From string
	Sequence                               int
	Stamp                                  time.Time
}

var ErrCalendarReplyAuthentication = errors.New("calendar reply sender could not be authenticated")
var ErrCalendarReplyAuthenticationTemporary = errors.New("calendar reply sender authentication is temporarily unavailable")

func calendarIncomingAddress(value string) string {
	if !strings.HasPrefix(strings.ToLower(value), "mailto:") {
		return ""
	}
	value = value[len("mailto:"):]
	a, err := stdmail.ParseAddress(value)
	if err != nil || a.Name != "" || a.Address != value || !strings.Contains(value, "@") || strings.ContainsAny(value, "\r\n?#") {
		return ""
	}
	return strings.ToLower(value)
}

// ExtractCalendarIncomingReply handles inline and attached MIME calendars,
// including transfer encodings. Forwarded message/rfc822 parts aren't replies.
func ExtractCalendarIncomingReply(raw []byte) (reply *CalendarIncomingReply, err error) {
	defer func() {
		if recover() != nil {
			reply, err = nil, fmt.Errorf("malformed calendar reply")
		}
	}()
	if len(raw) > CalendarIncomingMaxSize {
		return nil, fmt.Errorf("calendar reply message exceeds size limit")
	}
	headers, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if len(headers.Header["From"]) != 1 || len(headers.Header["Content-Type"]) != 1 || len(headers.Header["Content-Transfer-Encoding"]) > 1 {
		return nil, fmt.Errorf("ambiguous reply headers")
	}
	from, err := stdmail.ParseAddressList(headers.Header.Get("From"))
	if err != nil || len(from) != 1 {
		return nil, fmt.Errorf("ambiguous reply sender")
	}
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var calendarBody []byte
	var method string
	for count := 0; ; count++ {
		if count >= 64 {
			return nil, fmt.Errorf("too many MIME parts")
		}
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		var kind string
		var params map[string]string
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			kind, params, err = h.ContentType()
		case *mail.AttachmentHeader:
			kind, params, err = h.ContentType()
		}
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(kind, "text/calendar") {
			continue
		}
		if calendarBody != nil {
			return nil, fmt.Errorf("ambiguous calendar parts")
		}
		calendarBody, err = io.ReadAll(io.LimitReader(part.Body, (256<<10)+1))
		if err != nil || len(calendarBody) > 256<<10 {
			return nil, fmt.Errorf("invalid calendar part")
		}
		method = params["method"]
	}
	if calendarBody == nil {
		return nil, nil
	}
	decoder := ical.NewDecoder(bytes.NewReader(calendarBody))
	cal, err := decoder.Decode()
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Decode(); err != io.EOF {
		return nil, fmt.Errorf("additional calendar data")
	}
	if len(cal.Props["METHOD"]) != 1 || cal.Props.Get("METHOD").Value != "REPLY" {
		return nil, nil
	}
	if method != "" && !strings.EqualFold(method, "REPLY") {
		return nil, fmt.Errorf("calendar MIME method mismatch")
	}
	if len(cal.Events()) != 1 {
		return nil, fmt.Errorf("only single-event replies are supported")
	}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent && child.Name != ical.CompTimezone {
			return nil, fmt.Errorf("unsupported calendar component")
		}
	}
	props := cal.Events()[0].Props
	for _, name := range []string{"UID", "ORGANIZER", "ATTENDEE", "DTSTAMP"} {
		if len(props[name]) != 1 {
			return nil, fmt.Errorf("ambiguous %s", name)
		}
	}
	for _, name := range []string{"RECURRENCE-ID", "RRULE", "RDATE", "EXDATE"} {
		if len(props[name]) != 0 {
			return nil, fmt.Errorf("recurring replies are not supported")
		}
	}
	if len(props["SEQUENCE"]) > 1 {
		return nil, fmt.Errorf("ambiguous reply sequence")
	}
	sequence := 0
	if p := props.Get("SEQUENCE"); p != nil {
		sequence, err = strconv.Atoi(p.Value)
		if err != nil || sequence < 0 || sequence >= 2147483647 {
			return nil, fmt.Errorf("invalid reply sequence")
		}
	}
	stamp, err := time.Parse("20060102T150405Z", props.Get("DTSTAMP").Value)
	if err != nil || stamp.After(time.Now().Add(5*time.Minute)) {
		return nil, fmt.Errorf("invalid reply timestamp")
	}
	for _, name := range []string{"ORGANIZER", "ATTENDEE"} {
		person := props.Get(name)
		for _, param := range []string{"SENT-BY", "DELEGATED-TO", "DELEGATED-FROM", "MEMBER"} {
			if len(person.Params[param]) != 0 {
				return nil, fmt.Errorf("delegated replies are not supported")
			}
		}
		if kind := person.Params.Get("CUTYPE"); kind != "" && kind != "INDIVIDUAL" {
			return nil, fmt.Errorf("unsupported participant")
		}
	}
	person := props.Get("ATTENDEE")
	if len(person.Params["PARTSTAT"]) != 1 {
		return nil, fmt.Errorf("ambiguous participation status")
	}
	status := person.Params.Get("PARTSTAT")
	if status != "ACCEPTED" && status != "TENTATIVE" && status != "DECLINED" {
		return nil, fmt.Errorf("unsupported participation status")
	}
	organizer, attendee := calendarIncomingAddress(props.Get("ORGANIZER").Value), calendarIncomingAddress(person.Value)
	uid, err := props.Text("UID")
	if err != nil || uid == "" || len(uid) > 1024 || strings.ContainsAny(uid, "\r\n\x00") || organizer == "" || attendee == "" || !strings.EqualFold(from[0].Address, attendee) {
		return nil, fmt.Errorf("reply identity does not match its sender")
	}
	return &CalendarIncomingReply{UID: uid, Organizer: organizer, Attendee: attendee, From: strings.ToLower(from[0].Address), Status: status, Sequence: sequence, Stamp: stamp.UTC()}, nil
}

// Verify the signature ourselves; arbitrary Authentication-Results headers
// aren't a trust boundary. Reject unsigned/partial bodies and require signed
// MIME headers so unsigned decoding changes cannot alter the calendar content.
func VerifyCalendarIncomingSender(ctx context.Context, raw []byte, from string, lookup func(context.Context, string) ([]string, error)) error {
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	if len(raw) > CalendarIncomingMaxSize {
		return ErrCalendarReplyAuthentication
	}
	header, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ErrCalendarReplyAuthentication
	}
	domain := from[strings.LastIndex(from, "@")+1:]
	if !strings.Contains(from, "@") {
		return ErrCalendarReplyAuthentication
	}
	aligned := func(signer string) bool {
		if strings.EqualFold(domain, signer) {
			return true
		}
		a, e1 := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(domain))
		b, e2 := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(signer))
		return e1 == nil && e2 == nil && a == b
	}
	var lookupFailed atomic.Bool
	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{MaxVerifications: 5, LookupTXT: func(name string) ([]string, error) {
		records, err := lookup(ctx, name)
		var dnsErr *net.DNSError
		if err != nil && (!errors.As(err, &dnsErr) || !dnsErr.IsNotFound) {
			lookupFailed.Store(true)
		}
		return records, err
	}})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		if lookupFailed.Load() || ctx.Err() != nil {
			return ErrCalendarReplyAuthenticationTemporary
		}
		return ErrCalendarReplyAuthentication
	}
	temporary := false
	for _, verification := range verifications {
		if !aligned(verification.Domain) {
			continue
		}
		if verification.Err != nil {
			temporary = temporary || dkim.IsTempFail(verification.Err)
			continue
		}
		signed := map[string]bool{}
		for _, key := range verification.HeaderKeys {
			signed[strings.ToLower(key)] = true
		}
		if signed["from"] && signed["content-type"] && (header.Header.Get("Content-Transfer-Encoding") == "" || signed["content-transfer-encoding"]) {
			return nil
		}
	}
	if temporary || lookupFailed.Load() || ctx.Err() != nil {
		return ErrCalendarReplyAuthenticationTemporary
	}
	return ErrCalendarReplyAuthentication
}
