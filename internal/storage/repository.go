package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	mailmessage "gofer.email/internal/mail/message"
	"gofer.email/internal/models"
)

var reHTMLTag = regexp.MustCompile(`<[^>]*>`)
var reMultiNewline = regexp.MustCompile(`\n{3,}`)

func stripHTMLTags(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reMultiNewline.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func nullStringValue(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func truncatePreview(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	runes := []rune(s)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return s
}

func previewFromBodyPaths(textPath, htmlPath string) string {
	if textPath != "" {
		if data, err := os.ReadFile(textPath); err == nil && len(data) > 0 {
			if preview := truncatePreview(string(data)); preview != "" {
				return preview
			}
		}
	}
	if htmlPath != "" {
		if data, err := os.ReadFile(htmlPath); err == nil && len(data) > 0 {
			if preview := truncatePreview(stripHTMLTags(string(data))); preview != "" {
				return preview
			}
		}
	}
	return ""
}

func initials(name string) string {
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		return strings.ToUpper(firstRune(parts[0]) + firstRune(parts[1]))
	}
	runes := []rune(name)
	if len(runes) >= 2 {
		return strings.ToUpper(string(runes[:2]))
	}
	return strings.ToUpper(name)
}

func contactFromSender(name, email string) models.Contact {
	display := strings.TrimSpace(name)
	if display == "" {
		display = strings.TrimSpace(email)
	}
	return models.Contact{Name: display, Email: email, Initials: initials(display)}
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

func formatRelativeDate(t, now time.Time) string {
	t = t.Local()
	now = now.Local()
	tDay := t.Format("2006-01-02")
	nowDay := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	if tDay == nowDay {
		return t.Format("3:04 PM")
	}
	if tDay == yesterday {
		return "Yesterday"
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

func isStarredFolder(folderID string) bool {
	return folderID == "starred" || strings.HasPrefix(folderID, "starred-")
}

func isUnifiedFolderID(folderID string) bool {
	switch folderID {
	case "inbox", "starred", "sent", "drafts", "archive", "spam", "trash":
		return true
	default:
		return false
	}
}

func normalizeSubject(subject string) string {
	return mailmessage.BaseSubject(subject)
}

type folderRow struct {
	folder   models.Folder
	parentID sql.NullString
}

type UpsertFolderInput struct {
	ID        string
	AccountID string
	ParentID  string
	RemoteID  string
	Name      string
	Icon      string
	Role      string
	SortOrder int
}

func (db *DB) UpsertFolders(ctx context.Context, folders []UpsertFolderInput) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO folders (id, account_id, parent_id, remote_id, name, icon, role, sort_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_id = excluded.parent_id,
			remote_id = excluded.remote_id,
			name = excluded.name,
			icon = excluded.icon,
			role = excluded.role,
			sort_order = excluded.sort_order,
			updated_at = CURRENT_TIMESTAMP`)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	for _, f := range folders {
		var parentID interface{}
		if f.ParentID != "" {
			parentID = f.ParentID
		}
		if _, err := stmt.ExecContext(ctx, f.ID, f.AccountID, parentID, f.RemoteID, f.Name, f.Icon, f.Role, f.SortOrder); err != nil {
			return fmt.Errorf("upsert folder %s: %w", f.ID, err)
		}
	}

	return tx.Commit()
}

func (db *DB) reconcileMessageThreadTx(ctx context.Context, tx *sql.Tx, msgID int64, accountID, messageID, inReplyTo, refsRaw, subject string, sentAt time.Time) error {
	normalizedSubject := normalizeSubject(subject)
	refs := mailmessage.ThreadReferences(refsRaw, inReplyTo)

	if _, err := tx.ExecContext(ctx, `DELETE FROM message_references WHERE message_id = ?`, msgID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM unresolved_references WHERE child_message_id = ?`, msgID); err != nil {
		return err
	}
	for i, ref := range refs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO message_references (message_id, referenced_message_id, ordinal) VALUES (?, ?, ?)`,
			msgID, ref, i); err != nil {
			return err
		}
	}

	var parentID sql.NullInt64
	var threadID string
	for i := len(refs) - 1; i >= 0; i-- {
		var candidateID int64
		var candidateThread sql.NullString
		var candidateSubject string
		err := tx.QueryRowContext(ctx,
			`SELECT id, thread_id, normalized_subject FROM messages WHERE account_id = ? AND message_id_normalized = ? AND id != ? LIMIT 1`,
			accountID, refs[i], msgID).Scan(&candidateID, &candidateThread, &candidateSubject)
		if err == nil && candidateThread.Valid && candidateThread.String != "" {
			if refs[i] != inReplyTo && candidateSubject != normalizedSubject {
				continue
			}
			parentID = sql.NullInt64{Int64: candidateID, Valid: true}
			threadID = candidateThread.String
			break
		}
	}

	if threadID == "" && len(refs) == 0 {
		threadID = db.findSubjectFallbackThreadTx(ctx, tx, msgID, accountID, normalizedSubject, subject, sentAt)
	}
	if threadID == "" {
		var err error
		threadID, err = db.createThreadTx(ctx, tx, accountID, subject, normalizedSubject, msgID, sentAt)
		if err != nil {
			return err
		}
	}

	var parentValue any
	if parentID.Valid {
		parentValue = parentID.Int64
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET thread_id = ?, thread_parent_id = ?, message_id_normalized = ?, normalized_subject = ?, in_reply_to = ?, "references" = ? WHERE id = ?`,
		threadID, parentValue, messageID, normalizedSubject, inReplyTo, refsRaw, msgID); err != nil {
		return err
	}

	for i, ref := range refs {
		var exists int
		_ = tx.QueryRowContext(ctx,
			`SELECT 1 FROM messages WHERE account_id = ? AND message_id_normalized = ? LIMIT 1`,
			accountID, ref).Scan(&exists)
		if exists == 0 {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO unresolved_references (account_id, referenced_message_id, child_message_id, ordinal) VALUES (?, ?, ?, ?)`,
				accountID, ref, msgID, i); err != nil {
				return err
			}
		}
	}

	if messageID != "" {
		if err := db.resolveWaitingChildrenTx(ctx, tx, accountID, msgID, messageID, threadID); err != nil {
			return err
		}
		if err := db.reattachResolvedChildrenTx(ctx, tx, msgID, accountID, messageID, normalizedSubject, threadID); err != nil {
			return err
		}
	}
	return db.updateThreadAggregatesTx(ctx, tx, threadID)
}

func (db *DB) createThreadTx(ctx context.Context, tx *sql.Tx, accountID, subject, normalizedSubject string, rootMsgID int64, sentAt time.Time) (string, error) {
	threadID := uuid.NewString()
	if sentAt.IsZero() {
		sentAt = time.Now().UTC()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO threads (id, account_id, subject, normalized_subject, root_message_id, last_message_at, message_count, unread_count)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0)`,
		threadID, accountID, subject, normalizedSubject, rootMsgID, sentAt)
	return threadID, err
}

func (db *DB) findSubjectFallbackThreadTx(ctx context.Context, tx *sql.Tx, msgID int64, accountID, normalizedSubject, subject string, sentAt time.Time) string {
	if normalizedSubject == "" || !mailmessage.IsReplyOrForwardSubject(subject) {
		return ""
	}
	if sentAt.IsZero() {
		sentAt = time.Now().UTC()
	}
	participants := db.messageParticipantsTx(ctx, tx, msgID, accountID)
	if len(participants) == 0 {
		return ""
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, thread_id FROM messages
		 WHERE account_id = ? AND id != ? AND normalized_subject = ? AND thread_id IS NOT NULL AND thread_id != ''
		 AND date_received BETWEEN ? AND ?
		 ORDER BY date_received DESC LIMIT 50`,
		accountID, msgID, normalizedSubject, sentAt.AddDate(0, 0, -30), sentAt.AddDate(0, 0, 30))
	if err != nil {
		return ""
	}
	defer rows.Close()

	for rows.Next() {
		var candidateID int64
		var threadID string
		if rows.Scan(&candidateID, &threadID) != nil {
			continue
		}
		for p := range db.messageParticipantsTx(ctx, tx, candidateID, accountID) {
			if participants[p] {
				return threadID
			}
		}
	}
	return ""
}

func (db *DB) messageParticipantsTx(ctx context.Context, tx *sql.Tx, msgID int64, accountID string) map[string]bool {
	participants := make(map[string]bool)
	accountEmail := db.accountEmailTx(ctx, tx, accountID)
	add := func(email string) {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" || email == accountEmail {
			return
		}
		participants[email] = true
	}

	var from string
	_ = tx.QueryRowContext(ctx, `SELECT from_email FROM messages WHERE id = ?`, msgID).Scan(&from)
	add(from)
	rows, err := tx.QueryContext(ctx, `SELECT email FROM message_recipients WHERE message_id = ?`, msgID)
	if err != nil {
		return participants
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		if rows.Scan(&email) == nil {
			add(email)
		}
	}
	return participants
}

func (db *DB) accountEmailTx(ctx context.Context, tx *sql.Tx, accountID string) string {
	var email string
	_ = tx.QueryRowContext(ctx, `SELECT lower(email_address) FROM accounts WHERE id = ?`, accountID).Scan(&email)
	return strings.TrimSpace(email)
}

func (db *DB) resolveWaitingChildrenTx(ctx context.Context, tx *sql.Tx, accountID string, parentMsgID int64, parentMessageID, threadID string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT child_message_id FROM unresolved_references WHERE account_id = ? AND referenced_message_id = ?`,
		accountID, parentMessageID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var children []int64
	for rows.Next() {
		var childID int64
		if rows.Scan(&childID) == nil {
			children = append(children, childID)
		}
	}
	for _, childID := range children {
		var oldThread sql.NullString
		_ = tx.QueryRowContext(ctx, `SELECT thread_id FROM messages WHERE id = ?`, childID).Scan(&oldThread)
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET thread_id = ?, thread_parent_id = ? WHERE id = ?`, threadID, parentMsgID, childID); err != nil {
			return err
		}
		if oldThread.Valid && oldThread.String != "" && oldThread.String != threadID {
			if err := db.updateThreadAggregatesTx(ctx, tx, oldThread.String); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx,
		`DELETE FROM unresolved_references WHERE account_id = ? AND referenced_message_id = ?`,
		accountID, parentMessageID)
	return err
}

func (db *DB) reattachResolvedChildrenTx(ctx context.Context, tx *sql.Tx, parentMsgID int64, accountID, parentMessageID, normalizedSubject, threadID string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT m.id, m.thread_id, COALESCE(m.in_reply_to, ''), COALESCE(m.normalized_subject, '')
		 FROM messages m
		 JOIN message_references mr ON mr.message_id = m.id
		 WHERE m.account_id = ? AND m.id != ? AND mr.referenced_message_id = ?`,
		accountID, parentMsgID, parentMessageID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type child struct {
		id                int64
		threadID          sql.NullString
		inReplyTo         string
		normalizedSubject string
	}
	var children []child
	for rows.Next() {
		var c child
		if rows.Scan(&c.id, &c.threadID, &c.inReplyTo, &c.normalizedSubject) == nil {
			children = append(children, c)
		}
	}

	oldThreads := make(map[string]bool)
	for _, c := range children {
		if c.threadID.Valid && c.threadID.String == threadID {
			continue
		}
		if c.inReplyTo != parentMessageID && c.normalizedSubject != normalizedSubject {
			continue
		}
		if c.threadID.Valid && c.threadID.String != "" {
			oldThreads[c.threadID.String] = true
		}
		var parentValue any
		if c.inReplyTo == parentMessageID {
			parentValue = parentMsgID
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET thread_id = ?, thread_parent_id = ? WHERE id = ?`,
			threadID, parentValue, c.id); err != nil {
			return err
		}
	}

	for oldThread := range oldThreads {
		if oldThread != threadID {
			if err := db.updateThreadAggregatesTx(ctx, tx, oldThread); err != nil {
				return err
			}
		}
	}
	return nil
}

func (db *DB) updateThreadAggregatesTx(ctx context.Context, tx *sql.Tx, threadID string) error {
	var accountID, subject, normalizedSubject string
	var rootID int64
	err := tx.QueryRowContext(ctx,
		`SELECT account_id, subject, normalized_subject, id, date_received
		 FROM messages WHERE thread_id = ? ORDER BY date_received ASC, id ASC LIMIT 1`, threadID,
	).Scan(&accountID, &subject, &normalizedSubject, &rootID, new(sqliteNullTime))
	if err != nil {
		return nil
	}

	var count, unread int
	var latest sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN COALESCE(mfs.is_read, 1) = 0 THEN 1 ELSE 0 END), 0), MAX(m.date_received)
		 FROM messages m LEFT JOIN message_folder_state mfs ON m.id = mfs.message_id
		 WHERE m.thread_id = ?`, threadID,
	).Scan(&count, &unread, &latest); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE threads SET account_id = ?, subject = ?, normalized_subject = ?, root_message_id = ?, last_message_at = ?, message_count = ?, unread_count = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		accountID, subject, normalizedSubject, rootID, latest, count, unread, threadID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		_, err = tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO threads (id, account_id, subject, normalized_subject, root_message_id, last_message_at, message_count, unread_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			threadID, accountID, subject, normalizedSubject, rootID, latest, count, unread)
	}
	return err
}

func (db *DB) EnsureThreading(ctx context.Context) error {
	log.Printf("storage: EnsureThreading started")
	var needs int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT CASE WHEN EXISTS (SELECT 1 FROM messages WHERE COALESCE(thread_id, '') = '' OR COALESCE(message_id_normalized, '') = '')
		 OR (SELECT COUNT(*) FROM messages) > 0 AND (SELECT COUNT(*) FROM threads) = 0 THEN 1 ELSE 0 END`,
	).Scan(&needs); err != nil {
		return err
	}
	if needs == 0 {
		db.SetThreadingState(ThreadingState{InProgress: false, Processed: 0, Total: 0})
		log.Printf("storage: EnsureThreading not needed")
		return nil
	}
	log.Printf("storage: EnsureThreading backfill required")

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, account_id, internet_message_id, in_reply_to, "references", subject, date_received
		 FROM messages ORDER BY date_received ASC, id ASC`)
	if err != nil {
		return err
	}

	type row struct {
		id        int64
		accountID string
		msgID     string
		inReplyTo string
		refs      string
		subject   string
		sentAt    sql.NullString
	}
	var messages []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.accountID, &r.msgID, &r.inReplyTo, &r.refs, &r.subject, &r.sentAt); err != nil {
			rows.Close()
			return err
		}
		messages = append(messages, r)
	}
	rows.Close()
	log.Printf("storage: EnsureThreading loaded %d message(s)", len(messages))
	db.SetThreadingState(ThreadingState{InProgress: true, Processed: 0, Total: len(messages)})

	for i, m := range messages {
		messageID := mailmessage.NormalizeMessageID(m.msgID)
		if messageID == "" {
			messageID = fmt.Sprintf("local-%d@gofer.local", m.id)
		}
		inReplyTo := ""
		if ids := mailmessage.ParseMessageIDs(m.inReplyTo); len(ids) > 0 {
			inReplyTo = ids[0]
		}
		sentAt := time.Now().UTC()
		if m.sentAt.Valid {
			if parsed, ok := parseSQLiteDateTime(m.sentAt.String); ok {
				sentAt = parsed
			}
		}
		if err := db.reconcileMessageThreadTx(ctx, tx, m.id, m.accountID, messageID, inReplyTo, m.refs, m.subject, sentAt); err != nil {
			return err
		}
		if (i+1)%1000 == 0 {
			db.SetThreadingState(ThreadingState{InProgress: true, Processed: i + 1, Total: len(messages)})
			log.Printf("storage: EnsureThreading processed %d/%d", i+1, len(messages))
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("storage: EnsureThreading complete (%d messages)", len(messages))
	db.SetThreadingState(ThreadingState{InProgress: false, Processed: len(messages), Total: len(messages)})
	return nil
}

type sqliteNullTime struct {
	Time  time.Time
	Valid bool
}

func (nt *sqliteNullTime) Scan(value any) error {
	if value == nil {
		nt.Time = time.Time{}
		nt.Valid = false
		return nil
	}

	switch v := value.(type) {
	case time.Time:
		nt.Time = v.UTC()
		nt.Valid = true
		return nil
	case string:
		if parsed, ok := parseSQLiteDateTime(v); ok {
			nt.Time = parsed
			nt.Valid = true
			return nil
		}
	case []byte:
		if parsed, ok := parseSQLiteDateTime(string(v)); ok {
			nt.Time = parsed
			nt.Valid = true
			return nil
		}
	}

	nt.Time = time.Time{}
	nt.Valid = false
	return nil
}

func parseSQLiteDateTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 -0700",
		"2006-01-02 15:04:05 -0700 -0700",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

type SyncMessage struct {
	AccountID    string
	FolderID     string
	RemoteUID    uint32
	MessageID    string
	InReplyTo    string
	References   string
	Subject      string
	FromName     string
	FromEmail    string
	DateSent     time.Time
	Snippet      string
	IsRead       bool
	IsStarred    bool
	ToRecipients []Recipient
	CCRecipients []Recipient
}

type Recipient struct {
	Name  string
	Email string
}

func (db *DB) UpsertSyncMessages(ctx context.Context, msgs []SyncMessage) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	msgStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (account_id, internet_message_id, message_id_normalized, in_reply_to, "references", normalized_subject, subject, from_name, from_email,
			date_sent, date_received, snippet, preview_text)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, internet_message_id) DO UPDATE SET
			message_id_normalized = excluded.message_id_normalized,
			subject = excluded.subject,
			normalized_subject = excluded.normalized_subject,
			from_name = excluded.from_name,
			from_email = excluded.from_email,
			date_sent = excluded.date_sent,
			in_reply_to = excluded.in_reply_to,
			"references" = excluded."references",
			snippet = excluded.snippet,
			preview_text = excluded.preview_text,
			updated_at = CURRENT_TIMESTAMP`)
	if err != nil {
		return fmt.Errorf("prepare msg upsert: %w", err)
	}
	defer msgStmt.Close()

	dupUIDStmt, err := tx.PrepareContext(ctx, `
		DELETE FROM message_folder_state
		WHERE folder_id = ? AND remote_uid = ? AND message_id != ?`)
	if err != nil {
		return fmt.Errorf("prepare dup uid delete: %w", err)
	}
	defer dupUIDStmt.Close()

	stateStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, is_starred, is_flagged, is_draft, is_deleted, synced_at)
		VALUES (?, ?, ?, ?, ?, 0, 0, 0, ?)
		ON CONFLICT(message_id, folder_id) DO UPDATE SET
			remote_uid = excluded.remote_uid,
			is_read = excluded.is_read,
			is_starred = excluded.is_starred,
			synced_at = excluded.synced_at`)
	if err != nil {
		return fmt.Errorf("prepare state upsert: %w", err)
	}
	defer stateStmt.Close()

	recipStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO message_recipients (message_id, kind, name, email)
		VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare recip insert: %w", err)
	}
	defer recipStmt.Close()

	delRecipStmt, err := tx.PrepareContext(ctx, `DELETE FROM message_recipients WHERE message_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare recip delete: %w", err)
	}
	defer delRecipStmt.Close()

	for _, m := range msgs {
		messageIDNorm := mailmessage.NormalizeMessageID(m.MessageID)
		if messageIDNorm == "" {
			messageIDNorm = mailmessage.NormalizeMessageID(fmt.Sprintf("<%s-%d@sync.gofer>", m.FolderID, m.RemoteUID))
			m.MessageID = "<" + messageIDNorm + ">"
		}
		inReplyTo := ""
		if ids := mailmessage.ParseMessageIDs(m.InReplyTo); len(ids) > 0 {
			inReplyTo = ids[0]
		}
		normalizedSubject := normalizeSubject(m.Subject)

		var msgID int64
		if _, err := msgStmt.ExecContext(ctx, m.AccountID, m.MessageID, messageIDNorm, inReplyTo, m.References, normalizedSubject, m.Subject,
			m.FromName, m.FromEmail, m.DateSent, m.DateSent, m.Snippet, m.Snippet); err != nil {
			return fmt.Errorf("upsert message: %w", err)
		}
		if err := tx.QueryRow(`SELECT id FROM messages WHERE account_id = ? AND internet_message_id = ?`,
			m.AccountID, m.MessageID).Scan(&msgID); err != nil {
			return fmt.Errorf("query upserted message: %w", err)
		}

		if _, err := dupUIDStmt.ExecContext(ctx, m.FolderID, m.RemoteUID, msgID); err != nil {
			return fmt.Errorf("delete dup uid: %w", err)
		}

		if _, err := stateStmt.ExecContext(ctx, msgID, m.FolderID, m.RemoteUID,
			m.IsRead, m.IsStarred, time.Now().UTC()); err != nil {
			return fmt.Errorf("upsert state: %w", err)
		}

		if _, err := delRecipStmt.ExecContext(ctx, msgID); err != nil {
			return fmt.Errorf("delete recipients: %w", err)
		}

		for _, r := range m.ToRecipients {
			if _, err := recipStmt.ExecContext(ctx, msgID, "to", r.Name, r.Email); err != nil {
				return fmt.Errorf("insert to: %w", err)
			}
		}
		for _, r := range m.CCRecipients {
			if _, err := recipStmt.ExecContext(ctx, msgID, "cc", r.Name, r.Email); err != nil {
				return fmt.Errorf("insert cc: %w", err)
			}
		}

		if err := db.reconcileMessageThreadTx(ctx, tx, msgID, m.AccountID, messageIDNorm, inReplyTo, m.References, m.Subject, m.DateSent); err != nil {
			return fmt.Errorf("reconcile thread: %w", err)
		}
	}

	return tx.Commit()
}

func (db *DB) GetMessageLocalIDByInternetID(ctx context.Context, accountID, internetMessageID string) (int64, error) {
	var id int64
	err := db.Read().QueryRowContext(ctx,
		`SELECT id FROM messages WHERE account_id = ? AND internet_message_id = ?`, accountID, internetMessageID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (db *DB) GetFolderByAccountAndRemote(ctx context.Context, accountID, remoteID string) (string, error) {
	var id string
	err := db.Read().QueryRowContext(ctx,
		`SELECT id FROM folders WHERE account_id = ? AND remote_id = ?`, accountID, remoteID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (db *DB) UpdateFolderSyncState(ctx context.Context, folderID string, highestUID uint32, uidValidity uint32, totalCount int) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE folders SET highest_seen_uid = ?, uid_validity = ?, total_count = ?,
		 last_full_sync_at = CURRENT_TIMESTAMP, sync_error = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, highestUID, uidValidity, totalCount, folderID)
	return err
}

func (db *DB) UpdateFolderIncrementalSync(ctx context.Context, folderID string, highestUID uint32, uidValidity uint32, totalCount int) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE folders SET highest_seen_uid = MAX(COALESCE(highest_seen_uid, 0), ?), uid_validity = ?, total_count = ?,
		 last_incremental_sync_at = CURRENT_TIMESTAMP, sync_error = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, highestUID, uidValidity, totalCount, folderID)
	return err
}

func (db *DB) GetHighestSeenUID(ctx context.Context, folderID string) (uint32, error) {
	var uid uint32
	err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(
			NULLIF(highest_seen_uid, 0),
			(SELECT MAX(remote_uid) FROM message_folder_state WHERE folder_id = ?),
			0
		) FROM folders WHERE id = ?`, folderID, folderID,
	).Scan(&uid)
	return uid, err
}

