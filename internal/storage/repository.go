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
	"unicode"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
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
			if preview := mailmessage.PreviewFromText(string(data)); preview != "" {
				return preview
			}
		}
	}
	if htmlPath != "" {
		if data, err := os.ReadFile(htmlPath); err == nil && len(data) > 0 {
			if preview := mailmessage.PreviewFromHTML(data); preview != "" {
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
	return models.Contact{Name: display, Email: email, Initials: initials(display), AvatarHash: avatarresolver.GravatarHash(email)}
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

func formatRelativeDate(t, now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	t = t.In(loc)
	now = now.In(loc)
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

func formatFullDateTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("Mon, Jan 2, 2006 at 3:04 PM")
}

func isStarredFolder(folderID string) bool {
	return folderID == "starred" || strings.HasPrefix(folderID, "starred-")
}

func isUnifiedFolderID(folderID string) bool {
	switch folderID {
	case "inbox", "starred", "sent", "drafts", "scheduled", "archive", "spam", "trash":
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
	ID         string
	AccountID  string
	ParentID   string
	RemoteID   string
	Name       string
	Icon       string
	Role       string
	Selectable bool
	SortOrder  int
}

func (db *DB) UpsertFolders(ctx context.Context, folders []UpsertFolderInput) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO folders (id, account_id, parent_id, remote_id, name, icon, role, selectable, sort_order)
		VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_id = NULL,
			remote_id = excluded.remote_id,
			name = excluded.name,
			icon = excluded.icon,
			role = excluded.role,
			selectable = excluded.selectable,
			sort_order = excluded.sort_order,
			updated_at = CURRENT_TIMESTAMP`)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	for _, f := range folders {
		if _, err := stmt.ExecContext(ctx, f.ID, f.AccountID, f.RemoteID, f.Name, f.Icon, f.Role, boolInt(f.Selectable), f.SortOrder); err != nil {
			return fmt.Errorf("upsert folder %s: %w", f.ID, err)
		}
	}

	parentStmt, err := tx.PrepareContext(ctx, `UPDATE folders SET parent_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare folder parent update: %w", err)
	}
	defer parentStmt.Close()

	for _, f := range folders {
		if f.ParentID == "" {
			continue
		}
		if _, err := parentStmt.ExecContext(ctx, f.ParentID, f.ID); err != nil {
			return fmt.Errorf("update folder parent %s: %w", f.ID, err)
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
	const batchSize = 500

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

	rows, err := db.Read().QueryContext(ctx,
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	log.Printf("storage: EnsureThreading loaded %d message(s)", len(messages))
	db.SetThreadingState(ThreadingState{InProgress: true, Processed: 0, Total: len(messages)})

	for start := 0; start < len(messages); start += batchSize {
		end := start + batchSize
		if end > len(messages) {
			end = len(messages)
		}

		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		for i := start; i < end; i++ {
			var m row
			err := tx.QueryRowContext(ctx,
				`SELECT id, account_id, internet_message_id, in_reply_to, "references", subject, date_received
				 FROM messages WHERE id = ?`, messages[i].id,
			).Scan(&m.id, &m.accountID, &m.msgID, &m.inReplyTo, &m.refs, &m.subject, &m.sentAt)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				tx.Rollback()
				return err
			}
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
				tx.Rollback()
				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return err
		}

		db.SetThreadingState(ThreadingState{InProgress: true, Processed: end, Total: len(messages)})
		log.Printf("storage: EnsureThreading processed %d/%d", end, len(messages))
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

func formatDBTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999999")
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

type DraftMessageInput struct {
	AccountID         string
	FolderID          string
	InternetMessageID string
	InReplyTo         string
	References        string
	Subject           string
	FromName          string
	FromEmail         string
	Snippet           string
	ToRecipients      []Recipient
	CCRecipients      []Recipient
	BCCRecipients     []Recipient
	Date              time.Time
}

func (db *DB) ReindexMessageSearch(ctx context.Context, messageID int64) error {
	type searchDoc struct {
		accountID       string
		threadKey       string
		subject         string
		sender          string
		recipients      string
		snippet         string
		body            string
		attachmentNames string
		bodyTextPath    sql.NullString
	}

	var doc searchDoc
	err := db.Write().QueryRowContext(ctx, `
		SELECT m.account_id,
		       COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)),
		       COALESCE(m.subject, ''),
		       trim(COALESCE(m.from_name, '') || ' ' || COALESCE(m.from_email, '')),
		       COALESCE((SELECT group_concat(trim(COALESCE(mr.name, '') || ' ' || COALESCE(mr.email, '')), ' ') FROM message_recipients mr WHERE mr.message_id = m.id), ''),
		       COALESCE(m.snippet, ''),
		       COALESCE(m.preview_text, m.snippet, ''),
		       COALESCE((SELECT group_concat(att.filename, ' ') FROM attachments att WHERE att.message_id = m.id), ''),
		       m.body_text_path
		FROM messages m
		WHERE m.id = ?`, messageID).Scan(
		&doc.accountID, &doc.threadKey, &doc.subject, &doc.sender, &doc.recipients,
		&doc.snippet, &doc.body, &doc.attachmentNames, &doc.bodyTextPath,
	)
	if err == sql.ErrNoRows {
		_, deleteErr := db.Write().ExecContext(ctx, `DELETE FROM message_search WHERE rowid = ?`, messageID)
		return deleteErr
	}
	if err != nil {
		return fmt.Errorf("load message search doc: %w", err)
	}
	if doc.bodyTextPath.Valid && doc.bodyTextPath.String != "" {
		if body, readErr := os.ReadFile(doc.bodyTextPath.String); readErr == nil {
			doc.body = string(body)
		}
	}

	if _, err := db.Write().ExecContext(ctx, `DELETE FROM message_search WHERE rowid = ?`, messageID); err != nil {
		return fmt.Errorf("delete message search doc: %w", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO message_search(rowid, account_id, thread_key, subject, sender, recipients, snippet, body, attachment_names)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, doc.accountID, doc.threadKey, doc.subject, doc.sender, doc.recipients, doc.snippet, doc.body, doc.attachmentNames,
	); err != nil {
		return fmt.Errorf("insert message search doc: %w", err)
	}
	return nil
}

func (db *DB) deleteMessageSearch(ctx context.Context, messageID int64) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM message_search WHERE rowid = ?`, messageID)
	return err
}

func (db *DB) SaveDraftMessage(ctx context.Context, draft DraftMessageInput) (int64, error) {
	if draft.AccountID == "" || draft.FolderID == "" || draft.InternetMessageID == "" {
		return 0, fmt.Errorf("missing draft identity")
	}
	if draft.Date.IsZero() {
		draft.Date = time.Now().UTC()
	}
	draft.Date = draft.Date.UTC()
	draftDBDate := formatDBTime(draft.Date)

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	messageIDNorm := mailmessage.NormalizeMessageID(draft.InternetMessageID)
	if messageIDNorm == "" {
		return 0, fmt.Errorf("invalid draft message id")
	}
	inReplyTo := ""
	if ids := mailmessage.ParseMessageIDs(draft.InReplyTo); len(ids) > 0 {
		inReplyTo = ids[0]
	}
	normalizedSubject := normalizeSubject(draft.Subject)

	if _, err := tx.ExecContext(ctx, `
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
			date_received = excluded.date_received,
			in_reply_to = excluded.in_reply_to,
			"references" = excluded."references",
			snippet = excluded.snippet,
			preview_text = excluded.preview_text,
			updated_at = CURRENT_TIMESTAMP`, draft.AccountID, draft.InternetMessageID, messageIDNorm, inReplyTo, draft.References, normalizedSubject, draft.Subject,
		draft.FromName, draft.FromEmail, draftDBDate, draftDBDate, draft.Snippet, draft.Snippet); err != nil {
		return 0, fmt.Errorf("upsert draft message: %w", err)
	}

	var msgID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = ? AND internet_message_id = ?`, draft.AccountID, draft.InternetMessageID).Scan(&msgID); err != nil {
		return 0, fmt.Errorf("query draft message: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, is_starred, is_flagged, is_draft, is_deleted, synced_at)
		VALUES (?, ?, NULL, 1, 0, 0, 1, 0, ?)
		ON CONFLICT(message_id, folder_id) DO UPDATE SET
			is_read = 1,
			is_draft = 1,
			is_deleted = 0,
			synced_at = excluded.synced_at`, msgID, draft.FolderID, time.Now().UTC()); err != nil {
		return 0, fmt.Errorf("upsert draft state: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM message_recipients WHERE message_id = ?`, msgID); err != nil {
		return 0, fmt.Errorf("delete draft recipients: %w", err)
	}
	insertRecipient := func(kind string, r Recipient) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO message_recipients (message_id, kind, name, email) VALUES (?, ?, ?, ?)`, msgID, kind, r.Name, r.Email)
		return err
	}
	for _, r := range draft.ToRecipients {
		if err := insertRecipient("to", r); err != nil {
			return 0, fmt.Errorf("insert draft to: %w", err)
		}
	}
	for _, r := range draft.CCRecipients {
		if err := insertRecipient("cc", r); err != nil {
			return 0, fmt.Errorf("insert draft cc: %w", err)
		}
	}
	for _, r := range draft.BCCRecipients {
		if err := insertRecipient("bcc", r); err != nil {
			return 0, fmt.Errorf("insert draft bcc: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := db.ReindexMessageSearch(ctx, msgID); err != nil {
		return 0, err
	}
	db.UpsertObservedContactsForMessage(ctx, draft.AccountID, draft.FromName, draft.FromEmail, draft.ToRecipients, draft.CCRecipients, draft.BCCRecipients, draft.Date)
	db.RefreshFolderUnreadCount(ctx, draft.FolderID)
	return msgID, nil
}

func (db *DB) DeleteDraftMessage(ctx context.Context, accountID, internetMessageID string) (string, error) {
	if accountID == "" || internetMessageID == "" {
		return "", nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var msgID int64
	var folderID string
	err = tx.QueryRowContext(ctx, `
		SELECT m.id, mfs.folder_id
		FROM messages m
		JOIN message_folder_state mfs ON m.id = mfs.message_id
		WHERE m.account_id = ? AND m.internet_message_id = ? AND mfs.is_draft = 1
		LIMIT 1`, accountID, internetMessageID).Scan(&msgID, &folderID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM message_folder_state WHERE message_id = ?`, msgID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_recipients WHERE message_id = ?`, msgID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_references WHERE message_id = ?`, msgID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM unresolved_references WHERE child_message_id = ?`, msgID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, msgID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if err := db.deleteMessageSearch(ctx, msgID); err != nil {
		return "", err
	}
	db.RefreshFolderUnreadCount(ctx, folderID)
	return folderID, nil
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
			date_received = excluded.date_received,
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

	msgIDs := make([]int64, 0, len(msgs))
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
		m.DateSent = m.DateSent.UTC()
		messageDBDate := formatDBTime(m.DateSent)

		var msgID int64
		if _, err := msgStmt.ExecContext(ctx, m.AccountID, m.MessageID, messageIDNorm, inReplyTo, m.References, normalizedSubject, m.Subject,
			m.FromName, m.FromEmail, messageDBDate, messageDBDate, m.Snippet, m.Snippet); err != nil {
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
		msgIDs = append(msgIDs, msgID)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	for _, m := range msgs {
		db.UpsertObservedContactsForMessage(ctx, m.AccountID, m.FromName, m.FromEmail, m.ToRecipients, m.CCRecipients, nil, m.DateSent)
	}
	for _, msgID := range msgIDs {
		if err := db.ReindexMessageSearch(ctx, msgID); err != nil {
			return err
		}
	}
	return nil
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

func (db *DB) GetEmailSyncAccountIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT id FROM accounts WHERE user_id = ? AND COALESCE(is_deleting, 0) = 0 AND COALESCE(email_sync_enabled, 1) = 1 ORDER BY id`, userID)
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
	return ids, rows.Err()
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

func (db *DB) GetAllEmailSyncAccountIDs(ctx context.Context) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT id FROM accounts WHERE COALESCE(is_deleting, 0) = 0 AND COALESCE(email_sync_enabled, 1) = 1 ORDER BY id`)
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
	return ids, rows.Err()
}

func (db *DB) IsEmailSyncEnabled(ctx context.Context, accountID string) bool {
	var enabled int
	err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(email_sync_enabled, 1) FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 0`, accountID).Scan(&enabled)
	return err == nil && enabled == 1
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
	if err != nil {
		return 0, err
	}
	if err := db.RefreshFolderThreadState(ctx, folderID); err != nil {
		return count, err
	}
	return count, err
}

func (db *DB) RefreshFolderThreadState(ctx context.Context, folderID string) error {
	if strings.TrimSpace(folderID) == "" {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.refreshFolderThreadStateTx(ctx, tx, folderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) refreshFolderThreadStateTx(ctx context.Context, tx *sql.Tx, folderID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_thread_state WHERE folder_id = ?`, folderID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `WITH base AS (
			SELECT m.id, m.account_id, m.date_received, m.has_attachments,
			       mfs.is_read, mfs.is_starred,
			       COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) AS thread_key,
			       COALESCE(m.date_received, '') || ':' || printf('%020d', m.id) AS row_key
			FROM message_folder_state mfs
			JOIN messages m ON mfs.message_id = m.id
			WHERE mfs.folder_id = ? AND mfs.is_deleted = 0
		), grouped AS (
			SELECT thread_key, MAX(row_key) AS row_key, COUNT(*) AS thread_count,
			       MIN(is_read) AS thread_is_read, MAX(is_starred) AS thread_is_starred,
			       MAX(has_attachments) AS thread_has_attachments
			FROM base
			GROUP BY thread_key
		)
		INSERT OR REPLACE INTO folder_thread_state (
			folder_id, thread_key, head_message_id, account_id, last_message_at,
			thread_count, thread_is_read, thread_is_starred, thread_has_attachments, updated_at
		)
		SELECT ?, b.thread_key, b.id, b.account_id, b.date_received,
		       g.thread_count, g.thread_is_read, g.thread_is_starred, g.thread_has_attachments, CURRENT_TIMESTAMP
		FROM grouped g
		JOIN base b ON b.thread_key = g.thread_key AND b.row_key = g.row_key`, folderID, folderID)
	return err
}

func (db *DB) ensureFolderThreadState(ctx context.Context, folderID string) error {
	if strings.TrimSpace(folderID) == "" {
		return nil
	}
	var existing int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = ?`, folderID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	var hasVisible int
	if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message_folder_state WHERE folder_id = ? AND is_deleted = 0)`, folderID).Scan(&hasVisible); err != nil {
		return err
	}
	if hasVisible == 0 {
		return nil
	}
	return db.RefreshFolderThreadState(ctx, folderID)
}

func (db *DB) ensureFolderThreadStateForUserRole(ctx context.Context, userID, role string) error {
	rows, err := db.Read().QueryContext(ctx, `SELECT f.id
		FROM folders f
		JOIN accounts a ON f.account_id = a.id
		WHERE a.user_id = ? AND f.role = ?`, userID, role)
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
	for _, folderID := range folderIDs {
		if err := db.ensureFolderThreadState(ctx, folderID); err != nil {
			return err
		}
	}
	return nil
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

	var starredUnread int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT m.id)
		 FROM message_folder_state mfs
		 JOIN messages m ON mfs.message_id = m.id
		 JOIN accounts a ON m.account_id = a.id
		 WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_read = 0 AND mfs.is_deleted = 0`, userID).Scan(&starredUnread); err != nil {
		return nil, err
	}
	result["starred"] = starredUnread

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
		`SELECT a.id, a.provider, a.email_address, a.display_name, a.color, a.initials, COALESCE(a.is_deleting, 0), COALESCE(a.email_sync_enabled, 1),
		        CASE WHEN a.provider = 'gmail' THEN COALESCE(acc.enabled, 1) ELSE COALESCE(acc.enabled, 0) END AS contact_sync_enabled,
		        CASE WHEN a.provider = 'gmail' THEN 'gmail' ELSE COALESCE(acc.provider, '') END AS contact_sync_provider
		 FROM accounts a
		 LEFT JOIN account_contact_sync_configs acc ON acc.account_id = a.id AND acc.user_id = a.user_id
		 WHERE a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		 ORDER BY a.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []models.Account
	for rows.Next() {
		var a models.Account
		var isDeleting, emailSyncEnabled, contactSyncEnabled int
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.Name, &a.Color, &a.Initials, &isDeleting, &emailSyncEnabled, &contactSyncEnabled, &a.ContactSyncProvider); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		a.IsDeleting = isDeleting == 1
		a.EmailSyncEnabled = emailSyncEnabled == 1
		a.ContactSyncEnabled = contactSyncEnabled == 1
		accounts = append(accounts, a)
	}

	for i := range accounts {
		folders, err := db.getFolders(ctx, accounts[i].ID)
		if err != nil {
			return nil, fmt.Errorf("get folders for %s: %w", accounts[i].ID, err)
		}
		accounts[i].Folders = folders
	}
	db.attachContactAddressBooks(ctx, userID, accounts)

	return accounts, nil
}

func (db *DB) GetAccountsIncludingDeleting(ctx context.Context, userID string) ([]models.Account, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT a.id, a.provider, a.email_address, a.display_name, a.color, a.initials, COALESCE(a.is_deleting, 0), COALESCE(a.email_sync_enabled, 1),
		        CASE WHEN a.provider = 'gmail' THEN COALESCE(acc.enabled, 1) ELSE COALESCE(acc.enabled, 0) END AS contact_sync_enabled,
		        CASE WHEN a.provider = 'gmail' THEN 'gmail' ELSE COALESCE(acc.provider, '') END AS contact_sync_provider
		 FROM accounts a
		 LEFT JOIN account_contact_sync_configs acc ON acc.account_id = a.id AND acc.user_id = a.user_id
		 WHERE a.user_id = ?
		 ORDER BY a.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []models.Account
	for rows.Next() {
		var a models.Account
		var isDeleting, emailSyncEnabled, contactSyncEnabled int
		if err := rows.Scan(&a.ID, &a.Provider, &a.Email, &a.Name, &a.Color, &a.Initials, &isDeleting, &emailSyncEnabled, &contactSyncEnabled, &a.ContactSyncProvider); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		a.IsDeleting = isDeleting == 1
		a.EmailSyncEnabled = emailSyncEnabled == 1
		a.ContactSyncEnabled = contactSyncEnabled == 1
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
	db.attachContactAddressBooks(ctx, userID, accounts)

	return accounts, nil
}

func (db *DB) attachContactAddressBooks(ctx context.Context, userID string, accounts []models.Account) {
	books, err := db.ListContactAddressBooks(ctx, userID)
	if err != nil || len(books) == 0 {
		return
	}
	byAccount := make(map[string][]models.ContactAddressBook)
	for _, book := range books {
		byAccount[book.AccountID] = append(byAccount[book.AccountID], book)
	}
	for i := range accounts {
		accounts[i].ContactAddressBooks = byAccount[accounts[i].ID]
	}
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
	} else if folderID == "scheduled" {
		where = `JOIN scheduled_sends ss ON ss.message_id = m.id
			 JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND ss.status = ? AND mfs.is_deleted = 0`
		args = []any{userID, ScheduledSendPending}
	} else {
		where = `JOIN folders f ON mfs.folder_id = f.id
			 JOIN accounts a ON f.account_id = a.id
			 WHERE a.user_id = ? AND f.role = ? AND mfs.is_deleted = 0`
		args = []any{userID, folderID}
	}
	args = append(append([]any{}, filterSQL.withArgs...), args...)
	args = append(args, filterSQL.args...)

	var count int
	err := db.Read().QueryRowContext(ctx,
		`WITH `+filterSQL.withClause+`visible AS (
		 SELECT ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
		        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
		        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
		        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments,
		        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count
		 FROM message_folder_state mfs
		 JOIN messages m ON mfs.message_id = m.id
		 `+filterSQL.joinClause+`
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
			`WITH `+filterSQL.withClause+`visible AS (
			 SELECT ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
			        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
			        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
			        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments,
			        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count
			 FROM messages m
			 JOIN message_folder_state mfs ON m.id = mfs.message_id
			 JOIN folders f ON mfs.folder_id = f.id
			 `+filterSQL.joinClause+`
			 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
			 AND mfs.is_starred = 1 AND mfs.is_deleted = 0`+filterSQL.cteClause+`
			)
			SELECT COUNT(*) FROM visible WHERE rn = 1`+filterSQL.outerClause, append(append(append([]any{}, filterSQL.withArgs...), folderID), filterSQL.args...)...).Scan(&count)
		return count, err
	}

	var count int
	err := db.Read().QueryRowContext(ctx,
		`WITH `+filterSQL.withClause+`visible AS (
		 SELECT ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) ORDER BY m.date_received DESC, m.id DESC) AS rn,
		        MIN(mfs.is_read) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_read,
		        MAX(mfs.is_starred) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_is_starred,
		        MAX(m.has_attachments) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_has_attachments,
		        COUNT(*) OVER (PARTITION BY COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))) AS thread_count
		 FROM message_folder_state mfs JOIN messages m ON mfs.message_id = m.id
		 `+filterSQL.joinClause+`
		 WHERE mfs.folder_id = ? AND mfs.is_deleted = 0`+filterSQL.cteClause+`
		)
		SELECT COUNT(*) FROM visible WHERE rn = 1`+filterSQL.outerClause, append(append(append([]any{}, filterSQL.withArgs...), folderID), filterSQL.args...)...).Scan(&count)
	return count, err
}

func (db *DB) GetFolderEmailCountUnfilteredForUser(ctx context.Context, userID, folderID string) (int, error) {
	if folderID != "starred" && folderID != "scheduled" {
		if err := db.ensureFolderThreadStateForUserRole(ctx, userID, folderID); err != nil {
			return 0, err
		}
		var count int
		err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*)
			FROM folder_thread_state fts
			JOIN folders f ON fts.folder_id = f.id
			JOIN accounts a ON f.account_id = a.id
			WHERE a.user_id = ? AND f.role = ?`, userID, folderID).Scan(&count)
		return count, err
	}
	fromWhere, args := unifiedMailListFromWhere(userID, folderID)
	return db.countVisibleThreads(ctx, fromWhere, args)
}

func (db *DB) GetFolderEmailCountUnfiltered(ctx context.Context, folderID string) (int, error) {
	if !isStarredFolder(folderID) {
		if err := db.ensureFolderThreadState(ctx, folderID); err != nil {
			return 0, err
		}
		var count int
		err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = ?`, folderID).Scan(&count)
		return count, err
	}
	fromWhere, args := accountMailListFromWhere(folderID)
	return db.countVisibleThreads(ctx, fromWhere, args)
}

