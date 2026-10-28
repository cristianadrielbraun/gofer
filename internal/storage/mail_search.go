package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrMailSearchScope = errors.New("invalid or unavailable mail search scope")

// MaxMailSearchFolders bounds the number of explicit folder selections, not
// the number of descendants or the depth of a folder tree.
const MaxMailSearchFolders = 200

// mailSearchFoldersSQL validates explicit selections and returns a CTE of
// owned, active folders. Ownership is also checked in the result query so a
// concurrent removal cannot broaden a previously validated scope.
func (db *DB) mailSearchFoldersSQL(ctx context.Context, userID string, scope models.MailSearchScope) (string, []any, error) {
	if strings.TrimSpace(userID) == "" {
		return "", nil, ErrMailSearchScope
	}
	owned := `SELECT f.id, f.account_id FROM folders f JOIN accounts a ON a.id=f.account_id
		WHERE a.user_id=? AND a.is_deleting=0 AND COALESCE(f.discovery_state, 'active')='active'`
	args := []any{userID}
	switch scope.Kind {
	case models.MailSearchAll:
		if scope.AccountID != "" || len(scope.FolderIDs) != 0 || scope.IncludeDescendants {
			return "", nil, ErrMailSearchScope
		}
	case models.MailSearchAccount:
		if strings.TrimSpace(scope.AccountID) == "" || len(scope.FolderIDs) != 0 || scope.IncludeDescendants {
			return "", nil, ErrMailSearchScope
		}
		var exists bool
		if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=? AND user_id=? AND is_deleting=0)`, scope.AccountID, userID).Scan(&exists); err != nil {
			return "", nil, err
		}
		if !exists {
			return "", nil, ErrMailSearchScope
		}
		owned += ` AND a.id=?`
		args = append(args, scope.AccountID)
	case models.MailSearchFolders:
		if scope.AccountID != "" || len(scope.FolderIDs) == 0 || len(scope.FolderIDs) > MaxMailSearchFolders {
			return "", nil, ErrMailSearchScope
		}
		seen := make(map[string]bool)
		var ids []any
		var placeholders []string
		for _, id := range scope.FolderIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				return "", nil, ErrMailSearchScope
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
				placeholders = append(placeholders, "?")
			}
		}
		owned += ` AND f.id IN (` + strings.Join(placeholders, ",") + `)`
		args = append(args, ids...)
		var count int
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+owned+`)`, args...).Scan(&count); err != nil {
			return "", nil, err
		}
		if count != len(ids) {
			return "", nil, ErrMailSearchScope
		}
	default:
		return "", nil, ErrMailSearchScope
	}
	if scope.IncludeDescendants {
		// UNION terminates even with a cycle and deduplicates overlapping roots.
		owned += ` UNION SELECT child.id, child.account_id FROM folders child
			JOIN search_folders parent ON child.parent_id=parent.id AND child.account_id=parent.account_id
			WHERE COALESCE(child.discovery_state, 'active')='active'`
	}
	return `search_folders(id, account_id) AS (` + owned + `), `, args, nil
}

// mailSearchStatesSQL defines the live, deduplicated message memberships used
// by both searches and their actions. It ends with a comma for further CTEs.
func (db *DB) mailSearchStatesSQL(ctx context.Context, userID string, scope models.MailSearchScope) (string, []any, error) {
	folders, args, err := db.mailSearchFoldersSQL(ctx, userID, scope)
	if err != nil {
		return "", nil, err
	}
	excluded := ""
	if scope.ExcludeSpamTrash {
		excluded = ` AND f.role NOT IN ('spam', 'junk', 'trash')`
	}
	query := `WITH RECURSIVE ` + folders + `search_copies AS (
		SELECT s.message_id, s.folder_id,
			MIN(s.is_read) OVER (PARTITION BY s.message_id) AS is_read,
			MAX(s.is_starred) OVER (PARTITION BY s.message_id) AS is_starred,
			ROW_NUMBER() OVER (PARTITION BY s.message_id ORDER BY CASE f.role
				WHEN 'inbox' THEN 0 WHEN 'sent' THEN 1 WHEN 'archive' THEN 2
				WHEN 'custom' THEN 3 WHEN 'drafts' THEN 4 WHEN 'spam' THEN 5
				WHEN 'junk' THEN 5 WHEN 'trash' THEN 6 ELSE 7 END,
				f.sort_order, f.name, f.id) AS copy_rank
		FROM message_folder_state s JOIN search_folders sf ON sf.id=s.folder_id
		JOIN folders f ON f.id=sf.id JOIN accounts owner ON owner.id=f.account_id
		JOIN messages original ON original.id=s.message_id AND original.account_id=f.account_id
		WHERE s.is_deleted=0 AND owner.user_id=? AND owner.is_deleting=0` + excluded + `
	), search_states AS (SELECT * FROM search_copies WHERE copy_rank=1), `
	return query, append(args, userID), nil
}

// mailSearchVisibleSQL is shared by the page and count. Folder copies are
// collapsed before conversation aggregation, so labels cannot multiply counts
// or sizes. The chosen folder is a real, deterministic member of the scope.
func (db *DB) mailSearchVisibleSQL(ctx context.Context, userID string, scope models.MailSearchScope, filters models.EmailFilters) (string, []any, error) {
	states, args, err := db.mailSearchStatesSQL(ctx, userID, scope)
	if err != nil {
		return "", nil, err
	}
	parts := emailFilterSQL(filters)
	if searchMatchQuery(filters) != "" && !filters.ByMessage {
		// A matching reply must itself be in scope before its conversation
		// qualifies; a hit in another folder must not leak into this search.
		parts.withClause = `matched_threads AS (
			SELECT hit.account_id, COALESCE(NULLIF(hit.thread_id, ''), printf('msg:%d', hit.id)) AS thread_key
			FROM message_search CROSS JOIN messages hit ON hit.id=message_search.rowid
			JOIN search_states ss ON ss.message_id=hit.id
			WHERE message_search MATCH ? GROUP BY hit.account_id, thread_key
		), `
	}
	key := `m.account_id, ` + listRowKeySQL(filters)
	query := states + parts.withClause + `visible AS (
		SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
			m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
			mfs.folder_id, m.thread_id,
			ROW_NUMBER() OVER (PARTITION BY ` + key + ` ORDER BY m.date_received DESC, m.id DESC) AS rn,
			COUNT(*) OVER (PARTITION BY ` + key + `) AS thread_count,
			MIN(mfs.is_read) OVER (PARTITION BY ` + key + `) AS thread_is_read,
			MAX(mfs.is_starred) OVER (PARTITION BY ` + key + `) AS thread_is_starred,
			MAX(m.has_attachments) OVER (PARTITION BY ` + key + `) AS thread_has_attachments,
			SUM(COALESCE(m.size_bytes, 0)) OVER (PARTITION BY ` + key + `) AS thread_size_bytes
		FROM search_states mfs JOIN messages m ON m.id=mfs.message_id
		JOIN accounts a ON a.id=m.account_id ` + parts.joinClause + `
		WHERE 1=1` + parts.cteClause + `
	), results AS (SELECT * FROM visible WHERE rn=1` + parts.outerClause + `) `
	args = append(args, parts.withArgs...)
	args = append(args, parts.args...)
	return query, args, nil
}

// GetMailSearchPageForUser searches locally synced messages using the existing
// filters, sort order, and conversation/message display preference.
func (db *DB) GetMailSearchPageForUser(ctx context.Context, userID string, scope models.MailSearchScope, start, limit int, filters models.EmailFilters) (*models.EmailPage, error) {
	if start < 0 || limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("invalid mail search page")
	}
	query, args, err := db.mailSearchVisibleSQL(ctx, userID, scope, filters)
	if err != nil {
		return nil, err
	}
	var total int
	if err := db.Read().QueryRowContext(ctx, query+`SELECT COUNT(*) FROM results`, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count mail search: %w", err)
	}
	page := &models.EmailPage{TotalCount: total, DisplayTotalCount: total, WindowStart: start, WindowEnd: start}
	if start >= total {
		return page, nil
	}
	rows, err := db.Read().QueryContext(ctx, query+`
		SELECT id, account_id, account_color, subject, from_name, from_email, date_received,
			snippet, has_attachments, body_text_path, body_html_path, thread_has_attachments,
			folder_id, thread_is_read, thread_is_starred, thread_id, thread_count
		FROM results ORDER BY `+emailOrderSQL(filters)+` LIMIT ? OFFSET ?`, append(args, limit, start)...)
	if err != nil {
		return nil, fmt.Errorf("list mail search: %w", err)
	}
	defer rows.Close()
	page.Emails, err = db.scanEmailRows(ctx, rows)
	if err != nil {
		return nil, err
	}
	page.WindowEnd = start + len(page.Emails) - 1
	page.HasMore = len(page.Emails) > 0 && page.WindowEnd+1 < total
	if page.HasMore {
		page.NextCursor = page.Emails[len(page.Emails)-1].ID
	}
	return page, nil
}

type MailSearchRowID struct {
	ID       string
	Account  string
	FolderID string
	Thread   bool
}

// GetMailSearchRowIDsForUser selects rows without loading bodies, avatars or
// attachments. Callers may request one extra row to detect a truncated list.
func (db *DB) GetMailSearchRowIDsForUser(ctx context.Context, userID string, scope models.MailSearchScope, filters models.EmailFilters, limit int) ([]MailSearchRowID, int, error) {
	if limit <= 0 || limit > 10001 {
		return nil, 0, errors.New("invalid mail search ID limit")
	}
	query, args, err := db.mailSearchVisibleSQL(ctx, userID, scope, filters)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := db.Read().QueryRowContext(ctx, query+`SELECT COUNT(*) FROM results`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Read().QueryContext(ctx, query+`SELECT id, account_id, folder_id, thread_count>1 FROM results ORDER BY `+emailOrderSQL(filters)+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	ids := make([]MailSearchRowID, 0)
	for rows.Next() {
		var id MailSearchRowID
		if err := rows.Scan(&id.ID, &id.Account, &id.FolderID, &id.Thread); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	return ids, total, rows.Err()
}

type MailSearchMember struct {
	MessageID int64
	FolderID  string
}

// GetMailSearchMembersForUser resolves an action's message or conversation
// against the same scoped memberships as the list. The anchor must be live
// in scope; results never include another account's matching thread key.
func (db *DB) GetMailSearchMembersForUser(ctx context.Context, userID string, scope models.MailSearchScope, messageID int64, thread bool) ([]MailSearchMember, error) {
	query, args, err := db.mailSearchStatesSQL(ctx, userID, scope)
	if err != nil {
		return nil, err
	}
	query += `anchor AS (
		SELECT m.account_id, m.thread_id FROM messages m JOIN search_states s ON s.message_id=m.id WHERE m.id=?
	) SELECT s.message_id, s.folder_id FROM search_states s
	JOIN messages m ON m.id=s.message_id CROSS JOIN anchor
	WHERE m.id=? OR (? AND m.account_id=anchor.account_id AND m.thread_id=anchor.thread_id AND anchor.thread_id!='')
	ORDER BY CASE WHEN m.id=? THEN 0 ELSE 1 END, m.date_received, m.id`
	rows, err := db.Read().QueryContext(ctx, query, append(args, messageID, messageID, thread, messageID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []MailSearchMember
	for rows.Next() {
		var member MailSearchMember
		if err := rows.Scan(&member.MessageID, &member.FolderID); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}
