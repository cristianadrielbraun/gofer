package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func newContactSyncFixture(t *testing.T) (*DB, string, string) {
	t.Helper()
	db := newInboundContactFixture(t)
	input := InboundContact{Contact: models.Contact{Name: "Original", Email: "person@example.com", Phone: "111", PhoneLabel: "work", AdditionalEmails: []string{"other@example.com"}, AdditionalEmailLabels: []string{"home"}}, RemoteID: "people/original", Etag: "source-tag"}
	result := publishTestContact(t, db, input)
	enableInboundFanout(t, db, result.ProfileID)
	// Deliberately stale data/targets in the queue must not be sent.
	id, err := db.EnqueueContactSyncOperationFromAccount(t.Context(), "default", models.Contact{ID: result.ProfileID, Email: "old@example.com", Phone: "old", SaveTargets: []string{"account:foreign"}}, nil, "source")
	if err != nil {
		t.Fatal(err)
	}
	return db, result.ProfileID, id
}

func contactSyncClaimFor(t *testing.T, db *DB, owner string) *ContactSyncClaim {
	t.Helper()
	claims, err := db.ClaimUserContactSyncOperations(t.Context(), owner, 1, time.Minute, inboundTestGuard)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %d %v", len(claims), err)
	}
	return claims[0]
}

func contactSyncProfileFor(t *testing.T, db *DB, claim *ContactSyncClaim) *ContactSyncProfile {
	t.Helper()
	p, err := db.SnapshotUserContactSyncProfile(t.Context(), claim, inboundTestGuard)
	if err != nil || p == nil {
		t.Fatal("profile", p, err)
	}
	return p
}

func contactSyncDestinationFor(t *testing.T, db *DB, p *ContactSyncProfile, account, provider, book string) *ContactSyncDestination {
	t.Helper()
	d, err := db.SnapshotUserContactSyncDestination(t.Context(), p, account, provider, book, inboundTestGuard)
	if err != nil || d == nil {
		t.Fatal("destination", d, err)
	}
	return d
}

