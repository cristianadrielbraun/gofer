package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/directory"
)

// Test settings, declared without joining the registry.
var (
	testExport   = boolSetting(Spec{Key: "data.mail_export", Category: CategoryData, Kind: KindCapability, Label: "x"}, true)
	testAccounts = intSetting(Spec{Key: "mail.max_accounts", Category: CategoryMail, Kind: KindLimit, Label: "x"}, 0, 0, 50)
	testLanguage = enumSetting(Spec{Key: "prefs.language", Category: CategoryPreferences, Kind: KindPreference, Label: "x"}, "auto", "auto", "en", "es")
	testFonts    = boolSetting(Spec{Key: "privacy.google_fonts", Category: CategoryData, Kind: KindPrivacy, InstanceOnly: true, Label: "x"}, false)
	testSettings = []*Setting{testExport, testAccounts, testLanguage, testFonts}
)

func value(key, raw string) Value { return Value{Key: key, JSON: json.RawMessage(raw)} }

func TestSettingsCheckTheirValues(t *testing.T) {
	for _, test := range []struct {
		setting *Setting
		raw     string
		want    any
	}{
		{testExport, `false`, false},
		{testExport, `"no"`, nil},
		{testAccounts, `12`, 12},
		{testAccounts, `51`, nil},
		{testAccounts, `-1`, nil},
		{testAccounts, `1.5`, nil},
		{testLanguage, `"es"`, "es"},
		{testLanguage, `"fr"`, nil},
	} {
		got, err := test.setting.Decode(json.RawMessage(test.raw))
		if test.want == nil {
			if !errors.Is(err, ErrValueInvalid) {
				t.Errorf("%s %s = %v, %v; want refused", test.setting.Key, test.raw, got, err)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("%s %s = %v, %v; want %v", test.setting.Key, test.raw, got, err, test.want)
		}
	}
	if raw, err := testAccounts.Encode(7); err != nil || string(raw) != "7" {
		t.Fatalf("encode 7 = %s, %v", raw, err)
	}
	if _, err := testAccounts.Encode(99); !errors.Is(err, ErrValueInvalid) {
		t.Fatalf("encode 99 = %v", err)
	}
	for _, declare := range []func(){
		func() {
			Bool(Spec{Key: MFARequired.Key, Category: CategorySignIn, Kind: KindSecurity, Label: "x"}, false)
		},
		func() { Bool(Spec{Key: "Not A Key", Category: CategorySignIn, Kind: KindSecurity, Label: "x"}, false) },
		func() { intSetting(Spec{Key: "a.b", Label: "x"}, 9, 0, 5) },
		func() { enumSetting(Spec{Key: "a.b", Label: "x"}, "z", "a", "b") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("a broken declaration was accepted")
				}
			}()
			declare()
		}()
	}
}

func TestTheDeclaredSettingsKeepTodaysBehaviour(t *testing.T) {
	keys := []string{}
	for _, setting := range Settings() {
		if !strings.HasPrefix(setting.Key, "test.") {
			keys = append(keys, setting.Key)
		}
	}
	if got := strings.Join(keys, " "); got != "audit.security_retention_days auth.mfa_required mail.allowed_address_domains mail.allowed_providers mail.allowed_servers mail.own_proxies" {
		t.Fatalf("declared settings = %s", got)
	}
	empty := resolve(resolveInput{policies: []Policy{unsavedDefault()}, settings: Settings()})
	if empty.Bool(MFARequired) || empty.Int(SecurityRetentionDays) != 180 || empty.Bool(OwnProxies) ||
		len(empty.Strings(MailAllowedServers)) != 0 || len(empty.Strings(MailAllowedDomains)) != 0 || len(empty.Strings(MailAllowedProviders)) != 0 {
		t.Fatalf("with nothing set:\n%s", empty.describe())
	}
	if lookedUp, ok := Lookup("mail.own_proxies"); !ok || lookedUp != OwnProxies {
		t.Fatal("Lookup didn't find a declared setting")
	}
}