func (db *DB) countVisibleThreads(ctx context.Context, fromWhere string, args []any) (int, error) {
	query := `SELECT COUNT(*) FROM (
		SELECT COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) AS thread_key
		` + fromWhere + `
		GROUP BY thread_key
	)`
	var count int
	err := db.Read().QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (db *DB) GetEmailsRange(ctx context.Context, folderID string, start, limit int) (*models.EmailPage, error) {
	return db.GetEmailsRangeFiltered(ctx, folderID, start, limit, models.EmailFilters{})
}

func (db *DB) GetEmailsRangeForUser(ctx context.Context, userID, folderID string, start, limit int) (*models.EmailPage, error) {
	return db.GetEmailsRangeFilteredForUser(ctx, userID, folderID, start, limit, models.EmailFilters{})
}

func (db *DB) GetEmailsRangeFilteredForUser(ctx context.Context, userID, folderID string, start, limit int, filters models.EmailFilters) (*models.EmailPage, error) {
	return db.GetEmailsRangeFilteredForUserWithTotal(ctx, userID, folderID, start, limit, filters, -1)
}

func (db *DB) GetEmailsRangeFilteredForUserWithTotal(ctx context.Context, userID, folderID string, start, limit int, filters models.EmailFilters, knownTotal int) (*models.EmailPage, error) {
	if !isUnifiedFolderID(folderID) {
		return db.GetEmailsRangeFilteredWithTotal(ctx, folderID, start, limit, filters, knownTotal)
	}

	totalCount := knownTotal
	if totalCount < 0 {
		var err error
		if emailFiltersEmpty(filters) {
			totalCount, err = db.GetFolderEmailCountUnfilteredForUser(ctx, userID, folderID)
		} else {
			totalCount, err = db.GetFolderEmailCountFilteredForUser(ctx, userID, folderID, filters)
		}
		if err != nil {
			return nil, err
		}
	}
	if start >= totalCount {
		return &models.EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start}, nil
	}

	var emails []models.Email
	var err error
	if emailFiltersEmpty(filters) {
		emails, err = db.listEmailsUnfilteredForUser(ctx, userID, folderID, start, limit)
	} else {
		emails, err = db.listEmailsFilteredForUser(ctx, userID, folderID, start, limit, filters)
	}
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
	return db.GetEmailsRangeFilteredWithTotal(ctx, folderID, start, limit, filters, -1)
}

