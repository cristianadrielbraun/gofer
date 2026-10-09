package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The snapshot contains counts/digests, never filenames, mailbox contents or
// secrets. Missing cache files and malformed historical snapshots are retained
// facts, not repaired or authorized operations. Compare snapshots while holding
// the offline runtime locks; a snapshot alone is not migration publication.
type UserStorageMigrationFiles struct {
	Version           int    `json:"version"`
	References        int64  `json:"references"`
	MissingReferences int64  `json:"missing_references"`
	UnparsedSnapshots int64  `json:"unparsed_snapshots"`
	Files             int64  `json:"files"`
	Bytes             int64  `json:"bytes"`
	ReferenceDigest   string `json:"reference_digest"`
	TreeDigest        string `json:"tree_digest"`
}

// InspectUserStorageMigrationFiles verifies namespaces and fingerprints the
// complete existing blob tree, including compose uploads and remote assets that
// are not individually indexed in SQL. It opens no providers and writes nothing.
// Relative stored paths keep the original application working-directory meaning.
func InspectUserStorageMigrationFiles(ctx context.Context, source *DB, workingDirectory string) (result UserStorageMigrationFiles, err error) {
	if source == nil || ctx == nil {
		return result, errors.New("migration file inspection requires source and context")
	}
	result.Version = 1
	if workingDirectory == "" {
		workingDirectory, err = os.Getwd()
		if err != nil {
			return result, err
		}
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return result, err
	}
	base, err := filepath.Abs(filepath.Join(filepath.Dir(source.Path()), "accounts"))
	if err != nil {
		return result, err
	}
	tx, err := source.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := migrationAccountFileNamespaces(ctx, tx, base); err != nil {
		return result, err
	}
	references := sha256.New()
	migrationDigestFields(references, "gofer-migration-file-references-v1")
	check := func(owner, path, checksum string, avatar bool) error {
		if path == "" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stored := path
		if avatar {
			if filepath.IsAbs(path) || !filepath.IsLocal(path) {
				return errors.New("migration avatar path escapes the cache namespace")
			}
			path = filepath.Join(base, path)
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(workingDirectory, path)
		}
		path = filepath.Clean(path)
		relative, err := filepath.Rel(base, path)
		if err != nil || !filepath.IsLocal(relative) || relative == "." {
			if !avatar && !filepath.IsAbs(stored) {
				return fmt.Errorf("a stored file path does not resolve inside %s from the working directory %s; run Gofer from the directory the previous version was started from", base, workingDirectory)
			}
			return errors.New("migration file path escapes the blob namespace")
		}
		parts := strings.Split(relative, string(os.PathSeparator))
		if len(parts) < 2 {
			return errors.New("migration file has no owned namespace")
		}
		if avatar {
			if parts[0] != "avatars" {
				return errors.New("migration avatar path is outside its cache namespace")
			}
		} else if parts[0] == "_compose" {
			key := sha256.Sum256([]byte(owner))
			if len(parts) != 3 || parts[1] != hex.EncodeToString(key[:16]) {
				return errors.New("migration compose file belongs to another owner")
			}
		} else {
			var owned bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=? AND user_id=?)`, parts[0], owner).Scan(&owned); err != nil {
				return err
			}
			if !owned || reservedMigrationAccountDirectory(parts[0]) {
				return errors.New("migration file account belongs to another owner or is unresolved")
			}
		}
		result.References++
		migrationDigestFields(references, owner, relative, checksum)
		if err := migrationNoSymlinks(base, relative); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				result.MissingReferences++
				migrationDigestFields(references, "missing")
				return nil
			}
			return err
		}
		fingerprint, size, err := migrationHashFile(ctx, path)
		if err != nil {
			return err
		}
		if checksum != "" && !strings.EqualFold(checksum, fingerprint) {
			return errors.New("migration attachment checksum does not match original file")
		}
		migrationDigestFields(references, fingerprint, fmt.Sprint(size))
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.user_id,m.id,m.body_text_path,m.body_html_path,m.body_html_original_path,m.raw_path
 FROM messages m JOIN accounts a ON a.id=m.account_id ORDER BY m.id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var owner string
		var id int64
		var paths [4]sql.NullString
		if err := rows.Scan(&owner, &id, &paths[0], &paths[1], &paths[2], &paths[3]); err != nil {
			rows.Close()
			return result, err
		}
		migrationDigestFields(references, "message", fmt.Sprint(id))
		for index, path := range paths {
			migrationDigestFields(references, fmt.Sprint(index), fmt.Sprint(path.Valid))
			if err := check(owner, path.String, "", false); err != nil {
				rows.Close()
				return result, err
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT a.user_id,t.id,coalesce(t.storage_path,''),coalesce(t.sha256,'') FROM attachments t JOIN messages m ON m.id=t.message_id JOIN accounts a ON a.id=m.account_id ORDER BY t.id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var owner, path, checksum string
		var id int64
		if err := rows.Scan(&owner, &id, &path, &checksum); err != nil {
			rows.Close()
			return result, err
		}
		migrationDigestFields(references, "attachment", fmt.Sprint(id))
		if err := check(owner, path, checksum, false); err != nil {
			rows.Close()
			return result, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT a.user_id,s.id,s.message_json,
 s.status<>'canceled' AND NOT (s.status='sent' AND s.sent_copy_status IN ('complete','not_required'))
 FROM outgoing_sends s JOIN accounts a ON a.id=s.account_id WHERE s.message_json<>'' ORDER BY s.id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var owner, id, data string
		var retained bool
		if err := rows.Scan(&owner, &id, &data, &retained); err != nil {
			rows.Close()
			return result, err
		}
		migrationDigestFields(references, "send", owner, id, data, fmt.Sprint(retained))
		if !retained {
			continue // Terminal history keeps its bytes, but grants no file access.
		}
		var snapshot struct {
			Attachments []struct {
				Path string `json:"path"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal([]byte(data), &snapshot); err != nil || !strings.HasPrefix(strings.TrimSpace(data), "{") {
			result.UnparsedSnapshots++
			continue // Preserve the exact historical payload; never adopt/repair it.
		}
		for _, attachment := range snapshot.Attachments {
			if err := check(owner, attachment.Path, "", false); err != nil {
				rows.Close()
				return result, err
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT email_hash,storage_path FROM sender_avatars WHERE coalesce(storage_path,'')<>'' ORDER BY email_hash`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return result, err
		}
		migrationDigestFields(references, "avatar", id)
		if err := check("", path, "", true); err != nil {
			rows.Close()
			return result, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	result.ReferenceDigest = hex.EncodeToString(references.Sum(nil))
	tree := sha256.New()
	migrationDigestFields(tree, "gofer-migration-blob-tree-v1")
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == base {
			migrationDigestFields(tree, "absent-root")
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("migration blob tree contains a symlink requiring explicit resolution")
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			migrationDigestFields(tree, "directory", relative)
			return nil
		}
		fingerprint, size, err := migrationHashFile(ctx, path)
		if err != nil {
			return err
		}
		result.Files++
		result.Bytes += size
		migrationDigestFields(tree, "file", relative, fingerprint, fmt.Sprint(size))
		return nil
	})
	if err != nil {
		return result, err
	}
	result.TreeDigest = hex.EncodeToString(tree.Sum(nil))
	return result, nil
}

func reservedMigrationAccountDirectory(id string) bool {
	return id == "" || id == "." || id == ".." || id == "avatars" || strings.HasPrefix(id, "_") || strings.ContainsAny(id, `/\`) || id != strings.TrimSpace(id)
}

func migrationAccountFileNamespaces(ctx context.Context, tx *sql.Tx, base string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	var directories []os.FileInfo
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if reservedMigrationAccountDirectory(id) || !filepath.IsLocal(id) {
			return errors.New("migration account has an unsafe blob namespace")
		}
		key := id
		if runtime.GOOS == "windows" {
			key = strings.ToLower(id)
		}
		if seen[key] {
			return errors.New("migration account blob namespaces alias")
		}
		seen[key] = true
		info, err := os.Lstat(filepath.Join(base, id))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("migration account blob namespace is not a real directory")
		}
		for _, previous := range directories {
			if os.SameFile(previous, info) {
				return errors.New("migration account directories alias on this filesystem")
			}
		}
		directories = append(directories, info)
	}
	return rows.Err()
}

func migrationNoSymlinks(base, relative string) error {
	path := base
	for _, part := range append([]string{""}, strings.Split(relative, string(os.PathSeparator))...) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("migration file path contains a symlink requiring explicit resolution")
		}
	}
	return nil
}

func migrationHashFile(ctx context.Context, path string) (string, int64, error) {
	// Reject special files before opening: opening a FIFO could block indefinitely.
	pathBefore, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !pathBefore.Mode().IsRegular() {
		return "", 0, errors.New("migration blob is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(pathBefore, before) {
		return "", 0, errors.New("migration blob is not a regular file")
	}
	digest := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			digest.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	after, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("migration file changed while being inspected")
	}
	return hex.EncodeToString(digest.Sum(nil)), after.Size(), nil
}

func migrationDigestFields(digest hash.Hash, fields ...string) {
	var size [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		digest.Write(size[:])
		digest.Write([]byte(field))
	}
}
