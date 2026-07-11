package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type outgoingAddressSnapshot struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type outgoingAttachmentSnapshot struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Path        string `json:"path,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline,omitempty"`
}

type outgoingMessageSnapshot struct {
	FromName    string                       `json:"from_name,omitempty"`
	FromEmail   string                       `json:"from_email"`
	To          []outgoingAddressSnapshot    `json:"to"`
	CC          []outgoingAddressSnapshot    `json:"cc,omitempty"`
	BCC         []outgoingAddressSnapshot    `json:"bcc,omitempty"`
	Subject     string                       `json:"subject,omitempty"`
	TextBody    string                       `json:"text_body,omitempty"`
	HTMLBody    string                       `json:"html_body,omitempty"`
	InReplyTo   string                       `json:"in_reply_to,omitempty"`
	References  string                       `json:"references,omitempty"`
	MessageID   string                       `json:"message_id"`
	Date        time.Time                    `json:"date"`
	Attachments []outgoingAttachmentSnapshot `json:"attachments,omitempty"`
}

type sentCopyIMAPClient interface {
	AppendMessage(ctx context.Context, remoteName string, raw []byte, flags []goimap.Flag, date time.Time) (imapclient.AppendResult, error)
	FindUIDByMessageIDWithValidity(ctx context.Context, remoteName, messageID string) (uint32, uint32, error)
	FindUIDByHeaderWithValidity(ctx context.Context, remoteName, headerName, headerValue string) (uint32, uint32, error)
	DeleteMessagesIfUIDValidity(ctx context.Context, folderRemoteName string, uids []uint32, expectedUIDValidity uint32) (bool, error)
	Close() error
}

type sentCopyIMAPClientFactory func(ctx context.Context, cfg *models.AccountConfig, password string) (sentCopyIMAPClient, error)

func snapshotOutgoingMessage(msg *message.OutgoingMessage) outgoingMessageSnapshot {
	return outgoingMessageSnapshot{
		FromName:    msg.FromName,
		FromEmail:   msg.FromEmail,
		To:          snapshotOutgoingAddresses(msg.To),
		CC:          snapshotOutgoingAddresses(msg.CC),
		BCC:         snapshotOutgoingAddresses(msg.Bcc),
		Subject:     msg.Subject,
		TextBody:    msg.TextBody,
		HTMLBody:    msg.HTMLBody,
		InReplyTo:   msg.InReplyTo,
		References:  msg.References,
		MessageID:   msg.MessageID,
		Date:        msg.Date,
		Attachments: snapshotOutgoingAttachments(msg.Attachments),
	}
}

func (snapshot outgoingMessageSnapshot) outgoingMessage() *message.OutgoingMessage {
	return &message.OutgoingMessage{
		FromName:    snapshot.FromName,
		FromEmail:   snapshot.FromEmail,
		To:          restoreOutgoingAddresses(snapshot.To),
		CC:          restoreOutgoingAddresses(snapshot.CC),
		Bcc:         restoreOutgoingAddresses(snapshot.BCC),
		Subject:     snapshot.Subject,
		TextBody:    snapshot.TextBody,
		HTMLBody:    snapshot.HTMLBody,
		InReplyTo:   snapshot.InReplyTo,
		References:  snapshot.References,
		MessageID:   snapshot.MessageID,
		Date:        snapshot.Date,
		Attachments: restoreOutgoingAttachments(snapshot.Attachments),
	}
}

func snapshotOutgoingAddresses(addresses []*mail.Address) []outgoingAddressSnapshot {
	out := make([]outgoingAddressSnapshot, 0, len(addresses))
	for _, address := range addresses {
		if address != nil && strings.TrimSpace(address.Address) != "" {
			out = append(out, outgoingAddressSnapshot{Name: address.Name, Address: address.Address})
		}
	}
	return out
}

func restoreOutgoingAddresses(addresses []outgoingAddressSnapshot) []*mail.Address {
	out := make([]*mail.Address, 0, len(addresses))
	for _, address := range addresses {
		out = append(out, &mail.Address{Name: address.Name, Address: address.Address})
	}
	return out
}

