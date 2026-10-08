package mail

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const userRawMessageMaxBytes = 64 << 20

// RawMessage retains immutable MIME bytes and the exact storage identity used
// to read them. Validate it after subsequent provider/configuration waits.
type RawMessage struct {
	owner    string
	data     []byte
	snapshot *storage.RawMessageSnapshot
	runtime  *UserIMAP
}

func (s *UserIMAP) rawMessageGuard(ctx context.Context, owner, id string) func() error {
	return func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if err := s.Routing().ValidateUser(ctx, owner); err != nil {
			return err
		}
		state, err := s.Routing().AccountStateForUser(ctx, owner, id)
		if err != nil {
			return err
		}
		if state != storage.AccountActive {
			return storage.ErrAccountRoute
		}
		return nil
	}
}

func (s *UserIMAP) ValidateRawMessage(ctx context.Context, raw *RawMessage) error {
	if raw == nil || raw.runtime != s || raw.snapshot == nil {
		return storage.ErrMessageMutationSuperseded
	}
	info := raw.snapshot.Info()
	return s.Routing().WithAccountForUser(ctx, raw.owner, info.AccountID, func(db *storage.DB) error {
		// Once guarded publication/read completed, these are local immutable
		// bytes. A later Calendar scoped refresh may advance the same central
		// credential revision without invalidating the accepted mail cache.
		return db.ValidateRawMessage(ctx, raw.snapshot, s.rawMessageGuard(ctx, raw.owner, info.AccountID))
	})
}

func (raw *RawMessage) AccountID() string    { return raw.snapshot.Info().AccountID }
func (raw *RawMessage) EmailAddress() string { return raw.snapshot.Info().EmailAddress }
func (raw *RawMessage) Bytes() []byte        { return append([]byte(nil), raw.data...) }

// Bridge only MIME actually read by this runtime to a repository-bound incoming
// claim. The raw retrieval identity remains sealed; callers cannot invent paths
// or adopt another owner's recovery result.
func (s *UserIMAP) RefreshCalendarIncomingRaw(ctx context.Context, candidate *config.UserCalendarIncomingMessageSnapshot, raw *RawMessage) (*config.UserCalendarIncomingMessageSnapshot, error) {
	if candidate == nil || raw == nil || raw.runtime != s || raw.snapshot == nil || raw.owner != candidate.Candidate().UserID || raw.AccountID() != candidate.Candidate().AccountID {
		return nil, storage.ErrCalendarIncomingChanged
	}
	if err := s.ValidateRawMessage(ctx, raw); err != nil {
		return nil, err
	}
	return s.accounts.RefreshCalendarIncomingRaw(ctx, candidate, raw.snapshot)
}

