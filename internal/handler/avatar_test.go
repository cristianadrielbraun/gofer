package handler

import (
	"context"
	"errors"
	"testing"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
)

func TestResolveAvatarWithRetryRetriesOnce(t *testing.T) {
	attempts := 0
	image, found, err := resolveAvatarWithRetry(context.Background(), func(context.Context) (avatarresolver.Image, bool, error) {
		attempts++
		if attempts == 1 {
			return avatarresolver.Image{}, false, errors.New("temporary failure")
		}
		return avatarresolver.Image{Source: "gravatar"}, true, nil
	})
	if err != nil {
		t.Fatalf("resolveAvatarWithRetry() error = %v", err)
	}
	if !found || image.Source != "gravatar" {
		t.Fatalf("resolveAvatarWithRetry() = (%+v, %v), want found gravatar", image, found)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestResolveAvatarWithRetryStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, _, err := resolveAvatarWithRetry(ctx, func(context.Context) (avatarresolver.Image, bool, error) {
		attempts++
		cancel()
		return avatarresolver.Image{}, false, errors.New("temporary failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveAvatarWithRetry() error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
