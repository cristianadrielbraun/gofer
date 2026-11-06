package storage

import (
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Open pages are told when someone's contacts change, so they can update in
// place (the "contacts-changed" event). Contacts change from many places:
// edits, deletes, the Trash, imports, merges, the save queue and every
// provider's sync. Rather than each of those remembering to say so, SQLite
// reports every row its writer changes in these tables, and the owner of each
// row is told once the change is committed.
//
// Observations are left out: every synced message updates them, and the
// counts they feed are read again whenever the list is.
var contactChangeTables = map[string]bool{
	"contact_cards":            true,
	"contact_fields":           true,
	"contact_profiles":         true,
	"contact_identities":       true,
	"contact_sync_memberships": true,
	"contact_sync_operations":  true,
	"contact_remote_deletions": true,
	"contact_conflicts":        true,
}

// contactChangeDelay gathers the changes of a burst, such as a sync writing a
// whole address book, into one notice per owner.
const contactChangeDelay = 500 * time.Millisecond

type contactChangeWatch struct {
	mu sync.Mutex
	// columns holds where each table keeps its owner (user_id), once the
	// schema is known; until then, as while migrating, nothing is reported.
	columns map[string]int
	pending map[string]struct{} // owners changed in the open transaction
	ready   map[string]struct{} // owners changed in committed transactions, not yet told
	timer   *time.Timer
	notify  func(userID string)
}

// contactWatches holds the watch of each open database by path. The driver
// hooks every write connection it opens for a watched path.
var contactWatches sync.Map // string → *contactChangeWatch

func watchContactChanges(path string) *contactChangeWatch {
	watch := &contactChangeWatch{pending: map[string]struct{}{}, ready: map[string]struct{}{}}
	contactWatches.Store(path, watch)
	return watch
}

func unwatchContactChanges(path string, watch *contactChangeWatch) {
	if watch == nil {
		return
	}
	contactWatches.CompareAndDelete(path, watch)
	watch.mu.Lock()
	if watch.timer != nil {
		watch.timer.Stop()
		watch.timer = nil
	}
	watch.notify = nil
	watch.mu.Unlock()
}

// hookContactChanges attaches the watch for the database at the DSN's path,
// if any, to a newly opened connection. Only the writer is hooked; readers
// change nothing.
func hookContactChanges(dsn string, conn any) {
	if !strings.Contains(dsn, writePoolOption) {
		return
	}
	path, _, _ := strings.Cut(dsn, "?")
	value, ok := contactWatches.Load(path)
	if !ok {
		return
	}
	hooks, ok := conn.(sqlite.HookRegisterer)
	if !ok {
		return
	}
	watch := value.(*contactChangeWatch)
	hooks.RegisterPreUpdateHook(watch.changed)
	hooks.RegisterCommitHook(watch.committed)
	hooks.RegisterRollbackHook(watch.rolledBack)
}

// changed runs inside the statement writing the row, so it only notes the
// row's owner.
func (w *contactChangeWatch) changed(data sqlite.SQLitePreUpdateData) {
	if !contactChangeTables[data.TableName] || data.DatabaseName != "main" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	column, ok := w.columns[data.TableName]
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
	if owner, _ := row[column].(string); owner != "" {
		w.pending[owner] = struct{}{}
	}
}

func (w *contactChangeWatch) committed() int32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	for owner := range w.pending {
		w.ready[owner] = struct{}{}
	}
	clear(w.pending)
	if len(w.ready) > 0 && w.timer == nil && w.notify != nil {
		w.timer = time.AfterFunc(contactChangeDelay, w.flush)
	}
	// Zero lets the commit go ahead.
	return 0
}

func (w *contactChangeWatch) rolledBack() {
	w.mu.Lock()
	clear(w.pending)
	w.mu.Unlock()
}

func (w *contactChangeWatch) flush() {
	w.mu.Lock()
	owners := make([]string, 0, len(w.ready))
	for owner := range w.ready {
		owners = append(owners, owner)
	}
	clear(w.ready)
	w.timer = nil
	notify := w.notify
	w.mu.Unlock()
	if notify == nil {
		return
	}
	for _, owner := range owners {
		notify(owner)
	}
}

// watchContactTables starts reporting contact changes, now that the schema is
// current: it finds where each watched table keeps its owner.
func (db *DB) watchContactTables() {
	if db.contactWatch == nil {
		return
	}
	columns := make(map[string]int, len(contactChangeTables))
	for table := range contactChangeTables {
		rows, err := db.read.Query(`SELECT cid, name FROM pragma_table_info(?)`, table)
		if err != nil {
			continue
		}
		for rows.Next() {
			var cid int
			var name string
			if rows.Scan(&cid, &name) == nil && name == "user_id" {
				columns[table] = cid
			}
		}
		rows.Close()
	}
	watch := db.contactWatch
	watch.mu.Lock()
	watch.columns = columns
	watch.notify = db.notifyContactChange
	watch.mu.Unlock()
}

// SetContactChangeHook sets what is told, after a commit, whose contacts
// changed. Owned stores tell the system database's hook.
func (db *DB) SetContactChangeHook(hook func(userID string)) {
	db.contactHookMu.Lock()
	db.contactChangeHook = hook
	db.contactHookMu.Unlock()
}

func (db *DB) notifyContactChange(userID string) {
	db.contactHookMu.RLock()
	hook := db.contactChangeHook
	db.contactHookMu.RUnlock()
	if hook == nil && db.contactHookSource != nil {
		db.contactHookSource.contactHookMu.RLock()
		hook = db.contactHookSource.contactChangeHook
		db.contactHookSource.contactHookMu.RUnlock()
	}
	if hook != nil {
		hook(userID)
	}
}
