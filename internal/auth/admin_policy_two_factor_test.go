package auth

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// A policy's two-factor requirement applies at the person's next sign-in.
// A change that newly requires it of someone waits to be confirmed, saying
// who, and can sign them out now.
func TestAPolicysTwoFactorRequirementAsksFirstAndCanSignPeopleOut(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	admin := func() (string, string) { return "administrator", session.ID }
	user, sessionID := admin()
	weak := func(person string) *Session {
		t.Helper()
		created, err := manager.CreateAuthenticatedSession(ctx, person, person+"'s laptop", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	ines, pavel := weak("ines"), weak("pavel")
	if _, err := manager.db.Write().Exec(`UPDATE users SET mfa_required = 1 WHERE id = 'noor'`); err != nil {
		t.Fatal(err)
	}

	strict, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: user, ActorSessionID: sessionID, Name: "Strict"})
	if err != nil {
		t.Fatal(err)
	}
	// Assigned to nobody, it newly requires it of nobody.
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: strict.ID, Version: strict.Version,
		Changes: []policy.ValueChange{{Key: policy.MFARequired.Key, JSON: json.RawMessage(`true`)}}}); err != nil {
		t.Fatal(err)
	}

	toInes := ChangePolicyAssignmentsOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: strict.ID, Targets: []policy.Target{{Kind: policy.TargetUser, ID: "ines"}}}
	_, err = manager.AssignPolicy(ctx, toInes)
	var waiting *TwoFactorConfirmationError
	if !errors.As(err, &waiting) || waiting.Preview.People != 1 || waiting.Preview.WithoutAppTotal != 1 || waiting.Preview.WithoutApp[0].Username != "ines" || waiting.Preview.SessionsToEnd != 1 {
		t.Fatalf("unconfirmed assignment = %#v", err)
	}
	if assigned, err := policy.AssignedTo(ctx, manager.db.Read(), policy.Target{Kind: policy.TargetUser, ID: "ines"}); err != nil || len(assigned) != 0 {
		t.Fatalf("an unconfirmed assignment stayed: %v, %v", assigned, err)
	}
	toInes.TwoFactor = TwoFactorChoice{Confirmed: true}
	if _, err := manager.AssignPolicy(ctx, toInes); err != nil {
		t.Fatal(err)
	}
	// Her session stays until she signs in again, which asks her to set
	// up the app.
	if current, err := manager.GetSessionByToken(ctx, ines.Token); err != nil || current == nil {
		t.Fatalf("Ines's session after the requirement = %v, %v", current, err)
	}
	required, err := manager.loadAuthenticationPolicy(ctx, manager.db.Read(), "ines", 0)
	if err != nil || !required.PolicyMFARequired || !required.MFAEnrollmentRequired || required.allowsAssurance(AssuranceLevelSingleFactor) {
		t.Fatalf("Ines's sign-in policy = %#v, %v", required, err)
	}

	// Adding Pavel to a group with the policy asks too; confirmed with
	// sign-out, his weak session ends. Noor is required it already.
	field, err := manager.CreateGroup(ctx, CreateGroupOptions{ActorUserID: user, ActorSessionID: sessionID, Name: "Field"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AssignPolicy(ctx, ChangePolicyAssignmentsOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: strict.ID, Targets: []policy.Target{{Kind: policy.TargetGroup, ID: field.ID}}}); err != nil {
		t.Fatalf("assign to an empty group = %v", err)
	}
	add := ChangeGroupMembersOptions{ActorUserID: user, ActorSessionID: sessionID, GroupID: field.ID, UserIDs: []string{"pavel", "noor"}}
	if _, err := manager.AddGroupMembers(ctx, add); !errors.As(err, &waiting) || waiting.Preview.People != 1 || waiting.Preview.WithoutApp[0].Username != "pavel" {
		t.Fatalf("unconfirmed members = %v", err)
	}
	add.TwoFactor = TwoFactorChoice{Confirmed: true, SignOut: true}
	if change, err := manager.AddGroupMembers(ctx, add); err != nil || len(change.Changed) != 2 {
		t.Fatalf("confirmed members = %#v, %v", change, err)
	}
	if current, err := manager.GetSessionByToken(ctx, pavel.Token); err != nil || current != nil {
		t.Fatalf("Pavel's weak session after signing him out = %v, %v", current, err)
	}
	records := policyAuditRecords(t, manager)
	ended := 0
	for _, record := range records {
		if record.Action == "policy.sessions_ended" {
			ended++
			if count, _ := record.Metadata["count"].(float64); count != 1 || record.Target.Kind != "group" {
				t.Fatalf("sessions ended record = %#v", record)
			}
		}
	}
	if ended != 1 {
		t.Fatalf("sessions ended recorded %d times", ended)
	}

	// Taking the value off and putting it back asks again.
	current, err := policy.Get(ctx, manager.db.Read(), strict.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: strict.ID, Version: current.Version,
		Changes: []policy.ValueChange{{Key: policy.MFARequired.Key}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: strict.ID, Version: current.Version + 1,
		Changes: []policy.ValueChange{{Key: policy.MFARequired.Key, JSON: json.RawMessage(`true`)}}}); !errors.As(err, &waiting) || waiting.Preview.People != 2 {
		t.Fatalf("putting the requirement back = %v", err)
	}

	// While everyone is required it, a policy requires it of nobody more.
	if _, err := manager.db.Write().Exec(`INSERT INTO auth_system_state (id, initialized, mfa_policy) VALUES (1, 1, 'all_users') ON CONFLICT (id) DO UPDATE SET mfa_policy = 'all_users'`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: strict.ID, Version: current.Version + 1,
		Changes: []policy.ValueChange{{Key: policy.MFARequired.Key, JSON: json.RawMessage(`true`)}}}); err != nil {
		t.Fatalf("with everyone required = %v", err)
	}
}

