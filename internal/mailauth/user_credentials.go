package mailauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/retry"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

var ErrMailboxAuthorizationChanged = errors.New("mailbox authorization changed during refresh")

// UserCredentials keeps encrypted mailbox grants in the system DB, referencing
// the central account directory instead of shared mailbox rows. It is opt-in;
// the legacy Service and production startup retain their current layout.
type UserCredentials struct {
	routing    *storage.AccountRouting
	codec      *Service
	ctx        context.Context
	mu         sync.Mutex
	gates      map[string]*userCredentialGate
	operations sync.WaitGroup
	closing    bool
}
type userCredentialGate struct {
	token chan struct{}
	refs  int
}
type userCredentialIdentity struct{ owner, account, provider, subject string }
type userCredentialRecord struct {
	oauthTokenRecord
	owner         string
	revision      int64
	grantedScopes string
}

func NewUserCredentials(ctx context.Context, cfg *Config, routing *storage.AccountRouting, key []byte) (*UserCredentials, error) {
	if ctx == nil || routing == nil {
		return nil, errors.New("user credentials require lifecycle context and account routing")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	codec := New(cfg, routing.System(), key)
	if _, err := codec.mailboxCredentialAEAD(); err != nil {
		return nil, err
	}
	tx, err := routing.System().Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, userMailboxCredentialSchema+userMailboxCredentialGuards)
	if err != nil {
		return nil, fmt.Errorf("initialize user mailbox credentials: %w", err)
	}
	// Older opt-in stores have only the cached token scope. Preserve only that
	// recorded evidence on reads; missing historical grants require reconnect.
	columns, err := tx.QueryContext(ctx, `PRAGMA table_info(gofer_mailbox_credentials)`)
	if err != nil {
		return nil, err
	}
	hasGrants := false
	for columns.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			columns.Close()
			return nil, err
		}
		if name == "granted_scopes" {
			hasGrants = true
		}
	}
	scanErr := columns.Err()
	columns.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if !hasGrants {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE gofer_mailbox_credentials ADD COLUMN granted_scopes TEXT NOT NULL DEFAULT ''`); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &UserCredentials{routing: routing, codec: codec, ctx: ctx, gates: make(map[string]*userCredentialGate)}, nil
}

func (s *UserCredentials) Routing() *storage.AccountRouting { return s.routing }

// Wait joins synchronous operations after the application context is canceled.
func (s *UserCredentials) Wait() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.operations.Wait()
}

func (s *UserCredentials) operation(ctx context.Context, owner, id string, serialize bool, fn func(context.Context) error) error {
	if err := s.routing.ValidateUser(ctx, owner); err != nil {
		return err
	}
	state, err := s.routing.AccountStateForUser(ctx, owner, id)
	if err != nil {
		return err
	}
	if state != storage.AccountActive {
		return storage.ErrAccountRoute
	}
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	s.mu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.mu.Unlock()
		return errors.New("mailbox credentials are shutting down")
	}
	s.operations.Add(1)
	var gate *userCredentialGate
	if serialize {
		gate = s.gates[id]
		if gate == nil {
			gate = &userCredentialGate{token: make(chan struct{}, 1)}
			gate.token <- struct{}{}
			s.gates[id] = gate
		}
		gate.refs++
	}
	s.mu.Unlock()
	defer s.operations.Done()
	if gate != nil {
		defer func() {
			s.mu.Lock()
			gate.refs--
			if gate.refs == 0 {
				delete(s.gates, id)
			}
			s.mu.Unlock()
		}()
		select {
		case <-work.Done():
			return work.Err()
		case <-gate.token:
		}
		defer func() { gate.token <- struct{}{} }()
	}
	return s.routing.WithAccountActivityForUser(work, owner, id, func() error { return fn(work) })
}

// Writer ownership is needed only while publishing credentials. It prevents
// account identity edits from overtaking central publication. Neither this DB
// lease nor its writer connection spans a token endpoint request.
func (s *UserCredentials) withIdentity(ctx context.Context, owner, id string, writer bool, fn func(userCredentialIdentity) error) error {
	return s.routing.WithAccountForUser(ctx, owner, id, func(db *storage.DB) error {
		identity := userCredentialIdentity{owner: owner, account: id}
		var method string
		query := db.Read().QueryRowContext
		if writer {
			tx, err := db.Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			query = tx.QueryRowContext
		}
		if err := query(ctx, `SELECT provider,provider_account_id,auth_method FROM accounts WHERE id=? AND user_id=? AND COALESCE(is_deleting,0)=0`, id, owner).Scan(&identity.provider, &identity.subject, &method); err != nil {
			return err
		}
		if method != "oauth2" || identity.subject == "" {
			return errors.New("mailbox OAuth identity is unavailable")
		}
		provider, err := oauthProviderForAccountProvider(identity.provider)
		if err != nil {
			return err
		}
		identity.provider = provider
		return fn(identity)
	})
}

func (s *UserCredentials) UpsertForUser(ctx context.Context, owner, id, provider, subject, access, refresh, tokenType string, expires *time.Time, scopes string) error {
	provider, subject = strings.TrimSpace(provider), strings.TrimSpace(subject)
	if strings.TrimSpace(access) == "" || subject == "" {
		return errors.New("mailbox access token and provider identity are required")
	}
	if tokenType == "" {
		tokenType = "Bearer"
	}
	// A complete replacement grant may overtake an old refresh. When a provider
	// omits refresh_token, wait for any rotation before retaining its latest value.
	return s.operation(ctx, owner, id, refresh == "", func(ctx context.Context) error {
		return s.withIdentity(ctx, owner, id, true, func(identity userCredentialIdentity) error {
			if provider != identity.provider || subject != identity.subject {
				return errors.New("mailbox credential does not match its account")
			}
			tx, err := s.routing.System().Write().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var credential oauthCredentialContext
			var oldRefresh []byte
			err = tx.QueryRowContext(ctx, `SELECT id,account_id,provider,provider_account_id,refresh_token_ciphertext FROM gofer_mailbox_credentials WHERE account_id=? AND user_id=?`, id, owner).Scan(&credential.ID, &credential.AccountID, &credential.Provider, &credential.ProviderAccountID, &oldRefresh)
			existing := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if !existing {
				credential = oauthCredentialContext{ID: uuid.NewString(), AccountID: id}
			}
			if refresh == "" && len(oldRefresh) > 0 {
				if credential.Provider != provider || credential.ProviderAccountID != subject {
					return errors.New("new mailbox identity requires a new refresh token")
				}
				if _, err = s.codec.decryptOAuthToken(credential, "refresh", oldRefresh, mailboxCredentialKeyVersion); err != nil {
					return err
				}
			}
			credential.Provider, credential.ProviderAccountID = provider, subject
			accessBytes, err := s.codec.encryptOAuthToken(credential, "access", access)
			if err != nil {
				return err
			}
			refreshBytes := oldRefresh
			if refresh != "" {
				refreshBytes, err = s.codec.encryptOAuthToken(credential, "refresh", refresh)
				if err != nil {
					return err
				}
			}
			if existing {
				_, err = tx.ExecContext(ctx, `UPDATE gofer_mailbox_credentials SET provider=?,provider_account_id=?,access_token_ciphertext=?,refresh_token_ciphertext=?,token_type=?,expires_at=?,scopes=?,granted_scopes=?,revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE account_id=? AND user_id=?`, provider, subject, accessBytes, refreshBytes, tokenType, expires, scopes, scopes, id, owner)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO gofer_mailbox_credentials(id,account_id,user_id,provider,provider_account_id,access_token_ciphertext,refresh_token_ciphertext,key_version,token_type,expires_at,scopes,granted_scopes) VALUES(?,?,?,?,?,?,?,2,?,?,?,?)`, credential.ID, id, owner, provider, subject, accessBytes, refreshBytes, tokenType, expires, scopes, scopes)
			}
			if err != nil {
				return fmt.Errorf("save mailbox authorization: %w", err)
			}
			return tx.Commit()
		})
	})
}

