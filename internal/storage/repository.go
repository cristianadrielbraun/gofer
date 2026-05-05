package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"strconv"
	"strings"
	"time"

	"gofer.email/internal/models"
)

func initials(name string) string {
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		return strings.ToUpper(string(parts[0][0]) + string(parts[1][0]))
	}
	if len(name) >= 2 {
		return strings.ToUpper(name[:2])
	}
	return strings.ToUpper(name)
}

func formatRelativeDate(t, now time.Time) string {
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
	SortOrder  int
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

type SyncMessage struct {
	AccountID    string
	FolderID     string
	RemoteUID    uint32
	MessageID    string
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
		INSERT INTO messages (account_id, internet_message_id, subject, from_name, from_email,
			date_sent, date_received, snippet, preview_text)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, internet_message_id) DO UPDATE SET
			subject = excluded.subject,
			from_name = excluded.from_name,
			from_email = excluded.from_email,
			date_sent = excluded.date_sent,
			snippet = excluded.snippet,
			preview_text = excluded.preview_text,
			updated_at = CURRENT_TIMESTAMP`)
	if err != nil {
		return fmt.Errorf("prepare msg upsert: %w", err)
	}
	defer msgStmt.Close()

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

	for _, m := range msgs {
		var msgID int64
		err := tx.QueryRow(`SELECT id FROM messages WHERE account_id = ? AND internet_message_id = ?`,
			m.AccountID, m.MessageID).Scan(&msgID)
		if err != nil {
			if err != sql.ErrNoRows {
				return fmt.Errorf("query message: %w", err)
			}
			res, err := msgStmt.ExecContext(ctx, m.AccountID, m.MessageID, m.Subject,
				m.FromName, m.FromEmail, m.DateSent, m.DateSent, m.Snippet, m.Snippet)
			if err != nil {
				return fmt.Errorf("insert message: %w", err)
			}
			msgID, _ = res.LastInsertId()
		}

		if _, err := stateStmt.ExecContext(ctx, msgID, m.FolderID, m.RemoteUID,
			m.IsRead, m.IsStarred, time.Now().UTC()); err != nil {
			return fmt.Errorf("upsert state: %w", err)
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
	}

	return tx.Commit()
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
		`UPDATE folders SET highest_seen_uid = ?, uid_validity = ?, total_count = ?,
		 last_incremental_sync_at = CURRENT_TIMESTAMP, sync_error = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, highestUID, uidValidity, totalCount, folderID)
	return err
}

func (db *DB) GetHighestSeenUID(ctx context.Context, folderID string) (uint32, error) {
	var uid uint32
	err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(highest_seen_uid, 0) FROM folders WHERE id = ?`, folderID,
	).Scan(&uid)
	return uid, err
}

func (db *DB) GetAccountIDs(ctx context.Context) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT id FROM accounts ORDER BY id`)
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

func (db *DB) GetAllFolderUnreadCounts(ctx context.Context) (map[string]int, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, unread_count FROM folders`)
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
	return result, nil
}

func (db *DB) GetFolderHighestUID(ctx context.Context, folderID string) (uint32, error) {
	var uid uint32
	err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(remote_uid), 0) FROM message_folder_state WHERE folder_id = ?`, folderID,
	).Scan(&uid)
	return uid, err
}

