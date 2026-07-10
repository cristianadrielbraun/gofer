package handler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOutgoingSendContextIsBoundedAndKeepsParentCancellation(t *testing.T) {
	started := time.Now()
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := outgoingSendContext(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("outgoingSendContext() has no deadline")
	}
	if remaining := deadline.Sub(started); remaining < outgoingSendTimeout-time.Second || remaining > outgoingSendTimeout+time.Second {
		t.Fatalf("outgoing send deadline = %v, want about %v", remaining, outgoingSendTimeout)
	}

	cancelParent()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("outgoing send context error = %v, want parent cancellation", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("outgoing send context did not follow parent cancellation")
	}
}

func TestParseScheduledSendWallTimeUsesLocation(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	got, err := parseScheduledSendWallTime("2026-05-20", "09", "30", loc)
	if err != nil {
		t.Fatalf("parse scheduled send wall time: %v", err)
	}
	want := time.Date(2026, 5, 20, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseScheduledSendWallTimeRejectsInvalidDate(t *testing.T) {
	_, err := parseScheduledSendWallTime("2026-13-20", "09", "30", time.UTC)
	if err == nil {
		t.Fatal("expected invalid date error")
	}
}

func TestParseScheduledSendWallTimeRejectsNonDropdownMinute(t *testing.T) {
	_, err := parseScheduledSendWallTime("2026-05-20", "09", "31", time.UTC)
	if err == nil {
		t.Fatal("expected invalid minute error")
	}
}
