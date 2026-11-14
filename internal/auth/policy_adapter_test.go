package auth

import (
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// The settings that existed before policies are still changed on their
// own pages; the Default policy reads them where they live, and its cache
// notices each change.
func TestPolicyCacheSeesChangesMadeOnTheOldPages(t *testing.T) {
	now := time.Date(2026, time.August, 10, 0, 15, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ines", false, now)
	initializePolicyTestInstance(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	session := createPolicyAdministratorSession(t, manager)
	cache := policy.NewCache(manager.db.Read(), 10)

	before, err := cache.ForUser(t.Context(), "ines")
	if err != nil || before.Bool(policy.MFARequired) || before.Int(policy.SecurityRetentionDays) != DefaultAuthenticationEventRetentionDays {
		t.Fatalf("before = %v, %v", before, err)
	}
	if _, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, Policy: InstanceMFAPolicyAllUsers,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := cache.ForUser(t.Context(), "ines")
	if got := after.Get(policy.MFARequired); err != nil || got.Value != true || !got.Source.Enforced {
		t.Fatalf("after requiring it of everyone = %+v, %v", got, err)
	}
	if _, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
		ActorUserID: "administrator", ActorSessionID: session.ID, Days: 30,
	}); err != nil {
		t.Fatal(err)
	}
	if instance, err := cache.ForInstance(t.Context()); err != nil || instance.Int(policy.SecurityRetentionDays) != 30 {
		t.Fatalf("instance retention = %v, %v", instance.Int(policy.SecurityRetentionDays), err)
	}
}