func (db *DB) GetEmailsRangeFilteredWithTotal(ctx context.Context, folderID string, start, limit int, filters models.EmailFilters, knownTotal int) (*models.EmailPage, error) {
	totalCount := knownTotal
	if totalCount < 0 {
		var err error
		if emailFiltersEmpty(filters) {
			totalCount, err = db.GetFolderEmailCountUnfiltered(ctx, folderID)
		} else {
			totalCount, err = db.GetFolderEmailCountFiltered(ctx, folderID, filters)
		}
		if err != nil {
			return nil, err
		}
	}

	if start >= totalCount {
		return &models.EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start}, nil
	}

	var emails []models.Email
	var err error
	if emailFiltersEmpty(filters) {
		emails, err = db.listEmailsUnfiltered(ctx, folderID, start, limit)
	} else {
		emails, err = db.listEmailsFiltered(ctx, folderID, start, limit, filters)
	}
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
	withClause  string
	joinClause  string
	cteClause   string
	outerClause string
	withArgs    []any
	args        []any
}

func emailFiltersEmpty(filters models.EmailFilters) bool {
	return !filters.Unread && !filters.Starred && !filters.Attachments && !filters.Read && !filters.NoAttach && !filters.HasLabels && !filters.ThreadsOnly && filters.From == "" && filters.To == "" && filters.Subject == "" && filters.Body == "" && filters.FromDomain == "" && filters.Attachment == "" && filters.Label == "" && filters.AccountID == "" && filters.Query == "" && filters.After == "" && filters.Before == ""
}

