package mail

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

const userFileRetention = 24 * time.Hour

// CleanupUserFiles is local housekeeping, never an account/provider operation.
// Busy owners are skipped so this cannot deadlock a reader waiting for storage.
// The exclusive file guard is acquired before the one bounded user lease.
func (s *UserIMAP) CleanupUserFiles(ctx context.Context, owner string) (result store.UserFileCleanupResult, err error) {
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return result, err
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	release, acquired := s.blobs.TryUserFileCleanup(owner)
	if !acquired {
		result.Skipped = true
		return result, nil
	}
	defer release()
	opened := false
	err = s.Routing().WithExistingUser(work, owner, func(db *storage.DB) error {
		opened = true
		return db.WithUserBlobReferences(work, owner, func(ids []string, keep map[string]bool) error {
			// A local account row alone cannot authorize filesystem traversal.
			// Central ownership/deletion state remains authoritative.
			var active []string
			for _, id := range ids {
				state, err := s.Routing().AccountStateForUser(work, owner, id)
				if err != nil {
					return err
				}
				if state == storage.AccountActive {
					active = append(active, id)
				}
			}
			var err error
			result, err = s.blobs.CleanupUserFiles(work, owner, active, keep, userFileRetention)
			return err
		})
	})
	if !opened && errors.Is(err, os.ErrNotExist) {
		// Uploads can exist before a user ever opens a mailbox/store. Only an
		// owner with no account reservations can have no durable references.
		var accounts bool
		err = s.Routing().System().Read().QueryRowContext(work, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE user_id=? AND state<>'deleted')`, owner).Scan(&accounts)
		if err == nil && accounts {
			return result, storage.ErrAccountRoute
		}
		if err == nil {
			result, err = s.blobs.CleanupUserFiles(work, owner, nil, nil, userFileRetention)
		}
	}
	return result, err
}

// A separate low-frequency sweep covers inactive owners with no mail traffic,
// including abandoned uploads made before creating an account. Pages contain
// only IDs; at most one existing user DB is leased at a time, never across waits.
func (s *UserIMAP) runFileCleanup(interval time.Duration) {
	defer s.workers.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// Let startup and mail discovery settle before opening idle stores.
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		cursor := ""
		for s.ctx.Err() == nil {
			rows, err := s.Routing().System().Read().QueryContext(s.ctx, `SELECT id FROM users WHERE id>? AND status='active' AND deletion_pending=0 AND user_type='webmail' AND is_admin=0 ORDER BY id LIMIT 64`, cursor)
			if err != nil {
				if s.ctx.Err() == nil {
					log.Printf("user file cleanup discovery: %v", err)
				}
				break
			}
			var owners []string
			for rows.Next() {
				var owner string
				if err = rows.Scan(&owner); err != nil {
					break
				}
				owners = append(owners, owner)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil || len(owners) == 0 {
				break
			}
			for _, owner := range owners {
				s.MaybeCleanupUserFiles(s.ctx, owner)
				cursor = owner
			}
		}
	}
}

// MaybeCleanupUserFiles runs at most hourly after successful passes. The
// bounded cache stores scheduling hints, not permanent user/DB state. Eviction
// only makes housekeeping eligible sooner; failed/skipped passes remain due.
func (s *UserIMAP) MaybeCleanupUserFiles(ctx context.Context, owner string) {
	s.mu.Lock()
	if s.closing || s.ctx.Err() != nil || ctx.Err() != nil || time.Since(s.fileCleanup[owner]) < time.Hour {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	result, err := s.CleanupUserFiles(ctx, owner)
	if err != nil {
		if ctx.Err() == nil && s.ctx.Err() == nil {
			log.Printf("user file cleanup: %v", err)
		}
		return
	}
	if result.Skipped {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileCleanup == nil {
		s.fileCleanup = make(map[string]time.Time)
	}
	if len(s.fileCleanup) >= 1024 {
		var oldest string
		for id, at := range s.fileCleanup {
			if oldest == "" || at.Before(s.fileCleanup[oldest]) {
				oldest = id
			}
		}
		delete(s.fileCleanup, oldest)
	}
	s.fileCleanup[owner] = time.Now()
}