func snapshotOutgoingAttachments(attachments []message.OutgoingAttachment) []outgoingAttachmentSnapshot {
	out := make([]outgoingAttachmentSnapshot, 0, len(attachments))
	for _, attachment := range attachments {
		out = append(out, outgoingAttachmentSnapshot{
			Filename: attachment.Filename, ContentType: attachment.ContentType, Path: attachment.Path,
			Size: attachment.Size, ContentID: attachment.ContentID, Inline: attachment.Inline,
		})
	}
	return out
}

func restoreOutgoingAttachments(attachments []outgoingAttachmentSnapshot) []message.OutgoingAttachment {
	out := make([]message.OutgoingAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		out = append(out, message.OutgoingAttachment{
			Filename: attachment.Filename, ContentType: attachment.ContentType, Path: attachment.Path,
			Size: attachment.Size, ContentID: attachment.ContentID, Inline: attachment.Inline,
		})
	}
	return out
}

func outgoingTransportForConfig(cfg *models.AccountConfig) string {
	if cfg != nil {
		switch strings.TrimSpace(cfg.Provider) {
		case providers.ProviderGmail:
			return storage.OutgoingTransportGmail
		case providers.ProviderOutlook:
			return storage.OutgoingTransportOutlook
		}
	}
	return storage.OutgoingTransportSMTP
}

func buildOutgoingMIME(transport string, msg *message.OutgoingMessage) ([]byte, error) {
	if transport == storage.OutgoingTransportGmail || transport == storage.OutgoingTransportOutlook {
		return message.BuildMIMEMessageForGraph(msg)
	}
	return message.BuildMIMEMessage(msg)
}

func (h *Handler) queueOutgoingMessage(ctx context.Context, accountID string, localMessageID int64, draftID string, msg *message.OutgoingMessage, sendAfter time.Time, scheduled bool) (storage.OutgoingSend, error) {
	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return storage.OutgoingSend{}, fmt.Errorf("account not found")
	}
	transport := outgoingTransportForConfig(cfg)
	raw, err := buildOutgoingMIME(transport, msg)
	if err != nil {
		return storage.OutgoingSend{}, fmt.Errorf("build outgoing message: %w", err)
	}
	snapshotJSON, err := json.Marshal(snapshotOutgoingMessage(msg))
	if err != nil {
		return storage.OutgoingSend{}, fmt.Errorf("encode outgoing message: %w", err)
	}
	recipients := message.AllRecipients(msg)
	if len(recipients) == 0 {
		return storage.OutgoingSend{}, fmt.Errorf("no recipients")
	}
	return h.db.QueueOutgoingSend(ctx, storage.QueueOutgoingSendInput{
		AccountID:          accountID,
		MessageID:          localMessageID,
		DraftID:            strings.TrimSpace(draftID),
		Transport:          transport,
		EnvelopeFrom:       msg.FromEmail,
		EnvelopeRecipients: recipients,
		MIMEData:           raw,
		MessageJSON:        snapshotJSON,
		SendAfter:          sendAfter,
		IsScheduled:        scheduled,
	})
}

func (h *Handler) signalOutgoingWorker() {
	select {
	case h.outgoingWake <- struct{}{}:
	default:
	}
}

