package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The allowed mail lists are policy settings (internal/policy): Mail
// security changes the Default policy's, and exempting someone assigns
// them the exemption policy, which sets every list to nothing. Each change
// is a policy change, recorded as one.

// MailLists are the allowed mail lists as Mail security shows them: the
// Default policy's, and the people the exemption policy is assigned to.
type MailLists struct {
	storage.MailAllowlist
	// Exempt are the IDs of the people the exemption policy is assigned to
	// directly, sorted.
	Exempt []string
}

// ErrMailListProvider is a provider other than Gmail or Outlook.
var ErrMailListProvider = errors.New("only Gmail and Outlook are let in by name")

// MailListChange adds an entry to one of the Default policy's lists, or
// takes one off.
type MailListChange struct {
	ActorUserID    string
	ActorSessionID string
	// List is storage.MailAllowlistServer, MailAllowlistDomain or
	// MailAllowlistProvider.
	List  string
	Value string
	Add   bool
}

// MailLists reads the Default policy's lists and who is exempt from them.
func (m *Manager) MailLists(ctx context.Context, actorUserID string) (*MailLists, error) {
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin allowed mail lists read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, strings.TrimSpace(actorUserID), authz.SecurityRead); err != nil {
		return nil, err
	}
	lists := &MailLists{}
	if lists.MailAllowlist, err = policy.DefaultMailAllowlist(ctx, tx); err != nil {
		return nil, err
	}
	assignments, err := policy.Assignments(ctx, tx, policy.MailExemptionID)
	if err != nil {
		return nil, err
	}
	for _, assignment := range assignments {
		if assignment.Kind == policy.TargetUser {
			lists.Exempt = append(lists.Exempt, assignment.ID)
		}
	}
	sort.Strings(lists.Exempt)
	return lists, nil
}

// ChangeMailList adds an entry to one of the Default policy's lists, or
// takes one off, and returns it cleaned. Adding one that's there, or
// taking off one that isn't, changes nothing.
func (m *Manager) ChangeMailList(ctx context.Context, change MailListChange) (string, error) {
	setting, ok := policy.MailListSetting(change.List)
	if !ok {
		return "", fmt.Errorf("unknown allowed mail list %q", change.List)
	}
	value, err := cleanMailListEntry(change.List, change.Value)
	if err != nil {
		return "", err
	}
	err = m.changePolicies(ctx, change.ActorUserID, change.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		current, err := policy.Get(ctx, tx, policy.DefaultID)
		if err != nil {
			return err
		}
		lists, err := policy.ForInstance(ctx, tx)
		if err != nil {
			return err
		}
		entries := slices.Clone(lists.Strings(setting))
		at := slices.Index(entries, value)
		switch {
		case change.Add && at < 0:
			entries = append(entries, value)
		case !change.Add && at >= 0:
			entries = slices.Delete(entries, at, at+1)
		default:
			return nil
		}
		next := policy.ValueChange{Key: setting.Key}
		if len(entries) > 0 {
			if next.JSON, err = setting.Encode(entries); err != nil {
				return err
			}
		}
		_, err = m.setPolicyValuesTx(ctx, tx, actorUserID, actorSessionID, now, policy.DefaultID, current.Version, []policy.ValueChange{next})
		return err
	})
	return value, err
}

func cleanMailListEntry(list, value string) (string, error) {
	if list != storage.MailAllowlistProvider {
		return storage.NormalizeMailAllowlistEntry(list, value)
	}
	switch provider := strings.ToLower(strings.TrimSpace(value)); provider {
	case providers.ProviderGmail, providers.ProviderOutlook:
		return provider, nil
	}
	return "", ErrMailListProvider
}

// SetMailListExemption exempts someone from the allowed mail lists, by
// assigning them the exemption policy, or holds them to the lists again.
// The policy is made, first in the order, the first time it's needed.
func (m *Manager) SetMailListExemption(ctx context.Context, actorUserID, actorSessionID, userID string, exempt bool) (*PolicyAssignmentChange, error) {
	var result *PolicyAssignmentChange
	err := m.changePolicies(ctx, actorUserID, actorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) (err error) {
		if _, err := policy.Get(ctx, tx, policy.MailExemptionID); errors.Is(err, policy.ErrNotFound) {
			if !exempt {
				result = &PolicyAssignmentChange{}
				return nil
			}
			if err := m.createMailExemption(ctx, tx, actorUserID, actorSessionID, now); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		result, err = m.changePolicyAssignmentsTx(ctx, tx, actorUserID, actorSessionID, now, policy.MailExemptionID,
			[]policy.Target{{Kind: policy.TargetUser, ID: strings.TrimSpace(userID)}}, exempt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// createMailExemption makes the exemption policy: every list set to
// nothing, first in the order, so no other policy's list reaches the
// people it's assigned to.
func (m *Manager) createMailExemption(ctx context.Context, tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
	if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.PoliciesManageSecurity, now); err != nil {
		return err
	}
	var created policy.Policy
	var err error
	for n := 1; ; n++ {
		name := policy.MailExemptionName
		if n > 1 {
			name += " " + strconv.Itoa(n)
		}
		created, err = policy.Create(ctx, tx, policy.NewPolicy{ID: policy.MailExemptionID, Name: name, Description: policy.MailExemptionDescription, By: actorUserID, At: now})
		if !errors.Is(err, policy.ErrNameTaken) {
			break
		}
	}
	if err != nil {
		return err
	}
	order, err := policy.List(ctx, tx)
	if err != nil {
		return err
	}
	ids := []string{created.ID}
	for _, item := range order {
		if !item.Default && item.ID != created.ID {
			ids = append(ids, item.ID)
		}
	}
	if err := policy.Reorder(ctx, tx, ids); err != nil {
		return err
	}
	changes := map[string]audit.Change{"name": {To: created.Name}, "description": {To: created.Description}}
	var values []policy.ValueChange
	for _, setting := range []*policy.Setting{policy.MailAllowedServers, policy.MailAllowedDomains, policy.MailAllowedProviders} {
		values = append(values, policy.ValueChange{Key: setting.Key, JSON: json.RawMessage(`[]`)})
		changes[setting.Key] = audit.Change{To: []string{}}
	}
	if created, err = policy.SetValues(ctx, tx, created.ID, created.Version, values, actorUserID, now); err != nil {
		return err
	}
	return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
		Action: "policy.created", Target: policyAuditTarget(created), Changes: changes,
		Metadata: map[string]any{"priority": 1},
	})
}
