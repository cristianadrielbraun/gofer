package message

import (
	"bytes"
	"context"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestParseDraftAddressListKeepsValidEntries(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"ana@example.com, Bruno Costa <bruno@example.com>", []string{"ana@example.com", "bruno@example.com"}},
		{"ana@example.com, bru", []string{"ana@example.com"}},
		{"bru, \"Costa, Bruno\" <bruno@example.com>", []string{"bruno@example.com"}},
		{"Ana (home, work) <ana@example.com>, x y, cara@example.com", []string{"ana@example.com", "cara@example.com"}},
		{"bru", nil},
	} {
		got := ParseDraftAddressList(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("ParseDraftAddressList(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i].Address != tc.want[i] {
				t.Fatalf("ParseDraftAddressList(%q)[%d] = %q, want %q", tc.in, i, got[i].Address, tc.want[i])
			}
		}
	}
}

func TestParseMessageReadsBcc(t *testing.T) {
	raw, err := BuildMIMEMessageForIMAPDraft(&OutgoingMessage{
		FromEmail: "me@example.com",
		To:        ParseDraftAddressList("ana@example.com"),
		Bcc:       ParseDraftAddressList("Bruno Costa <bruno@example.com>"),
		Subject:   "Plans",
		TextBody:  "Hello",
	}, "revision")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMessage(context.Background(), bytes.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.BCC) != 1 || parsed.BCC[0].Email != "bruno@example.com" || parsed.BCC[0].Name != "Bruno Costa" {
		t.Fatalf("BCC = %+v", parsed.BCC)
	}
}