func TestUserContactSyncActivityGuardsAndSilentCancellation(t *testing.T) {
	db, profile, _ := newContactSyncFixture(t)
	claim := contactSyncClaimFor(t, db, "default")
	snapshot := contactSyncProfileFor(t, db, claim)
	var events []ContactActivityNotification
	db.SetContactActivityHook(func(event ContactActivityNotification) { events = append(events, event) })
	if _, err := db.Write().Exec(`CREATE TRIGGER fail_started_activity BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='contact_sync_started' BEGIN SELECT RAISE(ABORT,'activity failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.StartUserContactSyncOperation(t.Context(), snapshot, inboundTestGuard); err == nil || len(events) != 0 {
		t.Fatal("failed running event escaped", err, events)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER fail_started_activity`); err != nil {
		t.Fatal(err)
	}
	if err := db.StartUserContactSyncOperation(t.Context(), snapshot, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Status != "running" || events[0].ContactID != profile || events[0].Email == "old@example.com" {
		t.Fatal("wrong running activity", events)
	}
	if err := db.CancelUserContactSyncOperation(t.Context(), claim, inboundTestGuard); !errors.Is(err, ErrContactSyncSuperseded) {
		t.Fatal("live profile cancelled", err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_profiles SET sync_enabled=0 WHERE id=?`, profile); err != nil {
		t.Fatal(err)
	}
	if err := db.StartUserContactSyncOperation(t.Context(), snapshot, inboundTestGuard); !errors.Is(err, ErrContactSyncSuperseded) {
		t.Fatal("stale profile started", err)
	}
	if err := db.CancelUserContactSyncOperation(t.Context(), claim, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM contact_sync_operations WHERE id=?`, claim.OperationID()).Scan(&status); err != nil || status != "done" || len(events) != 1 {
		t.Fatal("cancelled job reported provider success", status, events, err)
	}
}

func TestUserContactSyncManualEnqueueIsAtomicAndCurrent(t *testing.T) {
	db, profile, _ := newContactSyncFixture(t)
	_ = contactSyncClaimFor(t, db, "default") // Keep the older inbound job out of this new manual claim.
	latest := models.Contact{Name: "New", Email: "new@example.com", Phone: "new-phone"}
	if err := db.ReplaceCanonicalContact(t.Context(), "default", profile, latest); err != nil {
		t.Fatal(err)
	}
	var events []ContactActivityNotification
	db.SetContactActivityHook(func(event ContactActivityNotification) { events = append(events, event) })
	if _, err := db.Write().Exec(`CREATE TRIGGER fail_queued_activity BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='contact_sync_queued' BEGIN SELECT RAISE(ABORT,'activity failure'); END`); err != nil {
		t.Fatal(err)
	}
	if id, err := db.EnqueueUserContactSync(t.Context(), "default", profile, inboundTestGuard); err == nil || id != "" || len(events) != 0 {
		t.Fatal("failed enqueue escaped", id, events, err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER fail_queued_activity`); err != nil {
		t.Fatal(err)
	}
	if id, err := db.EnqueueUserContactSync(t.Context(), "default", profile, inboundTestGuard); err != nil || id == "" {
		t.Fatal("manual enqueue", id, err)
	}
	if len(events) != 1 || events[0].Email != latest.Email || events[0].Status != "pending" {
		t.Fatal("stale queued activity", events)
	}
	claim := contactSyncClaimFor(t, db, "default")
	p := contactSyncProfileFor(t, db, claim)
	if p.Contact().Phone != latest.Phone || len(p.Targets()) != 2 {
		t.Fatal("manual job not current or retained inbound exclusion", p.Contact(), p.Targets())
	}
	if _, err := db.Write().Exec(`UPDATE contact_profiles SET sync_enabled=0 WHERE id=?`, profile); err != nil {
		t.Fatal(err)
	}
	if id, err := db.EnqueueUserContactSync(t.Context(), "default", profile, inboundTestGuard); !errors.Is(err, ErrContactSyncUnavailable) || id != "" {
		t.Fatal("disabled profile enqueued", id, err)
	}
}

func TestUserContactSyncCopiesLatestCanonicalAndExcludedTargets(t *testing.T) {
	db, profileID, id := newContactSyncFixture(t)
	latest := models.Contact{Name: "Latest", Email: "latest@example.com", Phone: "222", PhoneLabel: "mobile", AdditionalEmails: []string{"alternate@example.com"}, AdditionalEmailLabels: []string{"work"}}
	if err := db.ReplaceCanonicalContact(t.Context(), "default", profileID, latest); err != nil {
		t.Fatal(err)
	}
	c := contactSyncClaimFor(t, db, "default")
	if c.OperationID() != id || c.ContactID() != profileID || c.AttemptCount() != 1 {
		t.Fatal("wrong claim")
	}
	p := contactSyncProfileFor(t, db, c)
	copy := p.Contact()
	if copy.Name != latest.Name || copy.Email != latest.Email || copy.Phone != latest.Phone || !reflect.DeepEqual(copy.AdditionalEmailLabels, latest.AdditionalEmailLabels) {
		t.Fatalf("stale projection: %+v", copy)
	}
	ui, err := db.GetContact(t.Context(), "default", profileID)
	if err != nil || !reflect.DeepEqual(contactFieldSnapshotValues(copy), contactFieldSnapshotValues(*ui)) {
		t.Fatal("worker/UI canonical mismatch", err)
	}
	if targets := p.Targets(); len(targets) != 1 || targets[0].AccountID != "target" {
		t.Fatal("wrong targets", targets)
	}
	copy.AdditionalEmails[0] = "escaped@example.com"
	copy.SaveTargets[0] = "account:foreign"
	targets := p.Targets()
	targets[0].AccountID = "foreign"
	if p.Contact().AdditionalEmails[0] != latest.AdditionalEmails[0] || p.Targets()[0].AccountID != "target" {
		t.Fatal("snapshot slices escaped")
	}
	d := contactSyncDestinationFor(t, db, p, "target", "outlook", "")
	if d.Source() != nil {
		t.Fatal("unexpected destination source")
	}
	if err := db.PublishUserContactSyncResult(t.Context(), d, "graph-created", "graph-version", inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetContact(t.Context(), "default", profileID)
	if err != nil || current.Phone != latest.Phone || current.Name != latest.Name {
		t.Fatal("canonical data changed", err)
	}
	fields, err := db.ListContactFields(t.Context(), "default", profileID)
	if err != nil {
		t.Fatal(err)
	}
	var synced bool
	for _, field := range fields {
		if field.Source == "synced:target" && field.Kind == "phone" && field.Value == latest.Phone && field.Label == latest.PhoneLabel {
			synced = true
		}
	}
	if !synced {
		t.Fatal("source values not acknowledged")
	}
	var queue int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queue); err != nil || queue != 1 {
		t.Fatal("outbound acknowledgement echoed fanout", queue, err)
	}
	if err := db.PublishUserContactSyncResult(t.Context(), d, "late", "late", inboundTestGuard); !errors.Is(err, ErrContactSyncSuperseded) {
		t.Fatal("old empty source snapshot reused", err)
	}
	if err := db.FinishUserContactSyncOperation(t.Context(), c, "done", "discard", time.Time{}, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishUserContactSyncOperation(t.Context(), c, "error", "late", time.Time{}, inboundTestGuard); !errors.Is(err, ErrContactSyncSuperseded) {
		t.Fatal("terminal claim reused", err)
	}
}

func TestUserContactSyncClaimIsolationRecoveryAndRetry(t *testing.T) {
	db, _, id := newContactSyncFixture(t)
	foreign, err := db.EnqueueContactSyncOperation(t.Context(), "other", models.Contact{ID: "foreign-profile", Email: "other@example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := contactSyncClaimFor(t, db, "default")
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM contact_sync_operations WHERE id=?`, foreign).Scan(&status); err != nil || status != "pending" {
		t.Fatal("foreign job claimed", status, err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_sync_operations SET locked_at=datetime('now','-2 minutes') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	newClaim := contactSyncClaimFor(t, db, "default")
	if newClaim.AttemptCount() != 2 {
		t.Fatal("recovery did not replace attempt")
	}
	for _, invoke := range []func() error{
		func() error {
			_, err := db.SnapshotUserContactSyncProfile(t.Context(), c, inboundTestGuard)
			return err
		},
		func() error {
			return db.FinishUserContactSyncOperation(t.Context(), c, "done", "", time.Time{}, inboundTestGuard)
		},
		func() error {
			return db.FinishUserContactSyncOperation(t.Context(), c, "error", "late", time.Time{}, inboundTestGuard)
		},
	} {
		if err := invoke(); !errors.Is(err, ErrContactSyncSuperseded) {
			t.Fatal("old worker overtook replacement", err)
		}
	}
	deferUntil := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	if err := db.FinishUserContactSyncOperation(t.Context(), newClaim, "pending", "provider busy", deferUntil, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	next, err := db.NextUserContactSyncAttempt(t.Context(), "default", time.Minute)
	if err != nil || !next.Equal(deferUntil) {
		t.Fatal("retry deadline lost", next, err)
	}
	claims, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 25, time.Minute, inboundTestGuard)
	if err != nil || len(claims) != 0 {
		t.Fatal("deferred work reclaimed", claims, err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_sync_operations SET next_attempt_at=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	final := contactSyncClaimFor(t, db, "default")
	if final.AttemptCount() != 3 {
		t.Fatal("retry counter reset")
	}
	if err := db.FinishUserContactSyncOperation(t.Context(), final, "error", "failed", time.Time{}, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextUserContactSyncAttempt(t.Context(), "default", time.Minute); err != nil || !next.IsZero() {
		t.Fatal("terminal job kept scheduling", next, err)
	}
}

func TestUserContactSyncRejectsChangesAfterSnapshot(t *testing.T) {
	for _, change := range []string{"canonical", "membership", "sync-off", "deleted-profile", "source", "provider-disabled", "account-deleting", "inactive-owner", "expired", "payload", "lock", "wrong-provider", "excluded", "foreign", "foreign-book"} {
		t.Run(change, func(t *testing.T) {
			db, profileID, _ := newContactSyncFixture(t)
			c := contactSyncClaimFor(t, db, "default")
			p := contactSyncProfileFor(t, db, c)
			d := contactSyncDestinationFor(t, db, p, "target", "outlook", "")
			var err error
			switch change {
			case "canonical":
				err = db.ReplaceCanonicalContact(t.Context(), "default", profileID, models.Contact{Email: "person@example.com", Phone: "new"})
			case "membership":
				err = db.ReplaceContactSyncMemberships(t.Context(), "default", profileID, []string{"account:source"})
			case "sync-off":
				err = db.SetContactProfileSyncEnabled(t.Context(), "default", profileID, false)
			case "deleted-profile":
				_, err = db.Write().Exec(`UPDATE contact_profiles SET is_deleted=1 WHERE id=?`, profileID)
			case "source":
				err = db.UpsertContactSource(t.Context(), ContactSource{UserID: "default", ContactID: profileID, Provider: "outlook", AccountID: "target", RemoteID: "new-source"})
			case "provider-disabled":
				_, err = db.Write().Exec(`INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('target','default','outlook',0)`)
			case "account-deleting":
				_, err = db.Write().Exec(`UPDATE accounts SET is_deleting=1 WHERE id='target'`)
			case "inactive-owner":
				_, err = db.Write().Exec(`UPDATE users SET status='disabled' WHERE id='default'`)
			case "expired":
				c.expires = time.Now().Add(-time.Second)
			case "payload":
				_, err = db.Write().Exec(`UPDATE contact_sync_operations SET payload_json='{}' WHERE id=?`, c.id)
			case "lock":
				_, err = db.Write().Exec(`UPDATE contact_sync_operations SET locked_at=CURRENT_TIMESTAMP WHERE id=?`, c.id)
			case "wrong-provider":
				_, err = db.SnapshotUserContactSyncDestination(t.Context(), p, "target", "gmail", "", inboundTestGuard)
			case "excluded":
				_, err = db.SnapshotUserContactSyncDestination(t.Context(), p, "source", "gmail", "", inboundTestGuard)
			case "foreign":
				_, err = db.SnapshotUserContactSyncDestination(t.Context(), p, "foreign", "gmail", "", inboundTestGuard)
			case "foreign-book":
				_, err = db.SnapshotUserContactSyncDestination(t.Context(), p, "target", "outlook", "dav-book", inboundTestGuard)
			}
			if change == "wrong-provider" || change == "excluded" || change == "foreign" || change == "foreign-book" {
				if err == nil {
					t.Fatal("invalid target accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			if err := db.ValidateUserContactSyncDestination(t.Context(), d, inboundTestGuard); err == nil {
				t.Fatal("stale request validation passed")
			}
			if err := db.PublishUserContactSyncResult(t.Context(), d, "late", "late", inboundTestGuard); err == nil {
				t.Fatal("stale acknowledgement committed")
			}
			if after := inboundPublicationState(t, db); after != before {
				t.Fatal("rejection mutated storage")
			}
		})
	}
}

func TestUserContactSyncAtomicAcknowledgementAndFinalization(t *testing.T) {
	for _, failure := range []string{"card", "fields", "activity"} {
		t.Run(failure, func(t *testing.T) {
			db, _, _ := newContactSyncFixture(t)
			c := contactSyncClaimFor(t, db, "default")
			d := contactSyncDestinationFor(t, db, contactSyncProfileFor(t, db, c), "target", "outlook", "")
			var trigger string
			switch failure {
			case "card":
				trigger = `CREATE TRIGGER fail_contact_ack BEFORE INSERT ON contact_cards BEGIN SELECT RAISE(ABORT,'card failure'); END`
			case "fields":
				trigger = `CREATE TRIGGER fail_contact_ack BEFORE INSERT ON contact_fields WHEN NEW.source='synced:target' BEGIN SELECT RAISE(ABORT,'field failure'); END`
			case "activity":
				trigger = `CREATE TRIGGER fail_contact_ack BEFORE INSERT ON contact_activity_events BEGIN SELECT RAISE(ABORT,'event failure'); END`
			}
			if _, err := db.Write().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			if failure == "activity" {
				if err := db.FinishUserContactSyncOperation(t.Context(), c, "done", "", time.Time{}, inboundTestGuard); err == nil {
					t.Fatal("failure not injected")
				}
			} else if err := db.PublishUserContactSyncResult(t.Context(), d, "remote", "version", inboundTestGuard); err == nil {
				t.Fatal("failure not injected")
			}
			if inboundPublicationState(t, db) != before {
				t.Fatal("partial publication")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER fail_contact_ack`); err != nil {
				t.Fatal(err)
			}
			if failure == "activity" {
				if err := db.FinishUserContactSyncOperation(t.Context(), c, "done", "", time.Time{}, inboundTestGuard); err != nil {
					t.Fatal(err)
				}
			} else if err := db.PublishUserContactSyncResult(t.Context(), d, "remote", "version", inboundTestGuard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactSyncDAVBookScopeAndRemoteIdentity(t *testing.T) {
	db, profileID, _ := newContactSyncFixture(t)
	if err := db.ReplaceContactSyncMemberships(t.Context(), "default", profileID, []string{"book:dav-book"}); err != nil {
		t.Fatal(err)
	}
	c := contactSyncClaimFor(t, db, "default")
	p := contactSyncProfileFor(t, db, c)
	d := contactSyncDestinationFor(t, db, p, "dav", "carddav", "dav-book")
	for _, remote := range []string{"", "https://dav.test/other/a.vcf", "https://foreign.test/book/a.vcf", "https://dav.test/book/../other/a.vcf", "https://dav.test/book/%2e%2e/other/a.vcf", "https://dav.test/book/a.vcf#fragment"} {
		if err := db.PublishUserContactSyncResult(t.Context(), d, remote, "", inboundTestGuard); !errors.Is(err, ErrContactPublication) {
			t.Fatal("out-of-book acknowledgement", err)
		}
	}
	if err := db.PublishUserContactSyncResult(t.Context(), d, "https://dav.test/book/a.vcf", "v1", inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	d = contactSyncDestinationFor(t, db, p, "dav", "carddav", "dav-book")
	source := d.Source()
	source.Etag = "escaped"
	if d.Source().Etag != "v1" {
		t.Fatal("source escaped")
	}
	if err := db.PublishUserContactSyncResult(t.Context(), d, "https://dav.test/book/b.vcf", "v2", inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE account_id='dav'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("source replacement duplicated card", count, err)
	}
	other := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "separate@example.com"}, RemoteID: "people/separate"})
	if err := db.UpsertContactSource(t.Context(), ContactSource{UserID: "default", ContactID: other.ProfileID, Provider: "carddav", AccountID: "dav", AddressBookID: "dav-book", RemoteID: "https://dav.test/book/stolen.vcf"}); err != nil {
		t.Fatal(err)
	}
	d = contactSyncDestinationFor(t, db, p, "dav", "carddav", "dav-book")
	before := inboundPublicationState(t, db)
	if err := db.ValidateUserContactSyncRemote(t.Context(), d, "https://dav.test/book/stolen.vcf", inboundTestGuard); !errors.Is(err, ErrContactPublication) {
		t.Fatal("preflight allowed another profile's remote", err)
	}
	if err := db.PublishUserContactSyncResult(t.Context(), d, "https://dav.test/book/stolen.vcf", "", inboundTestGuard); !errors.Is(err, ErrContactPublication) {
		t.Fatal("remote reassigned from another profile", err)
	}
	if inboundPublicationState(t, db) != before {
		t.Fatal("stolen card moved")
	}
	if _, err := db.Write().Exec(`UPDATE contact_cards SET is_deleted=1 WHERE account_id='dav' AND profile_id=?`, other.ProfileID); err != nil {
		t.Fatal(err)
	}
	before = inboundPublicationState(t, db)
	if err := db.PublishUserContactSyncResult(t.Context(), d, "https://dav.test/book/stolen.vcf", "", inboundTestGuard); !errors.Is(err, ErrContactPublication) {
		t.Fatal("deleted history moved from another profile", err)
	}
	if inboundPublicationState(t, db) != before {
		t.Fatal("deleted card moved")
	}
}

func TestUserContactSyncClaimRollbackMissingDisabledAndCancellation(t *testing.T) {
	db, profileID, _ := newContactSyncFixture(t)
	if _, err := db.Write().Exec(`INSERT INTO contact_sync_operations(id,user_id,contact_id,email,payload_json,created_at) VALUES('bad','default','bad','bad@example.com','invalid','9999-01-01')`); err != nil {
		t.Fatal(err)
	}
	before := inboundPublicationState(t, db)
	if claims, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 25, time.Minute, inboundTestGuard); err == nil || claims != nil {
		t.Fatal("malformed batch returned claims", err)
	}
	if inboundPublicationState(t, db) != before {
		t.Fatal("claim batch partially committed")
	}
	if _, err := db.Write().Exec(`DELETE FROM contact_sync_operations WHERE id='bad'`); err != nil {
		t.Fatal(err)
	}
	c := contactSyncClaimFor(t, db, "default")
	if err := db.SetContactProfileSyncEnabled(t.Context(), "default", profileID, false); err != nil {
		t.Fatal(err)
	}
	if p, err := db.SnapshotUserContactSyncProfile(t.Context(), c, inboundTestGuard); err != nil || p != nil {
		t.Fatal("disabled profile not cancelled", p, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before = inboundPublicationState(t, db)
	if err := db.FinishUserContactSyncOperation(ctx, c, "done", "", time.Time{}, inboundTestGuard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if inboundPublicationState(t, db) != before {
		t.Fatal("cancelled finalization wrote")
	}
	if err := db.FinishUserContactSyncOperation(t.Context(), c, "done", "", time.Time{}, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	missing, err := db.EnqueueContactSyncOperation(t.Context(), "default", models.Contact{ID: "missing", Email: "missing@example.com"}, nil)
	if err != nil || missing == "" {
		t.Fatal(err)
	}
	c = contactSyncClaimFor(t, db, "default")
	if p, err := db.SnapshotUserContactSyncProfile(t.Context(), c, inboundTestGuard); err != nil || p != nil {
		t.Fatal("missing profile not cancelled", p, err)
	}
	if err := db.FinishUserContactSyncOperation(t.Context(), c, "done", "", time.Time{}, inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	if claims, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 25, time.Minute, nil); err == nil || claims != nil {
		t.Fatal("unguarded claims accepted")
	}
	if claims, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 25, time.Minute, func(*sql.Tx) error { return errors.New("lifecycle changed") }); err == nil || claims != nil {
		t.Fatal("guard ignored")
	}
}

func TestUserContactSyncRejectsCommitFailureWithoutCandidates(t *testing.T) {
	for _, stage := range []string{"claim", "ack", "finish"} {
		t.Run(stage, func(t *testing.T) {
			db, _, _ := newContactSyncFixture(t)
			var claim *ContactSyncClaim
			var destination *ContactSyncDestination
			if stage != "claim" {
				claim = contactSyncClaimFor(t, db, "default")
				destination = contactSyncDestinationFor(t, db, contactSyncProfileFor(t, db, claim), "target", "outlook", "")
			}
			if _, err := db.Write().Exec(`CREATE TABLE contact_commit_failure(owner TEXT REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"claim":  `CREATE TRIGGER fail_contact_commit AFTER UPDATE ON contact_sync_operations WHEN NEW.status='running' BEGIN INSERT INTO contact_commit_failure VALUES('missing-owner'); END`,
				"ack":    `CREATE TRIGGER fail_contact_commit AFTER INSERT ON contact_cards WHEN NEW.account_id='target' BEGIN INSERT INTO contact_commit_failure VALUES('missing-owner'); END`,
				"finish": `CREATE TRIGGER fail_contact_commit AFTER INSERT ON contact_activity_events WHEN NEW.event_type='contact_synced' BEGIN INSERT INTO contact_commit_failure VALUES('missing-owner'); END`,
			}[stage]
			if _, err := db.Write().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			switch stage {
			case "claim":
				if claims, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 1, time.Minute, inboundTestGuard); err == nil || claims != nil {
					t.Fatal("failed COMMIT returned claims", err)
				}
			case "ack":
				if err := db.PublishUserContactSyncResult(t.Context(), destination, "remote", "tag", inboundTestGuard); err == nil {
					t.Fatal("COMMIT succeeded")
				}
			case "finish":
				if err := db.FinishUserContactSyncOperation(t.Context(), claim, "done", "", time.Time{}, inboundTestGuard); err == nil {
					t.Fatal("COMMIT succeeded")
				}
			}
			if inboundPublicationState(t, db) != before {
				t.Fatal("COMMIT failure left writes")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER fail_contact_commit`); err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "claim":
				contactSyncClaimFor(t, db, "default")
			case "ack":
				if err := db.PublishUserContactSyncResult(t.Context(), destination, "remote", "tag", inboundTestGuard); err != nil {
					t.Fatal(err)
				}
			case "finish":
				if err := db.FinishUserContactSyncOperation(t.Context(), claim, "done", "", time.Time{}, inboundTestGuard); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestUserContactSyncLegacyProfileAndMultipleDestinations(t *testing.T) {
	for _, canonical := range []bool{true, false} {
		t.Run(map[bool]string{true: "canonical", false: "legacy"}[canonical], func(t *testing.T) {
			db, profileID, _ := newContactSyncFixture(t)
			if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,email_address) VALUES('second','default','gmail','second@example.com')`); err != nil {
				t.Fatal(err)
			}
			if err := db.ReplaceContactSyncMemberships(t.Context(), "default", profileID, []string{"account:target", "account:second"}); err != nil {
				t.Fatal(err)
			}
			if !canonical {
				if _, err := db.Write().Exec(`DELETE FROM contact_fields WHERE profile_id=? AND source='canonical'`, profileID); err != nil {
					t.Fatal(err)
				}
			}
			c := contactSyncClaimFor(t, db, "default")
			p := contactSyncProfileFor(t, db, c)
			for _, target := range []struct{ account, provider, remote string }{{"target", "outlook", "graph"}, {"second", "gmail", "people/second"}} {
				d := contactSyncDestinationFor(t, db, p, target.account, target.provider, "")
				if err := db.PublishUserContactSyncResult(t.Context(), d, target.remote, "tag", inboundTestGuard); err != nil {
					t.Fatal("own acknowledgement invalidated later target", err)
				}
			}
			if err := db.FinishUserContactSyncOperation(t.Context(), c, "done", "", time.Time{}, inboundTestGuard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactSyncRecoveryDeadlineAndInvalidTargets(t *testing.T) {
	db, profileID, id := newContactSyncFixture(t)
	if err := db.ReplaceContactSyncMemberships(t.Context(), "default", profileID, []string{"account:target", "account:foreign", "book:dav-book"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET enabled=0 WHERE account_id='dav'`); err != nil {
		t.Fatal(err)
	}
	c := contactSyncClaimFor(t, db, "default")
	p := contactSyncProfileFor(t, db, c)
	if targets := p.Targets(); len(targets) != 1 || targets[0].AccountID != "target" {
		t.Fatal("unowned/disabled targets escaped", targets)
	}
	next, err := db.NextUserContactSyncAttempt(t.Context(), "default", time.Minute)
	if err != nil || !next.Equal(c.expires) {
		t.Fatal("running work lost recovery schedule", next, c.expires, err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_sync_operations SET locked_at=datetime('now','-2 minutes') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	next, err = db.NextUserContactSyncAttempt(t.Context(), "default", time.Minute)
	if err != nil || next.After(time.Now()) {
		t.Fatal("abandoned work not due", next, err)
	}
	newClaim := contactSyncClaimFor(t, db, "default")
	if newClaim.AttemptCount() != 2 {
		t.Fatal("recovery not claimable")
	}
}

func TestUserContactSyncExistingSourceChangesAndAmbiguity(t *testing.T) {
	for _, change := range []string{"etag", "remote", "deleted", "other-profile", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			db, profileID, _ := newContactSyncFixture(t)
			if err := db.UpsertContactSource(t.Context(), ContactSource{UserID: "default", ContactID: profileID, Provider: "outlook", AccountID: "target", RemoteID: "graph", Etag: "v1"}); err != nil {
				t.Fatal(err)
			}
			c := contactSyncClaimFor(t, db, "default")
			d := contactSyncDestinationFor(t, db, contactSyncProfileFor(t, db, c), "target", "outlook", "")
			q := map[string]string{"etag": `UPDATE contact_cards SET etag='v2' WHERE account_id='target'`, "remote": `UPDATE contact_cards SET remote_id='replacement' WHERE account_id='target'`, "deleted": `UPDATE contact_cards SET is_deleted=1 WHERE account_id='target'`, "other-profile": `UPDATE contact_cards SET profile_id=? WHERE account_id='target'`, "duplicate": `INSERT INTO contact_cards(id,user_id,profile_id,kind,provider,account_id,remote_id) VALUES('duplicate','default',?,'provider','outlook','target','other')`}[change]
			var args []any
			if change == "other-profile" {
				other := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "other@example.com"}, RemoteID: "people/other"})
				args = []any{other.ProfileID}
			} else if change == "duplicate" {
				args = []any{profileID}
			}
			if _, err := db.Write().Exec(q, args...); err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			if err := db.PublishUserContactSyncResult(t.Context(), d, "graph", "late", inboundTestGuard); err == nil {
				t.Fatal("new source state overwritten")
			}
			if inboundPublicationState(t, db) != before {
				t.Fatal("stale acknowledgement wrote")
			}
		})
	}
}

func TestUserContactSyncConcurrentClaimsAreExclusiveAndBounded(t *testing.T) {
	db, profileID, _ := newContactSyncFixture(t)
	for i := 0; i < 7; i++ {
		if _, err := db.EnqueueContactSyncOperation(t.Context(), "default", models.Contact{ID: profileID, Email: "same@example.com"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	claims := make(chan []*ContactSyncClaim, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copied, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 3, time.Minute, inboundTestGuard)
			claims <- copied
			failures <- err
		}()
	}
	wg.Wait()
	close(claims)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for batch := range claims {
		if len(batch) != 3 {
			t.Fatal("claim limit lost", len(batch))
		}
		for _, c := range batch {
			if seen[c.OperationID()] {
				t.Fatal("job claimed concurrently")
			}
			seen[c.OperationID()] = true
		}
	}
	if len(seen) != 6 {
		t.Fatal("wrong number of claims")
	}
	remaining, err := db.ClaimUserContactSyncOperations(t.Context(), "default", 3, time.Minute, inboundTestGuard)
	if err != nil || len(remaining) != 2 {
		t.Fatal("remaining claims", len(remaining), err)
	}
}

func TestUserContactSyncRejectsUnboundCopies(t *testing.T) {
	db := newInboundContactFixture(t)
	for _, p := range []*ContactSyncProfile{nil, {}, {claim: &ContactSyncClaim{}}} {
		if d, err := db.SnapshotUserContactSyncDestination(t.Context(), p, "target", "outlook", "", inboundTestGuard); err == nil || d != nil {
			t.Fatal("unbound profile accepted")
		}
	}
	for _, d := range []*ContactSyncDestination{nil, {}, {profile: &ContactSyncProfile{}}, {profile: &ContactSyncProfile{claim: &ContactSyncClaim{}}}} {
		if err := db.ValidateUserContactSyncDestination(t.Context(), d, inboundTestGuard); err == nil {
			t.Fatal("unbound destination validated")
		}
		if err := db.PublishUserContactSyncResult(t.Context(), d, "remote", "", inboundTestGuard); err == nil {
			t.Fatal("unbound acknowledgement accepted")
		}
	}
}
