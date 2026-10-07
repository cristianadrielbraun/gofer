package mail

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// WakeMutations follows a local queue commit, after releasing the lease. The
// durable deadline survives queue saturation; Start enables its discovery.
func (s *UserIMAP) WakeMutations(ctx context.Context, owner, id string) error {
	state, err := s.Routing().AccountStateForUser(ctx, owner, id)
	if err != nil {
		return err
	}
	if state != storage.AccountActive {
		return storage.ErrAccountRoute
	}
	resetErr := s.Routing().ResetAccountPolling(ctx, owner, id)
	s.requestDiscovery()
	// Still try the memory queue after a metadata write failure. The local
	// operation is already durable and the next receive/startup can recover it.
	return errors.Join(resetErr, s.QueueAccount(ctx, id))
}

// Replay is serialized with receiving by the account gate and shares the global
// session limit. One fresh claim at a time avoids using stale folder identities
// after a move advances other queued flags. A pass is bounded for fairness.
func (s *UserIMAP) replayMutations(ctx context.Context, scope *userIMAPScope) error {
	if err := scope.call(ctx, func(db *storage.DB) error {
		_, err := db.RecoverMessageMutationsForAccount(ctx, scope.id)
		return err
	}); err != nil {
		return err
	}
	var failures error
	for i := 0; i < 25; i++ {
		var claimed []storage.MessageMutation
		if err := scope.call(ctx, func(db *storage.DB) error {
			var err error
			claimed, err = db.ClaimDueMessageMutationsForAccount(ctx, scope.id, time.Now(), 1)
			return err
		}); err != nil {
			return errors.Join(failures, err)
		}
		if len(claimed) == 0 {
			return failures
		}
		mutation := claimed[0]
		work, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := s.applyMutation(work, scope, mutation)
		cancel()
		if ctx.Err() != nil {
			// Leave the durable claim for the next serialized pass/startup. Never
			// bypass routed authorization using a background database write.
			return errors.Join(failures, ctx.Err())
		}
		if err != nil {
			attempt := min(max(mutation.AttemptCount, 1), 7)
			next := time.Now().Add(30 * time.Second * time.Duration(1<<(attempt-1)))
			if saveErr := scope.call(ctx, func(db *storage.DB) error {
				if scope.retryAt.After(next) {
					next = scope.retryAt
				}
				return db.FinishMessageMutationAttempt(ctx, mutation, err.Error(), next)
			}); saveErr != nil {
				return errors.Join(failures, err, saveErr)
			}
			failures = errors.Join(failures, fmt.Errorf("queued %s: %w", mutation.Kind, err))
			if scope.retryAt.After(time.Now()) {
				return failures
			}
		}
	}
	return failures
}

func (s *UserIMAP) applyMutation(ctx context.Context, scope *userIMAPScope, mutation storage.MessageMutation) error {
	var info *storage.MessageMutationInfo
	var destination string
	var validity uint32
	if err := scope.call(ctx, func(db *storage.DB) error {
		var err error
		if mutation.Kind == storage.MessageMutationMove || mutation.Kind == storage.MessageMutationDelete {
			info, err = db.GetMessageMutationInfoIncludingDeletedInFolder(ctx, mutation.MessageID, mutation.FolderID)
		} else if mutation.FolderID == "" {
			info, err = db.GetMessageMutationInfoInternal(ctx, mutation.MessageID)
		} else {
			info, err = db.GetMessageMutationInfoInFolder(ctx, mutation.MessageID, mutation.FolderID)
		}
		if err != nil || info == nil {
			return err
		}
		validity, err = db.GetFolderUIDValidity(ctx, mutation.FolderID)
		if err == nil && mutation.Kind == storage.MessageMutationMove {
			destination, err = db.GetFolderRemoteID(ctx, mutation.DestinationFolderID)
		}
		return err
	}); err != nil {
		return err
	}
	if info == nil {
		return scope.call(ctx, func(db *storage.DB) error { return db.DiscardMessageMutation(ctx, mutation.ID) })
	}
	if scope.config.Provider == "gmail" || scope.config.Provider == "outlook" {
		return s.applyProviderMutation(ctx, scope, mutation, *info)
	}
	if info.AccountID != scope.id || mutation.AccountID != scope.id || info.AccountProvider != "imap" || mutation.ProviderType != storage.MessageMutationProviderIMAP {
		return errors.New("queued mutation account or provider changed")
	}
	if info.FolderRemoteID == "" {
		return errors.New("message has no remote IMAP folder identity")
	}
	if mutation.Kind == storage.MessageMutationMove && destination == "" {
		return errors.New("destination has no remote IMAP folder identity")
	}
	client, err := imap.NewContextClient(ctx, scope.config, scope.password)
	if err != nil {
		return err
	}
	defer client.Close()
	uid := info.RemoteUID
	if uid == 0 {
		uid, validity, err = client.FindUIDByMessageIDWithValidity(ctx, info.FolderRemoteID, info.InternetMessageID)
		if err != nil {
			return err
		}
	}
	alreadyGone := false
	switch mutation.Kind {
	case storage.MessageMutationRead, storage.MessageMutationStarred:
		if uid == 0 {
			return errors.New("message has no remote IMAP identity")
		}
		op, flag := goimap.StoreFlagsDel, goimap.FlagSeen
		if mutation.TargetValue {
			op = goimap.StoreFlagsAdd
		}
		if mutation.Kind == storage.MessageMutationStarred {
			flag = goimap.FlagFlagged
		}
		err = client.StoreFlagsIfUIDValidity(ctx, info.FolderRemoteID, uid, op, []goimap.Flag{flag}, validity)
	case storage.MessageMutationDelete:
		if uid == 0 {
			alreadyGone = true
			break
		}
		if mutation.SourceUIDValidity != 0 {
			validity = mutation.SourceUIDValidity
		}
		if validity == 0 {
			return errors.New("IMAP UIDVALIDITY is unavailable; waiting for folder sync before deleting")
		}
		var changed bool
		changed, err = client.DeleteMessagesIfUIDValidity(ctx, info.FolderRemoteID, []uint32{uid}, validity)
		if err == nil && changed {
			err = imap.ErrMutationUIDValidityChanged
		}
	case storage.MessageMutationMove:
		var destinationUID uint32
		if uid != 0 {
			destinationUID, err = client.MoveMessageIfUIDValidity(ctx, info.FolderRemoteID, uid, destination, validity)
		}
		if !errors.Is(err, imap.ErrMutationUIDValidityChanged) && (err != nil || destinationUID == 0) && ctx.Err() == nil {
			recovered, _, findErr := client.FindUIDByMessageIDWithValidity(ctx, destination, info.InternetMessageID)
			if findErr == nil && recovered > 0 {
				destinationUID, err = recovered, nil
			} else if uid == 0 && err == nil {
				err = errors.New("message has no remote IMAP identity")
			}
		}
		if err == nil {
			err = scope.call(ctx, func(db *storage.DB) error {
				return db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, "", destinationUID)
			})
		}
	default:
		err = fmt.Errorf("unsupported IMAP mutation %q", mutation.Kind)
	}
	if err != nil {
		return err
	}
	return scope.call(ctx, func(db *storage.DB) error {
		if mutation.Kind == storage.MessageMutationDelete {
			if err := db.CompleteMessageDeleteMutation(ctx, mutation.ID); err != nil {
				return err
			}
			if alreadyGone {
				return db.ConfirmMessageDeleteMutation(ctx, mutation.ID)
			}
			return nil
		}
		return db.CompleteMessageMutation(ctx, mutation.ID)
	})
}
