package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Migration must never fall back to generating/replacing an application secret.
// Retain the ordinary runtime's environment-over-file precedence.
func loadExistingMigrationKey(sourcePath string) ([]byte, error) {
	if configured := os.Getenv("GOFER_SECRET_KEY"); configured != "" {
		key, err := hex.DecodeString(configured)
		if err != nil || len(key) != 32 {
			return nil, errors.New("invalid GOFER_SECRET_KEY: expected 64 hex characters")
		}
		return key, nil
	}
	path := filepath.Join(filepath.Dir(sourcePath), "secret.key")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, errors.New("migration requires an existing regular 32-byte secret.key or GOFER_SECRET_KEY")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("migration requires the existing application secret.key or GOFER_SECRET_KEY")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, errors.New("existing application secret.key must be a regular 32-byte file")
	}
	key, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil || len(key) != 32 {
		return nil, errors.New("cannot read the existing 32-byte application secret.key")
	}
	return key, nil
}

func validateMigrationSourceKey(ctx context.Context, source *storage.DB, key []byte) error {
	if err := config.ValidateUserStorageMigrationPasswords(ctx, source, key); err != nil {
		return err
	}
	if err := auth.ValidateUserStorageMigrationSecrets(ctx, source, key); err != nil {
		return err
	}
	return mailauth.ValidateUserStorageMigrationCredentials(ctx, source, key)
}
