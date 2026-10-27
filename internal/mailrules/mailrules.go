// Package mailrules checks mail rules and tells which messages they match.
// It does not act on mail; storage applies what a matching rule asks for.
package mailrules

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Limits on what a rule may hold.
const (
	MaxNameLength    = 120
	MaxValueLength   = 500
	MaxConditions    = 20
	MaxActions       = 10
	MaxRulesPerOwner = 500
)

// TargetRoles are the folder roles a rule may name for any account.
var TargetRoles = []string{"inbox", "archive", "trash", "junk", "sent", "drafts"}

// Message is what rules see of a message: its headers as synced, and the
// folders it shows in.
type Message struct {
	FromName  string
	FromEmail string
	To        []Address
	CC        []Address
	Subject   string
	ListID    string
	Folders   []Folder
}

// Address is one recipient.
type Address struct {
	Name  string
	Email string
}

// Folder is one folder a message shows in.
type Folder struct {
	ID   string
	Role string
}

// InFolderRole reports whether the message shows in a folder with that role.
func (m Message) InFolderRole(role string) bool {
	for _, folder := range m.Folders {
		if sameRole(folder.Role, role) {
			return true
		}
	}
	return false
}

// sameRole treats junk and spam as one role: providers call the folder
// either.
func sameRole(a, b string) bool {
	if a == "spam" {
		a = "junk"
	}
	if b == "spam" {
		b = "junk"
	}
	return a != "" && a == b
}

// Rule is a rule ready to be matched: checked, with its patterns compiled.
type Rule struct {
	models.MailRule
	patterns []*regexp.Regexp
}

// Compile checks a rule and prepares it for matching.
func Compile(rule models.MailRule) (*Rule, error) {
	if err := Validate(&rule); err != nil {
		return nil, err
	}
	compiled := &Rule{MailRule: rule, patterns: make([]*regexp.Regexp, len(rule.Conditions))}
	for i, condition := range rule.Conditions {
		if condition.Operator == models.MailRuleOpMatches {
			compiled.patterns[i] = regexp.MustCompile("(?i)" + condition.Value)
		}
	}
	return compiled, nil
}

// Validate checks a rule and puts it in the form it is stored in: trimmed,
// with lower-case values where case doesn't matter. Its error is shown to
// the person who wrote the rule.
func Validate(rule *models.MailRule) error {
	rule.Name = strings.TrimSpace(rule.Name)
	rule.AccountID = strings.TrimSpace(rule.AccountID)
	if rule.Name == "" {
		return errors.New("give the rule a name")
	}
	if utf8.RuneCountInString(rule.Name) > MaxNameLength {
		return fmt.Errorf("the name can be at most %d characters", MaxNameLength)
	}
	if len(rule.Conditions) == 0 {
		return errors.New("add at least one condition")
	}
	if len(rule.Conditions) > MaxConditions {
		return fmt.Errorf("a rule can have at most %d conditions", MaxConditions)
	}
	for i := range rule.Conditions {
		if err := validateCondition(&rule.Conditions[i], rule.AccountID); err != nil {
			return err
		}
	}
	if len(rule.Actions) == 0 {
		return errors.New("add at least one action")
	}
	if len(rule.Actions) > MaxActions {
		return fmt.Errorf("a rule can have at most %d actions", MaxActions)
	}
	seen := map[string]bool{}
	location := ""
	for i := range rule.Actions {
		action := &rule.Actions[i]
		if err := validateAction(action, rule.AccountID); err != nil {
			return err
		}
		if MovesMessage(action.Kind) {
			if location != "" {
				return errors.New("a rule can only move mail to one place")
			}
			location = action.Kind
		}
		key := action.Kind + "\x00" + strings.ToLower(action.Label)
		if seen[key] {
			return errors.New("the rule has the same action twice")
		}
		seen[key] = true
	}
	return nil
}

// MovesMessage reports whether an action takes a message out of the folder
// it is in.
func MovesMessage(kind string) bool {
	switch kind {
	case models.MailRuleActionMove, models.MailRuleActionArchive, models.MailRuleActionTrash, models.MailRuleActionSpam:
		return true
	}
	return false
}

func validateCondition(condition *models.MailRuleCondition, accountID string) error {
	condition.Field = strings.TrimSpace(condition.Field)
	condition.Operator = strings.TrimSpace(condition.Operator)
	if condition.Field == models.MailRuleFieldFolder {
		if condition.Operator != models.MailRuleOpIs && condition.Operator != models.MailRuleOpIsNot {
			return errors.New("a folder condition can only be \"is\" or \"is not\"")
		}
		condition.Value = ""
		return validateFolderTarget(condition.Folder, accountID)
	}
	condition.Folder = nil
	switch condition.Field {
	case models.MailRuleFieldFrom, models.MailRuleFieldTo, models.MailRuleFieldCC, models.MailRuleFieldRecipients,
		models.MailRuleFieldSubject, models.MailRuleFieldListID:
	default:
		return fmt.Errorf("unknown condition %q", condition.Field)
	}
	condition.Value = strings.TrimSpace(condition.Value)
	if condition.Value == "" {
		return errors.New("every condition needs something to compare with")
	}
	if utf8.RuneCountInString(condition.Value) > MaxValueLength {
		return fmt.Errorf("a condition can be at most %d characters", MaxValueLength)
	}
	switch condition.Operator {
	case models.MailRuleOpContains, models.MailRuleOpNotContains, models.MailRuleOpIs, models.MailRuleOpIsNot,
		models.MailRuleOpStartsWith, models.MailRuleOpEndsWith:
	case models.MailRuleOpMatches:
		if _, err := regexp.Compile("(?i)" + condition.Value); err != nil {
			return fmt.Errorf("%q is not a valid pattern", condition.Value)
		}
	default:
		return fmt.Errorf("unknown comparison %q", condition.Operator)
	}
	return nil
}

