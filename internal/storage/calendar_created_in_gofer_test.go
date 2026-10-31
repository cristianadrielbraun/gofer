package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

// A calendar created from Gofer is remembered as such across later
// discoveries, which keep the mark.
func TestCalendarCreatedInGoferIsKept(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('gm-1', 'default', 'gmail', 'person@gmail.com');`); err != nil {
		t.Fatal(err)
	}
	sources := []CalendarSource{{RemoteID: "primary", Name: "Person", IsPrimary: true, IsSelected: true}, {RemoteID: "made@group", Name: "Made here", IsSelected: true}}
	if err := db.ReplaceCalendarSources(t.Context(), "default", "gm-1", "gmail", sources); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCalendarCreatedInGofer(t.Context(), "default", "gm-1", "made@group"); err != nil {
		t.Fatal(err)
	}
	sources[1].Name = "Renamed"
	if err := db.ReplaceCalendarSources(t.Context(), "default", "gm-1", "gmail", sources); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListSelectedCalendarSources(t.Context(), "default")
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed %+v, %v", listed, err)
	}
	for _, source := range listed {
		if source.CreatedInGofer != (source.RemoteID == "made@group") {
			t.Fatalf("%s created in Gofer = %v", source.RemoteID, source.CreatedInGofer)
		}
	}
}

// A Google or Microsoft main calendar and Google's family calendar can't be
// deleted; other calendars can.
func TestCalendarUndeletableReason(t *testing.T) {
	for _, test := range []struct {
		source CalendarSource
		want   string
	}{
		{CalendarSource{Provider: "gmail", IsPrimary: true, RemoteID: "person@gmail.com"}, "main calendar"},
		{CalendarSource{Provider: "outlook", IsPrimary: true}, "main calendar"},
		{CalendarSource{Provider: "gmail", RemoteID: "family01234567890@group.calendar.google.com"}, "family calendar"},
		{CalendarSource{Provider: "gmail", RemoteID: "club@group.calendar.google.com"}, ""},
		{CalendarSource{Provider: "caldav", IsPrimary: true, RemoteID: "family1@group.calendar.google.com"}, ""},
	} {
		got := test.source.UndeletableReason()
		if (test.want == "") != (got == "") || !strings.Contains(got, test.want) {
			t.Fatalf("%+v = %q, want %q", test.source, got, test.want)
		}
	}
}
