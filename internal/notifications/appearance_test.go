package notifications

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestNewMailNotificationAppearance(t *testing.T) {
	data, err := os.ReadFile("testdata/appearance.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name              string
		Settings          map[string]string
		Count             int
		Payload           map[string]any
		Title, Body, Icon string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			title, body, icon := newMailNotificationAppearance(context.Background(), tc.Payload, tc.Count, tc.Settings)
			if title != tc.Title || body != tc.Body || icon != tc.Icon {
				t.Fatalf("got %q, %q, %q; want %q, %q, %q", title, body, icon, tc.Title, tc.Body, tc.Icon)
			}
		})
	}
}

func TestNewMailNotificationInitials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  map[string]any
		settings map[string]string
		want     string
	}{
		{"sender name", map[string]any{"from_name": "Dana Rivers", "from_email": "dana@example.test"}, map[string]string{"notification_icon": "avatar"}, "DR"},
		{"address only", map[string]any{"from_email": "kit@example.test"}, map[string]string{}, "KI"},
		{"gofer logo chosen", map[string]any{"from_name": "Dana Rivers"}, map[string]string{"notification_icon": "gofer"}, ""},
		{"no sender", map[string]any{}, map[string]string{"notification_icon": "avatar"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newMailNotificationInitials(tc.payload, tc.settings); got != tc.want {
				t.Fatalf("initials = %q, want %q", got, tc.want)
			}
		})
	}
}
