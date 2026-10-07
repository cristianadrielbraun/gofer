package mailauth

import "context"

// MailboxAuthorization uses the existing full mailbox token/refresh contract.
// Its private purpose prevents a contacts/calendar grant from authorizing MIME.
func (s *UserCredentials) MailboxAuthorization(ctx context.Context, owner, id string) (*UserServiceAuthorization, error) {
	return s.mailboxAuthorizationFrom(ctx, owner, id, false, false, nil)
}
func (s *UserCredentials) ValidateMailboxAuthorization(ctx context.Context, a *UserServiceAuthorization, owner, id string) error {
	if a == nil || a.owner != owner || a.account != id || a.purpose != userCredentialMailbox {
		return ErrMailboxAuthorizationChanged
	}
	return s.ValidateServiceAuthorization(ctx, a)
}
func (s *UserCredentials) RefreshMailboxAuthorization(ctx context.Context, a *UserServiceAuthorization) (*UserServiceAuthorization, error) {
	if a == nil {
		return nil, ErrMailboxAuthorizationChanged
	}
	if err := s.ValidateMailboxAuthorization(ctx, a, a.owner, a.account); err != nil {
		return nil, err
	}
	return s.mailboxAuthorizationFrom(ctx, a.owner, a.account, true, false, a)
}
