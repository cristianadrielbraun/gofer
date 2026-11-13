package storage

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

// Grouping by subject joins mail that carries no reply headers to a
// conversation with the same subject, as Gmail does: a reply (Re:, Fwd:)
// joins one it shares a participant with, other mail one from the same
// sender, such as a bank's repeated notices. A message joins only when a
// message of that conversation is within the window of days, and only while
// the conversation is under its size limit. It is off unless the user turns
// it on.
const (
	DefaultSubjectThreadingWindowDays  = 7
	DefaultSubjectThreadingMaxMessages = 100
	MaxSubjectThreadingWindowDays      = 365
	MaxSubjectThreadingMessages        = 10000
)

// SubjectThreading is a user's choice for grouping mail by subject.
// MaxMessages 0 means conversations grouped this way have no size limit.
type SubjectThreading struct {
	Enabled     bool
	WindowDays  int
	MaxMessages int
}

// ParseSubjectThreading reads the subject grouping settings from their
// stored values, using the defaults for missing or invalid ones.
func ParseSubjectThreading(enabled, windowDays, maxMessages string) SubjectThreading {
	return SubjectThreading{
		Enabled:     boolSetting(enabled, false),
		WindowDays:  SubjectThreadingWindowDays(windowDays),
		MaxMessages: SubjectThreadingMaxMessages(maxMessages),
	}
}

// SubjectThreadingWindowDays is the stored window, from 1 to
// MaxSubjectThreadingWindowDays days, or the default when it is not a number.
func SubjectThreadingWindowDays(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return DefaultSubjectThreadingWindowDays
	}
	return min(max(n, 1), MaxSubjectThreadingWindowDays)
}

// SubjectThreadingMaxMessages is the stored size limit, 0 for none, at most
// MaxSubjectThreadingMessages, or the default when it is not a number. A
// conversation of one message is not a limit, so 1 counts as 2.
func SubjectThreadingMaxMessages(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return DefaultSubjectThreadingMaxMessages
	}
	if n == 1 {
		return 2
	}
	return min(n, MaxSubjectThreadingMessages)
}

