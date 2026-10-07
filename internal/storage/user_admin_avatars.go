package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ProviderAvatarIdentity matches the legacy fallback's complete provider key.
// Matching only a remote contact ID could mix unrelated provider accounts.
type ProviderAvatarIdentity struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	RemoteID string `json:"remote_id"`
}

type AdminProviderAvatar struct {
	Email     string
	URL       string
	UpdatedAt time.Time
}

type AdminProviderAvatarLink struct {
	Email    string
	Identity ProviderAvatarIdentity
}

type AdminProviderAvatarContacts struct {
	Direct []AdminProviderAvatar
	Links  []AdminProviderAvatarLink
}

type AdminProviderAvatarDonor struct {
	Identity  ProviderAvatarIdentity
	URL       string
	UpdatedAt time.Time
}

// ReadAdminProviderAvatarContacts copies the candidates for one admin sender
// page. Local IDs and provider credentials never leave this repository.
func (r *AccountRouting) ReadAdminProviderAvatarContacts(ctx context.Context, actor DiagnosticsActor, owner string, emails []string) (result AdminProviderAvatarContacts, err error) {
	if len(emails) > 200 {
		return result, errors.New("admin avatar page exceeds 200 senders")
	}
	normalized := []string{}
	seen := map[string]bool{}
	for _, email := range emails {
		email = normalizeContactEmail(email)
		if email != "" && !seen[email] {
			normalized = append(normalized, email)
			seen[email] = true
		}
	}
	encoded, _ := json.Marshal(normalized)
	err = r.withDiagnosticsStore(ctx, actor, owner, func(db *DB) error {
		if len(normalized) == 0 {
			return nil
		}
		rows, err := db.Read().QueryContext(ctx, `
 SELECT ci.normalized_value, cp.avatar_url, cp.updated_at,
 COALESCE(a.provider,''), COALESCE(a.provider_account_id,''), COALESCE(c.remote_id,'')
 FROM contact_identities ci
 JOIN contact_profiles cp ON cp.id=ci.profile_id AND cp.user_id=ci.user_id
 LEFT JOIN contact_cards c ON c.profile_id=ci.profile_id AND c.user_id=ci.user_id
   AND c.provider!='' AND c.remote_id!=''
 LEFT JOIN accounts a ON a.id=c.account_id AND a.user_id=c.user_id
   AND a.provider=c.provider AND a.provider_account_id!=''
 WHERE ci.user_id=? AND ci.kind='email' AND cp.is_deleted=0
   AND ci.normalized_value IN (SELECT value FROM json_each(?))
 ORDER BY cp.updated_at DESC, cp.id ASC`, owner, string(encoded))
		if err != nil {
			return err
		}
		defer rows.Close()
		direct := map[string]bool{}
		links := map[AdminProviderAvatarLink]bool{}
		for rows.Next() {
			var avatar AdminProviderAvatar
			var key ProviderAvatarIdentity
			if err := rows.Scan(&avatar.Email, &avatar.URL, &avatar.UpdatedAt, &key.Provider, &key.Account, &key.RemoteID); err != nil {
				return err
			}
			avatar.Email = normalizeContactEmail(avatar.Email)
			avatar.URL = strings.TrimSpace(avatar.URL)
			if avatar.URL != "" && !direct[avatar.Email] {
				result.Direct = append(result.Direct, avatar)
				direct[avatar.Email] = true
			}
			if key.Provider != "" && key.Account != "" && key.RemoteID != "" {
				link := AdminProviderAvatarLink{Email: avatar.Email, Identity: key}
				if !links[link] {
					result.Links = append(result.Links, link)
					links[link] = true
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return AdminProviderAvatarContacts{}, err
	}
	return result, nil
}

// ReadAdminProviderAvatarDonors checks exact provider identities in one existing
// owner store. Its result is bounded by requested keys, regardless of how many
// historical cards match each key. No local store stays leased across owners.
func (r *AccountRouting) ReadAdminProviderAvatarDonors(ctx context.Context, actor DiagnosticsActor, owner string, keys []ProviderAvatarIdentity) (result []AdminProviderAvatarDonor, err error) {
	encoded, _ := json.Marshal(keys)
	err = r.withDiagnosticsStore(ctx, actor, owner, func(db *DB) error {
		if len(keys) == 0 {
			return nil
		}
		rows, err := db.Read().QueryContext(ctx, `
 SELECT c.provider,a.provider_account_id,c.remote_id,cp.avatar_url,cp.updated_at
 FROM json_each(?) wanted
 JOIN contact_cards c ON c.provider=json_extract(wanted.value,'$.provider')
   AND c.remote_id=json_extract(wanted.value,'$.remote_id') AND c.user_id=?
 JOIN accounts a ON a.id=c.account_id AND a.user_id=c.user_id
   AND a.provider=c.provider AND a.provider_account_id=json_extract(wanted.value,'$.account')
 JOIN contact_profiles cp ON cp.id=c.profile_id AND cp.user_id=c.user_id
 WHERE cp.is_deleted=0 AND cp.avatar_url!=''
 ORDER BY cp.updated_at DESC, cp.id ASC`, string(encoded), owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := map[ProviderAvatarIdentity]bool{}
		for rows.Next() {
			var donor AdminProviderAvatarDonor
			if err := rows.Scan(&donor.Identity.Provider, &donor.Identity.Account, &donor.Identity.RemoteID, &donor.URL, &donor.UpdatedAt); err != nil {
				return err
			}
			donor.URL = strings.TrimSpace(donor.URL)
			if donor.URL != "" && !seen[donor.Identity] {
				result = append(result, donor)
				seen[donor.Identity] = true
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
