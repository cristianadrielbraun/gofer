package message

import (
	"fmt"
	"testing"
	"time"
)

func TestParseSyncHeadersReadsEveryField(t *testing.T) {
	raw := "Date: Fri, 2 Oct 2026 12:30:00 +0200\r\n" +
		"Subject: =?UTF-8?B?5Y2I6aSQ?= on Friday\r\n" +
		"From: =?UTF-8?Q?Ren=C3=A9e_Park?= <renee@example.com>\r\n" +
		"To: Sam Lee <sam@example.com>,\r\n kim@example.com (Kim)\r\n" +
		"Cc: Team: alex@example.com, \"Lee, Jo\" <jo@example.com>;\r\n" +
		"Bcc: <boss@example.com>\r\n" +
		"Message-ID: <lunch-friday@example.com>\r\n" +
		"In-Reply-To: <plans@example.com>\r\n" +
		"References: <start@example.com> <plans@example.com>\r\n" +
		"List-Id: Lunch club <lunch.example.com>\r\n" +
		"\r\n"
	got := ParseSyncHeaders([]byte(raw))
	if got.Subject != "午餐 on Friday" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.FromName != "Renée Park" || got.FromEmail != "renee@example.com" {
		t.Errorf("From = %q <%s>", got.FromName, got.FromEmail)
	}
	if want := time.Date(2026, 10, 2, 10, 30, 0, 0, time.UTC); !got.Date.Equal(want) {
		t.Errorf("Date = %v, want %v", got.Date, want)
	}
	if got.MessageID != "lunch-friday@example.com" || got.InReplyTo != "plans@example.com" {
		t.Errorf("Message-ID %q, In-Reply-To %q", got.MessageID, got.InReplyTo)
	}
	if got.References != "<start@example.com> <plans@example.com>" || got.ListID != "Lunch club <lunch.example.com>" {
		t.Errorf("References %q, List-Id %q", got.References, got.ListID)
	}
	checkRecipients(t, "To", got.To, []Recipient{{"Sam Lee", "sam@example.com"}, {"Kim", "kim@example.com"}})
	checkRecipients(t, "Cc", got.CC, []Recipient{{"", "alex@example.com"}, {"Lee, Jo", "jo@example.com"}})
	checkRecipients(t, "Bcc", got.BCC, []Recipient{{"", "boss@example.com"}})
}

// One broken entry doesn't cost the rest of its list.
func TestParseSyncHeadersKeepsTheGoodAddressesOfABrokenList(t *testing.T) {
	cases := []struct {
		header string
		want   []Recipient
	}{
		{"Sam Lee <sam@example.com>, broken@@example.com, Kim <kim@example.com>", []Recipient{{"Sam Lee", "sam@example.com"}, {"Kim", "kim@example.com"}}},
		{"\"Lee, Jo\" <jo@example.com>, Alex Doe <alex@example.com", []Recipient{{"Lee, Jo", "jo@example.com"}}},
		{"Team: sam@example.com, nobody; kim@example.com (Kim, the second)", []Recipient{{"", "sam@example.com"}, {"Kim, the second", "kim@example.com"}}},
		{"not an address", nil},
	}
	for _, tc := range cases {
		got := ParseSyncHeaders([]byte("To: " + tc.header + "\r\n\r\n"))
		checkRecipients(t, tc.header, got.To, tc.want)
	}
}

func TestParseSyncHeadersLeavesWhatItCannotReadEmpty(t *testing.T) {
	raw := "Subject: Lunch \"on\" Friday?\r\n" +
		"Date: sometime soon\r\n" +
		"Message-ID: not an id\r\n" +
		"From: Alex <alex@example.com>\r\n" +
		"a line without a colon\r\n" +
		"To: sam@example.com\r\n" +
		"\r\n"
	got := ParseSyncHeaders([]byte(raw))
	if got.Subject != `Lunch "on" Friday?` || got.FromEmail != "alex@example.com" {
		t.Errorf("Subject %q, From %q: the fields before the broken line should stay", got.Subject, got.FromEmail)
	}
	if !got.Date.IsZero() || got.MessageID != "" {
		t.Errorf("Date %v, Message-ID %q, want both empty", got.Date, got.MessageID)
	}
	if len(got.To) != 0 {
		t.Errorf("To = %v, want nothing past the broken line", got.To)
	}
	if empty := ParseSyncHeaders(nil); fmt.Sprint(empty) != fmt.Sprint(SyncHeaders{}) {
		t.Errorf("ParseSyncHeaders(nil) = %+v", empty)
	}
}

func checkRecipients(t *testing.T, name string, got, want []Recipient) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