func (h *Handler) StartOutgoingSendWorker(ctx context.Context) {
	go func() {
		if count, err := h.db.MarkInterruptedOutgoingSendsAmbiguous(ctx, "Gofer stopped while this message was being sent. It may have been delivered."); err != nil {
			log.Printf("outgoing-send: recover interrupted sends: %v", err)
		} else if count > 0 {
			log.Printf("outgoing-send: marked %d interrupted send(s) ambiguous", count)
		}
		if count, err := h.db.MarkInterruptedSentCopiesAmbiguous(ctx, "Gofer stopped while copying this message to Sent. The remote copy may already exist."); err != nil {
			log.Printf("outgoing-send: recover interrupted Sent copies: %v", err)
		} else if count > 0 {
			log.Printf("outgoing-send: marked %d interrupted Sent copy operation(s) ambiguous", count)
		}
		if count, err := h.db.MarkInterruptedIMAPDraftOperationsAmbiguous(ctx, "Gofer stopped while syncing this draft. The remote revision may already exist."); err != nil {
			log.Printf("imap-draft: recover interrupted operations: %v", err)
		} else if count > 0 {
			log.Printf("imap-draft: marked %d interrupted operation(s) ambiguous", count)
		}
		h.prepareMigratedOutgoingSends(ctx)
		h.runDueOutgoingSends(ctx)
		h.runDueSentCopies(ctx)
		h.runDueIMAPDraftOperations(ctx)

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.outgoingWake:
				h.runDueOutgoingSends(ctx)
				h.runDueSentCopies(ctx)
				h.runDueIMAPDraftOperations(ctx)
			case <-ticker.C:
				h.runDueOutgoingSends(ctx)
				h.runDueSentCopies(ctx)
				h.runDueIMAPDraftOperations(ctx)
			}
		}
	}()
}

func (h *Handler) prepareMigratedOutgoingSends(ctx context.Context) {
	sends, err := h.db.ListUnpreparedOutgoingSends(ctx)
	if err != nil {
		log.Printf("outgoing-send: list migrated sends: %v", err)
		return
	}
	for _, send := range sends {
		msg, err := h.outgoingMessageFromDraft(ctx, send.MessageID)
		if err != nil {
			_ = h.db.MarkPendingOutgoingSendFailed(ctx, send.ID, err.Error())
			continue
		}
		cfg, err := h.accountStore.GetConfig(ctx, send.AccountID)
		if err != nil {
			_ = h.db.MarkPendingOutgoingSendFailed(ctx, send.ID, "account not found")
			continue
		}
		transport := outgoingTransportForConfig(cfg)
		raw, err := buildOutgoingMIME(transport, msg)
		if err != nil {
			_ = h.db.MarkPendingOutgoingSendFailed(ctx, send.ID, err.Error())
			continue
		}
		snapshotJSON, err := json.Marshal(snapshotOutgoingMessage(msg))
		if err != nil {
			_ = h.db.MarkPendingOutgoingSendFailed(ctx, send.ID, err.Error())
			continue
		}
		if err := h.db.PrepareOutgoingSend(ctx, send.ID, send.DraftID, transport, msg.FromEmail, message.AllRecipients(msg), raw, snapshotJSON); err != nil {
			log.Printf("outgoing-send: prepare migrated send %s: %v", send.ID, err)
		}
	}
}

func (h *Handler) runDueOutgoingSends(ctx context.Context) {
	for {
		sends, err := h.db.ClaimDueOutgoingSends(ctx, time.Now(), 5)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("outgoing-send: claim due sends: %v", err)
			}
			return
		}
		if len(sends) == 0 {
			return
		}
		for _, send := range sends {
			h.deliverOutgoingSend(ctx, send)
		}
	}
}

