package config

import (
	"context"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type AccountCreationRecoveryResult struct{ Activated, Canceled int }

// RecoverPendingAccountCreations runs before owned provider workers start. Each
// bounded page contains central metadata only; each operation releases its store
// before the next and has a deadline for cache/writer waits. Any unverifiable
// reservation stops startup and remains durable for repair and retry.
func (s *UserAccountStore) RecoverPendingAccountCreations(ctx context.Context) (result AccountCreationRecoveryResult, err error) {
	after := ""
	for {
		page, err := s.routing.PendingAccountCreationPage(ctx, after, 64)
		if err != nil {
			return result, err
		}
		if len(page) == 0 {
			return result, nil
		}
		for _, entry := range page {
			work, cancel := context.WithTimeout(ctx, 30*time.Second)
			recovered, err := s.routing.RecoverPendingAccountCreation(work, entry.UserID, entry.AccountID)
			cancel()
			// A concurrent lifecycle change can deny admission after publication.
			// Preserve counts of already committed changes even on that error.
			if recovered.Changed {
				switch recovered.State {
				case storage.AccountActive:
					result.Activated++
				case storage.AccountDeleted:
					result.Canceled++
				}
			}
			if err != nil {
				return result, fmt.Errorf("recover pending account %s: %w", entry.AccountID, err)
			}
			after = entry.AccountID
		}
	}
}
