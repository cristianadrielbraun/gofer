package mailauth

import (
	"context"
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// A service token is bound to the exact central grant revision that produced it.
// No repository lease escapes, and checking this binding needs only a central
// read: it is safe inside a guarded local publication transaction.
type UserServiceAuthorization struct {
	repository                                   *UserCredentials
	owner, account, id, provider, subject, token string
	revision                                     int64
	purpose                                      userCredentialPurpose
	grantOnly                                    bool
}

func (s *UserServiceAuthorization) Token() string { return s.token }

// SnapshotCalendarAuthorization captures the current grant without obtaining an
// access token or waiting behind a credential refresh. It can authorize local
// recovery, but Token remains empty until EnsureServiceAuthorization succeeds.
func (s *UserCredentials) SnapshotCalendarAuthorization(ctx context.Context, owner, account, provider string, write bool) (*UserServiceAuthorization, error) {
	if s == nil {
		return nil, ErrMailboxAuthorizationChanged
	}
	expected := ""
	switch provider {
	case providers.ProviderGmail:
		expected = providers.OAuthGoogle
	case providers.ProviderOutlook:
		expected = providers.OAuthMicrosoft
	default:
		return nil, ErrMailboxAuthorizationChanged
	}
	purpose := userCredentialCalendarRead
	if write {
		purpose = userCredentialCalendarWrite
	}
	var snapshot *UserServiceAuthorization
	err := s.operation(ctx, owner, account, false, func(ctx context.Context) error {
		record, err := s.load(ctx, owner, account)
		if err != nil {
			return err
		}
		if record.Provider != expected {
			return ErrMailboxAuthorizationChanged
		}
		allowed := calendarWriteAllowed(record, provider)
		if !write {
			allowed = googleCalendarReadAllowed(record.grantedScopes)
			if provider == providers.ProviderOutlook {
				allowed = ownedGraphHasScopes(record.grantedScopes, microsoftGraphCalendarScope)
			}
		}
		if !allowed {
			return ErrMailboxAuthorizationChanged
		}
		snapshot = s.authorizationFor(record, purpose, "")
		snapshot.grantOnly = true
		return s.ValidateServiceAuthorization(ctx, snapshot)
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Obtain a token for this exact grant, refreshing only when necessary. The
// revision is checked again after waiting for the credential gate.
func (s *UserCredentials) EnsureServiceAuthorization(ctx context.Context, snapshot *UserServiceAuthorization) (*UserServiceAuthorization, error) {
	if snapshot == nil || snapshot.purpose == userCredentialMailbox {
		return nil, ErrMailboxAuthorizationChanged
	}
	if err := s.ValidateServiceAuthorization(ctx, snapshot); err != nil {
		return nil, err
	}
	return s.serviceAuthorizationFrom(ctx, snapshot.owner, snapshot.account, snapshot.purpose, snapshot.provider, false, snapshot)
}

// Force refresh the original service grant, checking its revision again after
// the credential gate wait. Publication still uses the existing central CAS.
func (s *UserCredentials) RefreshServiceAuthorization(ctx context.Context, snapshot *UserServiceAuthorization) (*UserServiceAuthorization, error) {
	if snapshot == nil || snapshot.purpose == userCredentialMailbox {
		return nil, ErrMailboxAuthorizationChanged
	}
	if err := s.ValidateServiceAuthorization(ctx, snapshot); err != nil {
		return nil, err
	}
	return s.serviceAuthorizationFrom(ctx, snapshot.owner, snapshot.account, snapshot.purpose, snapshot.provider, true, snapshot)
}

// A current service grant alone does not prove it belongs to this calendar
// operation. Keep purpose and owner/account matching inside the private facade.
// Validation reads only central state, including when called in a local TX.
func (s *UserCredentials) ValidateCalendarAuthorization(ctx context.Context, snapshot *UserServiceAuthorization, owner, account string, write bool) error {
	purpose := userCredentialCalendarRead
	if write {
		purpose = userCredentialCalendarWrite
	}
	if snapshot == nil || snapshot.owner != owner || snapshot.account != account || snapshot.purpose != purpose {
		return ErrMailboxAuthorizationChanged
	}
	return s.ValidateServiceAuthorization(ctx, snapshot)
}

func (s *UserCredentials) authorizationFor(record userCredentialRecord, purpose userCredentialPurpose, token string) *UserServiceAuthorization {
	return &UserServiceAuthorization{repository: s, owner: record.owner, account: record.AccountID, id: record.ID,
		provider: record.Provider, subject: record.ProviderAccountID, revision: record.revision, purpose: purpose, token: token}
}

func (s *UserAccountCredentials) ServiceAuthorization(ctx context.Context, force bool) (*UserServiceAuthorization, error) {
	if s == nil || s.credentials == nil || s.purpose == userCredentialMailbox {
		return nil, errors.New("service authorization requires a scoped credential facade")
	}
	return s.credentials.serviceAuthorization(ctx, s.owner, s.id, s.purpose, "", force)
}

func (s *UserCredentials) ValidateServiceAuthorization(ctx context.Context, snapshot *UserServiceAuthorization) error {
	if s == nil || snapshot == nil || snapshot.repository != s || snapshot.owner == "" || snapshot.account == "" || snapshot.id == "" || snapshot.revision < 1 || (snapshot.token == "") != snapshot.grantOnly || (snapshot.grantOnly && snapshot.purpose != userCredentialCalendarRead && snapshot.purpose != userCredentialCalendarWrite) || snapshot.purpose < userCredentialMailbox || snapshot.purpose > userCredentialCalendarWrite {
		return ErrMailboxAuthorizationChanged
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return errors.New("mailbox credentials are shutting down")
	}
	var found int
	err := s.routing.System().Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_mailbox_credentials c
 JOIN gofer_account_directory d ON d.account_id=c.account_id JOIN users u ON u.id=c.user_id
 WHERE c.id=? AND c.account_id=? AND c.user_id=? AND c.provider=? AND c.provider_account_id=? AND c.revision=?
 AND d.user_id=c.user_id AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0)`,
		snapshot.id, snapshot.account, snapshot.owner, snapshot.provider, snapshot.subject, snapshot.revision).Scan(&found)
	if err != nil {
		return err
	}
	if found != 1 {
		return ErrMailboxAuthorizationChanged
	}
	return nil
}