func (s *UserCredentials) load(ctx context.Context, owner, id string) (record userCredentialRecord, err error) {
	err = s.withIdentity(ctx, owner, id, false, func(identity userCredentialIdentity) error {
		var access, refresh []byte
		var version int
		err := s.routing.System().Read().QueryRowContext(ctx, `SELECT c.id,c.account_id,c.provider,c.provider_account_id,c.access_token_ciphertext,c.refresh_token_ciphertext,c.key_version,c.token_type,c.expires_at,c.scopes,COALESCE(NULLIF(c.granted_scopes,''),c.scopes),c.revision
		 FROM gofer_mailbox_credentials c JOIN gofer_account_directory d ON d.account_id=c.account_id JOIN users u ON u.id=d.user_id
		 WHERE c.account_id=? AND c.user_id=? AND c.provider=? AND c.provider_account_id=? AND d.user_id=c.user_id AND d.state='active' AND u.status='active' AND u.deletion_pending=0 AND u.user_type='webmail' AND u.is_admin=0`, id, owner, identity.provider, identity.subject).Scan(&record.ID, &record.AccountID, &record.Provider, &record.ProviderAccountID, &access, &refresh, &version, &record.TokenType, &record.ExpiresAt, &record.Scopes, &record.grantedScopes, &record.revision)
		if err != nil {
			return fmt.Errorf("load mailbox authorization: %w", err)
		}
		record.owner = owner
		record.AccessToken, err = s.codec.decryptOAuthToken(record.oauthCredentialContext, "access", access, version)
		if err != nil {
			return err
		}
		record.RefreshToken, err = s.codec.decryptOAuthToken(record.oauthCredentialContext, "refresh", refresh, version)
		return err
	})
	if err != nil {
		return userCredentialRecord{}, err
	}
	return record, nil
}

