package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type userFileActivities struct {
	mu     sync.Mutex
	owners map[string]*userFileActivity
}
type userFileActivity struct {
	readers  int
	cleaning bool
	removing bool
	done     chan struct{}
	drained  chan struct{}
}

var ErrUserFilesRemoving = errors.New("user files are being removed")

// SyncUserRemoval makes account/compose namespace unlinks durable before the
// central final-cleanup receipt. Call with exclusive owner-file admission after
// idempotent account and compose removal; it does not remove any files itself.
// Windows lacks portable directory fsync through os.File, as for user DB removal.
func (s *BlobStore) SyncUserRemoval(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, path := range []string{filepath.Join(s.basePath, "_compose"), s.basePath} {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// PinUserFiles protects copied paths and unpublished candidates, without a
// database lease. Acquire before leasing storage; nested pins are allowed.
// Cleanup skips busy owners, and new operations wait for an existing pass.
func (s *BlobStore) PinUserFiles(ctx context.Context, owner string) (func(), error) {
	if _, err := composeOwnerKey(owner); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.activity.mu.Lock()
		a := s.activity.owners[owner]
		if a == nil {
			a = &userFileActivity{}
			s.activity.owners[owner] = a
		}
		if a.removing {
			s.activity.mu.Unlock()
			return nil, ErrUserFilesRemoving
		}
		if !a.cleaning {
			a.readers++
			s.activity.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					s.activity.mu.Lock()
					defer s.activity.mu.Unlock()
					a.readers--
					if a.readers == 0 {
						if a.removing {
							close(a.drained)
						} else {
							delete(s.activity.owners, owner)
						}
					}
				})
			}, nil
		}
		done := a.done
		s.activity.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
	}
}

// BeginUserFileRemoval closes file admission and waits for existing pins. New
// pins fail rather than wait: an admitted request can take nested pins, and
// waiting would prevent its outer pin from draining. The caller must first
// persist deletion intent and drain account/provider work, without holding a
// database lease. Release on every exit; central intent guards later admission.
func (s *BlobStore) BeginUserFileRemoval(ctx context.Context, owner string) (func(), error) {
	if _, err := composeOwnerKey(owner); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.activity.mu.Lock()
		a := s.activity.owners[owner]
		if a != nil && a.cleaning {
			done := a.done
			s.activity.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		if a == nil {
			a = &userFileActivity{}
			s.activity.owners[owner] = a
		}
		a.cleaning, a.removing = true, true
		a.done, a.drained = make(chan struct{}), make(chan struct{})
		if a.readers == 0 {
			close(a.drained)
		}
		drained := a.drained
		s.activity.mu.Unlock()
		var once sync.Once
		release := func() {
			once.Do(func() {
				s.activity.mu.Lock()
				defer s.activity.mu.Unlock()
				a.cleaning, a.removing = false, false
				close(a.done)
				if a.readers == 0 {
					delete(s.activity.owners, owner)
				}
			})
		}
		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-drained:
			if err := ctx.Err(); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
	}
}

// TryUserFileCleanup reserves one owner's file namespace. The caller must
// release it after all storage callbacks finish, including on failure.
func (s *BlobStore) TryUserFileCleanup(owner string) (release func(), acquired bool) {
	s.activity.mu.Lock()
	defer s.activity.mu.Unlock()
	if s.activity.owners[owner] != nil {
		return nil, false
	}
	a := &userFileActivity{cleaning: true, done: make(chan struct{})}
	s.activity.owners[owner] = a
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activity.mu.Lock()
			defer s.activity.mu.Unlock()
			delete(s.activity.owners, owner)
			close(a.done)
		})
	}, true
}

type UserFileCleanupResult struct {
	Uploads, Versions int
	Skipped           bool
}

// CleanupUserFiles must run under TryUserFileCleanup and a current, complete
// reference snapshot protected from publication. It visits only supplied owned
// account directories and the selected owner's uploads. Legacy message files,
// unknown directories and symlinks are left untouched.
func (s *BlobStore) CleanupUserFiles(ctx context.Context, owner string, accounts []string, keep map[string]bool, olderThan time.Duration) (result UserFileCleanupResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if olderThan <= 0 {
		return result, errors.New("file retention must be positive")
	}
	ownerKey, err := composeOwnerKey(owner)
	if err != nil {
		return result, err
	}
	for _, id := range accounts {
		if id == "" || id == "." || id == ".." || id == "avatars" || strings.HasPrefix(id, "_") || strings.ContainsAny(id, `/\`) {
			return result, fmt.Errorf("invalid account blob owner")
		}
	}
	base, err := filepath.Abs(s.basePath)
	if err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-olderThan)
	retainedDirs := make(map[string]bool)
	normalized := make(map[string]bool)
	for path, retained := range keep {
		if retained && path != "" {
			abs, err := filepath.Abs(path)
			if err != nil {
				return result, err
			}
			normalized[blobPathKey(abs)] = true
			for dir := filepath.Dir(abs); dir != base; dir = filepath.Dir(dir) {
				rel, err := filepath.Rel(base, dir)
				if err != nil || !filepath.IsLocal(rel) || rel == "." {
					break
				}
				retainedDirs[blobPathKey(dir)] = true
			}
		}
	}
	compose := filepath.Join(base, "_compose", ownerKey)
	if realDirectory(base, "_compose", ownerKey) {
		entries, err := os.ReadDir(compose)
		if err != nil {
			return result, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || len(entry.Name()) < 34 || !versionName(entry.Name()[:32]) || entry.Name()[32] != '-' {
				continue
			}
			path := filepath.Join(compose, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return result, err
			}
			if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) || normalized[blobPathKey(path)] {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return result, err
			}
			result.Uploads++
		}
	}
	for _, id := range accounts {
		if !realDirectory(base, id, "messages") {
			continue
		}
		root := filepath.Join(base, id, "messages")
		messages, err := os.ReadDir(root)
		if err != nil {
			return result, err
		}
		for _, msg := range messages {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			localID, parseErr := strconv.ParseInt(msg.Name(), 10, 64)
			if parseErr != nil || localID <= 0 || strconv.FormatInt(localID, 10) != msg.Name() || !realDirectory(root, msg.Name()) {
				continue
			}
			versions, err := os.ReadDir(filepath.Join(root, msg.Name()))
			if err != nil {
				return result, err
			}
			for _, version := range versions {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				if !version.IsDir() || version.Type()&os.ModeSymlink != 0 || !versionName(version.Name()) {
					continue
				}
				path := filepath.Join(root, msg.Name(), version.Name())
				if retainedDirs[blobPathKey(path)] {
					continue
				}
				old := true
				// Include every descendant's age: adding a new attachment changes
				// its directory but may leave the version root's mtime unchanged.
				err := filepath.WalkDir(path, func(p string, entry os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					if entry.Type()&os.ModeSymlink != 0 {
						old = false
						return filepath.SkipAll
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					if !info.ModTime().Before(cutoff) {
						old = false
						return filepath.SkipAll
					}
					return nil
				})
				if err != nil {
					return result, err
				}
				if old {
					if err := os.RemoveAll(path); err != nil {
						return result, err
					}
					result.Versions++
				}
			}
		}
	}
	return result, nil
}

func versionName(name string) bool {
	if len(name) != 32 || name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func blobPathKey(path string) string {
	// Windows may spell the same persisted absolute path with a different
	// drive/directory case after a restart or configuration normalization.
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func realDirectory(base string, components ...string) bool {
	path := base
	for _, component := range components {
		path = filepath.Join(path, component)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
