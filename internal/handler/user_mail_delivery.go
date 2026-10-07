package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

// This runner shares UserIMAP's serialized account gate, global session limit
// and cancellation. A pass is bounded; leases never span a protocol command.
type userMailDelivery struct{ h *Handler }
type userMailDeliveryScope struct {
	runner                 *userMailDelivery
	owner, id              string
	cfg                    *models.AccountConfig
	password, smtpPassword string
	providerMail           *mail.UserProviderMail
}

func (s *userMailDeliveryScope) call(ctx context.Context, fn func(*storage.DB) error) error {
	return s.runner.h.userStorage.WithAccountForUser(ctx, s.owner, s.id, func(db *storage.DB) error {
		if s.cfg != nil && s.cfg.Provider != "imap" {
			var provider, subject, method string
			if err := db.Read().QueryRowContext(ctx, `SELECT provider,COALESCE(provider_account_id,''),auth_method FROM accounts WHERE id=?`, s.id).Scan(&provider, &subject, &method); err != nil {
				return err
			}
			if provider != s.cfg.Provider || subject != s.cfg.ProviderAccountID || method != s.cfg.AuthMethod {
				return errors.New("mail delivery mailbox identity changed")
			}
		}
		return fn(db)
	})
}

func (q *userMailDelivery) Run(ctx context.Context, owner, id string, providerMail *mail.UserProviderMail) error {
	s := &userMailDeliveryScope{runner: q, owner: owner, id: id, providerMail: providerMail}
	hasWork := false
	err := q.h.userAccounts.WithAccountForUser(ctx, owner, id, func(accounts *config.AccountStore, db *storage.DB) error {
		next, err := db.NextAccountMailQueueAttempt(ctx, id)
		if err != nil {
			return err
		}
		if next.IsZero() || next.After(time.Now()) {
			return nil
		}
		hasWork = true
		s.cfg, err = accounts.GetConfig(ctx, id)
		if err != nil {
			return err
		}
		if s.cfg.Provider == "imap" && s.cfg.AuthMethod == "plain" {
			s.password, err = accounts.DecryptPassword(ctx, id)
			if err != nil {
				return err
			}
			s.smtpPassword = s.password
			if s.cfg.SmtpUsername != "" {
				password, err := accounts.DecryptSmtpPassword(ctx, id)
				if err != nil {
					return err
				}
				if password != "" {
					s.smtpPassword = password
				}
			}
		} else if s.cfg.AuthMethod != "oauth2" || (s.cfg.Provider != "gmail" && s.cfg.Provider != "outlook") || providerMail == nil {
			return errors.New("routed mail delivery provider is unavailable")
		}
		return db.RecoverAccountMailQueue(ctx, id)
	})
	if err != nil || !hasWork {
		return err
	}
	// Empty mailbox/IDLE passes keep their single-lease fast path. Housekeeping
	// has its own timer and follows actual delivery work, not every notification.
	defer q.h.userIMAP.MaybeCleanupUserFiles(ctx, owner)
	var failures error
	for _, run := range []func(context.Context) (bool, error){s.send, s.sentCopy, s.draft} {
		if s.cfg.Provider != "imap" && s.providerMail.RetryAt().After(time.Now()) {
			return failures
		}
		for i := 0; i < 5; i++ {
			more, err := run(ctx)
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return errors.Join(failures, ctx.Err())
			}
			if !more {
				break
			}
			if s.cfg.Provider != "imap" && s.providerMail.RetryAt().After(time.Now()) {
				return failures
			}
		}
	}
	return failures
}

