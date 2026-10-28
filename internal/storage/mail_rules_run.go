package storage

import (
	"context"
	"database/sql"
	"errors"
	"log"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A run by hand goes through a folder this many messages at a time, so
// that a large folder doesn't hold the store for long.
const MailRuleRunScanBatch = 500

// MailRuleRunApplyBatch is the most messages one step of a run changes.
var MailRuleRunApplyBatch = 200

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

// MailRuleScanStep is one step of finding the mail already in a folder that
// a rule would change.
type MailRuleScanStep struct {
	// Total is how many messages the folder holds, given on the first step.
	Total int
	// Scanned counts the messages this step looked at, and Matches are the
	// ones the rule would change.
	Scanned int
	Matches []int64
	// Next goes on to the following step; Done says there is none.
	Next int64
	Done bool
}

// MailRuleRunResult says what a step of a run by hand did.
type MailRuleRunResult struct {
	// Changed counts the messages the rule acted on, by account; Unchanged
	// those it no longer matched, or had nothing left to do to; Failed
	// those it failed to act on, which its activity records.
	Changed   map[string]int
	Unchanged int
	Failed    int
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

// ScanMailRuleRun looks at the next limit messages in a folder, newest
// message first, and finds those a rule, active or not, would change,
// without changing them. The first step starts with before 0.
func (db *DB) ScanMailRuleRun(ctx context.Context, userID, ruleID string, scope MailRuleRunScope, allow func(accountID string) bool, before int64, limit int) (MailRuleScanStep, error) {
	step := MailRuleScanStep{Matches: []int64{}}
	_, targets, err := db.mailRuleRunTargets(ctx, userID, ruleID, scope, allow)
	if err != nil {
		return step, err
	}
	folderIDs := make([]any, 0, len(targets))
	byFolder := map[string]mailRuleRunTarget{}
	for _, target := range targets {
		folderIDs = append(folderIDs, target.folder.id)
		byFolder[target.folder.id] = target
	}
	in := sqlPlaceholders(len(folderIDs))
	if before == 0 {
		if err := db.Read().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM message_folder_state WHERE folder_id IN (`+in+`) AND is_deleted = 0`, folderIDs...).Scan(&step.Total); err != nil {
			return step, err
		}
	}
	args := append(folderIDs, before == 0, before, limit)
	rows, err := db.Read().QueryContext(ctx, `
		SELECT message_id, folder_id FROM message_folder_state
		WHERE folder_id IN (`+in+`) AND is_deleted = 0 AND (? OR message_id < ?)
		ORDER BY message_id DESC LIMIT ?`, args...)
	if err != nil {
		return step, err
	}
	type candidate struct {
		messageID int64
		folderID  string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.messageID, &c.folderID); err != nil {
			rows.Close()
			return step, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return step, err
	}
	step.Done = len(candidates) < limit
	for _, c := range candidates {
		step.Scanned++
		step.Next = c.messageID
		target := byFolder[c.folderID]
		m, err := target.runner.loadMessage(ctx, c.messageID)
		if err != nil {
			return step, err
		}
		if target.wouldChange(m) {
			step.Matches = append(step.Matches, c.messageID)
		}
	}
	return step, nil
}

// RunMailRuleNow applies a rule, active or not, to messages a scan found,
// at most MailRuleRunApplyBatch of them. A message that has left the
// folder, or that the rule no longer matches or has nothing left to do to,
// is left as it is. Only what fails is recorded in the rule's activity; the
// run records a summary when it ends.
func (db *DB) RunMailRuleNow(ctx context.Context, userID, ruleID string, scope MailRuleRunScope, messageIDs []int64, allow func(accountID string) bool) (MailRuleRunResult, error) {
	result := MailRuleRunResult{Changed: map[string]int{}}
	_, targets, err := db.mailRuleRunTargets(ctx, userID, ruleID, scope, allow)
	if err != nil {
		return result, err
	}
	for _, target := range targets {
		target.runner.quiet = true
	}
	if len(messageIDs) > MailRuleRunApplyBatch {
		messageIDs = messageIDs[:MailRuleRunApplyBatch]
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
		result.Changed[accountID]++
		for folderID := range outcome.Folders {
			changed[FolderRef{AccountID: accountID, FolderID: folderID}] = true
		}
	}
	for ref := range changed {
		result.Folders = append(result.Folders, ref)
	}
	for _, target := range targets {
		result.Failed += target.runner.failed
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

// RecordMailRuleRunSummary records in a rule's activity what a run by hand
// did in one account.
func (db *DB) RecordMailRuleRunSummary(ctx context.Context, userID, ruleID, accountID, detail string, changed int) error {
	rule, err := db.GetMailRule(ctx, userID, ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		rule = models.MailRule{ID: ruleID, Name: "A deleted rule"}
	} else if err != nil {
		return err
	}
	status := models.MailRuleRunApplied
	if changed == 0 {
		status = models.MailRuleRunSkipped
	}
	if err := recordMailRuleRun(ctx, db.Write(), userID, models.MailRuleRun{
		RuleID: rule.ID, RuleName: rule.Name, AccountID: accountID, Trigger: models.MailRuleTriggerManual, Status: status, Detail: detail,
	}); err != nil {
		return err
	}
	return pruneMailRuleRuns(ctx, db.Write(), userID)
}
