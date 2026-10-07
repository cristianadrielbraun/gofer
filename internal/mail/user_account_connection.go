package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var ErrUserMailConnectionChanged = errors.New("mailbox configuration changed during connection testing")

// TestAccount shares the receive gate and global session bound. Only copied
// credentials cross protocol waits; edits/deletion/shutdown cancel its activity.
func (s *UserIMAP) TestAccount(ctx context.Context, owner, id string) ([]models.ConnectionTestResult, error) {
	var results []models.ConnectionTestResult
	err := s.operation(ctx, owner, id, 0, 2*time.Minute, func(ctx context.Context) error {
		scope, err := s.snapshot(ctx, owner, id)
		if err != nil {
			return err
		}
		smtpPassword := func() (string, error) {
			password := scope.password
			if scope.config.SmtpUsername != "" {
				err := s.accounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, _ *storage.DB) error {
					var err error
					password, err = local.DecryptSmtpPassword(ctx, id)
					return err
				})
				return password, err
			}
			return password, nil
		}
		password, err := smtpPassword()
		if err != nil {
			return err
		}
		validate := func() error {
			current, err := s.snapshot(ctx, owner, id)
			if err != nil {
				return err
			}
			if *current.config != *scope.config || current.password != scope.password {
				return ErrUserMailConnectionChanged
			}
			latestPassword, err := smtpPassword()
			if err != nil {
				return err
			}
			if latestPassword != password {
				return ErrUserMailConnectionChanged
			}
			return ctx.Err()
		}
		probe := func(service, message, success string, test func() error) {
			result := models.ConnectionTestResult{Service: service, Message: message}
			err := RetryConnectionTest(ctx, 250*time.Millisecond, func() error {
				if err := validate(); err != nil {
					return err
				}
				return test()
			})
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Success = true
				result.Message = success
			}
			results = append(results, result)
		}
		if scope.tokens == nil {
			probe("imap", fmt.Sprintf("%s:%d (%s)", scope.config.IMAPHost, scope.config.IMAPPort, scope.config.IMAPTLSMode), "Connection successful", func() error { return imap.TestConnection(ctx, scope.config, scope.password) })
			probe("smtp", fmt.Sprintf("%s:%d (%s)", scope.config.SMTPHost, scope.config.SMTPPort, scope.config.SMTPTLSMode), "Connection successful", func() error { return smtpclient.TestConnection(ctx, scope.config, password) })
		} else {
			o := NewSyncOrchestrator(nil, nil, nil, scope.tokens)
			o.imapScope = scope
			service, message, success := "gmail", "Gmail API mail access", "Gmail API mail access successful"
			endpoint := gmailAPIBaseURL + "/users/me/profile"
			if scope.config.Provider == "outlook" {
				service, message, success = "graph", "Microsoft Graph mail", "Microsoft Graph mail access successful"
				endpoint = outlookGraphBaseURL + "/me/mailFolders?$top=1&$select=id,displayName"
			}
			probe(service, message, success, func() error {
				var token string
				var err error
				if scope.config.Provider == "outlook" {
					token, err = scope.tokens.(graphMailTokenProvider).GetMicrosoftGraphMailTokenForAccount(ctx, id)
				} else {
					token, err = scope.tokens.GetOAuthTokenForAccount(ctx, id)
				}
				if err != nil {
					return o.recordOutlookRetry(ctx, err)
				}
				return o.outlookRequest(ctx, token, func(access string) error {
					if err := validate(); err != nil {
						return err
					}
					headers := map[string]string(nil)
					if scope.config.Provider == "outlook" {
						headers = outlookImmutableIDHeaders()
					}
					var response map[string]any
					_, err := providerMailRaw(ctx, http.MethodGet, endpoint, access, "", headers, nil, &response)
					return err
				})
			})
		}
		return validate()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}
