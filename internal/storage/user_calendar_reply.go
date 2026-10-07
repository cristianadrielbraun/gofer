package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The resource is derived from the original private event, never from a form.
func calendarClaimReplyResource(claim *UserCalendarResponseClaim) (string, error) {
	source, event := claim.snapshot.source, claim.snapshot.event
	remote := event.RemoteID
	if event.SeriesRemoteID != "" {
		remote = event.SeriesRemoteID
	}
	base, err := url.Parse(source.RemoteID)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", ErrCalendarEventChanged
	}
	reference, err := url.Parse(remote)
	if err != nil {
		return "", ErrCalendarEventChanged
	}
	resource := base.ResolveReference(reference)
	resource.Fragment, resource.RawFragment = "", ""
	if resource.Scheme != base.Scheme || resource.Host != base.Host || resource.User != nil || resource.RawQuery != "" || resource.Opaque != "" || path.Clean(resource.Path) != resource.Path || !strings.HasPrefix(resource.Path, strings.TrimSuffix(base.Path, "/")+"/") || strings.HasSuffix(resource.Path, "/") {
		return "", ErrCalendarEventChanged
	}
	return resource.String(), nil
}

// Both rows and the original reservation are validated after the writer wait
// and again after inserts. A trigger cannot silently retarget a deliverable reply.
func (db *DB) QueueUserCalendarReply(ctx context.Context, claim *UserCalendarResponseClaim, job CalendarReplyJob, input QueueOutgoingSendInput, guard func(*sql.Tx, string) error) (string, error) {
	if claim == nil || claim.snapshot == nil || claim.id == "" || guard == nil || claim.snapshot.source.Provider != CalendarSourceProviderCalDAV {
		return "", ErrCalendarEventChanged
	}
	input.EnvelopeRecipients = append([]string(nil), input.EnvelopeRecipients...)
	input.MIMEData = append([]byte(nil), input.MIMEData...)
	input.MessageJSON = append([]byte(nil), input.MessageJSON...)
	resource, err := calendarClaimReplyResource(claim)
	if err != nil {
		return "", err
	}
	source := claim.snapshot.source
	if job.ResourceID != resource || job.RemoteID != claim.remote || job.Version != claim.version || job.Response != claim.response || !json.Valid([]byte(job.Payload)) || input.AccountID != source.AccountID || input.Transport != OutgoingTransportSMTP || input.EnvelopeFrom == "" || len(input.EnvelopeRecipients) != 1 || input.EnvelopeRecipients[0] == "" || len(input.MIMEData) == 0 || !json.Valid(input.MessageJSON) || input.ID != "" || input.MessageID != 0 || input.DraftID != "" || input.IsScheduled || !input.SendAfter.IsZero() {
		return "", ErrCalendarEventChanged
	}
	job.ID, job.UserID, job.SourceID = uuid.NewString(), source.UserID, source.ID
	job.State, job.SendStatus = "pending", OutgoingSendPending
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	validate := func() error {
		if err := validateUserCalendarEventTx(ctx, tx, claim.snapshot, guard); err != nil {
			return err
		}
		return calendarResponseClaimMatchesTx(ctx, tx, claim)
	}
	if err := validate(); err != nil {
		return "", err
	}
	if err := queueCalendarReplyTx(ctx, tx, job, input); err != nil {
		return "", err
	}
	if err := validateQueuedCalendarReplyTx(ctx, tx, job, input); err != nil {
		return "", err
	}
	if err := validate(); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return job.ID, nil
}

