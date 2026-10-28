package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// mailRuleArrivalBatch is how many arrivals one pass looks at; the rest wait
// for the next.
const mailRuleArrivalBatch = 500

// MailRuleOutcome is what rules did to the mail that just arrived, keyed by
// normalized Message-ID, so a sync can leave mail the rules moved away or
// marked read out of what it announces.
type MailRuleOutcome struct {
	Moved map[string]bool
	Read  map[string]bool
	// Folders are the folders whose contents changed.
	Folders map[string]bool
}

// Changed reports whether rules acted on any message.
func (o MailRuleOutcome) Changed() bool { return len(o.Folders) > 0 }

// MailRuleKey is how a MailRuleOutcome names a message synced from IMAP: its
// Message-ID, or the one made up for it when it has none.
func (m SyncMessage) MailRuleKey() string {
	if id := mailmessage.NormalizeMessageID(m.MessageID); id != "" {
		return id
	}
	return mailmessage.NormalizeMessageID(fmt.Sprintf("<%s-%d@sync.gofer>", m.FolderID, m.RemoteUID))
}

// MailRuleKey is how a MailRuleOutcome names a message synced from a
// provider API.
func (m ProviderSyncMessage) MailRuleKey() string {
	if id := mailmessage.NormalizeMessageID(m.InternetMessageID); id != "" {
		return id
	}
	return mailmessage.NormalizeMessageID(syntheticProviderMessageID(m.ProviderMessageID))
}

func newMailRuleOutcome() MailRuleOutcome {
	return MailRuleOutcome{Moved: map[string]bool{}, Read: map[string]bool{}, Folders: map[string]bool{}}
}

func (o MailRuleOutcome) add(other MailRuleOutcome) {
	for k := range other.Moved {
		o.Moved[k] = true
	}
	for k := range other.Read {
		o.Read[k] = true
	}
	for k := range other.Folders {
		o.Folders[k] = true
	}
}

