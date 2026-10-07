package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (s *userMailDeliveryScope) providerDraft(ctx context.Context) (bool, error) {
	var op *storage.UserProviderDraftOperation
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		op, err = db.ClaimUserProviderDraft(ctx, s.id, time.Now())
		return err
	}); err != nil {
		return false, err
	}
	if op == nil {
		return false, nil
	}
	if op.State.Provider != s.cfg.Provider || op.State.MailboxSubject != s.cfg.ProviderAccountID {
		return false, errors.New("provider draft mailbox changed")
	}
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	rejected, err := s.syncProviderDraft(work, *op)
	cancel()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		s.runner.h.userIMAP.Events().Publish(mail.Event{Type: mail.EventMutation, UserID: s.owner, AccountID: s.id, FolderID: op.State.FolderID})
		return true, nil
	}
	status := "ambiguous"
	if isPermanentOAuthError(err) || !mail.UserProviderDraftRetryable(err) {
		status = "blocked"
	} else if rejected {
		status = "failed"
	}
	text := sanitizeOutgoingErrorText(err.Error())
	next := time.Now().Add(sentCopyRetryDelay(op.AttemptCount))
	if s.providerMail.RetryAt().After(next) {
		next = s.providerMail.RetryAt()
	}
	saveErr := s.call(ctx, func(db *storage.DB) error { return db.FailUserProviderDraft(ctx, *op, status, text, next, rejected) })
	return saveErr == nil, errors.Join(errors.New(text), saveErr)
}

func (s *userMailDeliveryScope) syncProviderDraft(ctx context.Context, op storage.UserProviderDraftOperation) (bool, error) {
	p := s.providerMail
	if op.Kind == "delete" {
		found, err := p.FindDrafts(ctx, op.State.DraftKey)
		if err != nil {
			return false, err
		}
		tracked := false
		for _, d := range found {
			if d.ID == op.State.RemoteID {
				tracked = true
			}
			if err := p.DeleteDraft(ctx, d); err != nil {
				return false, err
			}
		}
		if !tracked && op.State.RemoteID != "" {
			if err := p.DeleteDraft(ctx, mail.UserProviderDraft{ID: op.State.RemoteID, InternetMessageID: op.State.DraftKey, RevisionToken: op.State.RemoteRevision}); err != nil {
				return false, err
			}
		}
		return false, s.call(ctx, func(db *storage.DB) error { return db.CompleteUserProviderDraftDelete(ctx, op) })
	}
	if op.Kind != "upsert" {
		return false, fmt.Errorf("unsupported provider draft operation %s", op.Kind)
	}
	candidate := mail.UserProviderDraft{ID: op.CandidateID, MessageID: op.CandidateMessageID, InternetMessageID: op.State.DraftKey, RevisionToken: op.RevisionToken}
	if candidate.ID != "" {
		current, err := p.GetDraft(ctx, candidate.ID)
		if err != nil {
			return false, err
		}
		if current.InternetMessageID != candidate.InternetMessageID || current.RevisionToken != candidate.RevisionToken || current.MessageID != candidate.MessageID {
			return false, errors.New("confirmed provider draft revision changed")
		}
	} else {
		found, err := p.FindDrafts(ctx, op.State.DraftKey)
		if err != nil {
			return false, err
		}
		for _, d := range found {
			if d.RevisionToken == op.RevisionToken {
				if candidate.ID != "" {
					return false, errors.New("provider draft revision has duplicate identities")
				}
				candidate = d
			}
		}
		if candidate.ID == "" && op.State.LocalRevision == "" {
			// Discard removes the local row and leaves a durable delete behind this
			// immutable request. Reconcile first, then avoid creating discarded MIME.
			return false, s.call(ctx, func(db *storage.DB) error { return db.AbandonDiscardedUserProviderDraft(ctx, op) })
		}
		if candidate.ID == "" {
			remote := mail.UserProviderDraft{}
			if op.State.RemoteID != "" {
				remote, err = p.GetDraft(ctx, op.State.RemoteID)
				if mail.IsUserProviderDraftMissing(err) {
					remote, err = mail.UserProviderDraft{}, nil
				}
				if err != nil {
					return false, err
				}
				if remote.ID != "" && remote.InternetMessageID != op.State.DraftKey {
					return false, errors.New("provider draft remote message changed")
				}
			} else {
				for _, d := range found {
					if op.State.RemoteMessageID == "" || d.MessageID == op.State.RemoteMessageID {
						if remote.ID != "" {
							return false, errors.New("provider draft prior identity is ambiguous")
						}
						remote = d
					}
				}
				if remote.ID != "" {
					if err := s.call(ctx, func(db *storage.DB) error {
						return db.AdoptUserProviderDraftRemote(ctx, op, remote.ID, remote.MessageID, remote.RevisionToken)
					}); err != nil {
						return false, err
					}
					op.State.RemoteID, op.State.RemoteMessageID, op.State.RemoteRevision = remote.ID, remote.MessageID, remote.RevisionToken
				}
			}
			if remote.RevisionToken == op.RevisionToken && remote.ID != "" {
				candidate = remote
			} else {
				id := ""
				if s.cfg.Provider == "gmail" {
					id = remote.ID
				}
				if id == "" {
					if op.CreateAttempted {
						return false, mail.ErrUserProviderDraftUncertain
					}
					if err := s.call(ctx, func(db *storage.DB) error { return db.BeginUserProviderDraftCreate(ctx, op) }); err != nil {
						return false, err
					}
				}
				candidate, err = p.SaveDraft(ctx, id, op.MIMEData)
				if err != nil {
					return !errors.Is(err, mail.ErrUserProviderDraftUncertain), err
				}
			}
		}
		if err := s.call(ctx, func(db *storage.DB) error {
			return db.RecordUserProviderDraftCandidate(ctx, op, candidate.ID, candidate.MessageID)
		}); err != nil {
			return false, err
		}
	}
	// Graph replacement preserves the old version until a confirmed candidate
	// is durable. If deletion/publication fails, retries reuse that candidate.
	if op.State.RemoteID != "" && op.State.RemoteID != candidate.ID {
		if err := p.DeleteDraft(ctx, mail.UserProviderDraft{ID: op.State.RemoteID, InternetMessageID: op.State.DraftKey, RevisionToken: op.State.RemoteRevision}); err != nil {
			return false, err
		}
	}
	return false, s.call(ctx, func(db *storage.DB) error { return db.CompleteUserProviderDraftUpsert(ctx, op) })
}