func validateQueuedCalendarReplyTx(ctx context.Context, tx *sql.Tx, job CalendarReplyJob, input QueueOutgoingSendInput) error {
	actual, err := scanCalendarReply(tx.QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=? AND j.user_id=?`, job.ID, job.UserID))
	if err != nil {
		return err
	}
	if actual != job {
		return ErrCalendarEventChanged
	}
	var account, transport, from, recipients, content, status, draft, lastError, sentID, copyStatus string
	var mime []byte
	var messageID, attempt, scheduled int
	var unlocked, initialDeadline bool
	err = tx.QueryRowContext(ctx, `SELECT account_id,transport,envelope_from,envelope_recipients,mime_data,message_json,status,
 COALESCE(message_id,0),draft_id,attempt_count,is_scheduled,last_error,sent_message_id,sent_copy_status,
 locked_at IS NULL,next_attempt_at=send_after FROM outgoing_sends WHERE id=?`, job.ID).Scan(&account, &transport, &from, &recipients, &mime, &content, &status, &messageID, &draft, &attempt, &scheduled, &lastError, &sentID, &copyStatus, &unlocked, &initialDeadline)
	if err != nil {
		return err
	}
	desiredRecipients, _ := json.Marshal(input.EnvelopeRecipients)
	if account != input.AccountID || transport != input.Transport || from != input.EnvelopeFrom || recipients != string(desiredRecipients) || !bytes.Equal(mime, input.MIMEData) || content != string(input.MessageJSON) || status != OutgoingSendPending || messageID != 0 || draft != "" || attempt != 0 || scheduled != 0 || lastError != "" || sentID != "" || copyStatus != SentCopyNotRequired || !unlocked || !initialDeadline {
		return ErrCalendarEventChanged
	}
	return nil
}

func (db *DB) ValidateUserCalendarReplyProof(ctx context.Context, job CalendarReplyJob, claim CalendarResponseClaimIdentity, source *UserCalendarSourceSnapshot, event *UserCalendarEventSnapshot, expected OutgoingSend, sendState [32]byte, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarReplyProofTx(ctx, tx, job, claim, source, event, expected, sendState, guard); err != nil {
		return err
	}
	return tx.Commit()
}

func validateUserCalendarReplyProofTx(ctx context.Context, tx *sql.Tx, job CalendarReplyJob, claim CalendarResponseClaimIdentity, source *UserCalendarSourceSnapshot, event *UserCalendarEventSnapshot, expected OutgoingSend, sendState [32]byte, guard func(*sql.Tx, string) error) error {

	if source == nil || claim.ID == "" || guard == nil || job.UserID != source.source.UserID || job.SourceID != source.source.ID || source.source.Provider != CalendarSourceProviderCalDAV || claim.RemoteID != job.RemoteID || claim.Version != job.Version || claim.Response != job.Response {
		return ErrCalendarEventChanged
	}
	if event != nil {
		if event.source.UserID != job.UserID || event.source.ID != job.SourceID {
			return ErrCalendarEventChanged
		}
		if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
			return err
		}
	} else if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
		return err
	}
	current, err := scanCalendarReply(tx.QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=? AND j.user_id=?`, job.ID, job.UserID))
	if err != nil {
		return err
	}
	if current != job {
		return ErrCalendarEventChanged
	}
	currentSend, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, job.ID))
	if err != nil {
		return err
	}
	if currentSend.ID != expected.ID || currentSend.AccountID != source.source.AccountID || currentSend.Transport != OutgoingTransportSMTP || currentSend.Status != expected.Status || currentSend.AttemptCount != expected.AttemptCount {
		return ErrCalendarEventChanged
	}
	if event != nil {
		state, err := CalendarReplySendFingerprint(currentSend)
		if err != nil {
			return err
		}
		if state != sendState {
			return ErrCalendarEventChanged
		}
	}
	var id, response string
	if err := tx.QueryRowContext(ctx, `SELECT claim_id,response FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=?`, job.UserID, job.SourceID, job.RemoteID, job.Version).Scan(&id, &response); err != nil {
		return err
	}
	if id != claim.ID || response != claim.Response {
		return ErrCalendarEventChanged
	}
	if err := guard(tx, source.source.AccountID); err != nil {
		return err
	}
	return nil
}