func (db *DB) GetAccounts(ctx context.Context) ([]models.Account, error) {
	rows, err := db.Read().QueryContext(ctx,
		`SELECT id, email_address, display_name, color, initials FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []models.Account
	for rows.Next() {
		var a models.Account
		if err := rows.Scan(&a.ID, &a.Email, &a.Name, &a.Color, &a.Initials); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
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
	if isStarredFolder(folderID) {
		var count int
		err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT mfs.message_id) FROM message_folder_state mfs
			 JOIN folders f ON mfs.folder_id = f.id
			 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
			 AND mfs.is_starred = 1 AND mfs.is_deleted = 0`, folderID).Scan(&count)
		return count, err
	}

	var count int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_folder_state WHERE folder_id = ? AND is_deleted = 0`, folderID).Scan(&count)
	return count, err
}

func (db *DB) GetEmailsRange(ctx context.Context, folderID string, start, limit int) (*models.EmailPage, error) {
	totalCount, err := db.GetFolderEmailCount(ctx, folderID)
	if err != nil {
		return nil, err
	}

	if start >= totalCount {
		return &models.EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start}, nil
	}

	emails, err := db.listEmails(ctx, folderID, start, limit)
	if err != nil {
		return nil, err
	}

	end := start + len(emails)
	hasMore := end < totalCount
	nextCursor := ""
	if end > 0 && hasMore {
		nextCursor = emails[end-1].ID
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

func (db *DB) GetEmailByID(ctx context.Context, id string) (*models.Email, error) {
	msgID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, nil
	}

	var (
		email             models.Email
		dateReceived      sql.NullTime
		fromName          string
		fromEmail         string
		subject           string
		snippet           string
		accountID         string
		hasAttach         int
		bodyTextPath      sql.NullString
		bodyHTMLPath      sql.NullString
		internetMessageID sql.NullString
	)

	err = db.Read().QueryRowContext(ctx,
		`SELECT m.id, m.account_id, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments,
		        m.body_text_path, m.body_html_path, m.internet_message_id
		 FROM messages m WHERE m.id = ?`, msgID,
	).Scan(&msgID, &accountID, &subject, &fromName, &fromEmail, &dateReceived, &snippet, &hasAttach, &bodyTextPath, &bodyHTMLPath, &internetMessageID)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query message: %w", err)
	}

	now := time.Now()
	email.ID = strconv.FormatInt(msgID, 10)
	email.AccountID = accountID
	email.Subject = subject
	email.From = models.Contact{Name: fromName, Email: fromEmail, Initials: initials(fromName)}
	email.Preview = snippet
	email.HasAttachment = hasAttach == 1

	if internetMessageID.Valid {
		email.InternetMessageID = internetMessageID.String
	}

	if bodyHTMLPath.Valid && bodyHTMLPath.String != "" {
		data, err := os.ReadFile(bodyHTMLPath.String)
		if err == nil {
			email.Body = template.HTML(data)
		} else {
			email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
		}
	} else if bodyTextPath.Valid && bodyTextPath.String != "" {
		data, err := os.ReadFile(bodyTextPath.String)
		if err == nil {
			email.Body = template.HTML("<pre style=\"white-space:pre-wrap;word-wrap:break-word;font-family:inherit\">" + template.HTML(template.HTMLEscapeString(string(data))) + "</pre>")
		} else {
			email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
		}
	} else {
		email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
	}
	if dateReceived.Valid {
		email.Date = formatRelativeDate(dateReceived.Time, now)
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

func (db *DB) listEmails(ctx context.Context, folderID string, offset, limit int) ([]models.Email, error) {
	query := `SELECT m.id, m.account_id, m.subject, m.from_name, m.from_email,
	          m.date_received, m.snippet, m.has_attachments,
	          mfs.folder_id, mfs.is_read, mfs.is_starred
	          FROM messages m
	          JOIN message_folder_state mfs ON m.id = mfs.message_id
	          WHERE mfs.folder_id = ? AND mfs.is_deleted = 0
	          ORDER BY m.date_received DESC
	          LIMIT ? OFFSET ?`

	var args []any
	if isStarredFolder(folderID) {
		query = `SELECT m.id, m.account_id, m.subject, m.from_name, m.from_email,
		         m.date_received, m.snippet, m.has_attachments,
		         mfs.folder_id, mfs.is_read, mfs.is_starred
		         FROM messages m
		         JOIN message_folder_state mfs ON m.id = mfs.message_id
		         JOIN folders f ON mfs.folder_id = f.id
		         WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
		         AND mfs.is_starred = 1 AND mfs.is_deleted = 0
		         ORDER BY m.date_received DESC
		         LIMIT ? OFFSET ?`
	}
	args = []any{folderID, limit, offset}

	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list emails: %w", err)
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
		var dateReceived sql.NullTime
		var isRead, isStarred, hasAttach int
		var subject, fromName, fromEmail, snippet, accountID string

		if err := rows.Scan(&r.msgID, &accountID, &subject, &fromName, &fromEmail,
			&dateReceived, &snippet, &hasAttach,
			&r.email.FolderID, &isRead, &isStarred); err != nil {
			return nil, fmt.Errorf("scan email: %w", err)
		}

		r.email.ID = strconv.FormatInt(r.msgID, 10)
		r.email.AccountID = accountID
		r.email.Subject = subject
		r.email.From = models.Contact{Name: fromName, Email: fromEmail, Initials: initials(fromName)}
		r.email.Preview = snippet
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
		for i := range items {
			items[i].email.Labels = labelsMap[items[i].msgID]
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
		query = `SELECT COUNT(DISTINCT mfs.message_id) FROM message_folder_state mfs
			 JOIN messages m ON mfs.message_id = m.id
			 JOIN folders f ON mfs.folder_id = f.id
			 WHERE f.account_id = (SELECT account_id FROM folders WHERE id = ?)
			 AND mfs.is_starred = 1 AND mfs.is_deleted = 0
			 AND m.date_received > (SELECT date_received FROM messages WHERE id = ?)`
	} else {
		query = `SELECT COUNT(*) FROM message_folder_state mfs
			 JOIN messages m ON mfs.message_id = m.id
			 WHERE mfs.folder_id = ? AND mfs.is_deleted = 0
			 AND m.date_received > (SELECT date_received FROM messages WHERE id = ?)`
	}
	args = []any{folderID, msgID}

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

func (db *DB) SearchMessages(ctx context.Context, query string, limit int) ([]models.Email, error) {
	if query == "" {
		return nil, nil
	}

	rows, err := db.Read().QueryContext(ctx,
		`SELECT m.id, m.account_id, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments,
		        mfs.folder_id, mfs.is_read, mfs.is_starred
		 FROM message_fts fts
		 JOIN messages m ON fts.rowid = m.id
		 JOIN message_folder_state mfs ON m.id = mfs.message_id
		 WHERE message_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, query, limit)
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
		var dateReceived sql.NullTime
		var isRead, isStarred, hasAttach int
		var subject, fromName, fromEmail, snippet, accountID string

		if err := rows.Scan(&r.msgID, &accountID, &subject, &fromName, &fromEmail,
			&dateReceived, &snippet, &hasAttach,
			&r.email.FolderID, &isRead, &isStarred); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}

		r.email.ID = strconv.FormatInt(r.msgID, 10)
		r.email.AccountID = accountID
		r.email.Subject = subject
		r.email.From = models.Contact{Name: fromName, Email: fromEmail, Initials: initials(fromName)}
		r.email.Preview = snippet
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
		for i := range items {
			items[i].email.Labels = labelsMap[items[i].msgID]
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
	AccountID        string
	FolderID         string
	FolderRemoteID   string
	RemoteUID        uint32
	FolderRole       string
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
	ID                  string
	AccountID           string
	RemoteID            string
	Role                string
	UIDValidity         uint32
	HighestSeenUID      uint32
	LastFullSyncAt      sql.NullTime
	LastIncrementalAt   sql.NullTime
	TotalCount          int
}

func (db *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := db.Read().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.Write().ExecContext(ctx,
		`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, key, value)
	return err
}

func (db *DB) GetSyncInterval(ctx context.Context) int {
	val, err := db.GetSetting(ctx, "sync_interval_minutes")
	if err != nil || val == "" {
		return 5
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 1 {
		return 5
	}
	return n
}

func (db *DB) GetIdleFoldersForAccount(ctx context.Context, accountID string) map[string]bool {
	val, err := db.GetSetting(ctx, "idle_folders")
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

func (db *DB) SetIdleFoldersAll(ctx context.Context, perAccount map[string][]string) error {
	val, err := json.Marshal(perAccount)
	if err != nil {
		return err
	}
	return db.SetSetting(ctx, "idle_folders", string(val))
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
