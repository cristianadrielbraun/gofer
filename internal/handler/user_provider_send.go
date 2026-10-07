package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (s *userMailDeliveryScope) sendProvider(ctx context.Context, send storage.OutgoingSend) (bool, error) {
	var snapshot outgoingMessageSnapshot
	err := json.Unmarshal(send.MessageJSON, &snapshot)
	result := mail.UserProviderSendResult{Outcome: models.SendFailed}
	if err == nil && (send.Transport != s.cfg.Provider || snapshot.MessageID == "" || snapshot.CalendarReply != "" || snapshot.CalendarNotification != nil) {
		err = errors.New("unsupported routed provider outgoing message")
	}
	if err == nil {
		work, cancel := context.WithTimeout(ctx, 2*time.Minute)
		result = s.providerMail.Send(work, send.MIMEData)
		cancel()
		err = result.Error
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	status, text := storage.OutgoingSendSent, ""
	next := time.Now().Add(outgoingSendRetryDelayWithJitter(send.AttemptCount, s.runner.h.outgoingRandomValue()))
	if s.providerMail.RetryAt().After(next) {
		next = s.providerMail.RetryAt()
	}
	if err != nil || result.Outcome != models.SendSuccess {
		if err == nil {
			err = errors.New("provider delivery was not confirmed")
		}
		text, status = sanitizeOutgoingErrorText(err.Error()), storage.OutgoingSendFailed
		if result.Outcome == models.SendAmbiguous {
			status = storage.OutgoingSendAmbiguous
		} else if result.Retryable && !isPermanentOAuthError(err) && send.AttemptCount < outgoingSendMaxAttempts {
			status = storage.OutgoingSendPending
		}
	}
	if text != "" {
		err = errors.New(text)
	} // Queue/worker logs keep the same redacted provider error.
	finalErr := s.call(ctx, func(db *storage.DB) error {
		if status == storage.OutgoingSendSent {
			return db.CompleteUserProviderSend(ctx, send, s.cfg.ProviderAccountID, snapshot.MessageID, result.MessageID)
		}
		if status == storage.OutgoingSendPending {
			return db.FinishOutgoingSendWithRetry(ctx, send.ID, text, next)
		}
		return db.FinishOutgoingSendWithError(ctx, send.ID, status, text)
	})
	if finalErr != nil {
		return false, errors.Join(err, finalErr)
	}
	payload := map[string]any{"send_id": send.ID}
	eventStatus := status
	if status == storage.OutgoingSendPending {
		eventStatus = "retrying"
		payload["attempt"], payload["next_attempt_at"], payload["retry_in_seconds"] = send.AttemptCount, next.Format(time.RFC3339), retryInSeconds(time.Until(next))
	}
	s.runner.h.userIMAP.Events().Publish(mail.Event{Type: mail.EventSendResult, UserID: s.owner, AccountID: s.id, Status: eventStatus, Error: text, Payload: payload})
	return true, err
}

func (s *userMailDeliveryScope) cacheProviderSent(ctx context.Context, send storage.OutgoingSend) error {
	var providerID, folder string
	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
		return err
	}
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		providerID, err = db.UserProviderSendReceipt(ctx, send.ID, s.id)
		if err != nil {
			return err
		}
		folder, _, err = db.GetFolderIDByRole(ctx, s.id, "sent")
		return err
	}); err != nil {
		return err
	}
	if folder == "" || snapshot.MessageID == "" {
		return errors.New("provider Sent cache identity is unavailable")
	}
	if err := s.cacheSent(ctx, send, folder, snapshot); err != nil {
		return err
	}
	var drafts string
	if err := s.runner.h.userAccounts.WithAccountForUser(ctx, s.owner, s.id, func(accounts *config.AccountStore, db *storage.DB) error {
		cfg, err := accounts.GetConfig(ctx, s.id)
		if err != nil {
			return err
		}
		if cfg.Provider != s.cfg.Provider || cfg.ProviderAccountID != s.cfg.ProviderAccountID || cfg.AuthMethod != s.cfg.AuthMethod {
			return errors.New("mail delivery mailbox identity changed")
		}
		if send.DraftID != "" {
			local := &Handler{db: db, accountStore: accounts, auth: s.runner.h.auth, blobStore: s.runner.h.blobStore}
			if local.deliveredSnapshotMatchesDraft(ctx, send) {
				var err error
				drafts, err = db.DiscardIMAPDraft(ctx, s.id, send.DraftID)
				if err != nil {
					return err
				}
			}
		}
		return db.CompleteUserProviderSentCache(ctx, send, s.cfg.ProviderAccountID, snapshot.MessageID, folder, providerID)
	}); err != nil {
		return err
	}
	s.runner.h.userIMAP.Events().Publish(mail.Event{Type: mail.EventMutation, UserID: s.owner, AccountID: s.id, FolderID: folder})
	if drafts != "" {
		s.runner.h.userIMAP.Events().Publish(mail.Event{Type: mail.EventMutation, UserID: s.owner, AccountID: s.id, FolderID: drafts})
	}
	return nil
}