func ftsQuery(input string) string {
	fields := strings.FieldsFunc(strings.ToLower(input), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if field == "" {
			continue
		}
		terms = append(terms, field+"*")
	}
	return strings.Join(terms, " ")
}

func emailFilterSQL(filters models.EmailFilters) emailFilterParts {
	var cteParts []string
	var outerParts []string
	var matchParts []string
	var withArgs []any
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
		if query := ftsQuery(filters.Query); query != "" {
			matchParts = append(matchParts, query)
		}
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
		if query := ftsQuery(filters.Body); query != "" {
			matchParts = append(matchParts, "body:("+query+")")
		}
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

	parts := emailFilterParts{withArgs: withArgs, args: args}
	if len(matchParts) > 0 {
		parts.withClause = `matched_threads AS (
			SELECT account_id, thread_key
			FROM message_search
			WHERE message_search MATCH ?
			GROUP BY account_id, thread_key
		), `
		parts.joinClause = ` JOIN matched_threads mt ON mt.account_id = m.account_id AND mt.thread_key = COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) `
		parts.withArgs = append(parts.withArgs, strings.Join(matchParts, " "))
	}
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
	loc := timezoneLocationFromContext(ctx)
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
		item.Preview = mailmessage.PreviewFromText(item.Preview)
		item.From = contactFromSender(fromName, fromEmail)
		db.hydrateContactAvatar(ctx, &item.From)
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
			item.Date = formatRelativeDate(dateReceived.Time, now, loc)
			item.DateFull = formatFullDateTime(dateReceived.Time, loc)
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
		email                models.Email
		dateReceived         sqliteNullTime
		fromName             string
		fromEmail            string
		subject              string
		snippet              string
		accountID            string
		accountColor         string
		hasAttach            int
		bodyTextPath         sql.NullString
		bodyHTMLPath         sql.NullString
		bodyHTMLOriginalPath sql.NullString
		internetMessageID    sql.NullString
		threadID             sql.NullString
		inReplyTo            string
		references           string
	)

	err = db.Read().QueryRowContext(ctx,
		`SELECT m.id, m.account_id, a.color, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments,
		        m.body_text_path, m.body_html_path, m.body_html_original_path, m.internet_message_id, m.thread_id, m.in_reply_to, m."references"
		 FROM messages m
		 JOIN accounts a ON m.account_id = a.id
		 WHERE m.id = ?`, msgID,
	).Scan(&msgID, &accountID, &accountColor, &subject, &fromName, &fromEmail, &dateReceived, &snippet, &hasAttach, &bodyTextPath, &bodyHTMLPath, &bodyHTMLOriginalPath, &internetMessageID, &threadID, &inReplyTo, &references)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query message: %w", err)
	}

	now := time.Now()
	loc := timezoneLocationFromContext(ctx)
	email.ID = strconv.FormatInt(msgID, 10)
	email.AccountID = accountID
	email.AccountColor = accountColor
	email.Subject = subject
	email.From = contactFromSender(fromName, fromEmail)
	db.hydrateContactAvatar(ctx, &email.From)
	email.Preview = mailmessage.PreviewFromText(snippet)
	email.HasAttachment = hasAttach == 1

	if internetMessageID.Valid {
		email.InternetMessageID = internetMessageID.String
	}
	email.InReplyTo = inReplyTo
	email.References = references

	if bodyHTMLPath.Valid && bodyHTMLPath.String != "" {
		if data, err := os.ReadFile(bodyHTMLPath.String); err == nil {
			email.HTMLBody = string(data)
		}
	}
	if bodyHTMLOriginalPath.Valid && bodyHTMLOriginalPath.String != "" {
		if data, err := os.ReadFile(bodyHTMLOriginalPath.String); err == nil {
			email.OriginalHTMLBody = string(data)
		}
	}

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
		email.Date = formatRelativeDate(dateReceived.Time, now, loc)
		email.DateFull = formatFullDateTime(dateReceived.Time, loc)
	}

	if threadID.Valid {
		email.ThreadID = threadID.String
	}

	var folderID string
	var isRead, isStarred, isDraft int
	err = db.Read().QueryRowContext(ctx,
		`SELECT folder_id, is_read, is_starred, is_draft FROM message_folder_state WHERE message_id = ? LIMIT 1`, msgID,
	).Scan(&folderID, &isRead, &isStarred, &isDraft)
	if err == nil {
		email.FolderID = folderID
		email.IsRead = isRead == 1
		email.IsStarred = isStarred == 1
		email.IsDraft = isDraft == 1
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
	email.BCC, _ = db.getRecipients(ctx, msgID, "bcc")
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

func unifiedMailListFromWhere(userID, folderID string) (string, []any) {
	if folderID == "starred" {
		return `FROM messages m
			JOIN message_folder_state mfs ON m.id = mfs.message_id
			JOIN accounts a ON m.account_id = a.id
			WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_deleted = 0`, []any{userID}
	}
	if folderID == "scheduled" {
		return `FROM messages m
			JOIN message_folder_state mfs ON m.id = mfs.message_id
			JOIN scheduled_sends ss ON ss.message_id = m.id
			JOIN accounts a ON m.account_id = a.id
			WHERE a.user_id = ? AND ss.status = ? AND mfs.is_deleted = 0`, []any{userID, ScheduledSendPending}
	}
	return `FROM messages m
		JOIN message_folder_state mfs ON m.id = mfs.message_id
		JOIN folders f ON mfs.folder_id = f.id
		JOIN accounts a ON f.account_id = a.id
		WHERE a.user_id = ? AND f.role = ? AND mfs.is_deleted = 0`, []any{userID, folderID}
}

func accountMailListFromWhere(folderID string) (string, []any) {
	if isStarredFolder(folderID) {
		return `FROM messages m
			JOIN message_folder_state mfs ON m.id = mfs.message_id
			JOIN folders f ON mfs.folder_id = f.id
			JOIN accounts a ON m.account_id = a.id
			WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
			AND mfs.is_starred = 1 AND mfs.is_deleted = 0`, []any{folderID}
	}
	return `FROM messages m
		JOIN message_folder_state mfs ON m.id = mfs.message_id
		JOIN accounts a ON m.account_id = a.id
		WHERE mfs.folder_id = ? AND mfs.is_deleted = 0`, []any{folderID}
}

func (db *DB) listEmailsUnfilteredForUser(ctx context.Context, userID, folderID string, offset, limit int) ([]models.Email, error) {
	if folderID != "starred" && folderID != "scheduled" {
		if err := db.ensureFolderThreadStateForUserRole(ctx, userID, folderID); err != nil {
			return nil, err
		}
		return db.listEmailsFromFolderThreadState(ctx, `JOIN folders f ON fts.folder_id = f.id
			JOIN accounts owner ON f.account_id = owner.id
			WHERE owner.user_id = ? AND f.role = ?`, []any{userID, folderID}, offset, limit)
	}
	fromWhere, args := unifiedMailListFromWhere(userID, folderID)
	return db.listEmailsUnfilteredFrom(ctx, fromWhere, args, offset, limit)
}

func (db *DB) listEmailsUnfiltered(ctx context.Context, folderID string, offset, limit int) ([]models.Email, error) {
	if !isStarredFolder(folderID) {
		if err := db.ensureFolderThreadState(ctx, folderID); err != nil {
			return nil, err
		}
		return db.listEmailsFromFolderThreadState(ctx, `WHERE fts.folder_id = ?`, []any{folderID}, offset, limit)
	}
	fromWhere, args := accountMailListFromWhere(folderID)
	return db.listEmailsUnfilteredFrom(ctx, fromWhere, args, offset, limit)
}

func (db *DB) listEmailsFromFolderThreadState(ctx context.Context, where string, args []any, offset, limit int) ([]models.Email, error) {
	query := `SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
		       m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
		       fts.thread_has_attachments, fts.folder_id, fts.thread_is_read, fts.thread_is_starred,
		       m.thread_id, fts.thread_count
		FROM folder_thread_state fts
		JOIN messages m ON fts.head_message_id = m.id
		JOIN accounts a ON m.account_id = a.id
		` + where + `
		ORDER BY fts.last_message_at DESC, fts.head_message_id DESC
		LIMIT ? OFFSET ?`
	args = append(append([]any{}, args...), limit, offset)
	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list emails: %w", err)
	}
	defer rows.Close()
	return db.scanEmailRows(ctx, rows)
}

