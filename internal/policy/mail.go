package policy

import (
	"context"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// MailExemptionID is the policy the allowed mail lists' exemptions became
// (storage.MailListExemptionPolicyID): it sets every list to nothing, first
// in the order, for the people it's assigned to.
const MailExemptionID = storage.MailListExemptionPolicyID

// MailExemptionName and MailExemptionDescription are what it's called
// when it's made.
const (
	MailExemptionName        = storage.MailListExemptionName
	MailExemptionDescription = storage.MailListExemptionDescription
)

// mailCacheKey is the policies' cache kept with the central database.
type mailCacheKey struct{}

// mailCache is db's cache of people's settings. Mailboxes check the lists
// each time they connect, so a check reads two revision counters and,
// unless something changed, nothing else.
func mailCache(db *storage.DB) *Cache {
	return db.Attached(mailCacheKey{}, func() any { return NewCache(db.Read(), DefaultCacheSize) }).(*Cache)
}

// MailAllowlistForUser returns the allowed mail lists as they apply to the
// person with userID, from the policies in db, the central database. With
// no userID it's the Default policy's, which holds everyone to them.
func MailAllowlistForUser(ctx context.Context, db *storage.DB, userID string) (storage.MailAllowlist, error) {
	cache := mailCache(db)
	var effective *Effective
	var err error
	if userID = strings.TrimSpace(userID); userID == "" {
		effective, err = cache.ForInstance(ctx)
	} else {
		effective, err = cache.ForUser(ctx, userID)
	}
	if err != nil {
		return storage.MailAllowlist{}, fmt.Errorf("work out the allowed mail lists: %w", err)
	}
	return mailAllowlistOf(effective), nil
}

// mailAllowlistOf is the lists effective sets. An empty list is nil, as
// it was when the lists had their own tables.
func mailAllowlistOf(effective *Effective) storage.MailAllowlist {
	list := storage.MailAllowlist{
		Servers: nonEmpty(effective.Strings(MailAllowedServers)),
		Domains: nonEmpty(effective.Strings(MailAllowedDomains)),
	}
	for _, provider := range effective.Strings(MailAllowedProviders) {
		list.Gmail = list.Gmail || provider == providers.ProviderGmail
		list.Outlook = list.Outlook || provider == providers.ProviderOutlook
	}
	return list
}

func nonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

// MailListsLimitAnyone reports whether any policy sets a list that isn't
// empty, so the lists may refuse someone's mailbox.
func MailListsLimitAnyone(ctx context.Context, q Querier) (bool, error) {
	var limited int
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM policy_values WHERE setting_key IN (?, ?, ?) AND value_json <> '[]')`,
		MailAllowedServers.Key, MailAllowedDomains.Key, MailAllowedProviders.Key).Scan(&limited)
	if err != nil {
		return false, fmt.Errorf("check whether the allowed mail lists limit anyone: %w", err)
	}
	return limited == 1, nil
}

// DefaultMailAllowlist is the Default policy's lists, read with q.
func DefaultMailAllowlist(ctx context.Context, q Querier) (storage.MailAllowlist, error) {
	effective, err := ForInstance(ctx, q)
	if err != nil {
		return storage.MailAllowlist{}, err
	}
	return mailAllowlistOf(effective), nil
}

// MailListSetting is the setting a list of the old kind became:
// storage.MailAllowlistServer, MailAllowlistDomain or MailAllowlistProvider.
func MailListSetting(kind string) (*Setting, bool) {
	switch kind {
	case storage.MailAllowlistServer:
		return MailAllowedServers, true
	case storage.MailAllowlistDomain:
		return MailAllowedDomains, true
	case storage.MailAllowlistProvider:
		return MailAllowedProviders, true
	}
	return nil, false
}
