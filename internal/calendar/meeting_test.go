package calendar

import "testing"

func TestMeetingJoinURL(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`{"joinUrl":"https://teams.example/123?token=abc&x=1","url":"https://legacy.example/123","isOnlineMeeting":true}`, "https://teams.example/123?token=abc&x=1"},
		{`{"joinUrl":"https://teams.example/123"}`, "https://teams.example/123"},
		{`{"url":"https://legacy.example/123","isOnlineMeeting":true}`, "https://legacy.example/123"},
		{`{"joinUrl":"https://teams.example/123","url":"https://legacy.example/123","isOnlineMeeting":false}`, ""},
		{`{"joinUrl":"javascript:alert(1)"}`, ""},
		{`{"joinUrl":"//teams.example/123"}`, ""},
		{`{"joinUrl":"https://user:pass@teams.example/123"}`, ""},
		{`{"joinUrl":"https://teams.example/123\n"}`, "https://teams.example/123"},
		{`{"joinUrl":"https://teams.example/123\nx"}`, ""},
		{`{"description":"https://teams.example/123"}`, ""},
		{`{}`, ""}, {`null`, ""}, {`invalid`, ""},
	} {
		if got := MeetingJoinURL(test.raw); got != test.want {
			t.Errorf("%s: %q want %q", test.raw, got, test.want)
		}
	}
}

func TestMeetingJoinURLWithDescription(t *testing.T) {
	link := "https://teams.live.com/meet/123?p=abc&lang=es"
	body := `<p><a href="https://teams.live.com/meet/123?p=abc&amp;lang=es">Join</a></p>`
	for _, tc := range []struct{ raw, description, want string }{
		{`{}`, body, link},
		{`null`, "Join <" + link + ">", link},
		{`{"isOnlineMeeting":true,"provider":"teamsForBusiness"}`, body, link},
		{`{"joinUrl":"https://teams.microsoft.com/l/meetup-join/native"}`, body, "https://teams.microsoft.com/l/meetup-join/native"},
		{`{"isOnlineMeeting":false}`, body, ""},
		{`{}`, `<a href="https://teams.live.com/meetingOptions/123">Options</a>`, ""},
		{`{}`, `<a href="https://teams.live.com.evil.example/meet/123">Join</a>`, ""},
		{`{}`, `<a href="javascript:alert(1)">Join</a>`, ""},
		{`{}`, "No meeting link", ""},
	} {
		if got := MeetingJoinURLWithDescription(tc.raw, tc.description); got != tc.want {
			t.Errorf("metadata=%s description=%q got=%q want=%q", tc.raw, tc.description, got, tc.want)
		}
	}
}

func TestTeamsJoinURLFromDescription(t *testing.T) {
	link := "https://teams.live.com/meet/123?p=abc&lang=es"
	for _, tc := range []struct{ description, want string }{
		{`<p><a href="https://teams.live.com/meet/123?p=abc&amp;lang=es">Join meeting</a></p>`, link},
		{"Join <" + link + ">", link},
		{"Join " + link + ".", link},
		{`<a href="https://teams.live.com/free">Teams</a>`, ""},
		{`<a href="https://teams.live.com/meetingOptions/123">Options</a>`, ""},
		{`<a href="https://teams.live.com.evil.example/meet/123">Join</a>`, ""},
		{`<a href="https://user@teams.live.com/meet/123">Join</a>`, ""},
		{`<a href="javascript:alert(1)">Join</a>`, ""},
		{`<a href="http://teams.live.com/meet/123">Join</a>`, ""},
		{`<a href="https://teams.microsoft.com/l/meetup-join/meeting">Join</a>`, "https://teams.microsoft.com/l/meetup-join/meeting"},
		{`<a href="https://teams.cloud.microsoft/meet/123?p=abc">Join</a>`, "https://teams.cloud.microsoft/meet/123?p=abc"},
		{`<template><a href="https://teams.live.com/meet/123">Join</a></template>`, ""},
	} {
		if got := TeamsJoinURLFromDescription(tc.description); got != tc.want {
			t.Errorf("description=%q got=%q want=%q", tc.description, got, tc.want)
		}
	}
}
