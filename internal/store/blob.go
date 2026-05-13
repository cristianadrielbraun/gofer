package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type BlobStore struct {
	basePath string
}

func NewBlobStore(basePath string) *BlobStore {
	return &BlobStore{basePath: basePath}
}

func (s *BlobStore) msgDir(accountID string, localID int64) string {
	return filepath.Join(s.basePath, accountID, "messages", fmt.Sprintf("%d", localID))
}

func (s *BlobStore) ensureDir(dir string) error {
	return os.MkdirAll(dir, 0755)
}

func (s *BlobStore) StoreRaw(ctx context.Context, accountID string, localID int64, data []byte) (string, error) {
	dir := s.msgDir(accountID, localID)
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create message dir: %w", err)
	}
	p := filepath.Join(dir, "raw.eml")
	return p, os.WriteFile(p, data, 0644)
}

func (s *BlobStore) StoreBodyText(ctx context.Context, accountID string, localID int64, data []byte) (string, error) {
	dir := s.msgDir(accountID, localID)
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create message dir: %w", err)
	}
	p := filepath.Join(dir, "body.txt")
	return p, os.WriteFile(p, data, 0644)
}

func (s *BlobStore) StoreBodyHTML(ctx context.Context, accountID string, localID int64, data []byte) (string, error) {
	dir := s.msgDir(accountID, localID)
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create message dir: %w", err)
	}
	p := filepath.Join(dir, "body.html")
	return p, os.WriteFile(p, data, 0644)
}

func (s *BlobStore) StoreBodyOriginalHTML(ctx context.Context, accountID string, localID int64, data []byte) (string, error) {
	dir := s.msgDir(accountID, localID)
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create message dir: %w", err)
	}
	p := filepath.Join(dir, "body_original.html")
	return p, os.WriteFile(p, data, 0644)
}

func (s *BlobStore) StoreAttachment(ctx context.Context, accountID string, localID int64, attID int64, filename string, r io.Reader) (string, error) {
	dir := filepath.Join(s.msgDir(accountID, localID), "attachments")
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create attachments dir: %w", err)
	}
	sanitized := sanitizeFilename(filename)
	if sanitized == "" {
		sanitized = fmt.Sprintf("attachment-%d", attID)
	}
	p := filepath.Join(dir, fmt.Sprintf("%d-%s", attID, sanitized))

	f, err := os.Create(p)
	if err != nil {
		return "", fmt.Errorf("create attachment file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		os.Remove(p)
		return "", fmt.Errorf("write attachment: %w", err)
	}
	return p, nil
}

func (s *BlobStore) StoreComposeAttachment(ctx context.Context, filename string, r io.Reader) (id, path string, err error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	id = hex.EncodeToString(b[:])
	dir := filepath.Join(s.basePath, "_compose")
	if err := s.ensureDir(dir); err != nil {
		return "", "", fmt.Errorf("create compose attachments dir: %w", err)
	}
	sanitized := sanitizeFilename(filename)
	if sanitized == "" {
		sanitized = "attachment"
	}
	path = filepath.Join(dir, id+"-"+sanitized)
	f, err := os.Create(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		os.Remove(path)
		return "", "", err
	}
	return id, path, nil
}

func (s *BlobStore) ComposeAttachmentPath(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid compose attachment id")
	}
	matches, err := filepath.Glob(filepath.Join(s.basePath, "_compose", id+"-*"))
	if err != nil || len(matches) == 0 {
		return "", os.ErrNotExist
	}
	return matches[0], nil
}

func (s *BlobStore) DeleteComposeAttachment(id string) error {
	path, err := s.ComposeAttachmentPath(id)
	if err != nil {
		return nil
	}
	return os.Remove(path)
}

func (s *BlobStore) CleanupComposeAttachments(olderThan time.Duration, keep map[string]bool) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	dir := filepath.Join(s.basePath, "_compose")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if keep != nil && keep[filepath.Clean(path)] {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *BlobStore) Open(path string) (io.ReadCloser, error) {
	absPath := filepath.Join(s.basePath, path)
	return os.Open(absPath)
}