// ApplyPendingMailRules runs the owner's rules on the mail that arrived in
// an account since they last ran. Each message is looked at once: what the
// rules did, or failed to do, is recorded, and the message is not looked at
// again.
func (db *DB) ApplyPendingMailRules(ctx context.Context, accountID string) (MailRuleOutcome, error) {
	outcome := newMailRuleOutcome()
	type pendingArrival struct {
		messageID int64
		folderID  string
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT message_id, folder_id FROM mail_rule_pending WHERE account_id = ? ORDER BY message_id LIMIT ?`, accountID, mailRuleArrivalBatch)
	if err != nil {
		return outcome, err
	}
	var pending []pendingArrival
	for rows.Next() {
		var arrival pendingArrival
		if err := rows.Scan(&arrival.messageID, &arrival.folderID); err != nil {
			rows.Close()
			return outcome, err
		}
		pending = append(pending, arrival)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(pending) == 0 {
		return outcome, err
	}

	runner, err := db.newMailRuleRunner(ctx, accountID, nil)
	if err != nil {
		return outcome, err
	}
	for _, arrival := range pending {
		if runner != nil {
			result, err := runner.run(ctx, arrival.messageID, arrival.folderID, models.MailRuleTriggerArrival)
			if err != nil {
				return outcome, err
			}
			outcome.add(result)
		}
		if _, err := db.Write().ExecContext(ctx, `DELETE FROM mail_rule_pending WHERE message_id = ?`, arrival.messageID); err != nil {
			return outcome, err
		}
	}
	if runner != nil && runner.recorded {
		if err := pruneMailRuleRuns(ctx, db.Write(), runner.owner); err != nil {
			log.Printf("mail rules: prune activity of %s: %v", accountID, err)
		}
	}
	return outcome, nil
}

// mailRuleRunner applies a list of rules to messages of one account.
type mailRuleRunner struct {
	db        *DB
	owner     string
	accountID string
	provider  string
	rules     []*mailrules.Rule
	folders   map[string]mailRuleFolder
	recorded  bool
	// quiet records only what failed: a run by hand over many messages
	// records one summary instead.
	quiet  bool
	failed int
}

type mailRuleFolder struct {
	id, name, role, remoteID string
}

// newMailRuleRunner prepares the owner's enabled rules for an account, or
// only the rules given. It is nil when no rule applies to the account.
func (db *DB) newMailRuleRunner(ctx context.Context, accountID string, only []models.MailRule) (*mailRuleRunner, error) {
	runner := &mailRuleRunner{db: db, accountID: accountID}
	var deleting bool
	err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(user_id, ''), provider, COALESCE(is_deleting, 0) FROM accounts WHERE id = ?`, accountID).
		Scan(&runner.owner, &runner.provider, &deleting)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (deleting || runner.owner == "")) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rules := only
	if rules == nil {
		rules, err = queryMailRules(ctx, db.Read(), `
			SELECT `+mailRuleColumns+` FROM mail_rules
			WHERE user_id = ? AND enabled = 1 AND (account_id IS NULL OR account_id = ?)
			ORDER BY sort_order, created_at, id`, runner.owner, accountID)
		if err != nil {
			return nil, err
		}
	}
	if len(rules) == 0 {
		return nil, nil
	}
	if err := runner.loadFolders(ctx); err != nil {
		return nil, err
	}
	for _, rule := range rules {
		// A folder named by ID may have a new ID since, after being renamed
		// outside Gofer; its name finds it.
		for i := range rule.Conditions {
			if target := rule.Conditions[i].Folder; target != nil && target.FolderID != "" {
				if folder, ok := runner.findFolder(*target); ok {
					target.FolderID = folder.id
				}
			}
		}
		compiled, err := mailrules.Compile(rule)
		if err != nil {
			log.Printf("mail rules: skip rule %s of %s: %v", rule.ID, runner.owner, err)
			continue
		}
		runner.rules = append(runner.rules, compiled)
	}
	if len(runner.rules) == 0 {
		return nil, nil
	}
	return runner, nil
}

func (r *mailRuleRunner) loadFolders(ctx context.Context) error {
	rows, err := r.db.Read().QueryContext(ctx, `
		SELECT id, name, role, COALESCE(remote_id, '') FROM folders
		WHERE account_id = ? AND COALESCE(discovery_state, 'active') = 'active'`, r.accountID)
	if err != nil {
		return err
	}
	defer rows.Close()
	r.folders = map[string]mailRuleFolder{}
	for rows.Next() {
		var folder mailRuleFolder
		if err := rows.Scan(&folder.id, &folder.name, &folder.role, &folder.remoteID); err != nil {
			return err
		}
		r.folders[folder.id] = folder
	}
	return rows.Err()
}

// findFolder finds the account's folder a target names: by role, by ID, or
// by the name the folder had when the rule was saved.
func (r *mailRuleRunner) findFolder(target models.MailRuleFolderTarget) (mailRuleFolder, bool) {
	if target.Role != "" {
		roles := []string{target.Role}
		if target.Role == "junk" {
			roles = append(roles, "spam")
		}
		for _, role := range roles {
			var found mailRuleFolder
			for _, folder := range r.folders {
				// Several folders can share a role; take the same one each time.
				if folder.role == role && (found.id == "" || folder.id < found.id) {
					found = folder
				}
			}
			if found.id != "" {
				return found, true
			}
		}
		return mailRuleFolder{}, false
	}
	if folder, ok := r.folders[target.FolderID]; ok {
		return folder, true
	}
	if target.Path != "" {
		for _, folder := range r.folders {
			if folder.remoteID == target.Path {
				return folder, true
			}
		}
	}
	return mailRuleFolder{}, false
}

// mailRuleMessage is a message as the rules see it, and what acting on it
// needs.
type mailRuleMessage struct {
	id           int64
	subject      string
	fromEmail    string
	normalizedID string
	draft        bool
	isRead       bool
	isStarred    bool
	// labels are the names of the message's labels, in lower case.
	labels map[string]bool
	facts  mailrules.Message
}