func validateAction(action *models.MailRuleAction, accountID string) error {
	action.Kind = strings.TrimSpace(action.Kind)
	switch action.Kind {
	case models.MailRuleActionMove:
		action.Label = ""
		if action.Folder == nil {
			return errors.New("choose the folder to move mail to")
		}
		if action.Folder.Role == "sent" || action.Folder.Role == "drafts" {
			return errors.New("mail can't be moved to Sent or Drafts")
		}
		return validateFolderTarget(action.Folder, accountID)
	case models.MailRuleActionLabel:
		action.Folder = nil
		action.Label = strings.TrimSpace(action.Label)
		if action.Label == "" {
			return errors.New("enter the label to add")
		}
		if utf8.RuneCountInString(action.Label) > MaxValueLength {
			return fmt.Errorf("a label can be at most %d characters", MaxValueLength)
		}
	case models.MailRuleActionArchive, models.MailRuleActionTrash, models.MailRuleActionSpam,
		models.MailRuleActionMarkRead, models.MailRuleActionStar:
		action.Folder, action.Label = nil, ""
	default:
		return fmt.Errorf("unknown action %q", action.Kind)
	}
	return nil
}

func validateFolderTarget(target *models.MailRuleFolderTarget, accountID string) error {
	if target == nil {
		return errors.New("choose a folder")
	}
	target.Role = strings.TrimSpace(target.Role)
	target.FolderID = strings.TrimSpace(target.FolderID)
	target.Path = strings.TrimSpace(target.Path)
	if target.Role != "" {
		if target.Role == "spam" {
			target.Role = "junk"
		}
		valid := false
		for _, role := range TargetRoles {
			valid = valid || role == target.Role
		}
		if !valid {
			return fmt.Errorf("unknown folder %q", target.Role)
		}
		target.FolderID, target.Path = "", ""
		return nil
	}
	if target.FolderID == "" {
		return errors.New("choose a folder")
	}
	// A folder belongs to one account; a rule for every account can only
	// name folders every account has.
	if accountID == "" {
		return errors.New("a rule for every account can only use Inbox, Archive, Trash, Spam, Sent and Drafts; choose an account to use its other folders")
	}
	return nil
}

// Match reports whether the rule's conditions hold for the message. A rule
// that doesn't name Sent or Drafts in a folder condition never matches mail
// in those folders: it is mail the owner wrote.
func (r *Rule) Match(m Message) bool {
	if !r.namesOwnMail() && (m.InFolderRole("sent") || m.InFolderRole("drafts")) {
		return false
	}
	for i, condition := range r.Conditions {
		held := r.holds(i, condition, m)
		if r.MatchAll && !held {
			return false
		}
		if !r.MatchAll && held {
			return true
		}
	}
	return r.MatchAll
}

func (r *Rule) namesOwnMail() bool {
	for _, condition := range r.Conditions {
		if condition.Field == models.MailRuleFieldFolder && condition.Operator == models.MailRuleOpIs && condition.Folder != nil &&
			(condition.Folder.Role == "sent" || condition.Folder.Role == "drafts") {
			return true
		}
	}
	return false
}

func (r *Rule) holds(i int, condition models.MailRuleCondition, m Message) bool {
	if condition.Field == models.MailRuleFieldFolder {
		in := inFolder(m, *condition.Folder)
		if condition.Operator == models.MailRuleOpIsNot {
			return !in
		}
		return in
	}
	var values []string
	switch condition.Field {
	case models.MailRuleFieldFrom:
		values = []string{m.FromEmail, m.FromName}
	case models.MailRuleFieldTo:
		values = addressValues(m.To)
	case models.MailRuleFieldCC:
		values = addressValues(m.CC)
	case models.MailRuleFieldRecipients:
		values = append(addressValues(m.To), addressValues(m.CC)...)
	case models.MailRuleFieldSubject:
		values = []string{m.Subject}
	case models.MailRuleFieldListID:
		values = []string{m.ListID}
	}
	// A negative comparison holds when the positive one holds for none of
	// the values: "From doesn't contain x" means neither name nor address.
	operator, negate := condition.Operator, false
	switch operator {
	case models.MailRuleOpNotContains:
		operator, negate = models.MailRuleOpContains, true
	case models.MailRuleOpIsNot:
		operator, negate = models.MailRuleOpIs, true
	}
	want := strings.ToLower(condition.Value)
	for _, value := range values {
		if value == "" {
			continue
		}
		if compare(operator, strings.ToLower(value), want, r.patterns[i], value) {
			return !negate
		}
	}
	return negate
}

func compare(operator, value, want string, pattern *regexp.Regexp, original string) bool {
	switch operator {
	case models.MailRuleOpContains:
		return strings.Contains(value, want)
	case models.MailRuleOpIs:
		return value == want
	case models.MailRuleOpStartsWith:
		return strings.HasPrefix(value, want)
	case models.MailRuleOpEndsWith:
		return strings.HasSuffix(value, want)
	case models.MailRuleOpMatches:
		return pattern != nil && pattern.MatchString(original)
	}
	return false
}

func addressValues(addresses []Address) []string {
	values := make([]string, 0, len(addresses)*2)
	for _, address := range addresses {
		values = append(values, address.Email, address.Name)
	}
	return values
}

func inFolder(m Message, target models.MailRuleFolderTarget) bool {
	if target.Role != "" {
		return m.InFolderRole(target.Role)
	}
	for _, folder := range m.Folders {
		if folder.ID == target.FolderID {
			return true
		}
	}
	return false
}