func (s *BlobStore) ReadFile(path string) ([]byte, error) {
	absPath := filepath.Join(s.basePath, path)
	return os.ReadFile(absPath)
}

func (s *BlobStore) ReadBodyText(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (s *BlobStore) ReadBodyHTML(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (s *BlobStore) ReadAttachment(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (s *BlobStore) DeleteMessage(accountID string, localID int64) error {
	return os.RemoveAll(s.msgDir(accountID, localID))
}

func (s *BlobStore) RemoteAssetsDir(accountID string, localID int64) string {
	return filepath.Join(s.msgDir(accountID, localID), "remote_assets")
}

func (s *BlobStore) StoreRemoteAsset(accountID string, localID int64, url string, data []byte) (string, error) {
	dir := s.RemoteAssetsDir(accountID, localID)
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create remote assets dir: %w", err)
	}
	h := sha256.Sum256([]byte(url))
	filename := fmt.Sprintf("%x", h[:8])
	ext := assetExtension(url, data)
	if ext != "" {
		filename += ext
	}
	p := filepath.Join(dir, filename)
	return p, os.WriteFile(p, data, 0644)
}

func (s *BlobStore) StoreRemoteBodyHTML(accountID string, localID int64, data []byte) (string, error) {
	dir := s.msgDir(accountID, localID)
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create message dir: %w", err)
	}
	p := filepath.Join(dir, "body_remote.html")
	return p, os.WriteFile(p, data, 0644)
}

func (s *BlobStore) ReadRemoteBodyHTML(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (s *BlobStore) StoreAvatar(hash, contentType string, data []byte) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) < 4 || strings.ContainsAny(hash, `/\`) || strings.Contains(hash, "..") {
		return "", fmt.Errorf("invalid avatar hash")
	}
	dir := filepath.Join(s.basePath, "avatars", hash[:2], hash[2:4])
	if err := s.ensureDir(dir); err != nil {
		return "", fmt.Errorf("create avatar dir: %w", err)
	}
	relPath := filepath.Join("avatars", hash[:2], hash[2:4], hash+avatarExtension(contentType, data))
	absPath := filepath.Join(s.basePath, relPath)
	tmp, err := os.CreateTemp(dir, hash+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, absPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return relPath, nil
}

func (s *BlobStore) ReadAvatar(relPath string) ([]byte, error) {
	if relPath == "" || filepath.IsAbs(relPath) || strings.Contains(relPath, "..") {
		return nil, fmt.Errorf("invalid avatar path")
	}
	clean := filepath.Clean(relPath)
	if !strings.HasPrefix(clean, "avatars"+string(os.PathSeparator)) {
		return nil, fmt.Errorf("invalid avatar path")
	}
	return os.ReadFile(filepath.Join(s.basePath, clean))
}

func assetExtension(url string, data []byte) string {
	lower := strings.ToLower(url)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".bmp"} {
		if strings.Contains(lower, ext) {
			return ext
		}
	}
	if len(data) > 4 {
		switch {
		case data[0] == 0x89 && data[1] == 0x50:
			return ".png"
		case data[0] == 0xFF && data[1] == 0xD8:
			return ".jpg"
		case data[0] == 0x47 && data[1] == 0x49:
			return ".gif"
		case data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46:
			return ".webp"
		}
	}
	return ""
}

func avatarExtension(contentType string, data []byte) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml", "application/svg+xml":
		return ".svg"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico"
	}
	return assetExtension("", data)
}

func (s *BlobStore) DeleteAccount(accountID string) error {
	return os.RemoveAll(filepath.Join(s.basePath, accountID))
}

func sanitizeFilename(name string) string {
	clean := filepath.Base(name)
	if clean == "." || clean == ".." {
		return ""
	}
	return clean
}