func (db *DB) GetAccountIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT id FROM accounts WHERE user_id = ? AND COALESCE(is_deleting, 0) = 0 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (db *DB) GetAllAccountIDs(ctx context.Context) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT id FROM accounts WHERE COALESCE(is_deleting, 0) = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (db *DB) GetAccountUserID(ctx context.Context, accountID string) (string, error) {
	var userID sql.NullString
	err := db.Read().QueryRowContext(ctx,
		`SELECT user_id FROM accounts WHERE id = ?`, accountID).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return userID.String, nil
}

func (db *DB) GetFolderIDByRole(ctx context.Context, accountID, role string) (string, string, error) {
	var id, remoteID string
	err := db.Read().QueryRowContext(ctx,
		`SELECT id, remote_id FROM folders WHERE account_id = ? AND role = ? LIMIT 1`, accountID, role,
	).Scan(&id, &remoteID)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return id, remoteID, err
}

func (db *DB) GetFolderRole(ctx context.Context, folderID string) (string, error) {
	var role string
	err := db.Read().QueryRowContext(ctx,
		`SELECT role FROM folders WHERE id = ? LIMIT 1`, folderID,
	).Scan(&role)
	if err != nil {
		return "", err
	}
	return role, nil
}

func (db *DB) RefreshFolderUnreadCount(ctx context.Context, folderID string) (int, error) {
	var count int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_folder_state
		 WHERE folder_id = ? AND is_read = 0 AND is_deleted = 0`, folderID,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	_, err = db.Write().ExecContext(ctx,
		`UPDATE folders SET unread_count = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		count, folderID)
	return count, err
}

