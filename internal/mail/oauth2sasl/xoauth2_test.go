package oauth2sasl

import (
	"errors"
	"testing"

	"github.com/emersion/go-sasl"
)

func TestClientStart(t *testing.T) {
	client := NewClient("person@example.com", "token")

	mech, ir, err := client.Start()
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Fatalf("mechanism = %q, want XOAUTH2", mech)
	}
	want := "user=person@example.com\x01auth=Bearer token\x01\x01"
	if string(ir) != want {
		t.Fatalf("initial response = %q, want %q", string(ir), want)
	}
}

func TestClientNextRejectsChallenge(t *testing.T) {
	client := NewClient("person@example.com", "token")

	_, err := client.Next([]byte("challenge"))
	if !errors.Is(err, sasl.ErrUnexpectedServerChallenge) {
		t.Fatalf("Next error = %v, want ErrUnexpectedServerChallenge", err)
	}
}
