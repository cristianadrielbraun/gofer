package auth

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var loginThrottleTestKey = []byte("login-throttle-test-key-32-bytes!")

func openLoginThrottleTestManager(t *testing.T, path string, clock Clock, key []byte) (*Manager, *storage.DB) {
	t.Helper()
	db, err := storage.New(path)
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	manager := NewManager(&Config{Enabled: true}, db, Dependencies{
		Clock:         clock,
		BucketHashKey: key,
	})
	return manager, db
}

func TestLoginThrottleAppliesIdentifierSourceAndInstancePolicies(t *testing.T) {
	now := time.Date(2026, time.August, 5, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		attempts int
		values   func(int) (string, string)
	}{
		{
			name:     "identifier",
			attempts: int(loginThrottlePolicies[loginThrottleBucketIdentifier].delayStartsAt),
			values: func(attempt int) (string, string) {
				return "Person@Example.com", fmt.Sprintf("198.51.100.%d", attempt)
			},
		},
		{
			name:     "source",
			attempts: int(loginThrottlePolicies[loginThrottleBucketSource].delayStartsAt),
			values: func(attempt int) (string, string) {
				return fmt.Sprintf("person-%d", attempt), "198.51.100.10"
			},
		},
		{
			name:     "instance",
			attempts: int(loginThrottlePolicies[loginThrottleBucketInstance].delayStartsAt),
			values: func(attempt int) (string, string) {
				return fmt.Sprintf("person-%d", attempt), fmt.Sprintf("203.0.113.%d", attempt)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &fixedClock{now: now}
			manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
			t.Cleanup(func() { _ = db.Close() })

			for attempt := 1; attempt <= test.attempts; attempt++ {
				identifier, source := test.values(attempt)
				decision, err := manager.RecordLoginFailure(t.Context(), identifier, source)
				if err != nil {
					t.Fatalf("RecordLoginFailure(attempt %d) error = %v", attempt, err)
				}
				if attempt < test.attempts && decision.Throttled {
					t.Fatalf("attempt %d unexpectedly throttled until %v", attempt, decision.RetryAt)
				}
				if attempt == test.attempts {
					wantRetryAt := now.Add(time.Second)
					if !decision.Throttled || !decision.RetryAt.Equal(wantRetryAt) {
						t.Fatalf("threshold decision = %#v, want retry at %v", decision, wantRetryAt)
					}
				}
			}
		})
	}
}

func TestLoginThrottleExponentialBackoffCapAndRecovery(t *testing.T) {
	now := time.Date(2026, time.August, 5, 10, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = db.Close() })

	identifierPolicy := loginThrottlePolicies[loginThrottleBucketIdentifier]
	var decision LoginThrottleDecision
	for attempt := int64(1); attempt <= identifierPolicy.delayStartsAt+10; attempt++ {
		attemptedAt := clock.now
		var err error
		decision, err = manager.RecordLoginFailure(t.Context(), "person@example.com", fmt.Sprintf("source-%d", attempt))
		if err != nil {
			t.Fatalf("RecordLoginFailure(attempt %d) error = %v", attempt, err)
		}
		if decision.Throttled {
			if attempt == identifierPolicy.delayStartsAt+10 {
				if got := decision.RetryAt.Sub(attemptedAt); got != identifierPolicy.maximumDelay {
					t.Fatalf("maximum backoff = %v, want %v", got, identifierPolicy.maximumDelay)
				}
			}
			clock.now = decision.RetryAt
		}
	}

	blockedAt := clock.now
	decision, err := manager.RecordLoginFailure(t.Context(), "person@example.com", "another-source")
	if err != nil {
		t.Fatalf("RecordLoginFailure(capped) error = %v", err)
	}
	if !decision.Throttled || !decision.RetryAt.Equal(blockedAt.Add(identifierPolicy.maximumDelay)) {
		t.Fatalf("capped decision = %#v, want %v", decision, blockedAt.Add(identifierPolicy.maximumDelay))
	}

	clock.now = blockedAt.Add(identifierPolicy.retention + time.Second)
	decision, err = manager.RecordLoginFailure(t.Context(), "person@example.com", "recovered-source")
	if err != nil {
		t.Fatalf("RecordLoginFailure(after recovery) error = %v", err)
	}
	if decision.Throttled {
		t.Fatalf("first failure after recovery = %#v, want unthrottled", decision)
	}
}