func (s *UserCredentials) publish(ctx context.Context, record userCredentialRecord, token *oauth2.Token) error {
	return s.withIdentity(ctx, record.owner, record.AccountID, true, func(identity userCredentialIdentity) error {
		if identity.provider != record.Provider || identity.subject != record.ProviderAccountID {
			return ErrMailboxAuthorizationChanged
		}
		access, err := s.codec.encryptOAuthToken(record.oauthCredentialContext, "access", token.AccessToken)
		if err != nil {
			return err
		}
		var refresh []byte
		if token.RefreshToken != "" {
			refresh, err = s.codec.encryptOAuthToken(record.oauthCredentialContext, "refresh", token.RefreshToken)
			if err != nil {
				return err
			}
		}
		var expiry *time.Time
		if !token.Expiry.IsZero() {
			expiry = &token.Expiry
		}
		scopes, _ := token.Extra("scope").(string)
		granted := record.grantedScopes
		if strings.TrimSpace(scopes) != "" {
			if record.Provider == providers.OAuthGoogle {
				granted = scopes
			} else {
				granted = mergeKnownScopes(granted, scopes)
			}
		}
		if strings.TrimSpace(scopes) == "" && record.Provider == providers.OAuthGoogle {
			scopes = record.Scopes
		}
		// A scoped Graph refresh can change permissions/resources. An omitted
		// response scope cannot relabel its new token with the old cache scope.
		tokenType := token.TokenType
		if tokenType == "" {
			tokenType = "Bearer"
		}
		result, err := s.routing.System().Write().ExecContext(ctx, `UPDATE gofer_mailbox_credentials SET access_token_ciphertext=?,refresh_token_ciphertext=CASE WHEN ? IS NULL THEN refresh_token_ciphertext ELSE ? END,token_type=?,expires_at=?,scopes=?,granted_scopes=?,revision=revision+1,updated_at=CURRENT_TIMESTAMP
		 WHERE id=? AND account_id=? AND user_id=? AND provider=? AND provider_account_id=? AND revision=?`, access, refresh, refresh, tokenType, expiry, scopes, granted, record.ID, record.AccountID, record.owner, record.Provider, record.ProviderAccountID, record.revision)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrMailboxAuthorizationChanged
		}
		return nil
	})
}

