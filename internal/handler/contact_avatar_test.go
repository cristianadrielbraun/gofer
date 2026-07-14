package handler

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestContactAvatarFromForm(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52}
	raw := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	got, remove, err := contactAvatarFromForm("replace", raw)
	if err != nil {
		t.Fatalf("contactAvatarFromForm() error = %v", err)
	}
	if remove || !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("contactAvatarFromForm() = %q, remove=%t", got, remove)
	}

	if got, remove, err = contactAvatarFromForm("remove", ""); err != nil || got != "" || !remove {
		t.Fatalf("remove = %q, %t, %v", got, remove, err)
	}
	if _, _, err = contactAvatarFromForm("replace", "data:text/plain;base64,dGVzdA=="); err == nil {
		t.Fatal("contactAvatarFromForm() accepted non-image data")
	}
}