// The plan's example: Contractors (priority 1) through the Contractors
// group, Finance (priority 2) through the Finance group, and Ana in both.
func TestTheFirstPolicyInOrderWinsAndEnforcedBeatsIt(t *testing.T) {
	contractors := Policy{ID: "p-contractors", Name: "Contractors", Priority: 1}
	finance := Policy{ID: "p-finance", Name: "Finance", Priority: 2}
	defaults := Policy{ID: DefaultID, Name: defaultName, Default: true}
	in := resolveInput{
		userID:   "u-ana",
		policies: []Policy{contractors, finance, defaults},
		values: map[string][]Value{
			"p-contractors": {value("data.mail_export", `false`)},
			"p-finance":     {value("data.mail_export", `true`), value("mail.max_accounts", `3`), {Key: "prefs.language", JSON: json.RawMessage(`"es"`), Locked: true}},
			DefaultID:       {value("mail.max_accounts", `10`), value("privacy.google_fonts", `true`)},
		},
		applies: map[string]applied{
			"p-contractors": {groupID: "g-contractors", groupName: "Contractors"},
			"p-finance":     {groupID: "g-finance", groupName: "Finance"},
		},
		settings: testSettings,
	}
	want := strings.Join([]string{
		"data.mail_export=false from policy p-contractors via g-contractors",
		"mail.max_accounts=3 from policy p-finance via g-finance",
		"prefs.language=es from policy p-finance via g-finance locked",
		"privacy.google_fonts=true from default default",
	}, "\n")
	if got := resolve(in).describe(); got != want {
		t.Fatalf("Ana:\n%s\nwant:\n%s", got, want)
	}

	// Finance first: its export value wins.
	in.policies = []Policy{finance, contractors, defaults}
	if got := resolve(in).Get(testExport); got.Value != true || got.Source.PolicyID != "p-finance" {
		t.Fatalf("Finance first = %+v", got)
	}

	// The Default policy enforces export off: nobody gets it, and it's locked.
	in.values[DefaultID] = append(in.values[DefaultID], Value{Key: "data.mail_export", JSON: json.RawMessage(`false`), Enforced: true})
	got := resolve(in).Get(testExport)
	if got.Value != false || got.Source.Kind != SourceDefault || !got.Source.Enforced || !got.Locked {
		t.Fatalf("enforced export = %+v", got)
	}

	// Someone the policies don't apply to gets the Default policy's
	// values, and the built-in default for the rest.
	in.applies = map[string]applied{}
	nobody := resolve(in)
	if nobody.Int(testAccounts) != 10 || nobody.String(testLanguage) != "auto" || nobody.Get(testLanguage).Source.Kind != SourceBuiltin {
		t.Fatalf("someone with no policy:\n%s", nobody.describe())
	}
}

func TestValuesThatDontCheckAreSkipped(t *testing.T) {
	in := resolveInput{
		policies: []Policy{{ID: "p-1", Name: "One", Priority: 1}, {ID: DefaultID, Default: true}},
		values: map[string][]Value{
			"p-1": {
				value("mail.max_accounts", `500`),     // out of range
				value("privacy.google_fonts", `true`), // whole-server only
				value("mail.removed_setting", `true`), // no longer declared
				value("data.mail_export", `false`),    // fine
			},
			DefaultID: {value("mail.max_accounts", `"many"`)},
		},
		applies:  map[string]applied{"p-1": {direct: true}},
		settings: testSettings,
	}
	effective := resolve(in)
	if effective.Int(testAccounts) != 0 || effective.Bool(testFonts) || effective.Bool(testExport) {
		t.Fatalf("resolved:\n%s", effective.describe())
	}
	if got := effective.Get(testExport).Source; got.PolicyID != "p-1" || got.GroupID != "" {
		t.Fatalf("a direct assignment's source = %+v", got)
	}
	reasons := map[string]error{}
	for _, ignored := range effective.Ignored {
		reasons[ignored.PolicyID+" "+ignored.Key] = ignored.Err
	}
	if len(reasons) != 4 || !errors.Is(reasons["p-1 mail.max_accounts"], ErrValueInvalid) || !errors.Is(reasons["p-1 privacy.google_fonts"], ErrInstanceOnly) ||
		!errors.Is(reasons["p-1 mail.removed_setting"], ErrUnknownSetting) || !errors.Is(reasons["default mail.max_accounts"], ErrValueInvalid) {
		t.Fatalf("ignored = %+v", effective.Ignored)
	}
}