func (db *DB) GetAllFolderUnreadCounts(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT f.id, f.unread_count FROM folders f JOIN accounts a ON f.account_id = a.id WHERE a.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		result[id] = count
	}

	unifiedRows, err := db.Read().QueryContext(ctx,
		`SELECT f.role, COUNT(*)
		 FROM message_folder_state mfs
		 JOIN folders f ON mfs.folder_id = f.id
		 JOIN accounts a ON f.account_id = a.id
		 WHERE a.user_id = ? AND f.role IN ('inbox', 'sent', 'drafts', 'archive', 'spam', 'trash')
		 AND mfs.is_read = 0 AND mfs.is_deleted = 0
		 GROUP BY f.role`, userID)
	if err != nil {
		return nil, err
	}
	defer unifiedRows.Close()
	for unifiedRows.Next() {
		var role string
		var count int
		if err := unifiedRows.Scan(&role, &count); err != nil {
			return nil, err
		}
		result[role] = count
	}

	var starred int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT m.id)
		 FROM message_folder_state mfs
		 JOIN messages m ON mfs.message_id = m.id
		 JOIN accounts a ON m.account_id = a.id
		 WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_deleted = 0`, userID).Scan(&starred); err != nil {
		return nil, err
	}
	result["starred"] = starred
	return result, nil
}

func (db *DB) GetFolderHighestUID(ctx context.Context, folderID string) (uint32, error) {
	var uid uint32
	err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(remote_uid), 0) FROM message_folder_state WHERE folder_id = ?`, folderID,
	).Scan(&uid)
	return uid, err
}

func (db *DB) GetAccounts(ctx context.Context, userID string) ([]models.Account, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, email_address, display_name, color, initials, COALESCE(is_deleting, 0) FROM accounts WHERE user_id = ? AND COALESCE(is_deleting, 0) = 0 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []models.Account
	for rows.Next() {
		var a models.Account
		var isDeleting int
		if err := rows.Scan(&a.ID, &a.Email, &a.Name, &a.Color, &a.Initials, &isDeleting); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		a.IsDeleting = isDeleting == 1
		accounts = append(accounts, a)
	}

	for i := range accounts {
		folders, err := db.getFolders(ctx, accounts[i].ID)
		if err != nil {
			return nil, fmt.Errorf("get folders for %s: %w", accounts[i].ID, err)
		}
		accounts[i].Folders = folders
	}

	return accounts, nil
}

func (db *DB) GetAccountsIncludingDeleting(ctx context.Context, userID string) ([]models.Account, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, email_address, display_name, color, initials, COALESCE(is_deleting, 0) FROM accounts WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []models.Account
	for rows.Next() {
		var a models.Account
		var isDeleting int
		if err := rows.Scan(&a.ID, &a.Email, &a.Name, &a.Color, &a.Initials, &isDeleting); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		a.IsDeleting = isDeleting == 1
		accounts = append(accounts, a)
	}

	for i := range accounts {
		if accounts[i].IsDeleting {
			continue
		}
		folders, err := db.getFolders(ctx, accounts[i].ID)
		if err != nil {
			return nil, fmt.Errorf("get folders for %s: %w", accounts[i].ID, err)
		}
		accounts[i].Folders = folders
	}

	return accounts, nil
}

