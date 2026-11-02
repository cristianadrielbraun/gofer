package notifications

import (
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
			title, body, icon := newMailNotificationAppearance(tc.Payload, tc.Count, tc.Settings)
			if title != tc.Title || body != tc.Body || icon != tc.Icon {
				t.Fatalf("got %q, %q, %q; want %q, %q, %q", title, body, icon, tc.Title, tc.Body, tc.Icon)
			}
		})
	}
}