// randomInput is a random set of policies, values and assignments over the
// test settings.
func randomInput(r *rand.Rand) resolveInput {
	in := resolveInput{userID: "u", values: map[string][]Value{}, applies: map[string]applied{}, settings: testSettings}
	count := r.IntN(6)
	for i := range count {
		id := fmt.Sprintf("p-%d", i)
		in.policies = append(in.policies, Policy{ID: id, Name: id, Priority: i + 1})
		switch r.IntN(3) {
		case 0:
			in.applies[id] = applied{direct: true}
		case 1:
			group := fmt.Sprintf("g-%d", r.IntN(3))
			in.applies[id] = applied{groupID: group, groupName: group}
		}
	}
	in.policies = append(in.policies, Policy{ID: DefaultID, Name: defaultName, Default: true})
	for _, policy := range in.policies {
		for _, setting := range testSettings {
			if r.IntN(2) == 0 {
				continue
			}
			var raw string
			switch setting {
			case testAccounts:
				raw = fmt.Sprint(r.IntN(60)) // sometimes out of range
			case testLanguage:
				raw = []string{`"en"`, `"es"`, `"fr"`}[r.IntN(3)]
			default:
				raw = []string{"true", "false"}[r.IntN(2)]
			}
			in.values[policy.ID] = append(in.values[policy.ID], Value{
				Key: setting.Key, JSON: json.RawMessage(raw), Locked: r.IntN(3) == 0, Enforced: policy.Default && r.IntN(4) == 0,
			})
		}
	}
	return in
}

func TestResolutionPropertiesHoldForRandomPolicies(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 113))
	for round := range 2000 {
		in := randomInput(r)
		got := resolve(in)
		// Deterministic: the same input, the same answer.
		if again := resolve(in); again.describe() != got.describe() {
			t.Fatalf("round %d: two answers:\n%s\n---\n%s", round, got.describe(), again.describe())
		}
		for _, setting := range in.settings {
			resolved := got.Get(setting)
			// The value always checks.
			if raw, err := json.Marshal(resolved.Value); err != nil {
				t.Fatal(err)
			} else if _, err := setting.Decode(raw); err != nil {
				t.Fatalf("round %d: %s resolved to a value that doesn't check: %v", round, setting.Key, resolved.Value)
			}
			// An enforced Default value beats everything, locked.
			for _, stored := range in.values[DefaultID] {
				if stored.Key != setting.Key || !stored.Enforced {
					continue
				}
				if _, err := setting.Decode(stored.JSON); err == nil && (!resolved.Source.Enforced || !resolved.Locked) {
					t.Fatalf("round %d: %s enforced by Default, resolved %+v", round, setting.Key, resolved)
				}
			}
			// Otherwise the first policy in order that applies and sets a
			// value that checks wins, then the Default policy, then the
			// built-in default.
			if !resolved.Source.Enforced {
				want := ""
				for _, policy := range in.policies {
					if _, applies := in.applies[policy.ID]; (!applies || setting.InstanceOnly) && !policy.Default {
						continue
					}
					for _, stored := range in.values[policy.ID] {
						if _, err := setting.Decode(stored.JSON); stored.Key == setting.Key && err == nil {
							want = policy.ID
						}
					}
					if want != "" {
						break
					}
				}
				if resolved.Source.PolicyID != want {
					t.Fatalf("round %d: %s from %q, want %q", round, setting.Key, resolved.Source.PolicyID, want)
				}
			}
			// A policy's value only wins when it applies to the person,
			// and the setting isn't for the whole server.
			if resolved.Source.Kind == SourcePolicy {
				if _, applies := in.applies[resolved.Source.PolicyID]; !applies || setting.InstanceOnly {
					t.Fatalf("round %d: %s from %s, which doesn't apply", round, setting.Key, resolved.Source.PolicyID)
				}
			}
		}

		// Removing a policy changes only the settings that came from it.
		for _, removed := range in.policies {
			if removed.Default {
				continue
			}
			without := in
			without.policies = nil
			for _, policy := range in.policies {
				if policy.ID != removed.ID {
					without.policies = append(without.policies, policy)
				}
			}
			after := resolve(without)
			for _, setting := range in.settings {
				before := got.Get(setting)
				if before.Source.PolicyID == removed.ID {
					continue
				}
				if now := after.Get(setting); now.Value != before.Value || now.Source.PolicyID != before.Source.PolicyID {
					t.Fatalf("round %d: removing %s changed %s from %+v to %+v", round, removed.ID, setting.Key, before, now)
				}
			}
		}

		// Leaving a group changes only the settings that came through it.
		for _, group := range []string{"g-0", "g-1", "g-2"} {
			left := in
			left.applies = map[string]applied{}
			for id, how := range in.applies {
				if how.groupID != group {
					left.applies[id] = how
				}
			}
			after := resolve(left)
			for _, setting := range in.settings {
				before := got.Get(setting)
				if before.Source.GroupID == group {
					continue
				}
				if now := after.Get(setting); now.Value != before.Value || now.Source.PolicyID != before.Source.PolicyID {
					t.Fatalf("round %d: leaving %s changed %s from %+v to %+v", round, group, setting.Key, before, now)
				}
			}
		}
	}
}

