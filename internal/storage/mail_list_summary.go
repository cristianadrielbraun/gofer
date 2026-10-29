package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A mailListSource is a table holding one row per mail list row in each
// folder, with everything the list shows and filters on for that row, kept
// up to date as mail changes. Its rows are read through the alias s.
type mailListSource struct {
	table string
	// live is added to every scope to leave out rows the list never shows.
	live string
	// key names a row within its folder, and keyOf gives the key of the
	// row listing the message bound to its ?.
	key, keyOf string
	// head is the message the row shows, last when the row last changed
	// and count how many messages it stands for.
	head, last, count string
	// read, starred and attachments are the row's flags as listed.
	read, starred, attachments string
	account                    string
	// summarized means the table is summaries that must be brought up to
	// date before reading.
	summarized bool
	// matching keeps the rows with a full-text match for a folder's list,
	// or for a unified list when folderID is a role.
	matching func(match, folderID string) (string, []any)
}

// conversationListSource lists each conversation once, from
// folder_thread_state.
var conversationListSource = mailListSource{
	table:       "folder_thread_state",
	key:         "s.thread_key",
	keyOf:       "(SELECT COALESCE(NULLIF(thread_id, ''), printf('msg:%d', id)) FROM messages WHERE id = ?)",
	head:        "s.head_message_id",
	last:        "s.last_message_at",
	count:       "s.thread_count",
	read:        "s.thread_is_read",
	starred:     "s.thread_is_starred",
	attachments: "s.thread_has_attachments",
	account:     "s.account_id",
	summarized:  true,
	// A search keeps the conversations with a matching message anywhere,
	// as the full list query does. A folder belongs to one account, so its
	// list compares conversation keys alone, which SQLite looks up directly;
	// a unified list compares account and key. The alias is not m, so the
	// page is still chosen from the summary index before joining messages.
	matching: func(match, folderID string) (string, []any) {
		// CROSS JOIN keeps the search first: otherwise SQLite may walk the
		// account's messages and search once per message.
		hits := `FROM message_search CROSS JOIN messages hit ON hit.id = message_search.rowid WHERE message_search MATCH ?`
		key := `COALESCE(NULLIF(hit.thread_id, ''), printf('msg:%d', hit.id))`
		if isUnifiedFolderID(folderID) {
			return ` AND (s.account_id, s.thread_key) IN (SELECT hit.account_id, ` + key + ` ` + hits + `)`, []any{match}
		}
		return ` AND s.thread_key IN (SELECT ` + key + ` ` + hits + `
			AND hit.account_id = (SELECT account_id FROM folders WHERE id = ?))`, []any{match, folderID}
	},
}

// messageListSource lists each message once per folder it is in, from
// message_folder_state. Removed folder entries are kept there until they are
// reconciled, so live leaves them out.
var messageListSource = mailListSource{
	table:       "message_folder_state",
	live:        " AND s.is_deleted = 0",
	key:         "s.message_id",
	keyOf:       "?",
	head:        "s.message_id",
	last:        "s.message_date",
	count:       "1",
	read:        "s.is_read",
	starred:     "s.is_starred",
	attachments: "s.message_has_attachments",
	account:     "(SELECT account_id FROM folders WHERE id = s.folder_id)",
	// A search keeps the messages that match.
	matching: func(match, _ string) (string, []any) {
		return ` AND s.message_id IN (SELECT rowid FROM message_search WHERE message_search MATCH ?)`, []any{match}
	},
}

// mailListSourceFor is the source of a list with the given filters.
func mailListSourceFor(filters models.EmailFilters) *mailListSource {
	if filters.ByMessage {
		return &messageListSource
	}
	return &conversationListSource
}

// listRowKeySQL is the key the general list query groups the messages m by
// into list rows: their conversation, or each message on its own.
func listRowKeySQL(filters models.EmailFilters) string {
	if filters.ByMessage {
		return "m.id"
	}
	return "COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))"
}

// A mailListScope is the rows of one list in its source: the joins and
// WHERE clause after "FROM <source table> s".
type mailListScope struct {
	src   *mailListSource
	where string
	args  []any
}