func (h *Handler) deliverOutgoingSend(parent context.Context, send storage.OutgoingSend) {
	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
		h.finishOutgoingSend(send, storage.OutgoingSendFailed, fmt.Errorf("decode outgoing message: %w", err))
		return
	}
	msg := snapshot.outgoingMessage()
	cfg, err := h.accountStore.GetConfig(parent, send.AccountID)
	if err != nil {
		h.finishOutgoingSend(send, storage.OutgoingSendFailed, fmt.Errorf("account not found"))
		return
	}

	sendCtx, cancel := outgoingSendContext(parent)
	defer cancel()
	status := storage.OutgoingSendFailed
	var providerMessageID, providerToken string
	switch send.Transport {
	case storage.OutgoingTransportGmail:
		providerMessageID, providerToken, err = h.sendGmailAPIRaw(sendCtx, cfg, send.MIMEData)
	case storage.OutgoingTransportOutlook:
		providerToken, err = h.sendOutlookGraphRaw(sendCtx, cfg, send.MIMEData)
	case storage.OutgoingTransportSMTP:
		err = h.sendSMTPOutgoingRaw(sendCtx, cfg, send)
	default:
		err = fmt.Errorf("unsupported outgoing transport %q", send.Transport)
	}
	if err != nil {
		if errors.Is(err, errOutgoingSendAmbiguous) {
			status = storage.OutgoingSendAmbiguous
		}
		h.finishOutgoingSend(send, status, err)
		return
	}

	h.saveSentMessageSnapshot(parent, send.AccountID, msg, send.MIMEData)
	if send.Transport == storage.OutgoingTransportGmail {
		h.cacheGmailSentMessageID(parent, send.AccountID, msg, providerToken, providerMessageID)
	}
	if send.Transport == storage.OutgoingTransportOutlook {
		h.cacheOutlookSentMessageID(parent, send.AccountID, msg, providerToken)
	}
	needsSentCopy := send.Transport == storage.OutgoingTransportSMTP
	if err := h.db.CompleteOutgoingSend(parent, send.ID, msg.MessageID, needsSentCopy); err != nil {
		log.Printf("outgoing-send: complete %s: %v", send.ID, err)
		return
	}
	h.cleanupDeliveredDraft(parent, send)
	h.publishOutgoingResult(send, "sent", "")
}

func (h *Handler) runDueSentCopies(ctx context.Context) {
	for {
		sends, err := h.db.ClaimDueSentCopies(ctx, time.Now(), 5)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("outgoing-send: claim due Sent copies: %v", err)
			}
			return
		}
		if len(sends) == 0 {
			return
		}
		for _, send := range sends {
			h.reconcileSentCopy(ctx, send)
		}
	}
}

func (h *Handler) reconcileSentCopy(parent context.Context, send storage.OutgoingSend) {
	folderID, remoteName, err := h.db.GetFolderIDByRole(parent, send.AccountID, "sent")
	if err != nil {
		h.finishSentCopy(send, storage.SentCopyFailed, fmt.Errorf("load Sent folder: %w", err))
		return
	}
	if folderID == "" || remoteName == "" {
		h.finishSentCopy(send, storage.SentCopyFailed, fmt.Errorf("remote Sent folder is not available"))
		return
	}
	cfg, err := h.accountStore.GetConfig(parent, send.AccountID)
	if err != nil {
		h.finishSentCopy(send, storage.SentCopyFailed, fmt.Errorf("account not found"))
		return
	}
	password, err := h.resolvePassword(parent, cfg, send.AccountID)
	if err != nil {
		h.finishSentCopy(send, storage.SentCopyFailed, fmt.Errorf("get IMAP credentials: %w", err))
		return
	}
	factory := h.sentCopyIMAPFactory
	if factory == nil {
		factory = func(ctx context.Context, cfg *models.AccountConfig, password string) (sentCopyIMAPClient, error) {
			return imapclient.NewClient(ctx, cfg, password)
		}
	}
	client, err := factory(parent, cfg, password)
	if err != nil {
		h.finishSentCopy(send, storage.SentCopyFailed, fmt.Errorf("connect to IMAP for Sent copy: %w", err))
		return
	}
	defer client.Close()

	var snapshot outgoingMessageSnapshot
	if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
		h.finishSentCopy(send, storage.SentCopyFailed, fmt.Errorf("decode Sent copy message: %w", err))
		return
	}
	messageID := strings.TrimSpace(snapshot.MessageID)
	if messageID == "" {
		messageID = strings.TrimSpace(send.SentMessageID)
	}

	if send.SentCopyStatus == storage.SentCopyAmbiguous {
		uid, uidValidity, err := client.FindUIDByMessageIDWithValidity(parent, remoteName, messageID)
		if err != nil {
			h.finishSentCopy(send, storage.SentCopyAmbiguous, fmt.Errorf("check ambiguous Sent copy: %w", err))
			return
		}
		if uid > 0 {
			h.completeSentCopy(parent, send, folderID, uid, uidValidity)
			return
		}
	}

	result, err := client.AppendMessage(parent, remoteName, send.MIMEData, []goimap.Flag{goimap.FlagSeen}, snapshot.Date)
	if err != nil {
		status := storage.SentCopyFailed
		if imapclient.IsAppendAmbiguous(err) {
			status = storage.SentCopyAmbiguous
		}
		h.finishSentCopy(send, status, err)
		return
	}
	uid := result.UID
	if uid == 0 {
		if foundUID, foundUIDValidity, findErr := client.FindUIDByMessageIDWithValidity(parent, remoteName, messageID); findErr == nil {
			uid = foundUID
			if result.UIDValidity == 0 {
				result.UIDValidity = foundUIDValidity
			}
		} else {
			log.Printf("outgoing-send: find appended Sent copy account=%s message=%s: %v", send.AccountID, messageID, findErr)
		}
	}
	h.completeSentCopy(parent, send, folderID, uid, result.UIDValidity)
}

