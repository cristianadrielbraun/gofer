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
	ID, Email                                    string
	IMAPHost, IMAPTLSMode, SMTPHost, SMTPTLSMode string
	IMAPPort, SMTPPort                           int
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
	rows, err := db.Read().QueryContext(ctx, `SELECT id,email_address,
 lower(trim(COALESCE(imap_host,''))),COALESCE(imap_port,0),lower(trim(COALESCE(imap_tls_mode,''))),
 lower(trim(COALESCE(smtp_host,''))),COALESCE(smtp_port,0),lower(trim(COALESCE(smtp_tls_mode,'')))
 FROM accounts WHERE user_id=? AND COALESCE(is_deleting,0)=0`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DiagnosticTransportAccount
	for rows.Next() {
		var account DiagnosticTransportAccount
		if err := rows.Scan(&account.ID, &account.Email, &account.IMAPHost, &account.IMAPPort, &account.IMAPTLSMode, &account.SMTPHost, &account.SMTPPort, &account.SMTPTLSMode); err != nil {
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
	if err := r.ValidateDiagnosticsAccess(ctx, actor, owner); err != nil {
		return err
	}
	if _, statErr := os.Lstat(r.stores.userPath(owner)); errors.Is(statErr, os.ErrNotExist) {
		var known bool
		if err := r.System().Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gofer_account_directory WHERE user_id=?)`, owner).Scan(&known); err != nil {
			return err
		}
		if known {
			return errors.Join(ErrAccountRoute, statErr)
		}
		return r.ValidateDiagnosticsAccess(ctx, actor, owner)
	} else if statErr != nil {
		return statErr
	}
	return r.withUserStore(ctx, owner, true, func(db *DB) error {
		if err := r.ValidateDiagnosticsAccess(ctx, actor, owner); err != nil {
			return err
		}
		if err := read(db); err != nil {
			return err
		}
		return r.ValidateDiagnosticsAccess(ctx, actor, owner)
	})
}
