package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The allowed mail lists are policy settings (internal/policy), changed one
// entry at a time on any policy: the Default policy's apply to everyone, and
// a named policy's to the people it's assigned to, an empty one meaning no
// limit for them. Each change is a policy change, recorded as one.

// MailLists are the allowed mail lists as Mail security shows them: the
// Default policy's, and the people the exemption policy is assigned to.
type MailLists struct {
	storage.MailAllowlist
	// Exemption is the exemption policy, nil when there's none, and Exempt
	// the IDs of the people it's assigned to directly, sorted.
	Exemption *policy.Policy
	Exempt    []string
}

// ErrMailListProvider is a provider other than Gmail or Outlook.
var ErrMailListProvider = errors.New("only Gmail and Outlook are let in by name")

// MailListOp is what a change does to a list.
type MailListOp string

const (
	// MailListAdd adds Value; on a policy that left the list to others, the
	// list then holds Value alone.
	MailListAdd MailListOp = "add"
	// MailListRemove takes Value off.
	MailListRemove MailListOp = "remove"
	// MailListSet makes a named policy set the list, empty: no limit for
	// the people it's assigned to.
	MailListSet MailListOp = "set"
	// MailListClear leaves the list to the policies after this one.
	MailListClear MailListOp = "clear"
)

// MailListChange changes one of a policy's allowed mail lists.
type MailListChange struct {
	ActorUserID    string
	ActorSessionID string
	// PolicyID is the policy; the Default one when empty.
	PolicyID string
	// List is storage.MailAllowlistServer, MailAllowlistDomain or
	// MailAllowlistProvider.
	List  string
	Value string
	Op    MailListOp
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
	if exemption, err := policy.Get(ctx, tx, policy.MailExemptionID); err == nil {
		lists.Exemption = &exemption
	} else if !errors.Is(err, policy.ErrNotFound) {
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

// ChangeMailList changes one of a policy's lists and returns the entry it
// added or took off, cleaned. A change that changes nothing, such as adding
// an entry that's there, isn't made. Emptying the Default policy's list
// clears it, which is the same: no limit.
func (m *Manager) ChangeMailList(ctx context.Context, change MailListChange) (string, error) {
	setting, ok := policy.MailListSetting(change.List)
	if !ok {
		return "", fmt.Errorf("unknown allowed mail list %q", change.List)
	}
	policyID := strings.TrimSpace(change.PolicyID)
	if policyID == "" {
		policyID = policy.DefaultID
	}
	var value string
	switch change.Op {
	case MailListAdd, MailListRemove:
		var err error
		if value, err = cleanMailListEntry(change.List, change.Value); err != nil {
			return "", err
		}
	case MailListSet, MailListClear:
	default:
		return "", fmt.Errorf("unknown change %q to an allowed mail list", change.Op)
	}
	err := m.changePolicies(ctx, change.ActorUserID, change.ActorSessionID, func(tx *sql.Tx, actorUserID, actorSessionID string, now time.Time) error {
		current, err := policy.Get(ctx, tx, policyID)
		if err != nil {
			return err
		}
		values, err := policy.Values(ctx, tx, policyID)
		if err != nil {
			return err
		}
		entries, set := []string{}, false
		for _, stored := range values {
			if stored.Key != setting.Key {
				continue
			}
			set = true
			if decoded, err := setting.Decode(stored.JSON); err == nil {
				entries = slices.Clone(decoded.([]string))
			}
		}
		at := slices.Index(entries, value)
		switch {
		case change.Op == MailListAdd && (at < 0 || !set):
			if at < 0 {
				entries = append(entries, value)
			}
		case change.Op == MailListRemove && set && at >= 0:
			entries = slices.Delete(entries, at, at+1)
		case change.Op == MailListSet && !set && !current.Default:
		case change.Op == MailListClear && set:
			entries = nil
		default:
			return nil
		}
		next := policy.ValueChange{Key: setting.Key}
		if entries != nil && !(current.Default && len(entries) == 0) {
			if next.JSON, err = setting.Encode(entries); err != nil {
				return err
			}
		}
		_, err = m.setPolicyValuesTx(ctx, tx, actorUserID, actorSessionID, now, policyID, current.Version, []policy.ValueChange{next})
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
