package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const outlookCalendarUIDHeader = "040000008200e00074c5b7101a82e008"
const outlookCalendarUIDMarker = "vCal-Uid\x01\x00\x00\x00"

// Outlook can return a hex GlobalObjectId containing the original iCalendar UID.
// Decode only the documented vCal-Uid form; native Outlook IDs stay unchanged.
// https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-asemail/9f00dbd2-c0e5-406c-8d74-52fffd0bbfd3
func mailCalendarUID(uid string) string {
	if len(uid) < 108 || len(uid) > 4096 || !strings.EqualFold(uid[:32], outlookCalendarUIDHeader) {
		return uid
	}
	data, err := hex.DecodeString(uid)
	if err != nil || len(data) < 54 || int(binary.LittleEndian.Uint32(data[36:40])) != len(data)-40 || !bytes.HasPrefix(data[40:], []byte(outlookCalendarUIDMarker)) || data[len(data)-1] != 0 {
		return uid
	}
	original := data[52 : len(data)-1]
	if len(original) == 0 || bytes.ContainsRune(original, 0) || !utf8.Valid(original) {
		return uid
	}
	return string(original)
}

// Invitations are resolved within the receiving account, never across a user's
// other accounts. Selected-calendar ownership rules match GetCalendarEvent.
func (db *DB) ListMailCalendarEvents(ctx context.Context, userID, accountID, uid string) ([]CalendarEvent, error) {
	if strings.TrimSpace(userID) == "" || accountID == "" || uid == "" {
		return nil, nil
	}
	originalUID := mailCalendarUID(uid)
	// The 40-byte header can vary in creation/instance fields. Match the exact
	// encoded payload, then validate the complete wrapper before accepting it.
	payload := hex.EncodeToString([]byte(outlookCalendarUIDMarker + originalUID + "\x00"))
	rows, err := db.Read().QueryContext(ctx, calendarEventSelect+` AND account.id=? AND (
	 event.ical_uid IN (?,?) OR (source.provider='outlook' AND lower(substr(event.ical_uid,81))=?)
	) LIMIT 257`, userID, accountID, uid, originalUID, payload)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []CalendarEvent
	for rows.Next() {
		event, err := scanCalendarEvent(rows)
		if err != nil {
			return nil, err
		}
		if event.ICalUID != uid && event.ICalUID != originalUID && (event.SourceProvider != "outlook" || mailCalendarUID(event.ICalUID) != originalUID) {
			continue
		}
		events = append(events, event)
		if len(events) > 256 {
			return nil, fmt.Errorf("too many matching calendar occurrences")
		}
	}
	return events, rows.Err()
}