func (db *DB) getFolders(ctx context.Context, accountID string) ([]models.Folder, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, name, icon, role, unread_count, parent_id
		 FROM folders WHERE account_id = ? ORDER BY sort_order`, accountID)
	if err != nil {
		return nil, fmt.Errorf("query folders: %w", err)
	}
	defer rows.Close()

	var flat []folderRow
	for rows.Next() {
		var fr folderRow
		var role string
		if err := rows.Scan(&fr.folder.ID, &fr.folder.Name, &fr.folder.Icon, &role, &fr.folder.Unread, &fr.parentID); err != nil {
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		fr.folder.Role = role
		fr.folder.IsSystem = role != "custom"
		flat = append(flat, fr)
	}

	return buildFolderTree(flat), nil
}

func buildFolderTree(flat []folderRow) []models.Folder {
	childrenMap := make(map[string][]models.Folder)
	var roots []models.Folder

	for _, fr := range flat {
		if fr.parentID.Valid && fr.parentID.String != "" {
			childrenMap[fr.parentID.String] = append(childrenMap[fr.parentID.String], fr.folder)
		} else {
			roots = append(roots, fr.folder)
		}
	}

	for i := range roots {
		if children, ok := childrenMap[roots[i].ID]; ok {
			roots[i].Children = children
		}
	}
	return roots
}

func (db *DB) GetFolderEmailCount(ctx context.Context, folderID string) (int, error) {
	return db.GetFolderEmailCountFiltered(ctx, folderID, models.EmailFilters{})
}

func (db *DB) GetFolderEmailCountForUser(ctx context.Context, userID, folderID string) (int, error) {
	return db.GetFolderEmailCountFilteredForUser(ctx, userID, folderID, models.EmailFilters{})
}

func (db *DB) GetFolderEmailCountFilteredForUser(ctx context.Context, userID, folderID string, filters models.EmailFilters) (int, error) {
	if !isUnifiedFolderID(folderID) {
		return db.GetFolderEmailCountFiltered(ctx, folderID, filters)
	}

	filterSQL := emailFilterSQL(filters)
	var where string
	var args []any
	if folderID == "starred" {
		where = `JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_deleted = 0`
		args = []any{userID}
	} else {
		where = `JOIN folders f ON mfs.folder_id = f.id
			 JOIN accounts a ON f.account_id = a.id
			 WHERE a.user_id = ? AND f.role = ? AND mfs.is_deleted = 0`
		args = []any{userID, folderID}
	}
	args = append(args, filterSQL.args...)

	var count int
	err := db.Read().QueryRowContext(ctx,
		`WITH visible AS (
		 SELECT ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
		        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
		        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
		        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments,
		        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count
		 FROM message_folder_state mfs
		 JOIN messages m ON mfs.message_id = m.id
		 `+where+filterSQL.cteClause+`
	)
	SELECT COUNT(*) FROM visible WHERE rn = 1`+filterSQL.outerClause, args...).Scan(&count)
	return count, err
}

func (db *DB) GetFolderEmailCountFiltered(ctx context.Context, folderID string, filters models.EmailFilters) (int, error) {
	filterSQL := emailFilterSQL(filters)
	if isStarredFolder(folderID) {
		var count int
		err := db.Read().QueryRowContext(ctx,
			`WITH visible AS (
			 SELECT ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
			        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
			        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
			        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments,
			        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count
			 FROM messages m
			 JOIN message_folder_state mfs ON m.id = mfs.message_id
			 JOIN folders f ON mfs.folder_id = f.id
			 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
			 AND mfs.is_starred = 1 AND mfs.is_deleted = 0`+filterSQL.cteClause+`
			)
			SELECT COUNT(*) FROM visible WHERE rn = 1`+filterSQL.outerClause, append([]any{folderID}, filterSQL.args...)...).Scan(&count)
		return count, err
	}

	var count int
	err := db.Read().QueryRowContext(ctx,
		`WITH visible AS (
		 SELECT ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
		        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
		        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
		        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments,
		        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count
		 FROM message_folder_state mfs JOIN messages m ON mfs.message_id = m.id
		 WHERE mfs.folder_id = ? AND mfs.is_deleted = 0`+filterSQL.cteClause+`
		)
		SELECT COUNT(*) FROM visible WHERE rn = 1`+filterSQL.outerClause, append([]any{folderID}, filterSQL.args...)...).Scan(&count)
	return count, err
}

func (db *DB) GetEmailsRange(ctx context.Context, folderID string, start, limit int) (*models.EmailPage, error) {
	return db.GetEmailsRangeFiltered(ctx, folderID, start, limit, models.EmailFilters{})
}

func (db *DB) GetEmailsRangeForUser(ctx context.Context, userID, folderID string, start, limit int) (*models.EmailPage, error) {
	return db.GetEmailsRangeFilteredForUser(ctx, userID, folderID, start, limit, models.EmailFilters{})
}

func (db *DB) GetEmailsRangeFilteredForUser(ctx context.Context, userID, folderID string, start, limit int, filters models.EmailFilters) (*models.EmailPage, error) {
	if !isUnifiedFolderID(folderID) {
		return db.GetEmailsRangeFiltered(ctx, folderID, start, limit, filters)
	}

	totalCount, err := db.GetFolderEmailCountFilteredForUser(ctx, userID, folderID, filters)
	if err != nil {
		return nil, err
	}
	if start >= totalCount {
		return &models.EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start}, nil
	}

	emails, err := db.listEmailsFilteredForUser(ctx, userID, folderID, start, limit, filters)
	if err != nil {
		return nil, err
	}
	end := start + len(emails)
	hasMore := end < totalCount
	nextCursor := ""
	if len(emails) > 0 && hasMore {
		nextCursor = emails[len(emails)-1].ID
	}

	return &models.EmailPage{
		Emails:      emails,
		TotalCount:  totalCount,
		WindowStart: start,
		WindowEnd:   end - 1,
		NextCursor:  nextCursor,
		HasMore:     hasMore,
	}, nil
}

func (db *DB) GetEmailsRangeFiltered(ctx context.Context, folderID string, start, limit int, filters models.EmailFilters) (*models.EmailPage, error) {
	totalCount, err := db.GetFolderEmailCountFiltered(ctx, folderID, filters)
	if err != nil {
		return nil, err
	}

	if start >= totalCount {
		return &models.EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start}, nil
	}

	emails, err := db.listEmailsFiltered(ctx, folderID, start, limit, filters)
	if err != nil {
		return nil, err
	}

	end := start + len(emails)
	hasMore := end < totalCount
	nextCursor := ""
	if len(emails) > 0 && hasMore {
		nextCursor = emails[len(emails)-1].ID
	}

	return &models.EmailPage{
		Emails:      emails,
		TotalCount:  totalCount,
		WindowStart: start,
		WindowEnd:   end - 1,
		NextCursor:  nextCursor,
		HasMore:     hasMore,
	}, nil
}

type emailFilterParts struct {
	cteClause   string
	outerClause string
	args        []any
}

func emailFilterSQL(filters models.EmailFilters) emailFilterParts {
	var cteParts []string
	var outerParts []string
	var args []any
	if filters.Unread {
		outerParts = append(outerParts, "thread_is_read = 0")
	}
	if filters.Read {
		outerParts = append(outerParts, "thread_is_read = 1")
	}
	if filters.Starred {
		outerParts = append(outerParts, "thread_is_starred = 1")
	}
	if filters.Attachments {
		outerParts = append(outerParts, "thread_has_attachments = 1")
	}
	if filters.NoAttach {
		outerParts = append(outerParts, "thread_has_attachments = 0")
	}
	if filters.ThreadsOnly {
		outerParts = append(outerParts, "thread_count > 1")
	}
	if filters.HasLabels {
		cteParts = append(cteParts, "EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id)")
	}
	if filters.AccountID != "" {
		cteParts = append(cteParts, "m.account_id = ?")
		args = append(args, filters.AccountID)
	}
	if filters.Query != "" {
		cteParts = append(cteParts, `EXISTS (
			SELECT 1 FROM message_search_docs msd
			WHERE msd.message_id = m.id
			AND (msd.subject LIKE ? OR msd.sender LIKE ? OR msd.recipients LIKE ? OR msd.body_text LIKE ? OR msd.attachment_names LIKE ?)
		)`)
		like := "%" + filters.Query + "%"
		args = append(args, like, like, like, like, like)
	}
	if filters.From != "" {
		cteParts = append(cteParts, "(m.from_name LIKE ? OR m.from_email LIKE ?)")
		like := "%" + filters.From + "%"
		args = append(args, like, like)
	}
	if filters.FromDomain != "" {
		domain := strings.TrimPrefix(strings.ToLower(filters.FromDomain), "@")
		cteParts = append(cteParts, "lower(m.from_email) LIKE ?")
		args = append(args, "%@"+domain)
	}
	if filters.To != "" {
		cteParts = append(cteParts, "EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id = m.id AND mr.kind IN ('to', 'cc') AND (mr.name LIKE ? OR mr.email LIKE ?))")
		like := "%" + filters.To + "%"
		args = append(args, like, like)
	}
	if filters.Subject != "" {
		cteParts = append(cteParts, "m.subject LIKE ?")
		args = append(args, "%"+filters.Subject+"%")
	}
	if filters.Body != "" {
		cteParts = append(cteParts, "EXISTS (SELECT 1 FROM message_search_docs msd WHERE msd.message_id = m.id AND msd.body_text LIKE ?)")
		args = append(args, "%"+filters.Body+"%")
	}
	if filters.Attachment != "" {
		cteParts = append(cteParts, "EXISTS (SELECT 1 FROM attachments att WHERE att.message_id = m.id AND att.filename LIKE ?)")
		args = append(args, "%"+filters.Attachment+"%")
	}
	if filters.Label != "" {
		cteParts = append(cteParts, "EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON ml.label_id = l.id WHERE ml.message_id = m.id AND l.name LIKE ?)")
		args = append(args, "%"+filters.Label+"%")
	}
	if filters.After != "" {
		cteParts = append(cteParts, "date(m.date_received) >= date(?)")
		args = append(args, filters.After)
	}
	if filters.Before != "" {
		cteParts = append(cteParts, "date(m.date_received) <= date(?)")
		args = append(args, filters.Before)
	}

	parts := emailFilterParts{args: args}
	if len(cteParts) > 0 {
		parts.cteClause = " AND " + strings.Join(cteParts, " AND ")
	}
	if len(outerParts) > 0 {
		parts.outerClause = " AND " + strings.Join(outerParts, " AND ")
	}
	return parts
}

func (db *DB) GetThreadMessages(ctx context.Context, accountID, threadID string) ([]models.ThreadItem, error) {
	if threadID == "" {
		return nil, nil
	}

	now := time.Now()
	rows, err := db.Read().QueryContext(ctx,
		`SELECT m.id, m.account_id, a.color, m.subject, m.from_name, m.from_email, m.snippet,
		        m.date_received, m.has_attachments,
		        COALESCE((SELECT is_read FROM message_folder_state WHERE message_id = m.id LIMIT 1), 1),
		        COALESCE((SELECT is_starred FROM message_folder_state WHERE message_id = m.id LIMIT 1), 0),
		        COALESCE((SELECT f.name FROM message_folder_state mfs JOIN folders f ON mfs.folder_id = f.id WHERE mfs.message_id = m.id AND mfs.is_deleted = 0 LIMIT 1), ''),
		        COALESCE((SELECT f.role FROM message_folder_state mfs JOIN folders f ON mfs.folder_id = f.id WHERE mfs.message_id = m.id AND mfs.is_deleted = 0 LIMIT 1), ''),
		        m.internet_message_id, m."references", m.body_text_path
		 FROM messages m
		 JOIN accounts a ON m.account_id = a.id
		 WHERE m.account_id = ? AND m.thread_id = ?
		 ORDER BY m.date_received ASC`, accountID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.ThreadItem
	for rows.Next() {
		var (
			item          models.ThreadItem
			fromName      string
			fromEmail     string
			dateReceived  sqliteNullTime
			hasAttach     int
			isRead        int
			isStarred     int
			internetMsgID sql.NullString
			refs          sql.NullString
			bodyTextPath  sql.NullString
		)
		if err := rows.Scan(&item.ID, &item.AccountID, &item.AccountColor, &item.Subject, &fromName, &fromEmail, &item.Preview,
			&dateReceived, &hasAttach, &isRead, &isStarred, &item.FolderName, &item.FolderRole,
			&internetMsgID, &refs, &bodyTextPath); err != nil {
			continue
		}
		item.From = contactFromSender(fromName, fromEmail)
		item.IsRead = isRead == 1
		item.IsStarred = isStarred == 1
		item.HasAttachment = hasAttach == 1
		if internetMsgID.Valid {
			item.InternetMessageID = internetMsgID.String
		}
		if refs.Valid {
			item.References = refs.String
		}
		if bodyTextPath.Valid && bodyTextPath.String != "" {
			if data, err := os.ReadFile(bodyTextPath.String); err == nil {
				item.TextBody = strings.TrimSpace(string(data))
			} else {
				item.TextBody = item.Preview
			}
		} else {
			item.TextBody = item.Preview
		}
		if dateReceived.Valid {
			item.Date = formatRelativeDate(dateReceived.Time, now)
			item.DateFull = dateReceived.Time.Local().Format("Mon, Jan 2, 2006 at 3:04 PM")
		}
		items = append(items, item)
	}
	if len(items) > 0 {
		msgIDs := make([]int64, 0, len(items))
		index := make(map[string]int, len(items))
		for i, item := range items {
			id, err := strconv.ParseInt(item.ID, 10, 64)
			if err == nil {
				msgIDs = append(msgIDs, id)
				index[item.ID] = i
			}
		}
		labelsMap, _ := db.batchGetLabels(ctx, msgIDs)
		for msgID, labels := range labelsMap {
			if i, ok := index[strconv.FormatInt(msgID, 10)]; ok {
				items[i].Labels = labels
			}
		}
		for i, item := range items {
			id, err := strconv.ParseInt(item.ID, 10, 64)
			if err != nil {
				continue
			}
			items[i].To, _ = db.getRecipients(ctx, id, "to")
			items[i].CC, _ = db.getRecipients(ctx, id, "cc")
			if item.HasAttachment {
				items[i].Attachments, _ = db.GetAttachments(ctx, id)
			}
		}
	}

	return items, nil
}

func (db *DB) GetEmailByID(ctx context.Context, id string) (*models.Email, error) {
	msgID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, nil
	}

	var (
		email             models.Email
		dateReceived      sqliteNullTime
		fromName          string
		fromEmail         string
		subject           string
		snippet           string
		accountID         string
		accountColor      string
		hasAttach         int
		bodyTextPath      sql.NullString
		bodyHTMLPath      sql.NullString
		internetMessageID sql.NullString
		threadID          sql.NullString
		inReplyTo         string
		references        string
	)

	err = db.Read().QueryRowContext(ctx,
		`SELECT m.id, m.account_id, a.color, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments,
		        m.body_text_path, m.body_html_path, m.internet_message_id, m.thread_id, m.in_reply_to, m."references"
		 FROM messages m
		 JOIN accounts a ON m.account_id = a.id
		 WHERE m.id = ?`, msgID,
	).Scan(&msgID, &accountID, &accountColor, &subject, &fromName, &fromEmail, &dateReceived, &snippet, &hasAttach, &bodyTextPath, &bodyHTMLPath, &internetMessageID, &threadID, &inReplyTo, &references)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query message: %w", err)
	}

	now := time.Now()
	email.ID = strconv.FormatInt(msgID, 10)
	email.AccountID = accountID
	email.AccountColor = accountColor
	email.Subject = subject
	email.From = contactFromSender(fromName, fromEmail)
	email.Preview = snippet
	email.HasAttachment = hasAttach == 1

	if internetMessageID.Valid {
		email.InternetMessageID = internetMessageID.String
	}
	email.InReplyTo = inReplyTo
	email.References = references

	if bodyTextPath.Valid && bodyTextPath.String != "" {
		data, err := os.ReadFile(bodyTextPath.String)
		if err == nil {
			email.TextBody = strings.TrimSpace(string(data))
			email.Body = template.HTML("<pre style=\"white-space:pre-wrap;word-wrap:break-word;font-family:inherit\">" + template.HTML(template.HTMLEscapeString(string(data))) + "</pre>")
		} else {
			email.TextBody = snippet
			email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
		}
	} else if bodyHTMLPath.Valid && bodyHTMLPath.String != "" {
		data, err := os.ReadFile(bodyHTMLPath.String)
		if err == nil {
			email.TextBody = stripHTMLTags(string(data))
			email.Body = template.HTML(data)
		} else {
			email.TextBody = snippet
			email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
		}
	} else {
		email.TextBody = snippet
		email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
	}
	if dateReceived.Valid {
		email.Date = formatRelativeDate(dateReceived.Time, now)
		email.DateFull = dateReceived.Time.Local().Format("Mon, Jan 2, 2006 at 3:04 PM")
	}

	if threadID.Valid {
		email.ThreadID = threadID.String
	}

	var folderID string
	var isRead, isStarred int
	err = db.Read().QueryRowContext(ctx,
		`SELECT folder_id, is_read, is_starred FROM message_folder_state WHERE message_id = ? LIMIT 1`, msgID,
	).Scan(&folderID, &isRead, &isStarred)
	if err == nil {
		email.FolderID = folderID
		email.IsRead = isRead == 1
		email.IsStarred = isStarred == 1
	}
	if email.ThreadID != "" {
		var threadCount, threadIsRead int
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT m.id), COALESCE(MIN(mfs.is_read), 1)
			 FROM messages m
			 JOIN message_folder_state mfs ON m.id = mfs.message_id
			 WHERE m.account_id = ? AND m.thread_id = ? AND mfs.is_deleted = 0`, accountID, email.ThreadID,
		).Scan(&threadCount, &threadIsRead); err == nil {
			email.ThreadCount = threadCount
			if threadCount > 1 {
				email.IsRead = threadIsRead == 1
			}
		}
	}

	email.To, _ = db.getRecipients(ctx, msgID, "to")
	email.CC, _ = db.getRecipients(ctx, msgID, "cc")
	email.Labels, _ = db.getMessageLabels(ctx, msgID)
	email.Attachments, _ = db.GetAttachments(ctx, msgID)

	return &email, nil
}

