package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestOwnedAvatarWarmupSharesWorkerAndScopesEvents(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	cache := New(f.system, nil, mail.NewSyncOrchestrator(f.system, nil, nil, nil), f.h.blobStore, nil, "")
	cache.avatarRouting = f.h.userStorage
	f.h.avatarWarmupOwner = cache
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	previous := http.DefaultTransport
	http.DefaultTransport = calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.gravatar.com" {
			t.Error("unexpected avatar provider", r.URL.Host)
		}
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader("shared-avatar")), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); cache.WaitAvatarWorkers() })
	cache.startAvatarWarmupWorkers(t.Context())
	events := cache.syncer.Events().Subscribe()
	defer cache.syncer.Events().Unsubscribe(events)
	request := func(owner string) int {
		r := httptest.NewRecorder()
		f.h.handleAvatarWarmup(r, ownedContentRequest(owner, "/api/avatars/warmup", `{"emails":["sender@example.com"]}`))
		var result struct {
			Queued int `json:"queued"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil {
			t.Fatal(owner, r.Code, r.Body.String())
		}
		return result.Queued
	}
	if request("alice") != 1 {
		t.Fatal("Alice not queued")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warmup did not fetch")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.h.withUserDB(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal("provider held owned store", err)
	}
	if request("bob") != 0 {
		t.Fatal("same sender queued twice")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case e := <-events:
		if e.Type != mail.EventAvatarUpdated || len(e.UserIDs) != 2 || !sseEventVisible(e, "alice", nil, false) || !sseEventVisible(e, "bob", nil, false) || sseEventVisible(e, "charlie", nil, false) {
			t.Fatal("wrong update scope", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no scoped update")
	}
	hash := avatarresolver.GravatarHash("sender@example.com")
	if rec, err := f.system.GetSenderAvatarByHash(t.Context(), hash); err != nil || rec == nil || rec.Status != "found" {
		t.Fatal("warmup not persisted centrally", rec, err)
	}
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
		email, err := db.GetEmailByIDForUser(t.Context(), "1", "alice")
		if err == nil && (email == nil || !strings.Contains(email.From.AvatarURL, "/api/avatars/"+hash)) {
			t.Fatal("mail view did not hydrate central avatar", email)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DELETE FROM messages WHERE id=1`); return err }); err != nil {
		t.Fatal(err)
	}
	cache.publishAvatarUpdated(t.Context(), hash, "sender@example.com", time.Now().Add(time.Hour))
	select {
	case e := <-events:
		if len(e.UserIDs) != 1 || e.UserIDs[0] != "bob" {
			t.Fatal("stale interest authorized update", e)
		}
	default:
		t.Fatal("Bob lost update")
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	cache.publishAvatarUpdated(t.Context(), hash, "sender@example.com", time.Now().Add(time.Hour))
	select {
	case e := <-events:
		t.Fatal("disabled owner got update", e)
	default:
	}
	cache.WaitAvatarWorkers()
	if cache.enqueueAvatarWarmup(storage.SenderAvatarCandidate{EmailHash: hash, Email: "sender@example.com"}) {
		t.Fatal("warmup admitted after shutdown")
	}
}

func TestOwnedAvatarDiscoveryUsesLocalSourcesAndNoEmptyReplacement(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	cache := &Handler{db: f.system, avatarRouting: f.h.userStorage}
	if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('idle','idle','idle')`); err != nil {
		t.Fatal(err)
	}
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), "alice", models.Contact{Email: "contact@example.com", Name: "Contact"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.ensureAvatarCandidates(t.Context()); err != nil {
		t.Fatal(err)
	}
	idleHash := sha256.Sum256([]byte("idle"))
	if _, err := os.Stat(filepath.Join(f.system.Path()+".users", fmt.Sprintf("%x.db", idleHash))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery created idle store", err)
	}
	if rec, err := f.system.GetSenderAvatarByHash(t.Context(), avatarresolver.GravatarHash("sender@example.com")); err != nil || rec == nil {
		t.Fatal("missing owned discovery", rec, err)
	}
	if rec, err := f.system.GetSenderAvatarByHash(t.Context(), avatarresolver.GravatarHash("contact@example.com")); err != nil || rec != nil {
		t.Fatal("automatic sender discovery looked up contacts", rec, err)
	}
	if _, err := f.h.userStorage.ListAvatarInterestedUsers(t.Context(), avatarresolver.GravatarHash("sender@example.com"), "", 64); err != nil {
		t.Fatal(err)
	}
	var interests int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_avatar_interests`).Scan(&interests); err != nil || interests != 2 {
		t.Fatal("wrong interested owners", interests, err)
	}
	var accounts int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatal("discovery depended on central accounts", accounts, err)
	}
	var alicePath string
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { alicePath = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.h.withUserDB(t.Context(), "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alicePath); err != nil {
		t.Fatal(err)
	}
	if err := cache.ensureAvatarCandidates(t.Context()); err == nil {
		t.Fatal("missing populated store was ignored")
	}
	if _, err := os.Stat(alicePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing store replaced", err)
	}
}

