package storage

import (
	"context"
	"log"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Open pages are told when someone's background mail work changes (the
// "mail-operations-changed" event), so the Mail operations list and the
// attention mark update without polling. Work changes from many places: a
// change being queued, a worker trying, finishing or failing it, a folder
// removed on the server parking it, Retry, Undo and retention cleanup. As
// with contacts (contact_changes.go), SQLite reports every row its writer
// changes in these tables, and the owners of the accounts involved are told
// once the change is committed.
var mailOperationTables = map[string]bool{
	"message_mutations":               true,
	"label_mutation_queue":            true,
	"imap_draft_operations":           true,
	"outgoing_sends":                  true,
	"gofer_provider_draft_operations": true,
}

// operationChanged runs inside the statement writing the row, so it only
// notes the row's account.
func (w *contactChangeWatch) operationChanged(data sqlite.SQLitePreUpdateData) {
	w.mu.Lock()
	defer w.mu.Unlock()
	column, ok := w.operationColumns[data.TableName]
	if !ok {
		return
	}
	row := make([]any, column+1)
	var err error
	if data.Op == sqlite3.SQLITE_DELETE {
		err = data.Old(row...)
	} else {
		err = data.New(row...)
	}
	if err != nil {
		return
	}
	if account, _ := row[column].(string); account != "" {
		w.pendingOperations[account] = struct{}{}
	}
}

// SetMailOperationsChangeHook sets what is told, after a commit, whose
// background mail work changed. Owned stores tell the system database's
// hook.
func (db *DB) SetMailOperationsChangeHook(hook func(userID string)) {
	db.contactHookMu.Lock()
	db.mailOperationsChangeHook = hook
	db.contactHookMu.Unlock()
}

// notifyMailOperationsChange tells the hook about the owners of the
// accounts whose work changed. An account deleted since has no owner left
// to tell.
func (db *DB) notifyMailOperationsChange(accountIDs []string) {
	db.contactHookMu.RLock()
	hook := db.mailOperationsChangeHook
	db.contactHookMu.RUnlock()
	if hook == nil && db.contactHookSource != nil {
		db.contactHookSource.contactHookMu.RLock()
		hook = db.contactHookSource.mailOperationsChangeHook
		db.contactHookSource.contactHookMu.RUnlock()
	}
	if hook == nil || len(accountIDs) == 0 {
		return
	}
	args := stringsToAny(accountIDs)
	rows, err := db.read.QueryContext(context.Background(), `SELECT DISTINCT user_id FROM accounts WHERE id IN (`+sqlPlaceholders(len(accountIDs))+`) ORDER BY user_id`, args...)
	if err != nil {
		log.Printf("storage: find owners of changed mail operations: %v", err)
		return
	}
	var owners []string
	for rows.Next() {
		var owner string
		if rows.Scan(&owner) == nil && strings.TrimSpace(owner) != "" {
			owners = append(owners, owner)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		log.Printf("storage: find owners of changed mail operations: %v", err)
		return
	}
	for _, owner := range owners {
		hook(owner)
	}
}