// Mutable delivery status is checked separately from these immutable bytes.
func CalendarReplySendFingerprint(send OutgoingSend) ([32]byte, error) {
	data, err := json.Marshal([]any{"user-calendar-reply-send-v1", send.AccountID, send.Transport, send.EnvelopeFrom, send.EnvelopeRecipients, send.MIMEData, send.MessageJSON})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// Delivery publications deliberately retain immutable job/attempt authority
// rather than requiring the event cache or service settings to remain unchanged
// after SMTP may have accepted the message.
func validateUserCalendarReplyDeliveryTx(ctx context.Context, tx *sql.Tx, job CalendarReplyJob, claim CalendarResponseClaimIdentity, expected OutgoingSend, state [32]byte, guard func(*sql.Tx, string) error) error {
	if guard == nil || job.ID == "" || job.State != "pending" || job.UserID == "" || job.ID != expected.ID || expected.Transport != OutgoingTransportSMTP || expected.MessageID != 0 || expected.DraftID != "" || expected.IsScheduled || claim.ID == "" || claim.RemoteID != job.RemoteID || claim.Version != job.Version || claim.Response != job.Response {
		return ErrCalendarEventChanged
	}
	if err := guard(tx, expected.AccountID); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts a JOIN users u ON u.id=a.user_id
 WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND u.status='active' AND u.deletion_pending=0`, expected.AccountID, job.UserID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountRoute
	}
	current, err := scanCalendarReply(tx.QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=? AND j.user_id=?`, job.ID, job.UserID))
	if err != nil {
		return err
	}
	if current != job {
		return ErrCalendarEventChanged
	}
	send, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, job.ID))
	if err != nil {
		return err
	}
	if send.AccountID != expected.AccountID || send.Status != expected.Status || send.AttemptCount != expected.AttemptCount || send.MessageID != expected.MessageID || send.DraftID != expected.DraftID || send.IsScheduled != expected.IsScheduled {
		return ErrCalendarEventChanged
	}
	actual, err := CalendarReplySendFingerprint(send)
	if err != nil {
		return err
	}
	if actual != state {
		return ErrCalendarEventChanged
	}
	var id, response string
	if err := tx.QueryRowContext(ctx, `SELECT claim_id,response FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=?`, job.UserID, job.SourceID, job.RemoteID, job.Version).Scan(&id, &response); err != nil {
		return err
	}
	if id != claim.ID || response != claim.Response {
		return ErrCalendarEventChanged
	}
	return guard(tx, expected.AccountID)
}

func (db *DB) ValidateUserCalendarReplyDelivery(ctx context.Context, job CalendarReplyJob, claim CalendarResponseClaimIdentity, send OutgoingSend, state [32]byte, guard func(*sql.Tx, string) error) error {
	if send.Status != OutgoingSendSending {
		return ErrCalendarEventChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarReplyDeliveryTx(ctx, tx, job, claim, send, state, guard); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarReplySendResult struct {
	Status, InternetID, Error string
	NextAttemptAt             time.Time
}

func (db *DB) FinishUserCalendarReplySend(ctx context.Context, job CalendarReplyJob, claim CalendarResponseClaimIdentity, send OutgoingSend, state [32]byte, result CalendarReplySendResult, guard func(*sql.Tx, string) error) error {
	if send.Status != OutgoingSendSending || job.SendStatus != send.Status {
		return ErrCalendarEventChanged
	}
	switch result.Status {
	case OutgoingSendSent:
		if result.InternetID == "" || result.Error != "" {
			return ErrCalendarEventChanged
		}
	case OutgoingSendPending:
		if result.NextAttemptAt.IsZero() || result.InternetID != "" {
			return ErrCalendarEventChanged
		}
	case OutgoingSendFailed, OutgoingSendAmbiguous:
		if result.InternetID != "" {
			return ErrCalendarEventChanged
		}
	default:
		return ErrCalendarEventChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarReplyDeliveryTx(ctx, tx, job, claim, send, state, guard); err != nil {
		return err
	}
	copyStatus := send.SentCopyStatus
	if result.Status == OutgoingSendSent {
		copyStatus = SentCopyPending
	}
	next := send.NextAttemptAt
	if result.Status == OutgoingSendPending {
		next = result.NextAttemptAt.UTC()
	}
	changed, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status=?,last_error=?,locked_at=NULL,sent_message_id=?,next_attempt_at=?,
 sent_copy_status=?,sent_copy_attempt_count=0,sent_copy_last_error='',sent_copy_locked_at=NULL,sent_copy_next_attempt_at=CURRENT_TIMESTAMP,
 updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='sending' AND attempt_count=?`, result.Status, result.Error, result.InternetID, next, copyStatus, send.ID, send.AttemptCount)
	if err != nil {
		return err
	}
	if n, err := changed.RowsAffected(); err != nil || n != 1 {
		return ErrCalendarEventChanged
	}
	job.SendStatus, send.Status = result.Status, result.Status
	if err := validateUserCalendarReplyDeliveryTx(ctx, tx, job, claim, send, state, guard); err != nil {
		return err
	}
	actual, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, send.ID))
	if err != nil {
		return err
	}
	var unlocked bool
	if err := tx.QueryRowContext(ctx, `SELECT locked_at IS NULL FROM outgoing_sends WHERE id=?`, send.ID).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked || actual.SentMessageID != result.InternetID || actual.LastError != result.Error || actual.SentCopyStatus != copyStatus || actual.SentCopyAttempts != 0 || actual.SentCopyLastError != "" || !actual.NextAttemptAt.Equal(next) {
		return ErrCalendarEventChanged
	}
	return tx.Commit()
}