func (db *DB) GetEmailsAfterCursor(ctx context.Context, folderID, cursor string, limit int) (*models.EmailPage, error) {
	pos, err := db.findEmailPosition(ctx, folderID, cursor)
	if err != nil {
		return nil, err
	}
	return db.GetEmailsRange(ctx, folderID, pos+1, limit)
}

func (db *DB) GetEmailsAfterCursorForUser(ctx context.Context, userID, folderID, cursor string, limit int) (*models.EmailPage, error) {
	if !isUnifiedFolderID(folderID) {
		return db.GetEmailsAfterCursor(ctx, folderID, cursor, limit)
	}
	pos, err := db.findEmailPositionForUser(ctx, userID, folderID, cursor)
	if err != nil {
		return nil, err
	}
	return db.GetEmailsRangeForUser(ctx, userID, folderID, pos+1, limit)
}

func (db *DB) GetEmailsAroundEmail(ctx context.Context, folderID, emailID string, limit int) (*models.EmailPage, error) {
	pos, err := db.findEmailPosition(ctx, folderID, emailID)
	if err != nil {
		return nil, err
	}
	if pos < 0 {
		return db.GetEmailsRange(ctx, folderID, 0, limit)
	}

	half := limit / 2
	start := pos - half
	if start < 0 {
		start = 0
	}
	return db.GetEmailsRange(ctx, folderID, start, limit)
}

func (db *DB) GetEmailsAroundEmailForUser(ctx context.Context, userID, folderID, emailID string, limit int) (*models.EmailPage, error) {
	if !isUnifiedFolderID(folderID) {
		return db.GetEmailsAroundEmail(ctx, folderID, emailID, limit)
	}
	pos, err := db.findEmailPositionForUser(ctx, userID, folderID, emailID)
	if err != nil {
		return nil, err
	}
	if pos < 0 {
		return db.GetEmailsRangeForUser(ctx, userID, folderID, 0, limit)
	}

	half := limit / 2
	start := pos - half
	if start < 0 {
		start = 0
	}
	return db.GetEmailsRangeForUser(ctx, userID, folderID, start, limit)
}

func (db *DB) listEmails(ctx context.Context, folderID string, offset, limit int) ([]models.Email, error) {
	return db.listEmailsFiltered(ctx, folderID, offset, limit, models.EmailFilters{})
}

func (db *DB) listEmailsFilteredForUser(ctx context.Context, userID, folderID string, offset, limit int, filters models.EmailFilters) ([]models.Email, error) {
	filterSQL := emailFilterSQL(filters)
	var where string
	var args []any
	if folderID == "starred" {
		where = `JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_deleted = 0`
		args = []any{userID}
	} else {
		where = `JOIN folders f ON mfs.folder_id = f.id
			 JOIN accounts a ON f.account_id = a.id
			 WHERE a.user_id = ? AND f.role = ? AND mfs.is_deleted = 0`
		args = []any{userID, folderID}
	}

	query := `WITH visible AS (
			  SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
			         m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
			         mfs.folder_id, mfs.is_read, mfs.is_starred, m.thread_id,
			         ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
			         COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count,
			         MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
			         MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
			         MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments
			  FROM messages m
			  JOIN message_folder_state mfs ON m.id = mfs.message_id
			  ` + where + filterSQL.cteClause + `
			)
			SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
			       thread_has_attachments, folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
			FROM visible WHERE rn = 1` + filterSQL.outerClause + `
			ORDER BY date_received DESC
			LIMIT ? OFFSET ?`
	args = append(args, filterSQL.args...)
	args = append(args, limit, offset)

	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list emails: %w", err)
	}
	defer rows.Close()
	return db.scanEmailRows(ctx, rows)
}

func (db *DB) listEmailsFiltered(ctx context.Context, folderID string, offset, limit int, filters models.EmailFilters) ([]models.Email, error) {
	filterSQL := emailFilterSQL(filters)
	query := `WITH visible AS (
			  SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
			         m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
			         mfs.folder_id, mfs.is_read, mfs.is_starred, m.thread_id,
			         ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
			         COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count,
			         MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
			         MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
			         MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments
			  FROM messages m
			  JOIN message_folder_state mfs ON m.id = mfs.message_id
			  JOIN accounts a ON m.account_id = a.id
			  WHERE mfs.folder_id = ? AND mfs.is_deleted = 0` + filterSQL.cteClause + `
			)
			SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
			       thread_has_attachments, folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
			FROM visible WHERE rn = 1` + filterSQL.outerClause + `
			ORDER BY date_received DESC
			LIMIT ? OFFSET ?`

	var args []any
	if isStarredFolder(folderID) {
		query = `WITH visible AS (
			 SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
			        m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
				        mfs.folder_id, mfs.is_read, mfs.is_starred, m.thread_id,
				        ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
				        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count,
				        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
				        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
				        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments
			 FROM messages m
			 JOIN message_folder_state mfs ON m.id = mfs.message_id
			 JOIN folders f ON mfs.folder_id = f.id
			 JOIN accounts a ON m.account_id = a.id
					 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
					 AND mfs.is_starred = 1 AND mfs.is_deleted = 0` + filterSQL.cteClause + `
			)
			SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
			       thread_has_attachments, folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
			FROM visible WHERE rn = 1` + filterSQL.outerClause + `
			ORDER BY date_received DESC
			LIMIT ? OFFSET ?`
	}
	args = append([]any{folderID}, filterSQL.args...)
	args = append(args, limit, offset)

	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list emails: %w", err)
	}
	defer rows.Close()
	return db.scanEmailRows(ctx, rows)
}

func (db *DB) scanEmailRows(ctx context.Context, rows *sql.Rows) ([]models.Email, error) {
	type emailRow struct {
		email models.Email
		msgID int64
	}

	var items []emailRow
	now := time.Now()

	for rows.Next() {
		var r emailRow
		var dateReceived sqliteNullTime
		var isRead, isStarred, hasAttach, threadHasAttach int
		var subject, fromName, fromEmail, snippet, accountID, accountColor string
		var textPath, htmlPath sql.NullString
		var threadID sql.NullString
		var threadCount int

		if err := rows.Scan(&r.msgID, &accountID, &accountColor, &subject, &fromName, &fromEmail,
			&dateReceived, &snippet, &hasAttach, &textPath, &htmlPath,
			&threadHasAttach, &r.email.FolderID, &isRead, &isStarred,
			&threadID, &threadCount); err != nil {
			return nil, fmt.Errorf("scan email: %w", err)
		}

		r.email.ID = strconv.FormatInt(r.msgID, 10)
		r.email.AccountID = accountID
		r.email.AccountColor = accountColor
		r.email.Subject = subject
		r.email.From = contactFromSender(fromName, fromEmail)
		r.email.Preview = snippet
		if r.email.Preview == "" || r.email.Preview == subject {
			if preview := previewFromBodyPaths(nullStringValue(textPath), nullStringValue(htmlPath)); preview != "" {
				r.email.Preview = preview
			}
		}
		r.email.IsRead = isRead == 1
		r.email.IsStarred = isStarred == 1
		r.email.HasAttachment = hasAttach == 1 || threadHasAttach == 1
		if threadCount > 1 {
			r.email.ThreadCount = threadCount
		}
		if threadID.Valid {
			r.email.ThreadID = threadID.String
		}
		if dateReceived.Valid {
			r.email.Date = formatRelativeDate(dateReceived.Time, now)
		}
		items = append(items, r)
	}

	if len(items) > 0 {
		msgIDs := make([]int64, len(items))
		for i, r := range items {
			msgIDs[i] = r.msgID
		}
		labelsMap, _ := db.batchGetLabels(ctx, msgIDs)
		toMap, _ := db.batchGetRecipients(ctx, msgIDs, "to")
		for i := range items {
			items[i].email.Labels = labelsMap[items[i].msgID]
			items[i].email.To = toMap[items[i].msgID]
		}
	}

	emails := make([]models.Email, len(items))
	for i, r := range items {
		emails[i] = r.email
	}
	return emails, nil
}