func (db *DB) listEmailsUnfilteredFrom(ctx context.Context, fromWhere string, args []any, offset, limit int) ([]models.Email, error) {
	query := `WITH base AS (
			SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
			       m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
			       mfs.folder_id, mfs.is_read, mfs.is_starred, m.thread_id,
			       COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)) AS thread_key,
			       COALESCE(m.date_received, '') || ':' || printf('%020d', m.id) || ':' || mfs.folder_id AS row_key
			` + fromWhere + `
		), grouped AS (
			SELECT thread_key, MAX(row_key) AS row_key, COUNT(*) AS thread_count,
			       MIN(is_read) AS thread_is_read, MAX(is_starred) AS thread_is_starred,
			       MAX(has_attachments) AS thread_has_attachments
			FROM base
			GROUP BY thread_key
		)
		SELECT b.id, b.account_id, b.account_color, b.subject, b.from_name, b.from_email,
		       b.date_received, b.snippet, b.has_attachments, b.body_text_path, b.body_html_path,
		       g.thread_has_attachments, b.folder_id, g.thread_is_read, g.thread_is_starred,
		       b.thread_id, g.thread_count
		FROM grouped g
		JOIN base b ON b.thread_key = g.thread_key AND b.row_key = g.row_key
		ORDER BY b.date_received DESC, b.id DESC
		LIMIT ? OFFSET ?`
	args = append(append([]any{}, args...), limit, offset)
	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list emails: %w", err)
	}
	defer rows.Close()
	return db.scanEmailRows(ctx, rows)
}