// Where a policy lets a passkey be the second step, a passkey counts as set
// up and its sessions stay; making the policy take the app code instead
// asks again, for the passkey sessions it would end.
func TestAPolicyThatAcceptsAPasskeyCountsItAndTighteningAsks(t *testing.T) {
	manager, session := newGroupsManager(t)
	ctx := t.Context()
	user, sessionID := "administrator", session.ID
	now := time.Date(2026, time.March, 2, 9, 30, 0, 0, time.UTC)
	if _, err := manager.db.Write().Exec(`INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name, created_at, credential_ciphertext, key_version, rp_id)
		VALUES ('ines-key', 'ines', x'0405', x'02', 'Ines''s key', ?, x'03', 1, 'gofer.example')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAuthenticatedSession(ctx, "ines", "Ines's phone", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant); err != nil {
		t.Fatal(err)
	}
	keys, err := manager.CreatePolicy(ctx, CreatePolicyOptions{ActorUserID: user, ActorSessionID: sessionID, Name: "Passkeys"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: keys.ID, Version: keys.Version,
		Changes: []policy.ValueChange{{Key: policy.MFARequired.Key, JSON: json.RawMessage(`true`)}, {Key: policy.MFAPasskeyCounts.Key, JSON: json.RawMessage(`true`)}}}); err != nil {
		t.Fatal(err)
	}
	toInes := ChangePolicyAssignmentsOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: keys.ID, Targets: []policy.Target{{Kind: policy.TargetUser, ID: "ines"}}}
	_, err = manager.AssignPolicy(ctx, toInes)
	var waiting *TwoFactorConfirmationError
	if !errors.As(err, &waiting) || waiting.Preview.People != 1 || waiting.Preview.WithoutAppTotal != 0 || waiting.Preview.SessionsToEnd != 0 {
		t.Fatalf("assigning a policy that accepts her passkey = %#v", err)
	}
	toInes.TwoFactor = TwoFactorChoice{Confirmed: true}
	if _, err := manager.AssignPolicy(ctx, toInes); err != nil {
		t.Fatal(err)
	}
	required, err := manager.loadAuthenticationPolicy(ctx, manager.db.Read(), "ines", 0)
	if err != nil || !required.PasskeyCountsAsSecondFactor || !required.allowsAssurance(AssuranceLevelPhishingResistant) {
		t.Fatalf("Ines's sign-in policy = %#v, %v", required, err)
	}
	page, err := manager.ListAdministratorUserPage(ctx, user, sessionID, AdministratorUserQuery{Attention: AdministratorUserAttentionMFASetup})
	if err != nil || page.Matching != 0 {
		t.Fatalf("people to set up two-factor sign-in = %#v, %v", page, err)
	}

	// The app code instead: her passkey no longer counts, so it asks.
	current, err := policy.Get(ctx, manager.db.Read(), keys.ID)
	if err != nil {
		t.Fatal(err)
	}
	tighten := SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: keys.ID, Version: current.Version,
		Changes: []policy.ValueChange{{Key: policy.MFAPasskeyCounts.Key}}}
	if _, err := manager.SetPolicyValues(ctx, tighten); !errors.As(err, &waiting) || waiting.Preview.People != 1 ||
		waiting.Preview.WithoutAppTotal != 1 || waiting.Preview.SessionsToEnd != 1 {
		t.Fatalf("taking the passkey away = %v", err)
	}
	tighten.TwoFactor = TwoFactorChoice{Confirmed: true, SignOut: true}
	if _, err := manager.SetPolicyValues(ctx, tighten); err != nil {
		t.Fatal(err)
	}
	var working int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = 'ines' AND revoked_at IS NULL`).Scan(&working); err != nil || working != 0 {
		t.Fatalf("Ines's passkey session after signing her out = %d, %v", working, err)
	}
	if page, err := manager.ListAdministratorUserPage(ctx, user, sessionID, AdministratorUserQuery{Attention: AdministratorUserAttentionMFASetup}); err != nil || page.Matching != 1 {
		t.Fatalf("people to set up two-factor sign-in after = %#v, %v", page, err)
	}
	// The Default policy's value is fixed: the requirement for everyone
	// always takes the app code.
	defaultPolicy, err := policy.Get(ctx, manager.db.Read(), policy.DefaultID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetPolicyValues(ctx, SetPolicyValuesOptions{ActorUserID: user, ActorSessionID: sessionID, PolicyID: policy.DefaultID, Version: defaultPolicy.Version,
		Changes: []policy.ValueChange{{Key: policy.MFAPasskeyCounts.Key, JSON: json.RawMessage(`true`)}}}); !errors.Is(err, policy.ErrAdapted) {
		t.Fatalf("a passkey on the Default policy = %v", err)
	}
}