func (db *DB) findEmailPosition(ctx context.Context, folderID, emailID string) (int, error) {
	msgID, err := strconv.ParseInt(emailID, 10, 64)
	if err != nil {
		return -1, nil
	}

	var query string
	var args []any

	if isStarredFolder(folderID) {
		query = `WITH selected AS (
				 SELECT COALESCE(NULLIF(thread_id, ''), printf('msg:%d', id)) AS thread_key FROM messages WHERE id = ?
			), visible AS (
				 SELECT COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) AS thread_key, MAX(m.date_received) AS latest
				 FROM message_folder_state mfs
				 JOIN messages m ON mfs.message_id = m.id
				 JOIN folders f ON mfs.folder_id = f.id
				 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
				 AND mfs.is_starred = 1 AND mfs.is_deleted = 0
				 GROUP BY thread_key
			)
			SELECT COUNT(*) FROM visible WHERE latest > (SELECT latest FROM visible WHERE thread_key = (SELECT thread_key FROM selected))`
		args = []any{msgID, folderID}
	} else {
		query = `WITH selected AS (
				 SELECT COALESCE(NULLIF(thread_id, ''), printf('msg:%d', id)) AS thread_key FROM messages WHERE id = ?
			), visible AS (
				 SELECT COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) AS thread_key, MAX(m.date_received) AS latest
				 FROM message_folder_state mfs
				 JOIN messages m ON mfs.message_id = m.id
				 WHERE mfs.folder_id = ? AND mfs.is_deleted = 0
				 GROUP BY thread_key
			)
			SELECT COUNT(*) FROM visible WHERE latest > (SELECT latest FROM visible WHERE thread_key = (SELECT thread_key FROM selected))`
		args = []any{msgID, folderID}
	}

	var pos int
	if err := db.Read().QueryRowContext(ctx, query, args...).Scan(&pos); err != nil {
		return -1, err
	}
	return pos, nil
}

func (db *DB) findEmailPositionForUser(ctx context.Context, userID, folderID, emailID string) (int, error) {
	msgID, err := strconv.ParseInt(emailID, 10, 64)
	if err != nil {
		return -1, nil
	}

	var where string
	var args []any
	if folderID == "starred" {
		where = `JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_deleted = 0`
		args = []any{msgID, userID}
	} else {
		where = `JOIN folders f ON mfs.folder_id = f.id
			 JOIN accounts a ON f.account_id = a.id
			 WHERE a.user_id = ? AND f.role = ? AND mfs.is_deleted = 0`
		args = []any{msgID, userID, folderID}
	}

	query := `WITH selected AS (
			 SELECT COALESCE(NULLIF(thread_id, ''), printf('msg:%d', id)) AS thread_key FROM messages WHERE id = ?
		), visible AS (
			 SELECT COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) AS thread_key, MAX(m.date_received) AS latest
			 FROM message_folder_state mfs
			 JOIN messages m ON mfs.message_id = m.id
			 ` + where + `
			 GROUP BY thread_key
		)
		SELECT COUNT(*) FROM visible WHERE latest > (SELECT latest FROM visible WHERE thread_key = (SELECT thread_key FROM selected))`

	var pos int
	if err := db.Read().QueryRowContext(ctx, query, args...).Scan(&pos); err != nil {
		return -1, err
	}
	return pos, nil
}

func (db *DB) getRecipients(ctx context.Context, messageID int64, kind string) ([]models.Contact, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT name, email FROM message_recipients WHERE message_id = ? AND kind = ?`, messageID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contacts []models.Contact
	for rows.Next() {
		var c models.Contact
		if err := rows.Scan(&c.Name, &c.Email); err != nil {
			return nil, err
		}
		c.Initials = initials(c.Name)
		contacts = append(contacts, c)
	}
	return contacts, nil
}

func (db *DB) getMessageLabels(ctx context.Context, messageID int64) ([]models.Label, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT l.name, l.color FROM labels l
		 JOIN message_labels ml ON l.id = ml.label_id
		 WHERE ml.message_id = ?`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var labels []models.Label
	for rows.Next() {
		var l models.Label
		if err := rows.Scan(&l.Name, &l.Color); err != nil {
			return nil, err
		}
		labels = append(labels, l)
	}
	return labels, nil
}

func (db *DB) batchGetRecipients(ctx context.Context, msgIDs []int64, kind string) (map[int64][]models.Contact, error) {
	placeholders := make([]string, len(msgIDs))
	args := make([]any, 0, len(msgIDs)+1)
	for _, id := range msgIDs {
		placeholders[len(args)] = "?"
		args = append(args, id)
	}
	args = append(args, kind)

	query := fmt.Sprintf(
		`SELECT message_id, name, email
		 FROM message_recipients
		 WHERE message_id IN (%s) AND kind = ?
		 ORDER BY message_id, id`, strings.Join(placeholders, ","))

	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]models.Contact)
	for rows.Next() {
		var msgID int64
		var c models.Contact
		if err := rows.Scan(&msgID, &c.Name, &c.Email); err != nil {
			return nil, err
		}
		c.Initials = initials(c.Name)
		result[msgID] = append(result[msgID], c)
	}
	return result, nil
}

func (db *DB) batchGetLabels(ctx context.Context, msgIDs []int64) (map[int64][]models.Label, error) {
	placeholders := make([]string, len(msgIDs))
	args := make([]any, len(msgIDs))
	for i, id := range msgIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(
		`SELECT ml.message_id, l.name, l.color
		 FROM message_labels ml
		 JOIN labels l ON ml.label_id = l.id
		 WHERE ml.message_id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]models.Label)
	for rows.Next() {
		var msgID int64
		var l models.Label
		if err := rows.Scan(&msgID, &l.Name, &l.Color); err != nil {
			return nil, err
		}
		result[msgID] = append(result[msgID], l)
	}
	return result, nil
}

func (db *DB) SearchMessages(ctx context.Context, userID string, query string, limit int) ([]models.Email, error) {
	if query == "" {
		return nil, nil
	}

	rows, err := db.Read().QueryContext(ctx,
		`SELECT m.id, m.account_id, a.color, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
		        mfs.folder_id, mfs.is_read, mfs.is_starred
		 FROM message_fts fts
		 JOIN messages m ON fts.rowid = m.id
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 JOIN accounts a ON m.account_id = a.id
		 WHERE a.user_id = ? AND message_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, userID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	type emailRow struct {
		email models.Email
		msgID int64
	}

	var items []emailRow
	now := time.Now()

	for rows.Next() {
		var r emailRow
		var dateReceived sqliteNullTime
		var isRead, isStarred, hasAttach int
		var subject, fromName, fromEmail, snippet, accountID, accountColor string
		var textPath, htmlPath sql.NullString

		if err := rows.Scan(&r.msgID, &accountID, &accountColor, &subject, &fromName, &fromEmail,
			&dateReceived, &snippet, &hasAttach, &textPath, &htmlPath,
			&r.email.FolderID, &isRead, &isStarred); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}

		r.email.ID = strconv.FormatInt(r.msgID, 10)
		r.email.AccountID = accountID
		r.email.AccountColor = accountColor
		r.email.Subject = subject
		r.email.From = contactFromSender(fromName, fromEmail)
		r.email.Preview = snippet
		if r.email.Preview == "" || r.email.Preview == subject {
			if preview := previewFromBodyPaths(nullStringValue(textPath), nullStringValue(htmlPath)); preview != "" {
				r.email.Preview = preview
			}
		}
		r.email.IsRead = isRead == 1
		r.email.IsStarred = isStarred == 1
		r.email.HasAttachment = hasAttach == 1
		if dateReceived.Valid {
			r.email.Date = formatRelativeDate(dateReceived.Time, now)
		}
		items = append(items, r)
	}

	if len(items) > 0 {
		msgIDs := make([]int64, len(items))
		for i, r := range items {
			msgIDs[i] = r.msgID
		}
		labelsMap, _ := db.batchGetLabels(ctx, msgIDs)
		toMap, _ := db.batchGetRecipients(ctx, msgIDs, "to")
		for i := range items {
			items[i].email.Labels = labelsMap[items[i].msgID]
			items[i].email.To = toMap[items[i].msgID]
		}
	}

	emails := make([]models.Email, len(items))
	for i, r := range items {
		emails[i] = r.email
	}
	return emails, nil
}

type MessageFetchInfo struct {
	AccountID      string
	FolderRemoteID string
	RemoteUID      uint32
}

func (db *DB) GetMessageFetchInfo(ctx context.Context, messageID int64) (*MessageFetchInfo, error) {
	var info MessageFetchInfo
	var remoteUID sql.NullInt64

	err := db.Read().QueryRowContext(ctx,
		`SELECT m.account_id, f.remote_id, mfs.remote_uid
		 FROM messages m
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 JOIN folders f ON mfs.folder_id = f.id
		 WHERE m.id = ?
		 LIMIT 1`, messageID,
	).Scan(&info.AccountID, &info.FolderRemoteID, &remoteUID)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query fetch info: %w", err)
	}

	if remoteUID.Valid {
		info.RemoteUID = uint32(remoteUID.Int64)
	}
	return &info, nil
}

func (db *DB) IsBodyFetched(ctx context.Context, messageID int64) bool {
	var textPath, htmlPath *string
	err := db.Read().QueryRowContext(ctx,
		`SELECT body_text_path, body_html_path FROM messages WHERE id = ?`, messageID,
	).Scan(&textPath, &htmlPath)
	if err != nil {
		return false
	}
	return (textPath != nil && *textPath != "") || (htmlPath != nil && *htmlPath != "")
}

func (db *DB) GetEmailBody(ctx context.Context, id string) ([]byte, error) {
	msgID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, nil
	}

	var bodyTextPath, bodyHTMLPath sql.NullString
	err = db.Read().QueryRowContext(ctx,
		`SELECT body_text_path, body_html_path FROM messages WHERE id = ?`, msgID,
	).Scan(&bodyTextPath, &bodyHTMLPath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query body paths: %w", err)
	}

	if bodyHTMLPath.Valid && bodyHTMLPath.String != "" {
		data, err := os.ReadFile(bodyHTMLPath.String)
		if err != nil {
			return nil, fmt.Errorf("read html body: %w", err)
		}
		return data, nil
	}

	if bodyTextPath.Valid && bodyTextPath.String != "" {
		data, err := os.ReadFile(bodyTextPath.String)
		if err != nil {
			return nil, fmt.Errorf("read text body: %w", err)
		}
		wrapped := "<pre style=\"white-space:pre-wrap;word-wrap:break-word;font-family:inherit;margin:0;padding:8px\">" +
			template.HTMLEscapeString(string(data)) + "</pre>"
		return []byte(wrapped), nil
	}

	return nil, nil
}