// subjectThreadingTx reads the subject grouping settings of the user that
// owns accountID.
func subjectThreadingTx(ctx context.Context, tx *sql.Tx, accountID string) SubjectThreading {
	var enabled, windowDays, maxMessages sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT json_extract(s.value, '$.subject_threading'),
		        json_extract(s.value, '$.subject_threading_window_days'),
		        json_extract(s.value, '$.subject_threading_max_messages')
		 FROM accounts a JOIN app_settings s ON s.user_id = a.user_id AND s.key = 'ui_settings'
		 WHERE a.id = ? AND json_valid(s.value)`, accountID).Scan(&enabled, &windowDays, &maxMessages)
	if err != nil {
		return ParseSubjectThreading("", "", "")
	}
	return ParseSubjectThreading(enabled.String, windowDays.String, maxMessages.String)
}

// messageParties are the addresses on a message, compared without case.
type messageParties struct {
	// own is the account's address and from the sender, which may be own.
	own, from string
	// others are the sender and recipients other than own.
	others map[string]bool
}

func newMessageParties(own, from string) messageParties {
	p := messageParties{own: own, from: strings.ToLower(strings.TrimSpace(from)), others: map[string]bool{}}
	p.add(from)
	return p
}

func (p messageParties) add(email string) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email != "" && email != p.own {
		p.others[email] = true
	}
}

func (p messageParties) sharesOther(q messageParties) bool {
	for email := range q.others {
		if p.others[email] {
			return true
		}
	}
	return false
}

// findSubjectThreadTx finds the conversation a message without reply
// headers joins when the user groups mail by subject, or "" when it starts
// one. With olderOnly only older mail is looked at, as when every message is
// relinked oldest first; otherwise mail on both sides of the window is, as
// sync does not save mail in date order.
func (db *DB) findSubjectThreadTx(ctx context.Context, tx *sql.Tx, msgID int64, accountID, normalizedSubject, subject string, sentAt time.Time, olderOnly bool, messageParticipants func() messageParties) string {
	if normalizedSubject == "" {
		return ""
	}
	settings := subjectThreadingTx(ctx, tx, accountID)
	if !settings.Enabled {
		return ""
	}
	reply := mailmessage.IsReplyOrForwardSubject(subject)
	parties := messageParticipants()
	// A reply needs someone in common. Other mail needs its sender, and when
	// the account sent it, someone in common as well: the same subject sent to
	// different people is not one conversation.
	if reply && len(parties.others) == 0 || !reply && (parties.from == "" || parties.from == parties.own && len(parties.others) == 0) {
		return ""
	}
	if sentAt.IsZero() {
		sentAt = time.Now().UTC()
	}
	window := time.Duration(settings.WindowDays) * 24 * time.Hour
	latest := sentAt.Add(window)
	if olderOnly {
		latest = sentAt
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, thread_id, lower(trim(COALESCE(from_email, ''))) FROM messages
		 WHERE account_id = ? AND id != ? AND normalized_subject = ? AND thread_id IS NOT NULL AND thread_id != ''
		 AND date_received BETWEEN ? AND ?
		 ORDER BY date_received DESC LIMIT 50`,
		accountID, msgID, normalizedSubject, sentAt.Add(-window), latest)
	if err != nil {
		return ""
	}
	type candidate struct {
		id             int64
		threadID, from string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.id, &c.threadID, &c.from) == nil {
			candidates = append(candidates, c)
		}
	}
	rows.Close()

	full := map[string]bool{}
	for _, c := range candidates {
		if full[c.threadID] {
			continue
		}
		if !reply && c.from != parties.from {
			continue
		}
		if reply || parties.from == parties.own {
			if !parties.sharesOther(db.messageParticipantsTx(ctx, tx, c.id, accountID)) {
				continue
			}
		}
		if settings.MaxMessages > 0 {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE account_id = ? AND thread_id = ?`, accountID, c.threadID).Scan(&count); err != nil {
				continue
			}
			if count >= settings.MaxMessages {
				full[c.threadID] = true
				continue
			}
		}
		return c.threadID
	}
	return ""
}

// SubjectRegroupMessageIDs lists, oldest first, owner's messages without
// reply headers: those whose conversation grouping by subject decides.
func (db *DB) SubjectRegroupMessageIDs(ctx context.Context, owner string) ([]int64, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT m.id, COALESCE(m.in_reply_to, ''), COALESCE(m."references", '')
		 FROM messages m JOIN accounts a ON a.id = m.account_id
		 WHERE a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0 AND COALESCE(m.normalized_subject, '') != ''
		 ORDER BY m.date_received, m.id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var inReplyTo, refs string
		if err := rows.Scan(&id, &inReplyTo, &refs); err != nil {
			return nil, err
		}
		if len(mailmessage.ThreadReferences(refs, inReplyTo)) == 0 {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// RegroupSubjectThreads relinks, in one transaction and in the order given,
// messages SubjectRegroupMessageIDs listed, so the subject grouping settings
// apply to them. Replies that follow a message move with it.
func (db *DB) RegroupSubjectThreads(ctx context.Context, owner string, ids []int64) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var m threadingRepairMessage
		err := tx.QueryRowContext(ctx,
			`SELECT m.id, m.account_id, COALESCE(m.internet_message_id, ''), COALESCE(m.in_reply_to, ''), COALESCE(m."references", ''), m.subject, m.date_received
			 FROM messages m JOIN accounts a ON a.id = m.account_id
			 WHERE m.id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0`, id, owner,
		).Scan(&m.id, &m.accountID, &m.msgID, &m.inReplyTo, &m.refs, &m.subject, &m.sentAt)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		if err := db.relinkThreadingRepairMessageTx(ctx, tx, m, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}
