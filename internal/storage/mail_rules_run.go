package storage

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A run on mail already there looks at the newest messages of a folder, and
// acts on at most so many of them; the owner can run it again for the rest.
var (
	mailRuleRunScanLimit  = 5000
	mailRuleRunMatchLimit = 1000
	mailRulePreviewSample = 20
)

// ErrMailRuleRunFolder is returned when no account the rule applies to has
// the folder a run names.
var ErrMailRuleRunFolder = errors.New("no account the rule applies to has that folder")

// MailRuleRunScope is the mail a run of a rule looks at: a folder of one
// account, or the folder with a role in every account the rule applies to.
type MailRuleRunScope struct {
	// AccountID limits the run to one account; empty means all of them.
	AccountID string
	Folder    models.MailRuleFolderTarget
}

// MailRulePreview is what running a rule on mail already there would do.
type MailRulePreview struct {
	// Matches are the messages the rule would change, newest first.
	Matches []int64
	// Sample shows the first of them.
	Sample []MailRulePreviewMessage
	// Scanned counts the messages looked at.
	Scanned int
	// Truncated says the folder holds more mail than one run looks at or
	// acts on.
	Truncated bool
}

// MailRulePreviewMessage is a message a preview shows.
type MailRulePreviewMessage struct {
	ID        int64
	AccountID string
	FolderID  string
	Subject   string
	FromName  string
	FromEmail string
	Date      time.Time
}

// MailRuleRunResult says what a run on mail already there did.
type MailRuleRunResult struct {
	// Changed counts the messages the rule acted on; Unchanged those it no
	// longer matched, or had nothing left to do to.
	Changed   int
	Unchanged int
	// Folders are the folders whose contents changed.
	Folders []FolderRef
}

// mailRuleRunTarget is one account's part of a run.
type mailRuleRunTarget struct {
	runner *mailRuleRunner
	folder mailRuleFolder
}

// mailRuleRunTargets finds, for each account in scope, the rule ready to run
// and the folder to run it on. Accounts allow refuses are left out, when
// allow is set.
func (db *DB) mailRuleRunTargets(ctx context.Context, userID, ruleID string, scope MailRuleRunScope, allow func(accountID string) bool) (models.MailRule, map[string]mailRuleRunTarget, error) {
	rule, err := db.GetMailRule(ctx, userID, ruleID)
	if err != nil {
		return rule, nil, err
	}
	accountID := scope.AccountID
	if scope.Folder.Role == "" {
		// A folder named by ID belongs to one account.
		if scope.Folder.FolderID == "" {
			return rule, nil, ErrMailRuleRunFolder
		}
		var folderAccount string
		err := db.Read().QueryRowContext(ctx, `SELECT account_id FROM folders WHERE id = ?`, scope.Folder.FolderID).Scan(&folderAccount)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && accountID != "" && accountID != folderAccount) {
			return rule, nil, ErrMailRuleRunFolder
		}
		if err != nil {
			return rule, nil, err
		}
		accountID = folderAccount
		scope.Folder.Path = ""
	}
	if rule.AccountID != "" {
		if accountID != "" && accountID != rule.AccountID {
			return rule, nil, ErrMailRuleRunFolder
		}
		accountID = rule.AccountID
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id FROM accounts WHERE user_id = ? AND (? = '' OR id = ?) ORDER BY sort_order, id`, userID, accountID, accountID)
	if err != nil {
		return rule, nil, err
	}
	var accounts []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return rule, nil, err
		}
		accounts = append(accounts, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rule, nil, err
	}
	targets := map[string]mailRuleRunTarget{}
	for _, account := range accounts {
		if allow != nil && !allow(account) {
			continue
		}
		runner, err := db.newMailRuleRunner(ctx, account, []models.MailRule{rule})
		if err != nil {
			return rule, nil, err
		}
		if runner == nil {
			continue
		}
		if folder, ok := runner.findFolder(scope.Folder); ok {
			targets[account] = mailRuleRunTarget{runner: runner, folder: folder}
		}
	}
	if len(targets) == 0 {
		return rule, nil, ErrMailRuleRunFolder
	}
	return rule, targets, nil
}

// wouldChange reports whether the rule matches the message and has
// something to do to it.
func (t mailRuleRunTarget) wouldChange(m *mailRuleMessage) bool {
	if m == nil || m.draft || len(m.facts.Folders) == 0 {
		return false
	}
	rule := t.runner.rules[0]
	if !rule.Match(m.facts) {
		return false
	}
	for _, action := range rule.Actions {
		if _, needless := t.runner.needless(m, t.folder.id, action); !needless {
			return true
		}
	}
	return false
}

// PreviewMailRuleRun finds the mail already in a folder that a rule, enabled
// or not, would change, without changing it.
func (db *DB) PreviewMailRuleRun(ctx context.Context, userID, ruleID string, scope MailRuleRunScope, allow func(accountID string) bool) (MailRulePreview, error) {
	preview := MailRulePreview{Matches: []int64{}, Sample: []MailRulePreviewMessage{}}
	_, targets, err := db.mailRuleRunTargets(ctx, userID, ruleID, scope, allow)
	if err != nil {
		return preview, err
	}
	folderIDs := make([]any, 0, len(targets))
	byFolder := map[string]mailRuleRunTarget{}
	for _, target := range targets {
		folderIDs = append(folderIDs, target.folder.id)
		byFolder[target.folder.id] = target
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT message_id, folder_id, message_date FROM message_folder_state
		WHERE folder_id IN (`+sqlPlaceholders(len(folderIDs))+`) AND is_deleted = 0
		ORDER BY message_date DESC, message_id DESC LIMIT ?`, append(folderIDs, mailRuleRunScanLimit+1)...)
	if err != nil {
		return preview, err
	}
	type candidate struct {
		messageID int64
		folderID  string
		date      sqliteNullTime
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.messageID, &c.folderID, &c.date); err != nil {
			rows.Close()
			return preview, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return preview, err
	}
	if len(candidates) > mailRuleRunScanLimit {
		candidates, preview.Truncated = candidates[:mailRuleRunScanLimit], true
	}
	for _, c := range candidates {
		if len(preview.Matches) == mailRuleRunMatchLimit {
			preview.Truncated = true
			break
		}
		preview.Scanned++
		target := byFolder[c.folderID]
		m, err := target.runner.loadMessage(ctx, c.messageID)
		if err != nil {
			return preview, err
		}
		if !target.wouldChange(m) {
			continue
		}
		preview.Matches = append(preview.Matches, c.messageID)
		if len(preview.Sample) < mailRulePreviewSample {
			preview.Sample = append(preview.Sample, MailRulePreviewMessage{
				ID: m.id, AccountID: target.runner.accountID, FolderID: c.folderID,
				Subject: m.subject, FromName: m.facts.FromName, FromEmail: m.fromEmail, Date: c.date.Time,
			})
		}
	}
	return preview, nil
}