func (h *Handler) completeSentCopy(ctx context.Context, send storage.OutgoingSend, folderID string, uid, uidValidity uint32) {
	if uid > 0 {
		localID, err := h.db.GetMessageLocalIDByInternetID(ctx, send.AccountID, send.SentMessageID)
		if err == nil && localID > 0 {
			storedUIDValidity, validityErr := h.db.GetStoredUIDValidity(ctx, folderID)
			if validityErr != nil {
				log.Printf("outgoing-send: read Sent UID validity account=%s: %v", send.AccountID, validityErr)
			} else if uidValidity > 0 && storedUIDValidity > 0 && uidValidity != storedUIDValidity {
				log.Printf("outgoing-send: skip Sent UID link account=%s uidvalidity=%d stored=%d", send.AccountID, uidValidity, storedUIDValidity)
			} else if err := h.db.SetMessageFolderRemoteUID(ctx, localID, folderID, uid); err != nil {
				log.Printf("outgoing-send: link Sent UID account=%s message=%d uid=%d: %v", send.AccountID, localID, uid, err)
			}
		}
	}
	if err := h.db.CompleteSentCopy(ctx, send.ID, uid, uidValidity); err != nil {
		log.Printf("outgoing-send: complete Sent copy %s: %v", send.ID, err)
		nextAttempt := time.Now().Add(sentCopyRetryDelay(send.SentCopyAttempts))
		if retryErr := h.db.FinishSentCopyWithError(context.Background(), send.ID, storage.SentCopyAmbiguous, "The remote Sent copy exists, but Gofer could not save its result: "+err.Error(), nextAttempt); retryErr != nil {
			log.Printf("outgoing-send: schedule Sent copy reconciliation %s: %v", send.ID, retryErr)
		}
	}
}

func (h *Handler) finishSentCopy(send storage.OutgoingSend, status string, err error) {
	errText := "Failed to copy message to the remote Sent folder."
	if err != nil {
		errText = err.Error()
	}
	nextAttempt := time.Now().Add(sentCopyRetryDelay(send.SentCopyAttempts))
	if dbErr := h.db.FinishSentCopyWithError(context.Background(), send.ID, status, errText, nextAttempt); dbErr != nil {
		log.Printf("outgoing-send: finish Sent copy %s: %v", send.ID, dbErr)
		return
	}
	log.Printf("outgoing-send: Sent copy %s %s; retry at %s: %s", send.ID, status, nextAttempt.Format(time.RFC3339), errText)
}

func sentCopyRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	return 30 * time.Second * time.Duration(1<<(attempt-1))
}

var errOutgoingSendAmbiguous = errors.New("outgoing send status is ambiguous")