func TestLoginThrottleSuccessClearsOnlyIdentifierBucket(t *testing.T) {
	now := time.Date(2026, time.August, 5, 11, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = db.Close() })

	for attempt := int64(1); attempt <= loginThrottlePolicies[loginThrottleBucketIdentifier].delayStartsAt; attempt++ {
		if _, err := manager.RecordLoginFailure(t.Context(), "Person@Example.com", fmt.Sprintf("identifier-source-%d", attempt)); err != nil {
			t.Fatalf("RecordLoginFailure(identifier %d) error = %v", attempt, err)
		}
	}
	if err := manager.RecordLoginSuccess(t.Context(), " person@example.COM "); err != nil {
		t.Fatalf("RecordLoginSuccess() error = %v", err)
	}
	decision, err := manager.CheckLoginThrottle(t.Context(), "PERSON@example.com", "new-source")
	if err != nil || decision.Throttled {
		t.Fatalf("CheckLoginThrottle(after success) = %#v, %v, want allowed", decision, err)
	}

	sharedSource := "198.51.100.90"
	for attempt := int64(1); attempt <= loginThrottlePolicies[loginThrottleBucketSource].delayStartsAt; attempt++ {
		if _, err := manager.RecordLoginFailure(t.Context(), fmt.Sprintf("source-person-%d", attempt), sharedSource); err != nil {
			t.Fatalf("RecordLoginFailure(source %d) error = %v", attempt, err)
		}
	}
	if err := manager.RecordLoginSuccess(t.Context(), "source-person-20"); err != nil {
		t.Fatalf("RecordLoginSuccess(shared source) error = %v", err)
	}
	decision, err = manager.CheckLoginThrottle(t.Context(), "unseen-person", sharedSource)
	if err != nil || !decision.Throttled {
		t.Fatalf("CheckLoginThrottle(shared source) = %#v, %v, want throttled", decision, err)
	}
}

