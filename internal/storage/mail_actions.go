package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// MailAction is what queued a change to mail: one click in Gofer, or one
// action of a mail rule. Changes queued while a context carries an action
// record it, so the Mail operations list can say where each came from and
// keep the changes of one action together.
type MailAction struct {
	// ID is shared by every change the action queued.
	ID string
	// Kind is what was done, one of the MailAction* kinds.
	Kind string
	// Source is who started it: MailActionSourceYou or MailActionSourceRule.
	Source string
	// RuleID is the mail rule's ID when a rule started it.
	RuleID string
}

// Who started an action.
const (
	MailActionSourceYou  = "you"
	MailActionSourceRule = "rule"
)

// What an action did.
const (
	MailActionMove        = "move"
	MailActionArchive     = "archive"
	MailActionUnarchive   = "unarchive"
	MailActionDelete      = "delete"
	MailActionEmptyFolder = "empty_folder"
	// MailActionMarkFolderRead marks everything in a folder read.
	MailActionMarkFolderRead = "mark_folder_read"
	MailActionMarkRead       = "mark_read"
	MailActionMarkUnread     = "mark_unread"
	MailActionStar           = "star"
	MailActionUnstar         = "unstar"
	MailActionLabelAdd       = "label_add"
	MailActionLabelRemove    = "label_remove"
	MailActionSpam           = "spam"
	MailActionNotSpam        = "not_spam"
)

type mailActionKey struct{}

// WithUserMailAction returns ctx carrying a new action of kind, started by
// the person. When ctx already carries an action it is returned unchanged,
// so a step inside a larger action, such as moving to Trash while emptying
// a folder, stays part of that action.
func WithUserMailAction(ctx context.Context, kind string) context.Context {
	if _, ok := ctx.Value(mailActionKey{}).(MailAction); ok {
		return ctx
	}
	return context.WithValue(ctx, mailActionKey{}, MailAction{ID: uuid.NewString(), Kind: kind, Source: MailActionSourceYou})
}

// withMailAction returns ctx carrying action, replacing any it carried.
func withMailAction(ctx context.Context, action MailAction) context.Context {
	return context.WithValue(ctx, mailActionKey{}, action)
}

// MailActionFrom is the action ctx carries. Without one it is the zero
// MailAction, which leaves queued changes without an origin.
func MailActionFrom(ctx context.Context) MailAction {
	action, _ := ctx.Value(mailActionKey{}).(MailAction)
	return action
}

// mailActionArgs are an action's values for the action_id, action_kind,
// action_source and action_rule_id columns, in that order.
func mailActionArgs(ctx context.Context) []any {
	action := MailActionFrom(ctx)
	return []any{action.ID, action.Kind, action.Source, action.RuleID}
}

// mailActionColumns are the queue columns that record what queued a change.
var mailActionColumns = []string{"action_id", "action_kind", "action_source", "action_rule_id"}

// migrateV150ToV151 lets queued message and label changes record the action
// that queued them. Adding columns doesn't rewrite rows, so it is quick on
// any database.
func migrateV150ToV151(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 151 {
		return nil
	}
	for _, table := range []struct{ name, index string }{
		{"message_mutations", "idx_message_mutations_action"},
		{"label_mutation_queue", "idx_label_mutation_queue_action"},
	} {
		exists, err := tableExistsTx(tx, table.name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		for _, column := range mailActionColumns {
			present, err := columnExistsTx(tx, table.name, column)
			if err != nil {
				return err
			}
			if !present {
				if _, err := tx.Exec(`ALTER TABLE ` + table.name + ` ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("add %s.%s: %w", table.name, column, err)
				}
			}
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS ` + table.index + ` ON ` + table.name + `(action_id) WHERE action_id != ''`); err != nil {
			return fmt.Errorf("index %s actions: %w", table.name, err)
		}
	}
	if err := markSchemaVersion(tx, 151); err != nil {
		return err
	}
	return tx.Commit()
}
