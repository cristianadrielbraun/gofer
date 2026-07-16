package handler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunAccountConnectionTestUsesAtMostThreeAttempts(t *testing.T) {
	t.Run("stops after success", func(t *testing.T) {
		attempts := 0
		err := runAccountConnectionTest(t.Context(), 0, func() error {
			attempts++
			if attempts < accountConnectionTestAttempts {
				return errors.New("temporary failure")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("runAccountConnectionTest() error = %v", err)
		}
		if attempts != accountConnectionTestAttempts {
			t.Fatalf("attempts = %d, want %d", attempts, accountConnectionTestAttempts)
		}
	})

	t.Run("returns the third failure", func(t *testing.T) {
		wantErr := errors.New("still unavailable")
		attempts := 0
		err := runAccountConnectionTest(t.Context(), 0, func() error {
			attempts++
			return wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("runAccountConnectionTest() error = %v, want %v", err, wantErr)
		}
		if attempts != accountConnectionTestAttempts {
			t.Fatalf("attempts = %d, want %d", attempts, accountConnectionTestAttempts)
		}
	})

	t.Run("stops when canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		attempts := 0
		err := runAccountConnectionTest(ctx, 0, func() error {
			attempts++
			return errors.New("should not run")
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runAccountConnectionTest() error = %v, want context.Canceled", err)
		}
		if attempts != 0 {
			t.Fatalf("attempts = %d, want 0", attempts)
		}
	})

	t.Run("stops between attempts when canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		attempts := 0
		err := runAccountConnectionTest(ctx, time.Hour, func() error {
			attempts++
			cancel()
			return errors.New("temporary failure")
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runAccountConnectionTest() error = %v, want context.Canceled", err)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}
	})
}