func (r *mailRuleRunner) loadMessage(ctx context.Context, messageID int64) (*mailRuleMessage, error) {
	m := &mailRuleMessage{id: messageID, labels: map[string]bool{}}
	err := r.db.Read().QueryRowContext(ctx, `
		SELECT subject, from_name, from_email, list_id, message_id_normalized FROM messages WHERE id = ? AND account_id = ?`,
		messageID, r.accountID).Scan(&m.subject, &m.facts.FromName, &m.fromEmail, &m.facts.ListID, &m.normalizedID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.facts.FromEmail, m.facts.Subject = m.fromEmail, m.subject

	rows, err := r.db.Read().QueryContext(ctx, `
		SELECT folder_id, is_read, is_starred, is_draft FROM message_folder_state WHERE message_id = ? AND is_deleted = 0`, messageID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var folderID string
		var read, starred, draft bool
		if err := rows.Scan(&folderID, &read, &starred, &draft); err != nil {
			rows.Close()
			return nil, err
		}
		m.isRead = m.isRead || read
		m.isStarred = m.isStarred || starred
		m.draft = m.draft || draft
		if folder, ok := r.folders[folderID]; ok {
			m.facts.Folders = append(m.facts.Folders, mailrules.Folder{ID: folder.id, Role: folder.role})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = r.db.Read().QueryContext(ctx, `
		SELECT lower(l.name) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = ?`, messageID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		m.labels[strings.TrimSpace(name)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = r.db.Read().QueryContext(ctx, `SELECT kind, name, email FROM message_recipients WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var address mailrules.Address
		if err := rows.Scan(&kind, &address.Name, &address.Email); err != nil {
			return nil, err
		}
		switch kind {
		case "to":
			m.facts.To = append(m.facts.To, address)
		case "cc":
			m.facts.CC = append(m.facts.CC, address)
		}
	}
	return m, rows.Err()
}

// source is the folder a rule moves a message out of: its inbox copy, else
// the folder it arrived in, else any folder it shows in.
func (r *mailRuleRunner) source(m *mailRuleMessage, arrival string) string {
	for _, folder := range m.facts.Folders {
		if folder.Role == "inbox" {
			return folder.ID
		}
	}
	for _, folder := range m.facts.Folders {
		if folder.ID == arrival {
			return folder.ID
		}
	}
	if len(m.facts.Folders) > 0 {
		return m.facts.Folders[0].ID
	}
	return ""
}

// run applies the rules to one message and records what each one that
// matched did. Only a failure to read or record stops it; a failed action
// is recorded and the rest go on.
func (r *mailRuleRunner) run(ctx context.Context, messageID int64, arrival, trigger string) (MailRuleOutcome, error) {
	outcome := newMailRuleOutcome()
	m, err := r.loadMessage(ctx, messageID)
	if err != nil || m == nil || m.draft || len(m.facts.Folders) == 0 {
		return outcome, err
	}
	moved := ""
	for _, rule := range r.rules {
		if !rule.Match(m.facts) {
			continue
		}
		done, skipped, failures := []string{}, []string{}, []string{}
		// Flags and labels go first, while the message is still where it
		// arrived; a move carries them along.
		for _, action := range orderedMailRuleActions(rule.Actions) {
			if mailrules.MovesMessage(action.Kind) && moved != "" {
				skipped = append(skipped, "not moved: "+moved)
				continue
			}
			text, err := r.apply(ctx, m, arrival, action, outcome)
			if ctx.Err() != nil {
				return outcome, ctx.Err()
			}
			switch {
			case errors.Is(err, errMailRuleSkip):
				skipped = append(skipped, text)
			case err != nil:
				failures = append(failures, text+": "+err.Error())
			default:
				if text != "" {
					done = append(done, text)
				}
				if mailrules.MovesMessage(action.Kind) {
					moved = "already moved by “" + rule.Name + "”"
				}
			}
		}
		status := models.MailRuleRunApplied
		if len(failures) > 0 {
			status = models.MailRuleRunFailed
		} else if len(done) == 0 {
			status = models.MailRuleRunSkipped
		}
		detail := strings.Join(append(append(failures, done...), skipped...), "; ")
		if status == models.MailRuleRunFailed {
			r.failed++
		}
		if !r.quiet || status == models.MailRuleRunFailed {
			if err := recordMailRuleRun(ctx, r.db.Write(), r.owner, models.MailRuleRun{
				RuleID: rule.ID, RuleName: rule.Name, AccountID: r.accountID, MessageID: m.id,
				Subject: m.subject, FromEmail: m.fromEmail, Trigger: trigger, Status: status, Detail: detail,
			}); err != nil {
				return outcome, err
			}
			r.recorded = true
		}
		if rule.StopProcessing {
			break
		}
		// Later rules see the message where this one left it.
		if refreshed, err := r.loadMessage(ctx, messageID); err != nil {
			return outcome, err
		} else if refreshed == nil || len(refreshed.facts.Folders) == 0 {
			break
		} else {
			m = refreshed
		}
	}
	return outcome, nil
}

func orderedMailRuleActions(actions []models.MailRuleAction) []models.MailRuleAction {
	ordered := make([]models.MailRuleAction, 0, len(actions))
	for _, action := range actions {
		if !mailrules.MovesMessage(action.Kind) {
			ordered = append(ordered, action)
		}
	}
	for _, action := range actions {
		if mailrules.MovesMessage(action.Kind) {
			ordered = append(ordered, action)
		}
	}
	return ordered
}

// errMailRuleSkip means an action had nothing to do, or nowhere to do it.
var errMailRuleSkip = errors.New("skipped")

// apply does one action and says what it did, in words for the activity.
func (r *mailRuleRunner) apply(ctx context.Context, m *mailRuleMessage, arrival string, action models.MailRuleAction, outcome MailRuleOutcome) (string, error) {
	if text, needless := r.needless(m, arrival, action); needless {
		return text, errMailRuleSkip
	}
	switch action.Kind {
	case models.MailRuleActionMarkRead:
		if err := r.db.SetMessagesReadAndQueueForUser(ctx, []int64{m.id}, true, r.owner); err != nil {
			return "mark as read", err
		}
		m.isRead = true
		outcome.Read[m.normalizedID] = true
		r.touch(m, outcome)
		return "marked as read", nil
	case models.MailRuleActionStar:
		if err := r.db.SetMessagesStarredAndQueueForUser(ctx, []int64{m.id}, true, r.owner); err != nil {
			return "star", err
		}
		m.isStarred = true
		r.touch(m, outcome)
		return "starred", nil
	case models.MailRuleActionLabel:
		info, err := r.mutationInfo(ctx, m, r.source(m, arrival))
		if err != nil {
			return "add label “" + action.Label + "”", err
		}
		if err := r.db.QueueUserLabels(ctx, r.owner, []ThreadMessageMutationInfo{info}, action.Label, LabelMutationAdd); err != nil {
			return "add label “" + action.Label + "”", err
		}
		m.labels[strings.ToLower(strings.TrimSpace(action.Label))] = true
		r.touch(m, outcome)
		return "labeled “" + action.Label + "”", nil
	}

	verb := map[string]string{
		models.MailRuleActionMove:    "move",
		models.MailRuleActionArchive: "archive",
		models.MailRuleActionTrash:   "move to Trash",
		models.MailRuleActionSpam:    "mark as spam",
	}[action.Kind]
	destination, _ := r.findFolder(moveTarget(action))
	source := r.source(m, arrival)
	if action.Kind == models.MailRuleActionSpam && (r.provider == "imap" || r.provider == providers.ProviderJMAP) {
		// Spam on IMAP and JMAP also teaches the server's filter, as when
		// the owner marks mail as spam.
		info, err := r.mutationInfo(ctx, m, source)
		if err != nil {
			return verb, err
		}
		if err := r.db.QueueUserIMAPSpam(ctx, r.owner, []ThreadMessageMutationInfo{info}, destination.id, true); err != nil {
			return verb, err
		}
	} else if err := r.db.MoveMessagesAndQueueForUser(ctx, []int64{m.id}, source, destination.id, r.owner); err != nil {
		return verb, err
	}
	outcome.Moved[m.normalizedID] = true
	outcome.Folders[destination.id] = true
	r.touch(m, outcome)
	switch action.Kind {
	case models.MailRuleActionTrash:
		return "moved to Trash", nil
	case models.MailRuleActionSpam:
		return "marked as spam", nil
	case models.MailRuleActionArchive:
		return "archived", nil
	}
	return "moved to “" + destination.name + "”", nil
}

// moveTarget is the folder an action that moves mail moves it to.
func moveTarget(action models.MailRuleAction) models.MailRuleFolderTarget {
	switch action.Kind {
	case models.MailRuleActionMove:
		if action.Folder != nil {
			return *action.Folder
		}
	case models.MailRuleActionArchive:
		return models.MailRuleFolderTarget{Role: "archive"}
	case models.MailRuleActionTrash:
		return models.MailRuleFolderTarget{Role: "trash"}
	case models.MailRuleActionSpam:
		return models.MailRuleFolderTarget{Role: "junk"}
	}
	return models.MailRuleFolderTarget{}
}

// needless reports whether an action would leave the message as it is, and
// says why.
func (r *mailRuleRunner) needless(m *mailRuleMessage, arrival string, action models.MailRuleAction) (string, bool) {
	switch action.Kind {
	case models.MailRuleActionMarkRead:
		return "already read", m.isRead
	case models.MailRuleActionStar:
		return "already starred", m.isStarred
	case models.MailRuleActionLabel:
		return "already labeled “" + action.Label + "”", m.labels[strings.ToLower(strings.TrimSpace(action.Label))]
	case models.MailRuleActionMove, models.MailRuleActionArchive, models.MailRuleActionTrash, models.MailRuleActionSpam:
	default:
		return action.Kind + ": unknown action", true
	}
	target := moveTarget(action)
	destination, ok := r.findFolder(target)
	if !ok {
		name := target.Path
		if name == "" {
			name = map[string]string{"archive": "an Archive", "trash": "a Trash", "junk": "a Spam", "inbox": "an Inbox"}[target.Role]
			if name == "" {
				name = "that"
			}
			return "not moved: the account has no " + name + " folder", true
		}
		return "not moved: the folder “" + name + "” is gone", true
	}
	source := r.source(m, arrival)
	for _, folder := range m.facts.Folders {
		if folder.ID == destination.id && (source == destination.id || len(m.facts.Folders) == 1) {
			return "already in “" + destination.name + "”", true
		}
	}
	return "not moved: the message is in no folder", source == ""
}

func (r *mailRuleRunner) touch(m *mailRuleMessage, outcome MailRuleOutcome) {
	for _, folder := range m.facts.Folders {
		outcome.Folders[folder.ID] = true
	}
}

func (r *mailRuleRunner) mutationInfo(ctx context.Context, m *mailRuleMessage, folderID string) (ThreadMessageMutationInfo, error) {
	info, err := r.db.GetMessageMutationInfoForFolderForUser(ctx, m.id, folderID, r.owner)
	if err != nil {
		return ThreadMessageMutationInfo{}, err
	}
	if info == nil {
		return ThreadMessageMutationInfo{}, sql.ErrNoRows
	}
	return ThreadMessageMutationInfo{MessageID: m.id, MessageMutationInfo: *info, IsRead: m.isRead, IsStarred: m.isStarred}, nil
}