func (s *UserCredentials) token(ctx context.Context, owner, id string, force, graphOnly bool) (string, error) {
	authorization, err := s.mailboxAuthorizationFrom(ctx, owner, id, force, graphOnly, nil)
	if err != nil {
		return "", err
	}
	return authorization.Token(), nil
}
func (s *UserCredentials) mailboxAuthorizationFrom(ctx context.Context, owner, id string, force, graphOnly bool, expected *UserServiceAuthorization) (authorization *UserServiceAuthorization, err error) {
	err = s.operation(ctx, owner, id, true, func(ctx context.Context) error {
		record, err := s.load(ctx, owner, id)
		if err != nil {
			return err
		}
		if expected != nil && (expected.repository != s || expected.owner != owner || expected.account != id || expected.purpose != userCredentialMailbox || expected.id != record.ID || expected.provider != record.Provider || expected.subject != record.ProviderAccountID || expected.revision != record.revision) {
			return ErrMailboxAuthorizationChanged
		}
		if graphOnly && record.Provider != providers.OAuthMicrosoft {
			return errors.New("mailbox is not an Outlook account")
		}
		var scopes []string
		if record.Provider == providers.OAuthMicrosoft {
			scopes = microsoftGraphMailScopes()
		}
		if !force && record.AccessToken != "" && record.ExpiresAt.Valid && record.ExpiresAt.Time.After(time.Now().Add(5*time.Minute)) && ownedGraphHasScopes(record.Scopes, scopes...) {
			authorization = s.authorizationFor(record, userCredentialMailbox, record.AccessToken)
			return nil
		}
		if record.RefreshToken == "" {
			return errors.New("mailbox authorization has no refresh token")
		}
		cfg, err := s.codec.oauthConfigForProvider(record.Provider)
		if err != nil {
			return err
		}
		var token *oauth2.Token
		if record.Provider == providers.OAuthMicrosoft {
			token, err = refreshTokenForScopes(ctx, cfg, record.RefreshToken, scopes)
		} else {
			token, err = cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: record.RefreshToken, TokenType: "Bearer"}).Token()
			var response *oauth2.RetrieveError
			if errors.As(err, &response) && response.Response != nil {
				retryAt, _ := retry.ParseRetryAfter(response.Response.Header.Get("Retry-After"), time.Now().UTC())
				err = &OAuthTokenError{Status: response.Response.StatusCode, Code: response.ErrorCode, RetryAt: retryAt}
			}
		}
		if err != nil {
			return fmt.Errorf("refresh mailbox authorization: %w", err)
		}
		if token == nil || strings.TrimSpace(token.AccessToken) == "" {
			return errors.New("provider returned an empty mailbox access token")
		}
		if err := s.publish(ctx, record, token); err != nil {
			return err
		}
		record.revision++
		authorization = s.authorizationFor(record, userCredentialMailbox, token.AccessToken)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return authorization, nil
}

func (s *UserCredentials) GetOAuthTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.token(ctx, owner, id, false, false)
}
func (s *UserCredentials) RefreshOAuthTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.token(ctx, owner, id, true, false)
}
func (s *UserCredentials) GetMicrosoftGraphMailTokenForUser(ctx context.Context, owner, id string) (string, error) {
	return s.token(ctx, owner, id, false, true)
}

// Account binds the legacy provider interfaces to one authorized owner/account.
// Each call validates current routing and never consults shared mailbox rows.
func (s *UserCredentials) Account(owner, id string) *UserAccountCredentials {
	return &UserAccountCredentials{credentials: s, owner: owner, id: id}
}

type UserAccountCredentials struct {
	credentials *UserCredentials
	owner, id   string
	purpose     userCredentialPurpose
}

func (s *UserAccountCredentials) GetOAuthTokenForAccount(ctx context.Context, id string) (string, error) {
	if id != s.id {
		return "", storage.ErrAccountRoute
	}
	if s.purpose != userCredentialMailbox {
		return s.credentials.serviceToken(ctx, s.owner, id, s.purpose, "", false)
	}
	return s.credentials.GetOAuthTokenForUser(ctx, s.owner, id)
}
func (s *UserAccountCredentials) RefreshOAuthTokenForAccount(ctx context.Context, id string) (string, error) {
	if id != s.id {
		return "", storage.ErrAccountRoute
	}
	if s.purpose != userCredentialMailbox {
		return s.credentials.serviceToken(ctx, s.owner, id, s.purpose, "", true)
	}
	return s.credentials.RefreshOAuthTokenForUser(ctx, s.owner, id)
}
func (s *UserAccountCredentials) GetMicrosoftGraphMailTokenForAccount(ctx context.Context, id string) (string, error) {
	if id != s.id {
		return "", storage.ErrAccountRoute
	}
	return s.credentials.GetMicrosoftGraphMailTokenForUser(ctx, s.owner, id)
}

// CleanupAccount is an internal lifecycle hook, valid only after the directory
// blocks new activity. It also works for disabled/deleting owners and tombstones.
func (s *UserCredentials) CleanupAccount(ctx context.Context, id string) error {
	var state storage.AccountRouteState
	if err := s.routing.System().Read().QueryRowContext(ctx, `SELECT state FROM gofer_account_directory WHERE account_id=?`, id).Scan(&state); err != nil {
		return err
	}
	if state != storage.AccountDeleting && state != storage.AccountDeleted {
		return storage.ErrAccountRoute
	}
	_, err := s.routing.System().Write().ExecContext(ctx, `DELETE FROM gofer_mailbox_credentials WHERE account_id=? AND EXISTS(SELECT 1 FROM gofer_account_directory WHERE account_id=? AND state IN ('deleting','deleted'))`, id, id)
	return err
}
