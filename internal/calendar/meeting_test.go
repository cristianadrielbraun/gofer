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

func TestGoogleMeetJoinLinks(t *testing.T) {
	link := "https://meet.google.com/abc-defg-hij?authuser=1"
	for _, tc := range []struct{ raw, body, want string }{
		{`{"entryPoints":[{"entryPointType":"phone","uri":"tel:+420123456789"},{"entryPointType":"video","uri":"` + link + `"}]}`, "", link},
		{`{"entryPoints":[{"entryPointType":"phone","uri":"tel:+420123456789"},{"entryPointType":"more","uri":"https://example.com"}]}`, "", ""},
		{`{"entryPoints":[{"entryPointType":"video","uri":"javascript:alert(1)"}]}`, "", ""},
		{`{}`, `<p><a href="` + link + `">Join Google Meet</a></p>`, link},
		{`{}`, "Join " + link + ".", link},
		{`{}`, `<template><a href="` + link + `">Join</a></template>`, ""},
		{`{}`, `<script>"` + link + `"</script>`, ""},
		{`{}`, `<a href="https://meet.google.com.evil.example/abc-defg-hij">Join</a>`, ""},
		{`{}`, `<a href="https://user@meet.google.com/abc-defg-hij">Join</a>`, ""},
		{`{}`, "https://meet.google.com/landing", ""},
		{`{}`, "http://meet.google.com/abc-defg-hij", ""},
		{`{"isOnlineMeeting":false}`, link, ""},
		{`{"joinUrl":"https://teams.live.com/meet/123"}`, link, "https://teams.live.com/meet/123"},
	} {
		if got := MeetingJoinURLWithDescription(tc.raw, tc.body); got != tc.want {
			t.Errorf("metadata=%s body=%q got=%q want=%q", tc.raw, tc.body, got, tc.want)
		}
	}
	for _, tc := range []struct {
		url   string
		valid bool
	}{
		{link, true}, {"https://meet.google.com/lookup/team_sync", true}, {"https://meet.google.com/abc-defg-hij?x=1&y=2", true},
		{"https://meet.google.com:8443/abc-defg-hij", false}, {"https://meet.google.com/landing", false}, {"https://meet.google.com/abc-defg-hij/other", false},
	} {
		if (GoogleMeetJoinURL(tc.url) != "") != tc.valid {
			t.Errorf("incorrect join recognition for %q", tc.url)
		}
	}
}
