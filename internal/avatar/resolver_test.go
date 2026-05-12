package avatar

import "testing"

func TestGravatarHash(t *testing.T) {
	got := GravatarHash(" MyEmailAddress@example.com ")
	want := "0bc83cb571cd1c50ba6f3e8a78ef1346"
	if got != want {
		t.Fatalf("GravatarHash() = %q, want %q", got, want)
	}
}

func TestGravatarHashInvalidEmail(t *testing.T) {
	if got := GravatarHash("not-an-email"); got != "" {
		t.Fatalf("GravatarHash() = %q, want empty", got)
	}
}

func TestAvatarURL(t *testing.T) {
	got := AvatarURL("MyEmailAddress@example.com")
	want := "/api/avatars/0bc83cb571cd1c50ba6f3e8a78ef1346"
	if got != want {
		t.Fatalf("AvatarURL() = %q, want %q", got, want)
	}
}

func TestIsGravatarHash(t *testing.T) {
	if !IsGravatarHash("0bc83cb571cd1c50ba6f3e8a78ef1346") {
		t.Fatal("expected valid hash")
	}
	if IsGravatarHash("status") {
		t.Fatal("expected invalid hash")
	}
}