func (db *DB) UpdateMessageBody(ctx context.Context, messageID int64, textPath, htmlPath, rawPath string, snippet string) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET body_text_path = ?, body_html_path = ?, raw_path = ?, snippet = ?, preview_text = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, textPath, htmlPath, rawPath, snippet, snippet, messageID)
	return err
}

func (db *DB) ClearEmailBody(ctx context.Context, messageID int64) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET body_text_path = NULL, body_html_path = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID)
	return err
}

func (db *DB) ClearEmailData(ctx context.Context, messageID int64) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("delete attachments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_recipients WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("delete recipients: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET body_text_path = NULL, body_html_path = NULL, raw_path = NULL, has_attachments = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID); err != nil {
		return fmt.Errorf("clear body: %w", err)
	}

	return tx.Commit()
}

func (db *DB) UpdateMessageHeaders(ctx context.Context, messageID int64, subject, fromName, fromEmail, snippet string) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET subject = ?, from_name = ?, from_email = ?, snippet = ?, preview_text = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		subject, fromName, fromEmail, snippet, snippet, messageID)
	return err
}

func (db *DB) UpdateMessageThreadHeaders(ctx context.Context, messageID int64, accountID, inReplyTo, refs, subject string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var messageIDRaw string
	var sentAt sqliteNullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT internet_message_id, date_received FROM messages WHERE id = ?`, messageID,
	).Scan(&messageIDRaw, &sentAt); err != nil {
		return err
	}
	messageIDNorm := mailmessage.NormalizeMessageID(messageIDRaw)
	if messageIDNorm == "" {
		messageIDNorm = fmt.Sprintf("local-%d@gofer.local", messageID)
	}
	if ids := mailmessage.ParseMessageIDs(inReplyTo); len(ids) > 0 {
		inReplyTo = ids[0]
	}
	date := time.Now().UTC()
	if sentAt.Valid {
		date = sentAt.Time
	}
	if err := db.reconcileMessageThreadTx(ctx, tx, messageID, accountID, messageIDNorm, inReplyTo, refs, subject, date); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) UpsertRecipients(ctx context.Context, messageID int64, to, cc []Recipient) error {
	stmt, err := db.Write().PrepareContext(ctx,
		`INSERT INTO message_recipients (message_id, kind, name, email) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare recip: %w", err)
	}
	defer stmt.Close()

	for _, r := range to {
		if _, err := stmt.ExecContext(ctx, messageID, "to", r.Name, r.Email); err != nil {
			return err
		}
	}
	for _, r := range cc {
		if _, err := stmt.ExecContext(ctx, messageID, "cc", r.Name, r.Email); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) InsertAttachments(ctx context.Context, messageID int64, atts []AttachmentRow) error {
	if len(atts) == 0 {
		return nil
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO attachments (message_id, filename, content_type, size_bytes, content_id, inline, storage_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, a := range atts {
		var inline int
		if a.Inline {
			inline = 1
		}
		if _, err := stmt.ExecContext(ctx, messageID, a.Filename, a.ContentType, a.SizeBytes, a.ContentID, inline, a.StoragePath); err != nil {
			return fmt.Errorf("insert attachment: %w", err)
		}
	}

	hasAttach := 0
	if len(atts) > 0 {
		hasAttach = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET has_attachments = ? WHERE id = ?`, hasAttach, messageID); err != nil {
		return fmt.Errorf("update has_attachments: %w", err)
	}

	return tx.Commit()
}

type AttachmentRow struct {
	Filename    string
	ContentType string
	SizeBytes   int64
	ContentID   string
	Inline      bool
	StoragePath string
}

func (db *DB) GetAttachments(ctx context.Context, messageID int64) ([]models.Attachment, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, filename, content_type, size_bytes, content_id, inline, storage_path
		 FROM attachments WHERE message_id = ?`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var atts []models.Attachment
	for rows.Next() {
		var a models.Attachment
		var inline int
		if err := rows.Scan(&a.ID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.ContentID, &inline, &a.StoragePath); err != nil {
			return nil, err
		}
		a.Inline = inline == 1
		atts = append(atts, a)
	}
	return atts, nil
}

func (db *DB) GetMessageBodyPaths(ctx context.Context, messageID int64) (textPath, htmlPath sql.NullString, err error) {
	err = db.Read().QueryRowContext(ctx,
		`SELECT body_text_path, body_html_path FROM messages WHERE id = ?`, messageID,
	).Scan(&textPath, &htmlPath)
	return
}

type MessageMutationInfo struct {
	AccountID      string
	FolderID       string
	FolderRemoteID string
	RemoteUID      uint32
	FolderRole     string
}

type ThreadMessageMutationInfo struct {
	MessageID int64
	MessageMutationInfo
	IsRead    bool
	IsStarred bool
}

func (db *DB) GetMessageMutationInfo(ctx context.Context, messageID int64) (*MessageMutationInfo, error) {
	var info MessageMutationInfo
	var remoteUID sql.NullInt64
	var role string

	err := db.Read().QueryRowContext(ctx,
		`SELECT m.account_id, mfs.folder_id, f.remote_id, mfs.remote_uid, f.role
		 FROM messages m
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 JOIN folders f ON mfs.folder_id = f.id
		 WHERE m.id = ?
		 LIMIT 1`, messageID,
	).Scan(&info.AccountID, &info.FolderID, &info.FolderRemoteID, &remoteUID, &role)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query mutation info: %w", err)
	}

	info.FolderRole = role
	if remoteUID.Valid {
		info.RemoteUID = uint32(remoteUID.Int64)
	}
	return &info, nil
}

func (db *DB) GetThreadMutationInfos(ctx context.Context, accountID, threadID string) ([]ThreadMessageMutationInfo, error) {
	return db.getThreadMutationInfos(ctx, accountID, threadID, "")
}

func (db *DB) GetThreadMutationInfosInFolder(ctx context.Context, accountID, threadID, folderID string) ([]ThreadMessageMutationInfo, error) {
	return db.getThreadMutationInfos(ctx, accountID, threadID, folderID)
}

func (db *DB) getThreadMutationInfos(ctx context.Context, accountID, threadID, folderID string) ([]ThreadMessageMutationInfo, error) {
	if threadID == "" {
		return nil, nil
	}

	query := `SELECT m.id, m.account_id, mfs.folder_id, f.remote_id, mfs.remote_uid, f.role, mfs.is_read, mfs.is_starred
		 FROM messages m
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 JOIN folders f ON mfs.folder_id = f.id
		 WHERE m.account_id = ? AND m.thread_id = ? AND mfs.is_deleted = 0`
	args := []any{accountID, threadID}
	if folderID != "" {
		query += ` AND mfs.folder_id = ?`
		args = append(args, folderID)
	}
	query += ` ORDER BY m.date_received ASC, m.id ASC`

	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var infos []ThreadMessageMutationInfo
	for rows.Next() {
		var info ThreadMessageMutationInfo
		var remoteUID sql.NullInt64
		var isRead, isStarred int
		if err := rows.Scan(&info.MessageID, &info.AccountID, &info.FolderID, &info.FolderRemoteID, &remoteUID, &info.FolderRole, &isRead, &isStarred); err != nil {
			return nil, err
		}
		if remoteUID.Valid {
			info.RemoteUID = uint32(remoteUID.Int64)
		}
		info.IsRead = isRead == 1
		info.IsStarred = isStarred == 1
		infos = append(infos, info)
	}
	return infos, rows.Err()
}

func (db *DB) ThreadHasUnread(ctx context.Context, accountID, threadID string) (bool, error) {
	if threadID == "" {
		return false, nil
	}
	var hasUnread int
	err := db.Read().QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM messages m
			JOIN message_folder_state mfs ON m.id = mfs.message_id
			WHERE m.account_id = ? AND m.thread_id = ? AND mfs.is_deleted = 0 AND mfs.is_read = 0
		)`, accountID, threadID,
	).Scan(&hasUnread)
	return hasUnread == 1, err
}

func (db *DB) SetMessageRead(ctx context.Context, messageID int64, isRead bool) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE message_folder_state SET is_read = ? WHERE message_id = ?`,
		isRead, messageID)
	if err != nil {
		return err
	}

	var folderID string
	db.Read().QueryRowContext(ctx,
		`SELECT folder_id FROM message_folder_state WHERE message_id = ? LIMIT 1`, messageID,
	).Scan(&folderID)
	if folderID != "" {
		db.RefreshFolderUnreadCount(ctx, folderID)
	}
	return nil
}

func (db *DB) SetThreadRead(ctx context.Context, accountID, threadID string, isRead bool) error {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT DISTINCT mfs.folder_id
		 FROM messages m
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 WHERE m.account_id = ? AND m.thread_id = ? AND mfs.is_deleted = 0`, accountID, threadID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var folderIDs []string
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			return err
		}
		folderIDs = append(folderIDs, folderID)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Write().ExecContext(ctx,
		`UPDATE message_folder_state
		 SET is_read = ?
		 WHERE is_deleted = 0 AND message_id IN (SELECT id FROM messages WHERE account_id = ? AND thread_id = ?)`,
		isRead, accountID, threadID)
	if err != nil {
		return err
	}
	for _, folderID := range folderIDs {
		db.RefreshFolderUnreadCount(ctx, folderID)
	}
	return nil
}

func (db *DB) SetMessageStarred(ctx context.Context, messageID int64, isStarred bool) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE message_folder_state SET is_starred = ? WHERE message_id = ?`,
		isStarred, messageID)
	return err
}

func (db *DB) MarkMessageDeleted(ctx context.Context, messageID int64) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE message_folder_state SET is_deleted = 1 WHERE message_id = ?`,
		messageID)
	if err != nil {
		return err
	}

	var folderID string
	db.Read().QueryRowContext(ctx,
		`SELECT folder_id FROM message_folder_state WHERE message_id = ? LIMIT 1`, messageID,
	).Scan(&folderID)
	if folderID != "" {
		db.RefreshFolderUnreadCount(ctx, folderID)
	}
	return nil
}

func (db *DB) RemoveMessageFromFolder(ctx context.Context, messageID int64, folderID string) error {
	_, err := db.Write().ExecContext(ctx,
		`DELETE FROM message_folder_state WHERE message_id = ? AND folder_id = ?`,
		messageID, folderID)
	if err != nil {
		return err
	}
	db.RefreshFolderUnreadCount(ctx, folderID)
	return nil
}

func (db *DB) AddMessageToFolder(ctx context.Context, messageID int64, folderID string, remoteUID uint32, isRead, isStarred bool) error {
	_, err := db.Write().ExecContext(ctx,
		`DELETE FROM message_folder_state WHERE folder_id = ? AND remote_uid = ? AND message_id != ?`,
		folderID, remoteUID, messageID)
	if err != nil {
		return err
	}
	_, err = db.Write().ExecContext(ctx,
		`INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, is_starred, is_flagged, is_draft, is_deleted, synced_at)
		 VALUES (?, ?, ?, ?, ?, 0, 0, 0, ?)
		 ON CONFLICT(message_id, folder_id) DO UPDATE SET
			remote_uid = excluded.remote_uid`,
		messageID, folderID, remoteUID, isRead, isStarred, time.Now().UTC())
	if err != nil {
		return err
	}
	db.RefreshFolderUnreadCount(ctx, folderID)
	return nil
}

type FolderSyncInfo struct {
	ID                string
	AccountID         string
	RemoteID          string
	Role              string
	UIDValidity       uint32
	HighestSeenUID    uint32
	LastFullSyncAt    sqliteNullTime
	LastIncrementalAt sqliteNullTime
	TotalCount        int
}

func (db *DB) GetSetting(ctx context.Context, userID string, key string) (string, error) {
	var value string
	err := db.Read().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (db *DB) SetSetting(ctx context.Context, userID string, key, value string) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO app_settings (key, user_id, value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET user_id = excluded.user_id, value = excluded.value, updated_at = CURRENT_TIMESTAMP`, key, userID, value)
	return err
}