// summaryListScope answers a mail list from its source table when every
// active filter and the sort order only need what that table already keeps
// per row: read, starred and attachment state, size, account and the head
// message. Such lists then cost the same as the unfiltered list instead of
// grouping every message in the folder. It reports false for anything else
// (search, sender, dates, tags, the Starred and Scheduled views), which keep
// the general query.
func (db *DB) summaryListScope(ctx context.Context, userID, folderID string, filters models.EmailFilters) (mailListScope, bool, error) {
	if !summaryFiltersOnly(filters) || isStarredFolder(folderID) || folderID == "scheduled" {
		return mailListScope{}, false, nil
	}
	src := mailListSourceFor(filters)
	scope := mailListScope{src: src}
	if isUnifiedFolderID(folderID) {
		if userID == "" {
			return mailListScope{}, false, nil
		}
		if src.summarized {
			if err := db.ensureFolderThreadStateForUserRole(ctx, userID, folderID); err != nil {
				return mailListScope{}, false, err
			}
		}
		rolePredicate, roleArgs := unifiedFolderRolePredicate("f", folderID)
		accountFilter, accountArgs, err := db.unifiedFolderAccountFilter(ctx, userID, folderID, "owner")
		if err != nil {
			return mailListScope{}, false, err
		}
		scope.where = `JOIN folders f ON s.folder_id = f.id
			JOIN accounts owner ON f.account_id = owner.id
			WHERE owner.user_id = ? AND ` + rolePredicate + accountFilter
		scope.args = append([]any{userID}, roleArgs...)
		scope.args = append(scope.args, accountArgs...)
	} else {
		if src.summarized {
			if err := db.ensureFolderThreadState(ctx, folderID); err != nil {
				return mailListScope{}, false, err
			}
		}
		scope.where = `WHERE s.folder_id = ?`
		scope.args = []any{folderID}
	}
	scope.where += src.live
	if filters.Unread {
		scope.where += ` AND ` + src.read + ` = 0`
	} else if filters.Read {
		scope.where += ` AND ` + src.read + ` = 1`
	}
	if filters.Starred {
		scope.where += ` AND ` + src.starred + ` = 1`
	}
	if filters.Attachments {
		scope.where += ` AND ` + src.attachments + ` = 1`
	} else if filters.NoAttach {
		scope.where += ` AND ` + src.attachments + ` = 0`
	}
	if filters.ThreadsOnly {
		scope.where += ` AND ` + src.count + ` > 1`
	} else if filters.NoThreads {
		scope.where += ` AND ` + src.count + ` = 1`
	}
	if filters.AccountID != "" {
		scope.where += ` AND ` + src.account + ` = ?`
		scope.args = append(scope.args, filters.AccountID)
	}
	if match := searchMatchQuery(filters); match != "" {
		where, args := src.matching(match, folderID)
		scope.where += where
		scope.args = append(scope.args, args...)
	}
	return scope, true, nil
}

// summaryFiltersOnly reports whether the filters use nothing beyond the
// per-row summary, a full-text search and a supported sort.
func summaryFiltersOnly(filters models.EmailFilters) bool {
	flags := filters
	flags.Unread, flags.Read, flags.Starred = false, false, false
	flags.Attachments, flags.NoAttach = false, false
	flags.ThreadsOnly, flags.NoThreads = false, false
	flags.AccountID = ""
	flags.Query, flags.Body = "", ""
	if !emailFiltersEmpty(flags) {
		return false
	}
	switch filters.SortBy {
	case "", "date", "sender", "subject":
		return true
	}
	return false
}

// order is emailOrderSQL for lists read from the source, where the row's
// head message is m.
func (scope mailListScope) order(filters models.EmailFilters) string {
	direction := "DESC"
	if filters.SortOrder == "asc" {
		direction = "ASC"
	}
	switch filters.SortBy {
	case "sender":
		return "LOWER(COALESCE(NULLIF(m.from_name, ''), m.from_email, '')) " + direction + ", m.date_received DESC, m.id DESC"
	case "subject":
		return "LOWER(COALESCE(m.subject, '')) " + direction + ", m.date_received DESC, m.id DESC"
	default:
		return scope.src.last + " " + direction + ", " + scope.src.head + " " + direction
	}
}

// summaryOrderedPosition is summaryPosition for the other sort orders. It
// numbers the summary's rows, one per list row, in the list's order.
func (db *DB) summaryOrderedPosition(ctx context.Context, scope mailListScope, filters models.EmailFilters, messageID int64) (int, error) {
	src := scope.src
	var position int
	err := db.Read().QueryRowContext(ctx, `SELECT position FROM (
		SELECT `+src.key+` AS row_key, ROW_NUMBER() OVER (ORDER BY `+scope.order(filters)+`) - 1 AS position
		FROM `+src.table+` s
		JOIN messages m ON `+src.head+` = m.id
		JOIN accounts a ON m.account_id = a.id
		`+scope.where+`
	) WHERE row_key = `+src.keyOf+` LIMIT 1`, append(append([]any{}, scope.args...), messageID)...).Scan(&position)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return position, err
}