func (h *Handler) sendSMTPOutgoingRaw(ctx context.Context, cfg *models.AccountConfig, send storage.OutgoingSend) error {
	password, err := h.resolvePassword(ctx, cfg, send.AccountID)
	if err != nil {
		return fmt.Errorf("failed to get credentials")
	}
	smtpPassword := password
	if cfg.SmtpUsername != "" {
		if decrypted, err := h.accountStore.DecryptSmtpPassword(ctx, send.AccountID); err == nil && decrypted != "" {
			smtpPassword = decrypted
		}
	}
	result, err := smtpclient.SendRawMessage(ctx, cfg, smtpPassword, send.EnvelopeFrom, send.EnvelopeRecipients, send.MIMEData)
	if err != nil {
		if result == models.SendAmbiguous {
			return fmt.Errorf("%w: %v", errOutgoingSendAmbiguous, err)
		}
		return err
	}
	if result == models.SendAmbiguous {
		return errOutgoingSendAmbiguous
	}
	if result != models.SendSuccess {
		return fmt.Errorf("failed to send message")
	}
	return nil
}

func (h *Handler) finishOutgoingSend(send storage.OutgoingSend, status string, err error) {
	errText := "Failed to send message."
	if err != nil {
		errText = err.Error()
	}
	if dbErr := h.db.FinishOutgoingSendWithError(context.Background(), send.ID, status, errText); dbErr != nil {
		log.Printf("outgoing-send: finish %s: %v", send.ID, dbErr)
	}
	eventStatus := "failed"
	if status == storage.OutgoingSendAmbiguous {
		eventStatus = "ambiguous"
	}
	h.publishOutgoingResult(send, eventStatus, errText)
}

func (h *Handler) publishOutgoingResult(send storage.OutgoingSend, status, errText string) {
	if h.syncer == nil {
		return
	}
	event := mailpkg.Event{Type: mailpkg.EventSendResult, AccountID: send.AccountID, Status: status, Error: errText, Payload: map[string]any{"send_id": send.ID}}
	h.syncer.Events().Publish(event)
}

func (h *Handler) cleanupDeliveredDraft(ctx context.Context, send storage.OutgoingSend) {
	if strings.TrimSpace(send.DraftID) == "" {
		return
	}
	draftProvider, _ := h.db.GetDraftProviderInfo(ctx, send.AccountID, send.DraftID)
	if send.Transport == storage.OutgoingTransportSMTP {
		if err := h.queueIMAPDraftDelete(ctx, send.AccountID, send.DraftID, draftProvider); err != nil {
			log.Printf("outgoing-send: queue remote draft delete account=%s draft=%s: %v", send.AccountID, send.DraftID, err)
			return
		}
	}
	folderID, err := h.db.DeleteDraftMessage(ctx, send.AccountID, send.DraftID)
	if err != nil {
		log.Printf("outgoing-send: delete local draft account=%s draft=%s: %v", send.AccountID, send.DraftID, err)
		return
	}
	if folderID != "" {
		h.publishMutation(send.AccountID, folderID)
	}
	if draftProvider == nil || strings.TrimSpace(draftProvider.ProviderMessageID) == "" {
		return
	}
	var deleteErr error
	switch send.Transport {
	case storage.OutgoingTransportGmail:
		deleteErr = h.deleteGmailAPIDraft(ctx, send.AccountID, draftProvider.ProviderMessageID)
	case storage.OutgoingTransportOutlook:
		deleteErr = h.deleteOutlookGraphDraft(ctx, send.AccountID, draftProvider.ProviderMessageID)
	}
	if deleteErr != nil {
		log.Printf("outgoing-send: delete provider draft account=%s draft=%s: %v", send.AccountID, send.DraftID, deleteErr)
	}
}