// ReadRawMessage serializes with body/attachment recovery, joins account and
// runtime cancellation, and never holds a store lease across file/network I/O.
// Missing raw files are restored even when the parsed body is already cached.
func (s *UserIMAP) ReadRawMessage(ctx context.Context, owner string, id int64) (*RawMessage, error) {
	release, err := s.blobs.PinUserFiles(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	var initial *storage.RawMessageSnapshot
	if err := s.Routing().WithUser(ctx, owner, func(db *storage.DB) error {
		var err error
		initial, err = db.SnapshotRawMessage(ctx, owner, id)
		return err
	}); err != nil {
		return nil, err
	}
	account := initial.Info().AccountID
	var result *RawMessage
	err = s.operation(ctx, owner, account, id, 2*time.Minute, func(operation context.Context) error {
		var snapshot *storage.RawMessageSnapshot
		call := func(fn func(*storage.DB) error) error {
			return s.Routing().WithAccountForUser(operation, owner, account, fn)
		}
		if err := call(func(db *storage.DB) error {
			var err error
			snapshot, err = db.SnapshotRawMessage(operation, owner, id)
			return err
		}); err != nil {
			return err
		}
		info := snapshot.Info()
		if !initial.SameIdentity(snapshot) || info.AccountID != account {
			return storage.ErrMessageMutationSuperseded
		}
		guard := s.rawMessageGuard(operation, owner, account)
		validate := func() error {
			return call(func(db *storage.DB) error { return db.ValidateRawMessage(operation, snapshot, guard) })
		}
		var raw []byte
		var authorization *mailauth.UserServiceAuthorization
		var credentials *mailauth.UserCredentials
		if info.RawPath != "" {
			file, err := os.Open(info.RawPath)
			if err == nil {
				stat, err := file.Stat()
				if err != nil {
					file.Close()
					return err
				}
				if !stat.Mode().IsRegular() {
					file.Close()
					return errors.New("saved MIME is not a regular file")
				}
				raw, err = io.ReadAll(io.LimitReader(file, userRawMessageMaxBytes+1))
				file.Close()
				if err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if len(raw) == 0 {
			scope, err := s.snapshot(operation, owner, account)
			if err != nil {
				return err
			}
			if scope.config.Provider != info.Provider || scope.config.ProviderAccountID != info.Subject || scope.config.AuthMethod != info.AuthMethod {
				return storage.ErrMessageMutationSuperseded
			}
			if err := validate(); err != nil {
				return err
			}
			if info.Provider == "gmail" || info.Provider == "outlook" {
				s.mu.Lock()
				credentials = s.credentials
				s.mu.Unlock()
				if credentials == nil {
					return errors.New("mail credentials unavailable")
				}
				authorization, err = credentials.MailboxAuthorization(operation, owner, account)
				if err != nil {
					return err
				}
				originalGuard := guard
				guard = func() error {
					if err := originalGuard(); err != nil {
						return err
					}
					return credentials.ValidateMailboxAuthorization(operation, authorization, owner, account)
				}
				read := func() ([]byte, error) {
					if err := validate(); err != nil {
						return nil, err
					}
					if info.Provider == "gmail" {
						return getUserGmailRaw(operation, authorization.Token(), info.RemoteID)
					}
					return getUserOutlookRaw(operation, authorization.Token(), info.RemoteID)
				}
				raw, err = read()
				if providerAPIUnauthorized(err) {
					authorization, err = credentials.RefreshMailboxAuthorization(operation, authorization)
					if err == nil {
						raw, err = read()
					}
				}
			} else if info.Provider == "imap" && info.AuthMethod == "plain" {
				if info.RemoteUID == 0 || info.UIDValidity == 0 || info.FolderRemoteID == "" {
					return errors.New("message has no verified IMAP raw identity")
				}
				client, e := imap.NewContextClient(operation, scope.config, scope.password)
				if e != nil {
					return e
				}
				defer client.Close()
				raw, err = client.FetchBodyWithValidity(operation, info.FolderRemoteID, info.RemoteUID, info.UIDValidity)
			} else {
				return errors.New("raw retrieval unsupported for this account")
			}
			if err != nil {
				return err
			}
			if len(raw) == 0 || len(raw) > userRawMessageMaxBytes {
				return errors.New("raw message has an invalid size")
			}
			if err := validate(); err != nil {
				return err
			}
			candidate, err := s.blobs.NewMessageVersion()
			if err != nil {
				return err
			}
			published := false
			defer func() {
				if !published {
					_ = candidate.DeleteMessage(account, id)
				}
			}()
			path, err := candidate.StoreRaw(operation, account, id, raw)
			if err != nil {
				return err
			}
			if err := call(func(db *storage.DB) error {
				var err error
				snapshot, err = db.PublishRawMessage(operation, snapshot, path, guard)
				return err
			}); err != nil {
				return err
			}
			published = true
		}
		if len(raw) > userRawMessageMaxBytes {
			return errors.New("raw message exceeds the size limit")
		}
		if err := validate(); err != nil {
			return err
		}
		result = &RawMessage{owner: owner, data: raw, snapshot: snapshot, runtime: s}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