func TestLoginThrottleStoresOnlyKeyedHashes(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = db.Close() })

	identifier := " Sensitive.User@Example.COM "
	source := " 203.0.113.42:8443 "
	if _, err := manager.RecordLoginFailure(t.Context(), identifier, source); err != nil {
		t.Fatalf("RecordLoginFailure() error = %v", err)
	}

	rows, err := db.Read().QueryContext(t.Context(), `SELECT bucket_hash, action FROM auth_throttle`)
	if err != nil {
		t.Fatalf("query auth_throttle: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var bucketHash, action string
		if err := rows.Scan(&bucketHash, &action); err != nil {
			t.Fatalf("scan auth_throttle: %v", err)
		}
		count++
		if len(bucketHash) != sha256HexLength || strings.Contains(strings.ToLower(bucketHash), "sensitive") || strings.Contains(bucketHash, "203.0.113") {
			t.Fatalf("unsafe bucket hash %q", bucketHash)
		}
		if action != loginThrottleAction {
			t.Fatalf("action = %q, want %q", action, loginThrottleAction)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate auth_throttle: %v", err)
	}
	if count != 3 {
		t.Fatalf("stored bucket count = %d, want 3", count)
	}

	hash, err := manager.loginThrottleBucketHash(loginThrottleBucketIdentifier, "sensitive.user@example.com")
	if err != nil {
		t.Fatalf("loginThrottleBucketHash() error = %v", err)
	}
	var storedCount int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle WHERE bucket_hash = ?`, hash).Scan(&storedCount); err != nil {
		t.Fatalf("query canonical identifier bucket: %v", err)
	}
	if storedCount != 1 {
		t.Fatalf("canonical identifier bucket count = %d, want 1", storedCount)
	}
}

func TestLoginThrottlePersistsAcrossRestart(t *testing.T) {
	now := time.Date(2026, time.August, 5, 13, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	path := filepath.Join(t.TempDir(), "gofer.db")
	manager, db := openLoginThrottleTestManager(t, path, clock, loginThrottleTestKey)

	for attempt := int64(1); attempt <= loginThrottlePolicies[loginThrottleBucketIdentifier].delayStartsAt; attempt++ {
		if _, err := manager.RecordLoginFailure(t.Context(), "person@example.com", fmt.Sprintf("source-%d", attempt)); err != nil {
			t.Fatalf("RecordLoginFailure(%d) error = %v", attempt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first database: %v", err)
	}

	restarted, restartedDB := openLoginThrottleTestManager(t, path, clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = restartedDB.Close() })
	decision, err := restarted.CheckLoginThrottle(t.Context(), "PERSON@EXAMPLE.COM", "unseen-source")
	wantRetryAt := now.Add(time.Second)
	if err != nil || !decision.Throttled || !decision.RetryAt.Equal(wantRetryAt) {
		t.Fatalf("CheckLoginThrottle(after restart) = %#v, %v, want retry at %v", decision, err, wantRetryAt)
	}
}

func TestLoginThrottleConcurrentFailuresAreNotLost(t *testing.T) {
	now := time.Date(2026, time.August, 5, 14, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = db.Close() })

	const attempts = 50
	errors := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := manager.RecordLoginFailure(t.Context(), "person@example.com", "198.51.100.15")
			errors <- err
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("RecordLoginFailure(concurrent) error = %v", err)
		}
	}

	for _, bucket := range []struct {
		kind  loginThrottleBucketKind
		value string
	}{
		{loginThrottleBucketIdentifier, "person@example.com"},
		{loginThrottleBucketSource, "198.51.100.15"},
		{loginThrottleBucketInstance, "gofer"},
	} {
		hash, err := manager.loginThrottleBucketHash(bucket.kind, bucket.value)
		if err != nil {
			t.Fatalf("loginThrottleBucketHash(%s) error = %v", bucket.kind, err)
		}
		var failureCount int
		if err := db.Read().QueryRowContext(t.Context(), `SELECT failure_count FROM auth_throttle WHERE bucket_hash = ?`, hash).Scan(&failureCount); err != nil {
			t.Fatalf("query %s bucket: %v", bucket.kind, err)
		}
		if failureCount != attempts {
			t.Fatalf("%s failure count = %d, want %d", bucket.kind, failureCount, attempts)
		}
	}
}

func TestLoginThrottleFailureRollsBackEveryBucket(t *testing.T) {
	now := time.Date(2026, time.August, 5, 14, 30, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = db.Close() })

	instanceHash, err := manager.loginThrottleBucketHash(loginThrottleBucketInstance, "gofer")
	if err != nil {
		t.Fatalf("loginThrottleBucketHash(instance) error = %v", err)
	}
	if _, err := db.Write().ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER reject_instance_throttle
		BEFORE INSERT ON auth_throttle
		WHEN NEW.bucket_hash = '%s'
		BEGIN
			SELECT RAISE(ABORT, 'reject instance bucket');
		END`, instanceHash)); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if _, err := manager.RecordLoginFailure(t.Context(), "person@example.com", "198.51.100.16"); err == nil {
		t.Fatal("RecordLoginFailure() error = nil, want forced failure")
	}
	var count int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle`).Scan(&count); err != nil {
		t.Fatalf("count buckets after rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("bucket count after rollback = %d, want 0", count)
	}
}

func TestLoginThrottleCleanupUsesBoundedBatches(t *testing.T) {
	now := time.Date(2026, time.August, 5, 15, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, loginThrottleTestKey)
	t.Cleanup(func() { _ = db.Close() })

	tx, err := db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	for index := 0; index < loginThrottleCleanupBatchSize+1; index++ {
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO auth_throttle (
				bucket_hash, action, failure_count, first_attempt_at,
				last_attempt_at, expires_at
			) VALUES (?, ?, 1, ?, ?, ?)`,
			fmt.Sprintf("%064x", index+1), loginThrottleAction,
			now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Minute),
		); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed bucket %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}

	if err := manager.CleanupExpiredLoginThrottle(t.Context()); err != nil {
		t.Fatalf("CleanupExpiredLoginThrottle(first) error = %v", err)
	}
	var count int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle`).Scan(&count); err != nil {
		t.Fatalf("count after first cleanup: %v", err)
	}
	if count != 1 {
		t.Fatalf("bucket count after first cleanup = %d, want 1", count)
	}
	if err := manager.CleanupExpiredLoginThrottle(t.Context()); err != nil {
		t.Fatalf("CleanupExpiredLoginThrottle(second) error = %v", err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle`).Scan(&count); err != nil {
		t.Fatalf("count after second cleanup: %v", err)
	}
	if count != 0 {
		t.Fatalf("bucket count after second cleanup = %d, want 0", count)
	}
}

func TestLoginThrottleRequiresKeyedHashSecret(t *testing.T) {
	now := time.Date(2026, time.August, 5, 16, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager, db := openLoginThrottleTestManager(t, filepath.Join(t.TempDir(), "gofer.db"), clock, nil)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := manager.CheckLoginThrottle(t.Context(), "person@example.com", "source"); err == nil {
		t.Fatal("CheckLoginThrottle() error = nil, want missing key rejection")
	}
	if _, err := manager.RecordLoginFailure(t.Context(), "person@example.com", "source"); err == nil {
		t.Fatal("RecordLoginFailure() error = nil, want missing key rejection")
	}
	if err := manager.RecordLoginSuccess(t.Context(), "person@example.com"); err == nil {
		t.Fatal("RecordLoginSuccess() error = nil, want missing key rejection")
	}
}

const sha256HexLength = 64