func (db *DB) GetSyncInterval(ctx context.Context, userID string) int {
	val, err := db.GetSetting(ctx, userID, "sync_interval_minutes")
	if err != nil || val == "" {
		return 5
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 1 {
		return 5
	}
	return n
}

func (db *DB) GetIdleFoldersForAccount(ctx context.Context, userID string, accountID string) map[string]bool {
	val, err := db.GetSetting(ctx, userID, "idle_folders")
	if err != nil || val == "" {
		return map[string]bool{"inbox": true, "sent": true, "drafts": true}
	}
	if val == "none" {
		return map[string]bool{}
	}

	var perAccount map[string][]string
	if err := json.Unmarshal([]byte(val), &perAccount); err == nil {
		roles := perAccount[accountID]
		if roles == nil {
			return map[string]bool{"inbox": true, "sent": true, "drafts": true}
		}
		if len(roles) == 1 && roles[0] == "none" {
			return map[string]bool{}
		}
		result := make(map[string]bool)
		for _, role := range roles {
			if role != "" {
				result[role] = true
			}
		}
		return result
	}

	result := make(map[string]bool)
	for _, role := range strings.Split(val, ",") {
		role = strings.TrimSpace(role)
		if role != "" {
			result[role] = true
		}
	}
	return result
}

func (db *DB) SetIdleFoldersAll(ctx context.Context, userID string, perAccount map[string][]string) error {
	val, err := json.Marshal(perAccount)
	if err != nil {
		return err
	}
	return db.SetSetting(ctx, userID, "idle_folders", string(val))
}

func (db *DB) GetUISettings(ctx context.Context, userID string) map[string]string {
	val, err := db.GetSetting(ctx, userID, "ui_settings")
	if err != nil || val == "" {
		return defaultUISettings()
	}
	var settings map[string]string
	if err := json.Unmarshal([]byte(val), &settings); err != nil {
		return defaultUISettings()
	}
	for k, v := range defaultUISettings() {
		if settings[k] == "" {
			settings[k] = v
		}
	}
	return settings
}

func (db *DB) SetUISettings(ctx context.Context, userID string, settings map[string]string) error {
	val, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return db.SetSetting(ctx, userID, "ui_settings", string(val))
}

func defaultUISettings() map[string]string {
	return map[string]string{
		"theme":                     "dark",
		"theme_style":               "classic",
		"prefetch_on_hover":         "true",
		"default_compose_view":      "dialog",
		"sender_display":            "name",
		"auto_mark_read_after":      "0",
		"mail_table_columns":        "accountMarker,starred,attachment,thread,from,to,subject,date",
		"mail_table_column_widths":  "0.8,0.8,0.8,1,3,3,5,2",
		"sidebar_account_collapsed": "{}",
	}
}

func (db *DB) GetFoldersForAccount(ctx context.Context, accountID string) ([]FolderSyncInfo, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, account_id, remote_id, role,
		        COALESCE(uid_validity, 0), COALESCE(highest_seen_uid, 0),
		        last_full_sync_at, last_incremental_sync_at,
		        COALESCE(total_count, 0)
		 FROM folders WHERE account_id = ? ORDER BY sort_order`, accountID)
	if err != nil {
		return nil, fmt.Errorf("query folders: %w", err)
	}
	defer rows.Close()

	var folders []FolderSyncInfo
	for rows.Next() {
		var f FolderSyncInfo
		if err := rows.Scan(&f.ID, &f.AccountID, &f.RemoteID, &f.Role,
			&f.UIDValidity, &f.HighestSeenUID,
			&f.LastFullSyncAt, &f.LastIncrementalAt,
			&f.TotalCount); err != nil {
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		folders = append(folders, f)
	}
	return folders, nil
}

func (db *DB) GetStoredUIDValidity(ctx context.Context, folderID string) (uint32, error) {
	var uidValidity sql.NullInt64
	err := db.Read().QueryRowContext(ctx,
		`SELECT uid_validity FROM folders WHERE id = ?`, folderID,
	).Scan(&uidValidity)
	if err != nil {
		return 0, err
	}
	if uidValidity.Valid {
		return uint32(uidValidity.Int64), nil
	}
	return 0, nil
}

func (db *DB) GetLocalUIDs(ctx context.Context, folderID string) (map[uint32]int64, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT mfs.remote_uid, mfs.message_id
		 FROM message_folder_state mfs
		 WHERE mfs.folder_id = ? AND mfs.remote_uid IS NOT NULL`, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[uint32]int64)
	for rows.Next() {
		var uid uint32
		var msgID int64
		if err := rows.Scan(&uid, &msgID); err != nil {
			return nil, err
		}
		result[uid] = msgID
	}
	return result, nil
}

func (db *DB) RemoveExpungedUIDs(ctx context.Context, folderID string, expungedUIDs []uint32) (int, error) {
	if len(expungedUIDs) == 0 {
		return 0, nil
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(expungedUIDs))
	args := make([]any, len(expungedUIDs)+1)
	args[0] = folderID
	for i, uid := range expungedUIDs {
		placeholders[i] = "?"
		args[i+1] = uid
	}

	query := fmt.Sprintf(
		`DELETE FROM message_folder_state WHERE folder_id = ? AND remote_uid IN (%s)`,
		strings.Join(placeholders, ","))

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("delete expunged: %w", err)
	}
	removed, _ := res.RowsAffected()

	_, err = tx.ExecContext(ctx,
		`DELETE FROM messages WHERE id NOT IN (SELECT message_id FROM message_folder_state)`)
	if err != nil {
		return 0, fmt.Errorf("cleanup orphaned: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	return int(removed), nil
}

func (db *DB) ClearFolderMessages(ctx context.Context, folderID string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`DELETE FROM message_folder_state WHERE folder_id = ?`, folderID)
	if err != nil {
		return fmt.Errorf("delete states: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`DELETE FROM messages WHERE id NOT IN (SELECT message_id FROM message_folder_state)`)
	if err != nil {
		return fmt.Errorf("cleanup orphaned: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE folders SET highest_seen_uid = 0, total_count = 0, unread_count = 0,
		 last_full_sync_at = NULL, last_incremental_sync_at = NULL, sync_error = NULL,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`, folderID)
	if err != nil {
		return fmt.Errorf("reset folder state: %w", err)
	}

	return tx.Commit()
}

type FlagUpdate struct {
	UID       uint32
	IsRead    bool
	IsStarred bool
}

func (db *DB) BatchUpdateFlags(ctx context.Context, folderID string, updates []FlagUpdate) (int, error) {
	if len(updates) == 0 {
		return 0, nil
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE message_folder_state SET is_read = ?, is_starred = ?
		 WHERE folder_id = ? AND remote_uid = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	changed := 0
	for _, u := range updates {
		var isRead, isStarred int
		err := tx.QueryRow(
			`SELECT is_read, is_starred FROM message_folder_state WHERE folder_id = ? AND remote_uid = ?`,
			folderID, u.UID).Scan(&isRead, &isStarred)
		if err != nil {
			continue
		}

		newRead := 0
		if u.IsRead {
			newRead = 1
		}
		newStarred := 0
		if u.IsStarred {
			newStarred = 1
		}

		if isRead != newRead || isStarred != newStarred {
			if _, err := stmt.ExecContext(ctx, newRead, newStarred, folderID, u.UID); err != nil {
				continue
			}
			changed++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	return changed, nil
}

func (db *DB) GetMessageAllFolderStates(ctx context.Context, messageID int64) ([]struct {
	FolderID  string
	RemoteUID uint32
	IsRead    bool
	IsStarred bool
}, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT folder_id, remote_uid, is_read, is_starred FROM message_folder_state WHERE message_id = ?`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []struct {
		FolderID  string
		RemoteUID uint32
		IsRead    bool
		IsStarred bool
	}
	for rows.Next() {
		var item struct {
			FolderID  string
			RemoteUID uint32
			IsRead    bool
			IsStarred bool
		}
		var isRead, isStarred int
		var remoteUID sql.NullInt64
		if err := rows.Scan(&item.FolderID, &remoteUID, &isRead, &isStarred); err != nil {
			return nil, err
		}
		if remoteUID.Valid {
			item.RemoteUID = uint32(remoteUID.Int64)
		}
		item.IsRead = isRead == 1
		item.IsStarred = isStarred == 1
		results = append(results, item)
	}
	return results, nil
}

func (db *DB) IsRemoteContentAllowedForSender(ctx context.Context, email string) bool {
	var count int
	db.Read().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM remote_content_senders WHERE sender_email = ?`, strings.ToLower(email),
	).Scan(&count)
	return count > 0
}

func (db *DB) IsRemoteContentAllowedForMessage(ctx context.Context, messageID int64) bool {
	var count int
	db.Read().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM remote_content_messages WHERE message_id = ?`, messageID,
	).Scan(&count)
	return count > 0
}

func (db *DB) AllowRemoteContentForSender(ctx context.Context, email string) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT OR IGNORE INTO remote_content_senders (sender_email) VALUES (?)`, strings.ToLower(email),
	)
	return err
}

func (db *DB) AllowRemoteContentForMessage(ctx context.Context, messageID int64) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT OR IGNORE INTO remote_content_messages (message_id) VALUES (?)`, messageID,
	)
	return err
}

func (db *DB) GetMessageSenderEmail(ctx context.Context, messageID int64) (string, error) {
	var email string
	err := db.Read().QueryRowContext(ctx,
		`SELECT from_email FROM messages WHERE id = ?`, messageID,
	).Scan(&email)
	return email, err
}

func (db *DB) UpdateMessageBodyHTMLPath(ctx context.Context, messageID int64, htmlPath string) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET body_html_path = ? WHERE id = ?`, htmlPath, messageID,
	)
	return err
}