// RunMailRuleNow applies a rule, enabled or not, to messages a preview
// found. A message that has left the folder, or that the rule no longer
// matches or has nothing left to do to, is left as it is.
func (db *DB) RunMailRuleNow(ctx context.Context, userID, ruleID string, scope MailRuleRunScope, messageIDs []int64, allow func(accountID string) bool) (MailRuleRunResult, error) {
	var result MailRuleRunResult
	_, targets, err := db.mailRuleRunTargets(ctx, userID, ruleID, scope, allow)
	if err != nil {
		return result, err
	}
	if len(messageIDs) > mailRuleRunMatchLimit {
		messageIDs = messageIDs[:mailRuleRunMatchLimit]
	}
	changed := map[FolderRef]bool{}
	seen := map[int64]bool{}
	for _, messageID := range messageIDs {
		if seen[messageID] {
			continue
		}
		seen[messageID] = true
		var accountID string
		var inFolder bool
		err := db.Read().QueryRowContext(ctx, `SELECT account_id FROM messages WHERE id = ?`, messageID).Scan(&accountID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		target, ok := targets[accountID]
		if ok {
			if err := db.Read().QueryRowContext(ctx, `
				SELECT EXISTS(SELECT 1 FROM message_folder_state WHERE message_id = ? AND folder_id = ? AND is_deleted = 0)`,
				messageID, target.folder.id).Scan(&inFolder); err != nil {
				return result, err
			}
		}
		var m *mailRuleMessage
		if inFolder {
			if m, err = target.runner.loadMessage(ctx, messageID); err != nil {
				return result, err
			}
		}
		if !inFolder || !target.wouldChange(m) {
			result.Unchanged++
			continue
		}
		outcome, err := target.runner.run(ctx, messageID, target.folder.id, models.MailRuleTriggerManual)
		if err != nil {
			return result, err
		}
		if !outcome.Changed() {
			result.Unchanged++
			continue
		}
		result.Changed++
		for folderID := range outcome.Folders {
			changed[FolderRef{AccountID: accountID, FolderID: folderID}] = true
		}
	}
	for ref := range changed {
		result.Folders = append(result.Folders, ref)
	}
	for _, target := range targets {
		if target.runner.recorded {
			if err := pruneMailRuleRuns(ctx, db.Write(), target.runner.owner); err != nil {
				log.Printf("mail rules: prune activity of %s: %v", userID, err)
			}
			break
		}
	}
	return result, nil
}