// ForUser reads the policies, groups and assignments from the database.
func TestForUserReadsWhatAppliesFromTheDatabase(t *testing.T) {
	db := newStore(t)
	ctx := t.Context()
	create(t, db, "p-direct", "Ines only")
	create(t, db, "p-field", "Field rules")
	for _, statement := range []string{
		`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES ('g-field', 'u-ines', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if _, err := Assign(ctx, tx, "p-field", []Target{{TargetGroup, "g-field"}}, "u-admin", monday); err != nil {
			return nil, err
		}
		if _, err := Assign(ctx, tx, "p-direct", []Target{{TargetUser, "u-ines"}}, "u-admin", monday); err != nil {
			return nil, err
		}
		if _, err := SetValues(ctx, tx, "p-field", 1, []ValueChange{{Key: OwnProxies.Key, JSON: json.RawMessage(`true`)}, {Key: MFARequired.Key, JSON: json.RawMessage(`true`)}}, "u-admin", monday); err != nil {
			return nil, err
		}
		if _, err := SetValues(ctx, tx, "p-direct", 1, []ValueChange{{Key: MFARequired.Key, JSON: json.RawMessage(`false`)}}, "u-admin", monday); err != nil {
			return nil, err
		}
		// Retention lives in the instance's sign-in state.
		_, err := tx.ExecContext(ctx, `INSERT INTO auth_system_state (id, initialized, auth_event_retention_days) VALUES (1, 1, 90)`)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}

	ines, err := ForUser(ctx, db.Read(), "u-ines")
	if err != nil {
		t.Fatal(err)
	}
	if got := ines.Get(MFARequired); got.Value != false || got.Source.PolicyID != "p-direct" || got.Source.GroupID != "" {
		t.Fatalf("Ines's two-factor = %+v", got)
	}
	if got := ines.Get(OwnProxies); got.Value != true || got.Source.GroupID != "g-field" || got.Source.GroupName != "Field team" {
		t.Fatalf("Ines's own proxies = %+v", got)
	}
	if ines.Int(SecurityRetentionDays) != 90 {
		t.Fatalf("retention = %d", ines.Int(SecurityRetentionDays))
	}

	// Field rules second-to-first: the group's value wins now.
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return nil, Reorder(ctx, tx, []string{"p-field", "p-direct"})
	}); err != nil {
		t.Fatal(err)
	}
	if ines, err = ForUser(ctx, db.Read(), "u-ines"); err != nil || !ines.Bool(MFARequired) {
		t.Fatalf("after reordering = %v, %v", ines.Bool(MFARequired), err)
	}

	// The instance only reads the Default policy.
	instance, err := ForInstance(ctx, db.Read())
	if err != nil || instance.Bool(OwnProxies) || instance.Int(SecurityRetentionDays) != 90 || instance.Get(MFARequired).Source.Kind != SourceDefault || instance.Bool(MFARequired) {
		t.Fatalf("instance = %v:\n%s", err, instance.describe())
	}
	// Someone in no group, with nothing assigned, gets the same.
	if pavel, err := ForUser(ctx, db.Read(), "u-pavel"); err != nil || pavel.describe() != instance.describe() {
		t.Fatalf("someone with no policy = %v:\n%s", err, pavel.describe())
	}
}

// The three settings that existed before policies keep living where they
// did: the Default policy reads them there, and they're changed there.
func TestSettingsThatLiveElsewhereAreReadWhereTheyLive(t *testing.T) {
	db := newStore(t)
	ctx := t.Context()
	create(t, db, "p-relaxed", "Relaxed")
	if _, err := db.Write().Exec(`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES ('g-field', 'u-ines', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if _, err := Assign(ctx, tx, "p-relaxed", []Target{{TargetGroup, "g-field"}}, "u-admin", monday); err != nil {
			return nil, err
		}
		return SetValues(ctx, tx, "p-relaxed", 1, []ValueChange{{Key: MFARequired.Key, JSON: json.RawMessage(`false`)}}, "u-admin", monday)
	}); err != nil {
		t.Fatal(err)
	}
	for _, setting := range []*Setting{MFARequired, SecurityRetentionDays, OwnProxies} {
		if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (Policy, error) {
			return SetValues(ctx, tx, DefaultID, 0, []ValueChange{{Key: setting.Key, JSON: json.RawMessage(`true`)}}, "u-admin", monday)
		}); !errors.Is(err, ErrAdapted) && !errors.Is(err, ErrValueInvalid) {
			t.Fatalf("saving %s to the Default policy = %v", setting.Key, err)
		}
	}

	// Before setup, nothing is stored anywhere: the built-in defaults.
	instance, err := ForInstance(ctx, db.Read())
	if err != nil || instance.Get(MFARequired).Source.Kind != SourceBuiltin || instance.Int(SecurityRetentionDays) != 180 {
		t.Fatalf("before setup = %v:\n%s", err, instance.describe())
	}

	for _, statement := range []string{
		`INSERT INTO auth_system_state (id, initialized, mfa_policy, auth_event_retention_days) VALUES (1, 1, 'administrators', 30)`,
		`INSERT INTO outbound_proxy_policy (singleton, people_add) VALUES (1, 1)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ines, err := ForUser(ctx, db.Read(), "u-ines")
	if err != nil {
		t.Fatal(err)
	}
	if got := ines.Get(MFARequired); got.Value != false || got.Source.PolicyID != "p-relaxed" {
		t.Fatalf("two-factor for administrators only = %+v", got)
	}
	if got := ines.Get(OwnProxies); got.Value != true || got.Source.Kind != SourceDefault || got.Locked {
		t.Fatalf("own proxies = %+v", got)
	}
	if ines.Int(SecurityRetentionDays) != 30 {
		t.Fatalf("retention = %d", ines.Int(SecurityRetentionDays))
	}

	// Requiring it of everyone can't be turned off by a policy.
	if _, err := db.Write().Exec(`UPDATE auth_system_state SET mfa_policy = 'all_users' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if ines, err = ForUser(ctx, db.Read(), "u-ines"); err != nil {
		t.Fatal(err)
	}
	if got := ines.Get(MFARequired); got.Value != true || got.Source.Kind != SourceDefault || !got.Source.Enforced || !got.Locked {
		t.Fatalf("two-factor for everyone = %+v", got)
	}
}

