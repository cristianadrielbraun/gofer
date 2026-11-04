package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestBrandingSavesAndReplaces(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	empty, err := db.GetBranding(ctx)
	if err != nil || empty.Name != "" || len(empty.Logo) != 0 {
		t.Fatalf("unsaved branding = %+v, %v", empty, err)
	}

	logo := []byte("\x89PNG\r\n\x1a\nlogo")
	if err := db.SaveBranding(ctx, Branding{Name: "Acme Mail", Logo: logo, LogoType: "image/png", UpdatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetBranding(ctx)
	if err != nil || saved.Name != "Acme Mail" || !bytes.Equal(saved.Logo, logo) || saved.LogoType != "image/png" || len(saved.Favicon) != 0 || saved.UpdatedAt.IsZero() {
		t.Fatalf("saved branding = %+v, %v", saved, err)
	}

	// Saving again replaces everything, so a removed logo goes.
	if err := db.SaveBranding(ctx, Branding{Name: ""}); err != nil {
		t.Fatal(err)
	}
	cleared, err := db.GetBranding(ctx)
	if err != nil || cleared.Name != "" || len(cleared.Logo) != 0 || cleared.LogoType != "" {
		t.Fatalf("cleared branding = %+v, %v", cleared, err)
	}
}