func (db *DB) listEmailsFilteredForUser(ctx context.Context, userID, folderID string, offset, limit int, filters models.EmailFilters) ([]models.Email, error) {
	filterSQL := emailFilterSQL(filters)
	var where string
	var args []any
	if folderID == "starred" {
		where = `JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND mfs.is_starred = 1 AND mfs.is_deleted = 0`
		args = []any{userID}
	} else if folderID == "scheduled" {
		where = `JOIN scheduled_sends ss ON ss.message_id = m.id
			 JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND ss.status = ? AND mfs.is_deleted = 0`
		args = []any{userID, ScheduledSendPending}
	} else {
		where = `JOIN folders f ON mfs.folder_id = f.id
			 JOIN accounts a ON f.account_id = a.id
			 WHERE a.user_id = ? AND f.role = ? AND mfs.is_deleted = 0`
		args = []any{userID, folderID}
	}

	query := `WITH ` + filterSQL.withClause + `visible AS (
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
			  ` + filterSQL.joinClause + `
			  ` + where + filterSQL.cteClause + `
			)
			SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
			       thread_has_attachments, folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
			FROM visible WHERE rn = 1` + filterSQL.outerClause + `
			ORDER BY date_received DESC, id DESC
			LIMIT ? OFFSET ?`
	args = append(append([]any{}, filterSQL.withArgs...), args...)
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
	query := `WITH ` + filterSQL.withClause + `visible AS (
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
			  ` + filterSQL.joinClause + `
			  WHERE mfs.folder_id = ? AND mfs.is_deleted = 0` + filterSQL.cteClause + `
			)
			SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
			       thread_has_attachments, folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
			FROM visible WHERE rn = 1` + filterSQL.outerClause + `
			ORDER BY date_received DESC, id DESC
			LIMIT ? OFFSET ?`

	var args []any
	if isStarredFolder(folderID) {
		query = `WITH ` + filterSQL.withClause + `visible AS (
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
			 ` + filterSQL.joinClause + `
					 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
					 AND mfs.is_starred = 1 AND mfs.is_deleted = 0` + filterSQL.cteClause + `
			)
			SELECT id, account_id, account_color, subject, from_name, from_email, date_received, snippet, has_attachments, body_text_path, body_html_path,
			       thread_has_attachments, folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
			FROM visible WHERE rn = 1` + filterSQL.outerClause + `
			ORDER BY date_received DESC, id DESC
			LIMIT ? OFFSET ?`
	}
	args = append(append(append([]any{}, filterSQL.withArgs...), folderID), filterSQL.args...)
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
	loc := timezoneLocationFromContext(ctx)

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
		db.hydrateContactAvatar(ctx, &r.email.From)
		r.email.Preview = mailmessage.PreviewFromText(snippet)
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
			r.email.Date = formatRelativeDate(dateReceived.Time, now, loc)
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
	} else if folderID == "scheduled" {
		where = `JOIN scheduled_sends ss ON ss.message_id = m.id
			 JOIN accounts a ON m.account_id = a.id
			 WHERE a.user_id = ? AND ss.status = ? AND mfs.is_deleted = 0`
		args = []any{msgID, userID, ScheduledSendPending}
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
		c.AvatarHash = avatarresolver.GravatarHash(c.Email)
		db.hydrateContactAvatar(ctx, &c)
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
		c.AvatarHash = avatarresolver.GravatarHash(c.Email)
		db.hydrateContactAvatar(ctx, &c)
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
	query = ftsQuery(query)
	if query == "" {
		return nil, nil
	}

	rows, err := db.Read().QueryContext(ctx,
		`SELECT DISTINCT m.id, m.account_id, a.color, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
		        mfs.folder_id, mfs.is_read, mfs.is_starred
		 FROM message_search
		 JOIN messages m ON message_search.rowid = m.id
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 JOIN accounts a ON m.account_id = a.id
		 WHERE a.user_id = ? AND mfs.is_deleted = 0 AND message_search MATCH ?
		 ORDER BY bm25(message_search), m.date_received DESC, m.id DESC
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
	loc := timezoneLocationFromContext(ctx)

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
		db.hydrateContactAvatar(ctx, &r.email.From)
		r.email.Preview = mailmessage.PreviewFromText(snippet)
		if r.email.Preview == "" || r.email.Preview == subject {
			if preview := previewFromBodyPaths(nullStringValue(textPath), nullStringValue(htmlPath)); preview != "" {
				r.email.Preview = preview
			}
		}
		r.email.IsRead = isRead == 1
		r.email.IsStarred = isStarred == 1
		r.email.HasAttachment = hasAttach == 1
		if dateReceived.Valid {
			r.email.Date = formatRelativeDate(dateReceived.Time, now, loc)
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

func (db *DB) GetEmailOriginalHTMLBody(ctx context.Context, id string) ([]byte, error) {
	msgID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, nil
	}

	var bodyHTMLOriginalPath sql.NullString
	err = db.Read().QueryRowContext(ctx,
		`SELECT body_html_original_path FROM messages WHERE id = ?`, msgID,
	).Scan(&bodyHTMLOriginalPath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query original body path: %w", err)
	}
	if !bodyHTMLOriginalPath.Valid || bodyHTMLOriginalPath.String == "" {
		return nil, nil
	}
	data, err := os.ReadFile(bodyHTMLOriginalPath.String)
	if err != nil {
		return nil, fmt.Errorf("read original html body: %w", err)
	}
	return data, nil
}

func (db *DB) UpdateMessageBody(ctx context.Context, messageID int64, textPath, htmlPath, rawPath string, snippet string) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET body_text_path = ?, body_html_path = ?, raw_path = ?, snippet = ?, preview_text = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, textPath, htmlPath, rawPath, snippet, snippet, messageID)
	if err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
}

func (db *DB) UpdateMessageOriginalHTMLPath(ctx context.Context, messageID int64, htmlPath string) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET body_html_original_path = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, htmlPath, messageID)
	return err
}

func (db *DB) ClearEmailBody(ctx context.Context, messageID int64) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET body_text_path = NULL, body_html_path = NULL, body_html_original_path = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID)
	if err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
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
		`UPDATE messages SET body_text_path = NULL, body_html_path = NULL, body_html_original_path = NULL, raw_path = NULL, has_attachments = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, messageID); err != nil {
		return fmt.Errorf("clear body: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
}

func (db *DB) UpdateMessageHeaders(ctx context.Context, messageID int64, subject, fromName, fromEmail, snippet string) error {
	_, err := db.Write().ExecContext(ctx,
		`UPDATE messages SET subject = ?, from_name = ?, from_email = ?, snippet = ?, preview_text = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		subject, fromName, fromEmail, snippet, snippet, messageID)
	if err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
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
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
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
	return db.ReindexMessageSearch(ctx, messageID)
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

	if err := tx.Commit(); err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
}

func (db *DB) ReplaceAttachments(ctx context.Context, messageID int64, atts []AttachmentRow) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("delete attachments: %w", err)
	}
	if len(atts) > 0 {
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
	}
	hasAttach := 0
	if len(atts) > 0 {
		hasAttach = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET has_attachments = ? WHERE id = ?`, hasAttach, messageID); err != nil {
		return fmt.Errorf("update has_attachments: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.ReindexMessageSearch(ctx, messageID)
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
	folderIDs, _ := db.messageFolderIDs(ctx, messageID)
	_, err := db.Write().ExecContext(ctx,
		`UPDATE message_folder_state SET is_read = ? WHERE message_id = ?`,
		isRead, messageID)
	if err != nil {
		return err
	}

	for _, folderID := range folderIDs {
		db.RefreshFolderUnreadCount(ctx, folderID)
	}
	return nil
}

func (db *DB) messageFolderIDs(ctx context.Context, messageID int64) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT DISTINCT folder_id FROM message_folder_state WHERE message_id = ?`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folderIDs []string
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			return nil, err
		}
		folderIDs = append(folderIDs, folderID)
	}
	return folderIDs, rows.Err()
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
	folderIDs, _ := db.messageFolderIDs(ctx, messageID)
	_, err := db.Write().ExecContext(ctx,
		`UPDATE message_folder_state SET is_starred = ? WHERE message_id = ?`,
		isStarred, messageID)
	if err != nil {
		return err
	}
	for _, folderID := range folderIDs {
		if err := db.RefreshFolderThreadState(ctx, folderID); err != nil {
			return err
		}
	}
	return err
}

func (db *DB) MarkMessageDeleted(ctx context.Context, messageID int64) error {
	folderIDs, _ := db.messageFolderIDs(ctx, messageID)
	_, err := db.Write().ExecContext(ctx,
		`UPDATE message_folder_state SET is_deleted = 1 WHERE message_id = ?`,
		messageID)
	if err != nil {
		return err
	}

	for _, folderID := range folderIDs {
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
		`INSERT OR REPLACE INTO app_settings (key, user_id, value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, key, userID, value)
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
	if settings["default_compose_view"] != "" {
		if settings["default_new_compose_view"] == "" {
			settings["default_new_compose_view"] = settings["default_compose_view"]
		}
		if settings["default_reply_compose_view"] == "" {
			settings["default_reply_compose_view"] = settings["default_compose_view"]
		}
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

func (db *DB) ListSignatures(ctx context.Context, userID string) ([]models.Signature, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, name, html_body, text_body, created_at, updated_at
		 FROM signatures WHERE user_id = ? ORDER BY name COLLATE NOCASE, created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query signatures: %w", err)
	}
	defer rows.Close()

	var signatures []models.Signature
	for rows.Next() {
		var sig models.Signature
		if err := rows.Scan(&sig.ID, &sig.Name, &sig.HTMLBody, &sig.TextBody, &sig.CreatedAt, &sig.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan signature: %w", err)
		}
		signatures = append(signatures, sig)
	}
	return signatures, rows.Err()
}

func (db *DB) SaveSignature(ctx context.Context, userID string, sig models.Signature) (models.Signature, error) {
	sig.Name = strings.TrimSpace(sig.Name)
	if sig.Name == "" {
		return models.Signature{}, fmt.Errorf("signature name is required")
	}
	if strings.TrimSpace(sig.HTMLBody) == "" && strings.TrimSpace(sig.TextBody) == "" {
		return models.Signature{}, fmt.Errorf("signature body is required")
	}
	if sig.ID == "" {
		sig.ID = "sig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}

	res, err := db.Write().ExecContext(ctx,
		`INSERT INTO signatures (id, user_id, name, html_body, text_body)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			html_body = excluded.html_body,
			text_body = excluded.text_body,
			updated_at = CURRENT_TIMESTAMP
		 WHERE user_id = excluded.user_id`, sig.ID, userID, sig.Name, sig.HTMLBody, sig.TextBody)
	if err != nil {
		return models.Signature{}, fmt.Errorf("save signature: %w", err)
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return models.Signature{}, sql.ErrNoRows
	}

	return db.GetSignature(ctx, userID, sig.ID)
}

func (db *DB) GetSignature(ctx context.Context, userID, signatureID string) (models.Signature, error) {
	var sig models.Signature
	err := db.Read().QueryRowContext(ctx,
		`SELECT id, name, html_body, text_body, created_at, updated_at
		 FROM signatures WHERE user_id = ? AND id = ?`, userID, signatureID).
		Scan(&sig.ID, &sig.Name, &sig.HTMLBody, &sig.TextBody, &sig.CreatedAt, &sig.UpdatedAt)
	return sig, err
}

func (db *DB) DeleteSignature(ctx context.Context, userID, signatureID string) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM signatures WHERE user_id = ? AND id = ?`, userID, signatureID)
	return err
}

func (db *DB) GetAccountSignatureSettings(ctx context.Context, userID, accountID string) (models.AccountSignatureSettings, error) {
	settings := models.AccountSignatureSettings{AccountID: accountID, ReplyPlacement: "before", ForwardPlacement: "before"}
	var newID, replyID, forwardID sql.NullString
	var replyPlacement, forwardPlacement string
	var newEnabled, replyEnabled, forwardEnabled int
	err := db.Read().QueryRowContext(ctx, `
		SELECT COALESCE(s.new_enabled, 0), s.new_signature_id,
		       COALESCE(s.reply_enabled, 0), s.reply_signature_id,
		       COALESCE(s.forward_enabled, 0), s.forward_signature_id,
		       COALESCE(s.reply_placement, 'before'), COALESCE(s.forward_placement, 'before')
		FROM accounts a
		LEFT JOIN account_signature_settings s ON s.account_id = a.id
		WHERE a.user_id = ? AND a.id = ?`, userID, accountID).
		Scan(&newEnabled, &newID, &replyEnabled, &replyID, &forwardEnabled, &forwardID, &replyPlacement, &forwardPlacement)
	if err != nil {
		return settings, err
	}
	settings.NewEnabled = newEnabled == 1
	settings.ReplyEnabled = replyEnabled == 1
	settings.ForwardEnabled = forwardEnabled == 1
	settings.NewSignatureID = nullStringValue(newID)
	settings.ReplySignatureID = nullStringValue(replyID)
	settings.ForwardSignatureID = nullStringValue(forwardID)
	settings.ReplyPlacement = normalizeSignaturePlacement(replyPlacement)
	settings.ForwardPlacement = normalizeSignaturePlacement(forwardPlacement)
	return settings, nil
}

func (db *DB) SaveAccountSignatureSettings(ctx context.Context, userID string, settings models.AccountSignatureSettings) error {
	if settings.AccountID == "" {
		return fmt.Errorf("account id is required")
	}
	settings.NewSignatureID = strings.TrimSpace(settings.NewSignatureID)
	settings.ReplySignatureID = strings.TrimSpace(settings.ReplySignatureID)
	settings.ForwardSignatureID = strings.TrimSpace(settings.ForwardSignatureID)
	settings.ReplyPlacement = normalizeSignaturePlacement(settings.ReplyPlacement)
	settings.ForwardPlacement = normalizeSignaturePlacement(settings.ForwardPlacement)
	settings.NewEnabled = settings.NewEnabled && settings.NewSignatureID != ""
	settings.ReplyEnabled = settings.ReplyEnabled && settings.ReplySignatureID != ""
	settings.ForwardEnabled = settings.ForwardEnabled && settings.ForwardSignatureID != ""
	var exists int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE user_id = ? AND id = ?`, userID, settings.AccountID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	for _, signatureID := range []string{settings.NewSignatureID, settings.ReplySignatureID, settings.ForwardSignatureID} {
		signatureID = strings.TrimSpace(signatureID)
		if signatureID == "" {
			continue
		}
		var owned int
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM signatures WHERE user_id = ? AND id = ?`, userID, signatureID).Scan(&owned); err != nil {
			return err
		}
		if owned == 0 {
			return sql.ErrNoRows
		}
	}

	normalizeSignatureID := func(id string, enabled bool) any {
		id = strings.TrimSpace(id)
		if !enabled || id == "" {
			return nil
		}
		return id
	}
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO account_signature_settings (
			account_id, new_signature_id, reply_signature_id, forward_signature_id,
			new_enabled, reply_enabled, forward_enabled, reply_placement, forward_placement, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(account_id) DO UPDATE SET
			new_signature_id = excluded.new_signature_id,
			reply_signature_id = excluded.reply_signature_id,
			forward_signature_id = excluded.forward_signature_id,
			new_enabled = excluded.new_enabled,
			reply_enabled = excluded.reply_enabled,
			forward_enabled = excluded.forward_enabled,
			reply_placement = excluded.reply_placement,
			forward_placement = excluded.forward_placement,
			updated_at = CURRENT_TIMESTAMP`,
		settings.AccountID,
		normalizeSignatureID(settings.NewSignatureID, settings.NewEnabled),
		normalizeSignatureID(settings.ReplySignatureID, settings.ReplyEnabled),
		normalizeSignatureID(settings.ForwardSignatureID, settings.ForwardEnabled),
		boolInt(settings.NewEnabled), boolInt(settings.ReplyEnabled), boolInt(settings.ForwardEnabled),
		settings.ReplyPlacement, settings.ForwardPlacement)
	return err
}

func normalizeSignaturePlacement(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "after") {
		return "after"
	}
	return "before"
}

func (db *DB) GetComposeSignature(ctx context.Context, userID, accountID, mode string) (*models.Signature, bool, error) {
	settings, err := db.GetAccountSignatureSettings(ctx, userID, accountID)
	if err != nil {
		return nil, false, err
	}
	signatureID := ""
	enabled := false
	switch mode {
	case "reply", "reply-all":
		signatureID = settings.ReplySignatureID
		enabled = settings.ReplyEnabled
	case "forward":
		signatureID = settings.ForwardSignatureID
		enabled = settings.ForwardEnabled
	default:
		signatureID = settings.NewSignatureID
		enabled = settings.NewEnabled
	}
	if !enabled || signatureID == "" {
		return nil, false, nil
	}
	sig, err := db.GetSignature(ctx, userID, signatureID)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &sig, true, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func defaultUISettings() map[string]string {
	return map[string]string{
		"theme":                             "dark",
		"theme_style":                       "classic",
		"prefetch_on_hover":                 "true",
		"default_compose_view":              "dialog",
		"default_new_compose_view":          "dialog",
		"default_reply_compose_view":        "dialog",
		"compose_autosave_enabled":          "true",
		"compose_autosave_conditions":       "chars,attachment",
		"compose_autosave_min_chars":        "30",
		"compose_autosave_debounce":         "5",
		"sender_display":                    "name",
		"mail_pane_layout":                  "side",
		"mail_list_height":                  "360",
		"mail_list_navigation":              "infinite",
		"auto_mark_read_after":              "0",
		"desktop_notifications":             "false",
		"notification_mode":                 "auto",
		"mail_table_columns":                "accountMarker,starred,attachment,thread,from,to,subject,date",
		"mail_table_column_widths":          "0.8,0.8,0.8,1,3,3,5,2",
		"sidebar_account_collapsed":         "{}",
		"contacts_auto_create_observed":     "true",
		"contacts_prevent_recreate_deleted": "true",
		"contacts_observed_sources":         "senders,recipients",
		"timezone":                          "local",
	}
}

func (db *DB) GetFoldersForAccount(ctx context.Context, accountID string) ([]FolderSyncInfo, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, account_id, remote_id, role,
		        COALESCE(uid_validity, 0), COALESCE(highest_seen_uid, 0),
		        last_full_sync_at, last_incremental_sync_at,
		        COALESCE(total_count, 0)
		 FROM folders WHERE account_id = ? AND COALESCE(selectable, 1) = 1 ORDER BY sort_order`, accountID)
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
	if removed > 0 {
		db.RefreshFolderUnreadCount(ctx, folderID)
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
		`DELETE FROM folder_thread_state WHERE folder_id = ?`, folderID)
	if err != nil {
		return fmt.Errorf("delete thread states: %w", err)
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
	if changed > 0 {
		db.RefreshFolderUnreadCount(ctx, folderID)
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