func (s *userMailDeliveryScope) send(ctx context.Context) (bool, error) {
	var claimed []storage.OutgoingSend
	var calendarReply bool
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		claimed, err = db.ClaimDueOutgoingSendsForAccount(ctx, s.id, time.Now(), 1)
		if err == nil && len(claimed) != 0 {
			_, replyErr := db.CalendarReplyForSend(ctx, claimed[0].ID)
			calendarReply = replyErr == nil
			if replyErr != nil && !errors.Is(replyErr, sql.ErrNoRows) {
				return replyErr
			}
		}
		return err
	}); err != nil {
		return false, err
	}
	if len(claimed) == 0 {
		return false, nil
	}
	send := claimed[0]
	if calendarReply {
		return s.sendCalendarReply(ctx, send)
	}
	var calendarSnapshot outgoingMessageSnapshot
	if json.Unmarshal(send.MessageJSON, &calendarSnapshot) == nil && calendarSnapshot.CalendarNotification != nil {
		return s.sendCalendarNotification(ctx, send)
	}
	if s.cfg.Provider != "imap" {
		return s.sendProvider(ctx, send)
	}
	var snapshot outgoingMessageSnapshot
	err := json.Unmarshal(send.MessageJSON, &snapshot)
	result := models.SendFailed
	if err == nil && (send.Transport != storage.OutgoingTransportSMTP || snapshot.MessageID == "" || snapshot.CalendarReply != "" || snapshot.CalendarNotification != nil) {
		err = errors.New("unsupported routed outgoing message")
	}
	if err == nil {
		work, cancel := context.WithTimeout(ctx, 2*time.Minute)
		result, err, _ = smtpclient.SendRawMessageWithTiming(work, s.cfg, s.smtpPassword, send.EnvelopeFrom, send.EnvelopeRecipients, send.MIMEData)
		cancel()
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	} // Leave the claim for recovery, never silently resend.
	status := storage.OutgoingSendSent
	errText := ""
	retryDelay := outgoingSendRetryDelayWithJitter(send.AttemptCount, s.runner.h.outgoingRandomValue())
	nextAttempt := time.Now().Add(retryDelay)
	if err != nil || result != models.SendSuccess {
		if err == nil {
			err = errors.New("SMTP delivery was not confirmed")
		}
		errText = sanitizeOutgoingErrorText(err.Error())
		status = storage.OutgoingSendFailed
		if result == models.SendAmbiguous {
			status = storage.OutgoingSendAmbiguous
		} else if smtpclient.IsRetryable(err) && send.AttemptCount < outgoingSendMaxAttempts {
			status = storage.OutgoingSendPending
		}
	}
	finalErr := s.call(ctx, func(db *storage.DB) error {
		switch status {
		case storage.OutgoingSendSent:
			return db.CompleteOutgoingSend(ctx, send.ID, snapshot.MessageID, true)
		case storage.OutgoingSendPending:
			return db.FinishOutgoingSendWithRetry(ctx, send.ID, errText, nextAttempt)
		default:
			return db.FinishOutgoingSendWithError(ctx, send.ID, status, errText)
		}
	})
	if finalErr != nil {
		return false, errors.Join(err, finalErr)
	}
	eventStatus := status
	payload := map[string]any{"send_id": send.ID}
	if status == storage.OutgoingSendPending {
		eventStatus = "retrying"
		payload["attempt"] = send.AttemptCount
		payload["next_attempt_at"] = nextAttempt.Format(time.RFC3339)
		payload["retry_in_seconds"] = retryInSeconds(retryDelay)
	}
	s.runner.h.userIMAP.Events().Publish(mail.Event{Type: mail.EventSendResult, UserID: s.owner, AccountID: s.id, Status: eventStatus, Error: errText, Payload: payload})
	return true, err
}

func (s *userMailDeliveryScope) sentCopy(ctx context.Context) (bool, error) {
	var claimed []storage.OutgoingSend
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		claimed, err = db.ClaimDueSentCopiesForAccount(ctx, s.id, time.Now(), 1)
		return err
	}); err != nil {
		return false, err
	}
	if len(claimed) == 0 {
		return false, nil
	}
	send := claimed[0]
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var err error
	if s.cfg.Provider == "imap" {
		err = s.copySent(work, send)
	} else {
		err = s.cacheProviderSent(work, send)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		// Once claimed, any failure after APPEND may follow remote acceptance.
		// Search the stable Message-ID before every subsequent append.
		saveErr := s.call(ctx, func(db *storage.DB) error {
			return db.FinishSentCopyWithError(ctx, send.ID, storage.SentCopyAmbiguous, sanitizeOutgoingErrorText(err.Error()), time.Now().Add(sentCopyRetryDelay(send.SentCopyAttempts)))
		})
		return saveErr == nil, errors.Join(err, saveErr)
	}
	return true, nil
}

