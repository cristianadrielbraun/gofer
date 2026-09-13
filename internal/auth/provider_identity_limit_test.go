package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestProviderIdentityLimitSerializesConcurrentLinks(t *testing.T) {
	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
			insertActiveUser(t, m, "person", false, now)
			configureGoogleOAuthTest(m)
			configureMicrosoftOAuthTest(m)
			for _, id := range []string{"a", "b"} {
				insertGoogleLinkSession(t, m, "person", "session-"+id, "token-"+id, now, now)
			}
			begin := func(token string) (*PreAuthChallenge, error) {
				if provider == "google" {
					r, err := m.BeginGoogleIdentityLink(t.Context(), token)
					if err != nil {
						return nil, err
					}
					return r.Challenge, nil
				}
				r, err := m.BeginMicrosoftIdentityLink(t.Context(), token)
				if err != nil {
					return nil, err
				}
				return r.Challenge, nil
			}
			challenges := map[string]*PreAuthChallenge{}
			for _, id := range []string{"a", "b"} {
				challenge, err := begin("token-" + id)
				if err != nil {
					t.Fatal(err)
				}
				challenges[id] = challenge
			}
			var arrived sync.WaitGroup
			arrived.Add(2)
			release := make(chan struct{})
			client := &http.Client{Transport: oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				arrived.Done()
				<-release
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"access_token":"unused","token_type":"Bearer","id_token":"` + r.Form.Get("code") + `"}`)), Request: r}, nil
			})}
			ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
			m.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(_ context.Context, code string) (*GoogleIDTokenClaims, error) {
				return &GoogleIDTokenClaims{Subject: "subject-" + code, Nonce: challenges[code].Nonce, Email: code + "@private.example", EmailVerified: true}, nil
			})
			m.microsoftIDTokenVerifier = microsoftIDTokenVerifierFunc(func(_ context.Context, code string) (*MicrosoftIDTokenClaims, error) {
				return &MicrosoftIDTokenClaims{Issuer: microsoftTenantIssuer(microsoftTestTenantID), TenantID: microsoftTestTenantID,
					Subject: "subject-" + code, Nonce: challenges[code].Nonce, Email: code + "@private.example"}, nil
			})
			complete := func(id string) error {
				if provider == "google" {
					_, err := m.CompleteGoogleIdentityLink(ctx, challenges[id].Token, "token-"+id, id, "Browser")
					return err
				}
				_, err := m.CompleteMicrosoftIdentityLink(ctx, challenges[id].Token, "token-"+id, id, "Browser")
				return err
			}
			results := make(chan error, 2)
			for _, id := range []string{"a", "b"} {
				go func(id string) { results <- complete(id) }(id)
			}
			arrived.Wait()
			close(release)
			var successes, conflicts int
			for range 2 {
				err := <-results
				if err == nil {
					successes++
				} else if errors.Is(err, ErrFederatedIdentityConflict) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
			}
			var identities, events, successfulEvents, activeChallenges, disclosures int
			if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_identities WHERE user_id='person' AND provider=?`, provider).Scan(&identities); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT COUNT(*), SUM(success) FROM auth_events WHERE event_type=?`, AuthEventIdentityLinked).Scan(&events, &successfulEvents); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&activeChallenges); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE metadata_json LIKE '%private.example%' OR metadata_json LIKE '%subject-%'`).Scan(&disclosures); err != nil {
				t.Fatal(err)
			}
			if identities != 1 || events != 2 || successfulEvents != 1 || activeChallenges != 0 || disclosures != 0 {
				t.Fatalf("identities=%d events=%d successes=%d active=%d disclosures=%d", identities, events, successfulEvents, activeChallenges, disclosures)
			}
		})
	}
}