func TestTheCacheWorksSettingsOutAgainAfterAChange(t *testing.T) {
	db := newStore(t)
	ctx := t.Context()
	create(t, db, "p-field", "Field rules")
	if _, err := db.Write().Exec(`INSERT INTO auth_system_state (id, initialized) VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if _, err := Assign(ctx, tx, "p-field", []Target{{TargetGroup, "g-field"}}, "u-admin", monday); err != nil {
			return nil, err
		}
		return SetValues(ctx, tx, "p-field", 1, []ValueChange{{Key: OwnProxies.Key, JSON: json.RawMessage(`true`)}}, "u-admin", monday)
	}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	cache := NewCache(db.Read(), 2)
	cache.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	first, err := cache.ForUser(ctx, "u-ines")
	if err != nil || first.Bool(OwnProxies) {
		t.Fatalf("Ines outside the group = %v, %v", first.Bool(OwnProxies), err)
	}
	if again, _ := cache.ForUser(ctx, "u-ines"); again != first {
		t.Fatal("nothing changed, but the settings were worked out again")
	}

	// Joining the group moves the membership revision.
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		return directory.AddMembers(ctx, tx, "g-field", []string{"u-ines"}, "u-admin", monday)
	}); err != nil {
		t.Fatal(err)
	}
	if joined, err := cache.ForUser(ctx, "u-ines"); err != nil || !joined.Bool(OwnProxies) {
		t.Fatalf("Ines in the group = %v, %v", joined.Bool(OwnProxies), err)
	}

	// A value that no longer checks is skipped, and logged once.
	if _, err := db.Write().Exec(`UPDATE policy_values SET value_json = '"sometimes"' WHERE policy_id = 'p-field'`); err != nil {
		t.Fatal(err)
	}
	if _, err := inTx(t, db, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return nil, directory.Bump(ctx, tx, directory.RevisionPolicy)
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if skipped, err := cache.ForUser(ctx, "u-ines"); err != nil || skipped.Bool(OwnProxies) {
			t.Fatalf("a value that doesn't check = %v, %v", skipped.Bool(OwnProxies), err)
		}
		if _, err := cache.ForUser(ctx, "u-pavel"); err != nil {
			t.Fatal(err)
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "mail.own_proxies on policy p-field") {
		t.Fatalf("logged = %q", logged)
	}

	// Two people fit; a third pushes out whoever was asked for longest ago.
	if _, err := cache.ForInstance(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cache.entries) != 2 || cache.entries["u-ines"] != nil {
		t.Fatalf("cache holds %d, Ines still in: %v", len(cache.entries), cache.entries["u-ines"] != nil)
	}
}