func TestOwnedAvatarWarmupCancellationJoinsProvider(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	cache := New(f.system, nil, mail.NewSyncOrchestrator(f.system, nil, nil, nil), f.h.blobStore, nil, "")
	cache.avatarRouting = f.h.userStorage
	f.h.avatarWarmupOwner = cache
	entered := make(chan struct{})
	var once sync.Once
	previous := http.DefaultTransport
	http.DefaultTransport = calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	t.Cleanup(cache.WaitAvatarWorkers)
	cache.startAvatarWarmupWorkers(t.Context())
	r := httptest.NewRecorder()
	f.h.handleAvatarWarmup(r, ownedContentRequest("alice", "/api/avatars/warmup", `{"emails":["sender@example.com"]}`))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no provider request")
	}
	done := make(chan struct{})
	go func() { defer close(done); cache.WaitAvatarWorkers() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("avatar shutdown did not join provider")
	}
	rec, err := f.system.GetSenderAvatarByHash(t.Context(), avatarresolver.GravatarHash("sender@example.com"))
	if err != nil || rec == nil || rec.Status == "found" {
		t.Fatal("canceled lookup published image", rec, err)
	}
}

func TestOwnedProviderAvatarCentralProfileAndStaleResponse(t *testing.T) {
	for _, mode := range []string{"central-profile", "contact-change", "owner-disable", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedMessageContentFixture(t)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			f.h.userStorageContext = ctx
			url := "https://lh3.googleusercontent.com/private-photo"
			oldURL := "https://lh3.googleusercontent.com/stale-profile"
			if mode == "central-profile" {
				if _, err := f.system.Write().Exec(`UPDATE users SET avatar_url=? WHERE id='alice'`, url); err != nil {
					t.Fatal(err)
				}
				if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE users SET avatar_url=? WHERE id='alice'`, oldURL)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.SaveContact(t.Context(), "alice", models.Contact{ID: "photo-contact", Email: "photo@example.com", AvatarURL: url})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			f.h.providerAvatarHTTPClient = &http.Client{Transport: calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader("private-photo")), Request: r}, nil
			})}
			if mode == "central-profile" {
				r := httptest.NewRecorder()
				f.h.handleProviderAvatarImage(r, ownedContentRequest("alice", "/api/provider-avatar?url="+oldURL, ""))
				if r.Code != 404 {
					t.Fatal("stale local profile authorized", r.Code)
				}
			}
			r := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				f.h.handleProviderAvatarImage(r, ownedContentRequest("alice", "/api/provider-avatar?url="+url, ""))
			}()
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(func() { stop(); unblock(); <-done })
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no avatar request")
			}
			probe, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := f.h.withUserDB(probe, "bob", func(*storage.DB) error { return nil }); err != nil {
				t.Fatal("provider held store", err)
			}
			switch mode {
			case "contact-change":
				if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE contact_profiles SET avatar_url='' WHERE id='photo-contact'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "owner-disable":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				stop()
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("avatar did not stop")
			}
			if mode == "central-profile" {
				if r.Code != 200 || r.Body.String() != "private-photo" {
					t.Fatal("current central profile", r.Code, r.Body.String())
				}
			} else if r.Code == 200 || strings.Contains(r.Body.String(), "private-photo") {
				t.Fatal("stale provider photo exposed", r.Code, r.Body.String())
			}
		})
	}
}