func (s *userMailDeliveryScope) copySent(ctx context.Context, send storage.OutgoingSend) error {
	var folder, remote string
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		folder, remote, err = db.GetFolderIDByRole(ctx, s.id, "sent")
		return err
	}); err != nil {
		return err
	}
	if folder == "" || remote == "" {
		return errors.New("remote Sent folder is not available")
	}
	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
		return err
	}
	if snapshot.MessageID == "" {
		return errors.New("Sent message identity is unavailable")
	}
	client, err := imapclient.NewContextClient(ctx, s.cfg, s.password)
	if err != nil {
		return err
	}
	defer client.Close()
	// SMTP servers may already have stored their own Sent copy. Search on the
	// first pass as well as after uncertainty, preserving one stable identity.
	uid, validity, err := client.FindUIDByMessageIDWithValidity(ctx, remote, snapshot.MessageID)
	if err != nil {
		return err
	}
	if uid == 0 {
		result, err := client.AppendMessage(ctx, remote, send.MIMEData, []goimap.Flag{goimap.FlagSeen}, snapshot.Date)
		if err != nil {
			return err
		}
		uid, validity = result.UID, result.UIDValidity
		if uid == 0 {
			uid, validity, err = client.FindUIDByMessageIDWithValidity(ctx, remote, snapshot.MessageID)
			if err != nil {
				return err
			}
		}
	}
	if uid == 0 || validity == 0 {
		return errors.New("Sent copy has no confirmed IMAP identity")
	}
	if err := s.cacheSent(ctx, send, folder, snapshot); err != nil {
		return err
	}
	var events []mail.Event
	err = s.runner.h.userAccounts.WithAccountForUser(ctx, s.owner, s.id, func(accounts *config.AccountStore, db *storage.DB) error {
		stored, err := db.GetStoredUIDValidity(ctx, folder)
		if err != nil {
			return err
		}
		if stored == validity {
			id, err := db.GetMessageLocalIDByInternetIDInternal(ctx, s.id, snapshot.MessageID)
			if err != nil {
				return err
			}
			if err := db.SetMessageFolderRemoteUID(ctx, id, folder, uid); err != nil {
				return err
			}
		}
		local := &Handler{db: db, accountStore: accounts, auth: s.runner.h.auth, blobStore: s.runner.h.blobStore}
		if send.DraftID != "" && local.deliveredSnapshotMatchesDraft(ctx, send) {
			drafts, err := db.DiscardIMAPDraft(ctx, s.id, send.DraftID)
			if err != nil {
				return err
			}
			if drafts != "" {
				events = append(events, mail.Event{Type: mail.EventMutation, UserID: s.owner, AccountID: s.id, FolderID: drafts})
			}
		}
		return db.CompleteSentCopy(ctx, send.ID, uid, validity)
	})
	if err != nil {
		return err
	}
	events = append(events, mail.Event{Type: mail.EventMutation, UserID: s.owner, AccountID: s.id, FolderID: folder})
	for _, event := range events {
		s.runner.h.userIMAP.Events().Publish(event)
	}
	return nil
}

func (s *userMailDeliveryScope) cacheSent(ctx context.Context, send storage.OutgoingSend, folder string, snapshot outgoingMessageSnapshot) error {
	release, err := s.runner.h.blobStore.PinUserFiles(ctx, s.owner)
	if err != nil {
		return err
	}
	defer release()
	msg := snapshot.outgoingMessage()
	var id int64
	err = s.call(ctx, func(db *storage.DB) error {
		var to, cc []storage.Recipient
		for _, a := range msg.To {
			to = append(to, storage.Recipient{Name: a.Name, Email: a.Address})
		}
		for _, a := range msg.CC {
			cc = append(cc, storage.Recipient{Name: a.Name, Email: a.Address})
		}
		if err := db.UpsertSyncMessages(ctx, []storage.SyncMessage{{AccountID: s.id, FolderID: folder, MessageID: msg.MessageID, InReplyTo: msg.InReplyTo, References: msg.References, Subject: msg.Subject, FromName: msg.FromName, FromEmail: msg.FromEmail, DateSent: msg.Date, Snippet: sentSnippet(msg.TextBody, msg.Subject), IsRead: true, ToRecipients: to, CCRecipients: cc}}); err != nil {
			return err
		}
		var err error
		id, err = db.GetMessageLocalIDByInternetIDInternal(ctx, s.id, msg.MessageID)
		return err
	})
	if err != nil {
		return err
	}
	candidate, err := s.runner.h.blobStore.NewMessageVersion()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = candidate.DeleteMessage(s.id, id)
		}
	}()
	p, err := message.ParseMessage(ctx, bytes.NewReader(send.MIMEData), candidate, s.id, id)
	if err != nil {
		return err
	}
	if p.ParseError != nil {
		return p.ParseError
	}
	text, html := "", ""
	if p.TextBody != "" {
		text, err = candidate.StoreBodyText(ctx, s.id, id, []byte(p.TextBody))
		if err != nil {
			return err
		}
	}
	if len(p.HTMLBody) > 0 {
		html, err = candidate.StoreBodyHTML(ctx, s.id, id, message.SanitizeHTML(p.HTMLBody))
		if err != nil {
			return err
		}
	}
	err = s.call(ctx, func(db *storage.DB) error { return db.PublishSentBody(ctx, id, s.id, msg.MessageID, p, text, html) })
	committed = err == nil
	return err
}