func (h *Handler) outgoingMessageFromDraft(ctx context.Context, localMessageID int64) (*message.OutgoingMessage, error) {
	if localMessageID <= 0 {
		return nil, fmt.Errorf("scheduled draft no longer exists")
	}
	email, err := h.db.GetEmailByID(ctx, fmt.Sprintf("%d", localMessageID))
	if err != nil {
		return nil, err
	}
	if email == nil || !email.IsDraft {
		return nil, fmt.Errorf("scheduled draft no longer exists")
	}
	to, err := message.ParseAddressList(contactsToAddressList(email.To))
	if err != nil || len(to) == 0 {
		return nil, fmt.Errorf("scheduled draft has no valid recipients")
	}
	cc, _ := message.ParseAddressList(contactsToAddressList(email.CC))
	bcc, _ := message.ParseAddressList(contactsToAddressList(email.BCC))
	htmlBody := strings.TrimSpace(email.HTMLBody)
	if htmlBody != "" && !strings.Contains(strings.ToLower(htmlBody), "<html") {
		htmlBody = "<html><body>" + htmlBody + "</body></html>"
	}
	inReplyTo, references := h.validComposeThreadHeaders(ctx, email.AccountID, email.Subject, email.InReplyTo, email.References)
	fromName, fromEmail := email.From.Name, email.From.Email
	if account, err := h.accountStore.GetAccountByID(ctx, email.AccountID); err == nil && account != nil {
		fromName, fromEmail = account.Name, account.Email
	}
	return &message.OutgoingMessage{
		FromName: fromName, FromEmail: fromEmail, To: to, CC: cc, Bcc: bcc,
		Subject: email.Subject, TextBody: email.TextBody, HTMLBody: htmlBody,
		InReplyTo: inReplyTo, References: references, MessageID: message.NewMessageID(),
		Date: time.Now().UTC(), Attachments: outgoingAttachmentsFromStored(email.Attachments),
	}, nil
}

func (h *Handler) refreshScheduledOutgoingSend(ctx context.Context, saved composeDraftSaveResult) error {
	existing, err := h.db.OutgoingSendForMessage(ctx, saved.MessageID)
	if err != nil || existing == nil || !existing.IsScheduled || existing.Status != storage.OutgoingSendPending {
		return err
	}
	msg, err := h.outgoingMessageFromDraft(ctx, saved.MessageID)
	if err != nil {
		return err
	}
	_, err = h.queueOutgoingMessage(ctx, saved.AccountID, saved.MessageID, saved.DraftID, msg, existing.SendAfter, true)
	return err
}

func (h *Handler) saveSentMessageSnapshot(ctx context.Context, accountID string, msg *message.OutgoingMessage, raw []byte) {
	h.saveSentMessageRecord(ctx, accountID, msg)
	localID, err := h.db.GetMessageLocalIDByInternetID(ctx, accountID, msg.MessageID)
	if err != nil || localID == 0 {
		return
	}
	parsed, err := message.ParseMessage(ctx, bytes.NewReader(raw), h.blobStore, accountID, localID)
	if err != nil {
		log.Printf("outgoing-send: store sent MIME account=%s message=%s: %v", accountID, msg.MessageID, err)
		return
	}
	textPath, htmlPath := "", ""
	if parsed.TextBody != "" {
		textPath, _ = h.blobStore.StoreBodyText(ctx, accountID, localID, []byte(parsed.TextBody))
	}
	if len(parsed.HTMLBody) > 0 {
		htmlPath, _ = h.blobStore.StoreBodyHTML(ctx, accountID, localID, message.SanitizeHTML(parsed.HTMLBody))
	}
	_ = h.db.UpdateMessageBody(ctx, localID, textPath, htmlPath, parsed.RawPath, parsed.Snippet)
	attachments := make([]storage.AttachmentRow, 0, len(parsed.Attachments))
	for _, attachment := range parsed.Attachments {
		attachments = append(attachments, storage.AttachmentRow{
			Filename: attachment.Filename, ContentType: attachment.ContentType, SizeBytes: attachment.Size,
			ContentID: attachment.ContentID, Inline: attachment.Inline, StoragePath: attachment.BlobPath,
		})
	}
	_ = h.db.ReplaceAttachments(ctx, localID, attachments)
	h.deleteComposeAttachmentPaths(outgoingAttachmentPaths(msg.Attachments))
	if sentFolderID, _, err := h.db.GetFolderIDByRole(ctx, accountID, "sent"); err == nil && sentFolderID != "" {
		h.publishMutation(accountID, sentFolderID)
	}
}
