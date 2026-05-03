package storage

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
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
		email        models.Email
		dateReceived sql.NullTime
		fromName     string
		fromEmail    string
		subject      string
		snippet      string
		accountID    string
		hasAttach    int
	)

	err = db.Read().QueryRowContext(ctx,
		`SELECT m.id, m.account_id, m.subject, m.from_name, m.from_email,
		        m.date_received, m.snippet, m.has_attachments
		 FROM messages m WHERE m.id = ?`, msgID,
	).Scan(&msgID, &accountID, &subject, &fromName, &fromEmail, &dateReceived, &snippet, &hasAttach)

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
	email.Body = template.HTML(fmt.Sprintf("<p>%s</p>", snippet))
	email.HasAttachment = hasAttach == 1
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