func (s *userMailDeliveryScope) draft(ctx context.Context) (bool, error) {
	if s.cfg.Provider != "imap" {
		return s.providerDraft(ctx)
	}
	var claimed []storage.IMAPDraftOperation
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		claimed, err = db.ClaimDueIMAPDraftOperationsForAccount(ctx, s.id, time.Now(), 1)
		return err
	}); err != nil {
		return false, err
	}
	if len(claimed) == 0 {
		return false, nil
	}
	op := claimed[0]
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := s.syncDraft(work, op)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		status := storage.IMAPDraftStatusAmbiguous // Search revision after a failed publication/deletion too.
		saveErr := s.call(ctx, func(db *storage.DB) error {
			return db.FinishIMAPDraftOperationWithError(ctx, op.ID, status, sanitizeOutgoingErrorText(err.Error()), time.Now().Add(sentCopyRetryDelay(op.AttemptCount)))
		})
		return saveErr == nil, errors.Join(err, saveErr)
	}
	return true, nil
}

func (s *userMailDeliveryScope) syncDraft(ctx context.Context, op storage.IMAPDraftOperation) error {
	client, err := imapclient.NewContextClient(ctx, s.cfg, s.password)
	if err != nil {
		return err
	}
	defer client.Close()
	state := op.State
	remove := func(uid, validity uint32) error {
		if uid == 0 {
			return nil
		}
		if validity == 0 {
			return errors.New("draft UIDVALIDITY is unavailable; cannot delete safely")
		}
		changed, err := client.DeleteMessagesIfUIDValidity(ctx, state.FolderRemoteName, []uint32{uid}, validity)
		if err == nil && changed {
			return imapclient.ErrMutationUIDValidityChanged
		}
		return err
	}
	if op.Kind == storage.IMAPDraftOperationDelete {
		if err := remove(state.RemoteUID, state.UIDValidity); err != nil {
			if !errors.Is(err, imapclient.ErrMutationUIDValidityChanged) {
				return err
			}
			// The old mailbox identity no longer exists. Never apply its UID to
			// the replacement mailbox; the original revision is already gone.
		}
		return s.call(ctx, func(db *storage.DB) error { return db.CompleteIMAPDraftDelete(ctx, op.ID) })
	}
	if op.Kind != storage.IMAPDraftOperationUpsert {
		return fmt.Errorf("unsupported draft operation %s", op.Kind)
	}
	uid, validity, err := client.FindUIDByHeaderWithValidity(ctx, state.FolderRemoteName, imapDraftRevisionHeader, op.RevisionToken)
	if err != nil {
		return err
	}
	if uid == 0 {
		result, err := client.AppendMessage(ctx, state.FolderRemoteName, op.MIMEData, []goimap.Flag{goimap.FlagSeen, goimap.FlagDraft}, op.MessageDate)
		if err != nil {
			return err
		}
		uid, validity = result.UID, result.UIDValidity
		if uid == 0 {
			uid, validity, err = client.FindUIDByHeaderWithValidity(ctx, state.FolderRemoteName, imapDraftRevisionHeader, op.RevisionToken)
			if err != nil {
				return err
			}
		}
	}
	if uid == 0 || validity == 0 {
		return errors.New("draft revision has no confirmed IMAP identity")
	}
	if state.RemoteUID > 0 && state.RemoteUID != uid && state.UIDValidity == validity {
		if err := remove(state.RemoteUID, state.UIDValidity); err != nil {
			return err
		}
	}
	return s.call(ctx, func(db *storage.DB) error {
		updated, err := db.CompleteIMAPDraftUpsert(ctx, op.ID, uid, validity)
		if err != nil {
			return err
		}
		stored, err := db.GetStoredUIDValidity(ctx, updated.FolderID)
		if err != nil {
			return err
		}
		if stored == validity && updated.LocalMessageID > 0 {
			return db.SetMessageFolderRemoteUID(ctx, updated.LocalMessageID, updated.FolderID, uid)
		}
		return nil
	})
}