func (db *DB) countSummaryList(ctx context.Context, scope mailListScope) (int, error) {
	var count int
	err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+scope.src.table+` s `+scope.where, scope.args...).Scan(&count)
	return count, err
}

// summaryEmailPage loads one page of a summary-backed list. A negative
// knownTotal is counted here, and so is an unfiltered list's, which mail
// arriving makes stale and costs a single index count.
func (db *DB) summaryEmailPage(ctx context.Context, scope mailListScope, filters models.EmailFilters, start, limit, knownTotal int) (*models.EmailPage, error) {
	total := knownTotal
	if total < 0 || emailFiltersEmpty(filters) {
		var err error
		if total, err = db.countSummaryList(ctx, scope); err != nil {
			return nil, err
		}
	}
	if start >= total {
		return &models.EmailPage{TotalCount: total, DisplayTotalCount: total, WindowStart: start, WindowEnd: start}, nil
	}
	emails, err := db.listSummaryEmails(ctx, scope, scope.order(filters), start, limit)
	if err != nil {
		return nil, err
	}
	end := start + len(emails)
	page := &models.EmailPage{
		Emails:            emails,
		TotalCount:        total,
		DisplayTotalCount: total,
		WindowStart:       start,
		WindowEnd:         end - 1,
		HasMore:           len(emails) > 0 && end < total,
	}
	if page.HasMore {
		page.NextCursor = emails[len(emails)-1].ID
	}
	return page, nil
}

func (db *DB) listSummaryEmails(ctx context.Context, scope mailListScope, order string, offset, limit int) ([]models.Email, error) {
	src := scope.src
	from := `FROM ` + src.table + ` s
		JOIN messages m ON ` + src.head + ` = m.id
		JOIN accounts a ON m.account_id = a.id
		` + scope.where + `
		ORDER BY ` + order + `
		LIMIT ? OFFSET ?`
	if !referencesMessageOrAccount.MatchString(scope.where + " " + order) {
		// Choose the page from the source's index alone, then join only its
		// rows: OFFSET would otherwise join every skipped row first.
		from = `FROM ` + src.table + ` s
		JOIN messages m ON ` + src.head + ` = m.id
		JOIN accounts a ON m.account_id = a.id
		WHERE s.rowid IN (SELECT s.rowid FROM ` + src.table + ` s ` + scope.where + `
			ORDER BY ` + order + ` LIMIT ? OFFSET ?)
		ORDER BY ` + order
	}
	query := `SELECT m.id, m.account_id, a.color AS account_color, m.subject, m.from_name, m.from_email,
		       m.date_received, m.snippet, m.has_attachments, m.body_text_path, m.body_html_path,
		       ` + src.attachments + `, s.folder_id, ` + src.read + `, ` + src.starred + `,
		       m.thread_id, ` + src.count + `
		` + from
	args := append(append([]any{}, scope.args...), limit, offset)
	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list emails: %w", err)
	}
	defer rows.Close()
	return db.scanEmailRows(ctx, rows)
}

// summaryPosition returns how many rows come before the row of messageID in
// a summary-backed list, by counting index entries rather than grouping the
// folder's messages. A message the list does not show is reported at
// position 0.
func (db *DB) summaryPosition(ctx context.Context, scope mailListScope, filters models.EmailFilters, messageID int64) (int, error) {
	if !emailUsesDefaultSort(filters) {
		return db.summaryOrderedPosition(ctx, scope, filters, messageID)
	}
	src := scope.src
	var last sql.NullString
	var head int64
	err := db.Read().QueryRowContext(ctx, `SELECT CAST(`+src.last+` AS TEXT), `+src.head+`
		FROM `+src.table+` s `+scope.where+`
		  AND `+src.key+` = `+src.keyOf+`
		ORDER BY `+src.last+` DESC, `+src.head+` DESC
		LIMIT 1`, append(append([]any{}, scope.args...), messageID)...).Scan(&last, &head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// Rows before it in "last DESC, head DESC" order, where a missing date
	// sorts last.
	before := mailListScope{src: src, where: scope.where + ` AND (` + src.last + `, ` + src.head + `) > (?, ?)`}
	beforeArgs := []any{last.String, head}
	if !last.Valid {
		before.where = scope.where + ` AND (` + src.last + ` IS NOT NULL OR ` + src.head + ` > ?)`
		beforeArgs = []any{head}
	}
	before.args = append(append([]any{}, scope.args...), beforeArgs...)
	return db.countSummaryList(ctx, before)
}
