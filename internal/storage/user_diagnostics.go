package storage

import (
	"context"
	"errors"
	"os"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrUserDiagnosticsAccess = errors.New("user diagnostics access denied")

// DiagnosticsActor binds a central administrator to the authentication version
// admitted by middleware. Re-enabling a disabled identity does not restore an
// old request's authority after it has waited for cache capacity.
type DiagnosticsActor struct {
	ID          string
	AuthVersion int64
}

type UserDiagnosticsKind string

const (
	UserDiagnosticsContacts   UserDiagnosticsKind = "contacts"
	UserDiagnosticsLabels     UserDiagnosticsKind = "labels"
	UserDiagnosticsMail       UserDiagnosticsKind = "mail"
	UserDiagnosticsAvatars    UserDiagnosticsKind = "avatars"
	UserDiagnosticsTransports UserDiagnosticsKind = "transports"
)

type DiagnosticTransportAccount struct {
	ID, Email, Provider                          string
	IMAPHost, IMAPTLSMode, SMTPHost, SMTPTLSMode string
	IMAPPort, SMTPPort                           int
	JMAPSessionURL                               string
	// CardDAVURL and CalDAVURL are the account's contact and calendar
	// servers, when it syncs them over DAV.
	CardDAVURL, CalDAVURL string
}

type UserDiagnostics struct {
	Contacts     models.ContactAdminStatus
	Labels       models.LabelAdminStatus
	Mail         models.MailOperationsAdminStatus
	Idle         []ConfiguredIdleFolder
	AvatarEmails []string
	Transports   []DiagnosticTransportAccount
}

func (r *AccountRouting) ValidateDiagnosticsAdministrator(ctx context.Context, actor DiagnosticsActor) error {
	var allowed bool
	err := r.System().Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users
 WHERE id=? AND auth_version=? AND status='active' AND user_type='management'
 AND is_admin=1 AND deletion_pending=0)`, actor.ID, actor.AuthVersion).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrUserDiagnosticsAccess
	}
	return nil
}

// ValidateDiagnosticsAccess permits retained webmail data, including disabled
// owners, only for a current central management administrator. Pending deletion
// is excluded: local files may already be draining or removed.
func (r *AccountRouting) ValidateDiagnosticsAccess(ctx context.Context, actor DiagnosticsActor, owner string) error {
	var allowed bool
	err := r.System().Read().QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM users WHERE id=? AND auth_version=? AND status='active'
  AND user_type='management' AND is_admin=1 AND deletion_pending=0)
 AND EXISTS(SELECT 1 FROM users WHERE id=? AND user_type='webmail'
  AND is_admin=0 AND deletion_pending=0)`, actor.ID, actor.AuthVersion, owner).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrUserDiagnosticsAccess
	}
	return nil
}

// ReadUserDiagnostics copies one owner's diagnostics from an existing store.
// It neither creates unused owners' files nor exposes a database capability to
// administration handlers. A missing file for a known mailbox owner is an error.
func (r *AccountRouting) ReadUserDiagnostics(ctx context.Context, actor DiagnosticsActor, owner string, kind UserDiagnosticsKind) (result UserDiagnostics, err error) {
	if kind != UserDiagnosticsContacts && kind != UserDiagnosticsLabels && kind != UserDiagnosticsMail && kind != UserDiagnosticsAvatars && kind != UserDiagnosticsTransports {
		return result, errors.New("unknown user diagnostics kind")
	}
	err = r.withDiagnosticsStore(ctx, actor, owner, func(db *DB) error {
		var err error
		switch kind {
		case UserDiagnosticsContacts:
			result.Contacts, err = db.GetContactAdminStatus(ctx, owner)
		case UserDiagnosticsLabels:
			result.Labels, err = db.GetLabelAdminStatus(ctx, owner)
		case UserDiagnosticsMail:
			result.Mail, err = db.ListMailOperationsAdminStatusForUser(ctx, owner)
			if err == nil {
				result.Idle, err = db.ListConfiguredIdleFoldersForUser(ctx, owner)
			}
		case UserDiagnosticsAvatars:
			result.AvatarEmails, err = db.listAdminAvatarEmails(ctx, owner)
		case UserDiagnosticsTransports:
			result.Transports, err = db.listDiagnosticTransports(ctx, owner)
		}
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return UserDiagnostics{}, err
	}
	return result, nil
}

func (db *DB) listDiagnosticTransports(ctx context.Context, owner string) ([]DiagnosticTransportAccount, error) {
	return db.listTransportAccounts(ctx, `AND a.user_id=?`, owner)
}

// listTransportAccounts reads how accounts connect, for matching them with
// mail security exceptions.
func (db *DB) listTransportAccounts(ctx context.Context, filter string, args ...any) ([]DiagnosticTransportAccount, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT a.id,a.email_address,a.provider,
 lower(trim(COALESCE(a.imap_host,''))),COALESCE(a.imap_port,0),lower(trim(COALESCE(a.imap_tls_mode,''))),
 lower(trim(COALESCE(a.smtp_host,''))),COALESCE(a.smtp_port,0),lower(trim(COALESCE(a.smtp_tls_mode,''))),
 COALESCE(a.jmap_session_url,''),
 COALESCE((SELECT c.base_url FROM account_contact_sync_configs c WHERE c.account_id=a.id AND c.provider='carddav' AND c.enabled=1),''),
 COALESCE((SELECT d.base_url FROM account_caldav_configs d WHERE d.account_id=a.id),'')
 FROM accounts a WHERE COALESCE(a.is_deleting,0)=0 `+filter+`
 ORDER BY a.email_address COLLATE NOCASE`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DiagnosticTransportAccount
	for rows.Next() {
		var account DiagnosticTransportAccount
		if err := rows.Scan(&account.ID, &account.Email, &account.Provider, &account.IMAPHost, &account.IMAPPort, &account.IMAPTLSMode, &account.SMTPHost, &account.SMTPPort, &account.SMTPTLSMode, &account.JMAPSessionURL, &account.CardDAVURL, &account.CalDAVURL); err != nil {
			return nil, err
		}
		result = append(result, account)
	}
	return result, rows.Err()
}

// withDiagnosticsStore is private: callers receive copied typed results, never a
// database capability. Every diagnostic path shares existing-file admission and
// administrator/owner checks around waits and local reads.
func (r *AccountRouting) withDiagnosticsStore(ctx context.Context, actor DiagnosticsActor, owner string, read func(*DB) error) error {
	return r.withRetainedExistingStore(ctx, owner, func() error { return r.ValidateDiagnosticsAccess(ctx, actor, owner) }, read)
}

// The caller supplies its authority policy. Shared existing-file admission is
// used by administrator diagnostics and trusted startup repair, never by a
// private request to act as a disabled owner.
func (r *AccountRouting) withRetainedExistingStore(ctx context.Context, owner string, guard func() error, read func(*DB) error) error {
	if err := guard(); err != nil {
		return err
	}
	if _, statErr := os.Lstat(r.stores.userPath(owner)); errors.Is(statErr, os.ErrNotExist) {
		var known bool
		if err := r.System().Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE user_id=?) OR EXISTS(SELECT 1 FROM gofer_user_store_directory WHERE user_id=?)`, owner, owner).Scan(&known); err != nil {
			return err
		}
		if known {
			return errors.Join(ErrAccountRoute, statErr)
		}
		return guard()
	} else if statErr != nil {
		return statErr
	}
	return r.withUserStore(ctx, owner, true, func(db *DB) error {
		if err := guard(); err != nil {
			return err
		}
		if err := read(db); err != nil {
			return err
		}
		return guard()
	})
}
