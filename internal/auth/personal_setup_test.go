package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func personalSetupManager(t *testing.T) (*Manager, PersonalSetupInput) {
	t.Helper()
	m := newDeterministicManager(t, &fixedClock{now: time.Now().UTC()}, secureTokenGenerator{})
	m.config.Mode = ModePersonal
	provision, err := m.EnsureSetupToken(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := m.BeginSetup(t.Context(), BeginSetupOptions{Token: provision.Token, Origin: m.config.BaseURL})
	if err != nil {
		t.Fatal(err)
	}
	return m, PersonalSetupInput{Token: challenge.Token, Origin: m.config.BaseURL, Name: "Personal User", Username: "person", Password: "correct horse battery staple 874!", PasswordConfirmation: "correct horse battery staple 874!"}
}

func TestPersonalSetupPreservesLocalProfileAndCompletesOnce(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing local profile"}[existing], func(t *testing.T) {
			m, input := personalSetupManager(t)
			if existing {
				insertActiveUser(t, m, "default", false, m.clock.Now())
				if _, err := m.db.Write().Exec(`INSERT INTO accounts (id,user_id,email_address) VALUES ('mail-a','default','a@example.test'),('mail-b','default','b@example.test')`); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.ValidateRuntimeMode(t.Context()); err != nil {
				t.Fatal(err)
			}
			result, err := m.CompletePersonalSetup(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Session.UserID != "default" || result.Session.AssuranceLevel != AssuranceLevelSingleFactor {
				t.Fatalf("wrong session %#v", result.Session)
			}
			if err := m.ValidateRuntimeMode(t.Context()); err != nil {
				t.Fatal(err)
			}
			access, err := m.GetSecuritySettingsAccess(t.Context(), result.Session.Token)
			if err != nil || !access.StepUpFresh || access.HasTOTP || access.HasPasskey {
				t.Fatalf("security access %#v %v", access, err)
			}
			var users, admins, mailboxes, events int
			if err := m.db.Read().QueryRow(`SELECT COUNT(*),SUM(is_admin) FROM users`).Scan(&users, &admins); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM accounts WHERE user_id='default'`).Scan(&mailboxes); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type='setup_completed'`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			wantMail := 0
			if existing {
				wantMail = 2
			}
			if users != 1 || admins != 0 || mailboxes != wantMail || events != 1 {
				t.Fatalf("users %d admins %d mailboxes %d events %d", users, admins, mailboxes, events)
			}
			var metadata string
			if err := m.db.Read().QueryRow(`SELECT metadata_json FROM auth_events WHERE event_type='setup_completed'`).Scan(&metadata); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{input.Password, input.Token, input.Username, "a@example.test", result.Session.Token} {
				if strings.Contains(metadata, secret) {
					t.Fatal("setup event disclosed private data")
				}
			}
			if _, err := m.CompletePersonalSetup(t.Context(), input); err == nil {
				t.Fatal("replayed setup succeeded")
			}
			m.config.Mode = ModeManaged
			if err := m.ValidateRuntimeMode(t.Context()); err == nil {
				t.Fatal("managed mode accepted personal database")
			}
		})
	}
}

func TestPersonalSetupAuditFailureRollsBackAndForeignUsersBlockSetup(t *testing.T) {
	m, input := personalSetupManager(t)
	if _, err := m.db.Write().Exec(`CREATE TRIGGER fail_personal_setup BEFORE INSERT ON auth_events WHEN NEW.event_type='setup_completed' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompletePersonalSetup(t.Context(), input); err == nil {
		t.Fatal("audit failure did not block completion")
	}
	state, err := m.SetupState(t.Context())
	if err != nil || state.Initialized {
		t.Fatalf("state %#v %v", state, err)
	}
	var count int
	if err := m.db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM users)+(SELECT COUNT(*) FROM sessions)+(SELECT COUNT(*) FROM password_credentials)`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial setup persisted: %d %v", count, err)
	}
	if _, err := m.db.Write().Exec(`DROP TRIGGER fail_personal_setup`); err != nil {
		t.Fatal(err)
	}
	insertActiveUser(t, m, "another-user", false, m.clock.Now())
	if err := m.ValidateRuntimeMode(t.Context()); err == nil {
		t.Fatal("foreign topology accepted")
	}
	if _, err := m.CompletePersonalSetup(t.Context(), input); !errors.Is(err, ErrPersonalSetupBlocked) {
		t.Fatalf("foreign topology completion: %v", err)
	}
}

func TestAuthenticationModeConfiguration(t *testing.T) {
	for _, mode := range []Mode{ModeOpen, ModePersonal, ModeManaged, "invalid"} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("GOFER_AUTH_MODE", string(mode))
			t.Setenv("GOFER_AUTH_ENABLED", "false")
			cfg := LoadConfig("https://gofer.example")
			if cfg.AuthenticationMode() != mode || cfg.Enabled != (mode != ModeOpen) {
				t.Fatalf("wrong mode %#v", cfg)
			}
			if (cfg.ValidateMode() != nil) != (mode == "invalid") {
				t.Fatal("invalid mode validation")
			}
		})
	}
}

func TestPersonalSetupConcurrentCompletionAndLocalRecovery(t *testing.T) {
	m, input := personalSetupManager(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; _, err := m.CompletePersonalSetup(t.Context(), input); results <- err }()
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("setup successes %d, want one", successes)
	}
	provision, err := m.EnsureSetupToken(t.Context(), "")
	if err != nil || !provision.State.Initialized || provision.Created || provision.Token != "" {
		t.Fatalf("setup reopened: %#v %v", provision, err)
	}
	recovery, err := m.RecoverUserLocally(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	replacement := "Another long personal recovery phrase 936!"
	redeemed, err := m.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{Token: recovery.Token.Token, NewPassword: replacement, Purpose: EnrollmentTokenPurposeCredentialReset})
	if err != nil || redeemed.UserID != "default" {
		t.Fatalf("personal recovery: %#v %v", redeemed, err)
	}
	login, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: input.Username, Password: replacement, RequiredUserType: UserTypeWebmail, Source: "127.0.0.1"})
	if err != nil || login.Session == nil || login.Session.UserID != "default" {
		t.Fatalf("recovered login: %#v %v", login, err)
	}
}
