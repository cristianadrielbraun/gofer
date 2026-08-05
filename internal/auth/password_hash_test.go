package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPasswordHashProfileMatchesRFC9106Recommendation(t *testing.T) {
	if currentPasswordHashParameters.MemoryKiB != 64*1024 ||
		currentPasswordHashParameters.Iterations != 3 ||
		currentPasswordHashParameters.Parallelism != 4 ||
		currentPasswordHashParameters.SaltLength != 16 ||
		currentPasswordHashParameters.KeyLength != 32 {
		t.Fatalf("current password hash parameters = %#v", currentPasswordHashParameters)
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"
	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword(first) error = %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword(second) error = %v", err)
	}
	if first == second {
		t.Fatal("password hashes reused a salt")
	}
	if !strings.HasPrefix(first, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("password hash = %q, want canonical Argon2id PHC prefix", first)
	}

	parameters, salt, key, err := parsePasswordHash(first)
	if err != nil {
		t.Fatalf("parsePasswordHash() error = %v", err)
	}
	defer clear(key)
	if parameters != currentPasswordHashParameters || len(salt) != passwordHashSaltLength || len(key) != passwordHashKeyLength {
		t.Fatalf("parsed hash = params:%#v salt:%d key:%d", parameters, len(salt), len(key))
	}

	matches, needsRehash, err := VerifyPassword(first, password)
	if err != nil || !matches || needsRehash {
		t.Fatalf("VerifyPassword(correct) = matches:%t needsRehash:%t error:%v", matches, needsRehash, err)
	}
	matches, needsRehash, err = VerifyPassword(first, "wrong password")
	if err != nil || matches || needsRehash {
		t.Fatalf("VerifyPassword(wrong) = matches:%t needsRehash:%t error:%v", matches, needsRehash, err)
	}
	matches, replacement, err := VerifyAndRehashPassword(first, password)
	if err != nil || !matches || replacement != "" {
		t.Fatalf("VerifyAndRehashPassword(current) = matches:%t replacement:%q error:%v", matches, replacement, err)
	}
}

func TestVerifyAndRehashPasswordUpgradesStaleParameters(t *testing.T) {
	staleParameters := passwordHashParameters{
		MemoryKiB:   19 * 1024,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
	encoded, err := hashPassword("upgrade me", staleParameters, strings.NewReader("0123456789abcdef"))
	if err != nil {
		t.Fatalf("hashPassword(stale) error = %v", err)
	}

	matches, needsRehash, err := VerifyPassword(encoded, "upgrade me")
	if err != nil || !matches || !needsRehash {
		t.Fatalf("VerifyPassword(stale) = matches:%t needsRehash:%t error:%v", matches, needsRehash, err)
	}
	matches, replacement, err := VerifyAndRehashPassword(encoded, "upgrade me")
	if err != nil || !matches || replacement == "" {
		t.Fatalf("VerifyAndRehashPassword(stale) = matches:%t replacement:%q error:%v", matches, replacement, err)
	}
	matches, needsRehash, err = VerifyPassword(replacement, "upgrade me")
	if err != nil || !matches || needsRehash {
		t.Fatalf("VerifyPassword(replacement) = matches:%t needsRehash:%t error:%v", matches, needsRehash, err)
	}

	matches, replacement, err = VerifyAndRehashPassword(encoded, "wrong password")
	if err != nil || matches || replacement != "" {
		t.Fatalf("VerifyAndRehashPassword(wrong) = matches:%t replacement:%q error:%v", matches, replacement, err)
	}
}

func TestVerifyAndRehashPasswordUsesDummyHashForUnknownCredential(t *testing.T) {
	matches, replacement, err := VerifyAndRehashPassword("", "any supplied password")
	if err != nil || matches || replacement != "" {
		t.Fatalf("VerifyAndRehashPassword(unknown) = matches:%t replacement:%q error:%v", matches, replacement, err)
	}
	parameters, _, key, err := parsePasswordHash(dummyPasswordHash)
	if err != nil {
		t.Fatalf("parsePasswordHash(dummy) error = %v", err)
	}
	defer clear(key)
	if parameters != currentPasswordHashParameters {
		t.Fatalf("dummy parameters = %#v, want %#v", parameters, currentPasswordHashParameters)
	}
}

func TestPasswordHashRejectsMalformedOrUnsafePHCStrings(t *testing.T) {
	valid := encodePasswordHash(
		currentPasswordHashParameters,
		[]byte("0123456789abcdef"),
		make([]byte, passwordHashKeyLength),
	)
	shortSalt := base64.RawStdEncoding.EncodeToString(make([]byte, minPasswordHashSaltLength-1))
	shortKey := base64.RawStdEncoding.EncodeToString(make([]byte, minPasswordHashKeyLength-1))
	tests := []struct {
		name   string
		hash   string
		target error
	}{
		{name: "empty", hash: "", target: ErrPasswordHashInvalid},
		{name: "not PHC", hash: "not-a-password-hash", target: ErrPasswordHashInvalid},
		{name: "unsupported algorithm", hash: strings.Replace(valid, "$argon2id$", "$argon2i$", 1), target: ErrPasswordHashUnsupported},
		{name: "unsupported version", hash: strings.Replace(valid, "$v=19$", "$v=18$", 1), target: ErrPasswordHashUnsupported},
		{name: "malformed version", hash: strings.Replace(valid, "$v=19$", "$version=19$", 1), target: ErrPasswordHashInvalid},
		{name: "missing parameter", hash: strings.Replace(valid, "m=65536,t=3,p=4", "m=65536,t=3", 1), target: ErrPasswordHashInvalid},
		{name: "duplicate parameter", hash: strings.Replace(valid, "m=65536,t=3,p=4", "m=65536,t=3,m=4", 1), target: ErrPasswordHashInvalid},
		{name: "unknown parameter", hash: strings.Replace(valid, "m=65536,t=3,p=4", "m=65536,t=3,x=4", 1), target: ErrPasswordHashInvalid},
		{name: "zero parallelism", hash: strings.Replace(valid, "p=4", "p=0", 1), target: ErrPasswordHashInvalid},
		{name: "excessive parallelism", hash: strings.Replace(valid, "p=4", "p=17", 1), target: ErrPasswordHashInvalid},
		{name: "excessive memory", hash: strings.Replace(valid, "m=65536", "m=262145", 1), target: ErrPasswordHashInvalid},
		{name: "excessive iterations", hash: strings.Replace(valid, "t=3", "t=11", 1), target: ErrPasswordHashInvalid},
		{name: "invalid salt encoding", hash: replacePasswordHashField(valid, 4, "not+raw/base64"), target: ErrPasswordHashInvalid},
		{name: "short salt", hash: replacePasswordHashField(valid, 4, shortSalt), target: ErrPasswordHashInvalid},
		{name: "invalid key encoding", hash: replacePasswordHashField(valid, 5, "not+raw/base64"), target: ErrPasswordHashInvalid},
		{name: "short key", hash: replacePasswordHashField(valid, 5, shortKey), target: ErrPasswordHashInvalid},
		{name: "oversized PHC", hash: valid + strings.Repeat("x", maxPasswordHashPHCLength), target: ErrPasswordHashInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matches, needsRehash, err := VerifyPassword(test.hash, "password")
			if matches || needsRehash || !errors.Is(err, test.target) {
				t.Fatalf("VerifyPassword() = matches:%t needsRehash:%t error:%v, want %v", matches, needsRehash, err, test.target)
			}
		})
	}
}

func TestPasswordHashAcceptsReorderedCanonicalParameters(t *testing.T) {
	encoded := encodePasswordHash(
		currentPasswordHashParameters,
		[]byte("0123456789abcdef"),
		make([]byte, passwordHashKeyLength),
	)
	encoded = strings.Replace(encoded, "m=65536,t=3,p=4", "p=4,m=65536,t=3", 1)
	parameters, _, key, err := parsePasswordHash(encoded)
	if err != nil {
		t.Fatalf("parsePasswordHash(reordered) error = %v", err)
	}
	defer clear(key)
	if parameters != currentPasswordHashParameters {
		t.Fatalf("reordered parameters = %#v", parameters)
	}
}

func TestHashPasswordPropagatesRandomSourceFailure(t *testing.T) {
	randomErr := errors.New("random source unavailable")
	_, err := hashPassword("password", currentPasswordHashParameters, failingPasswordRandom{err: randomErr})
	if !errors.Is(err, randomErr) {
		t.Fatalf("hashPassword() error = %v, want %v", err, randomErr)
	}
}

func TestHashPasswordRejectsUnsafeParametersBeforeDerivation(t *testing.T) {
	parameters := currentPasswordHashParameters
	parameters.MemoryKiB = maxPasswordHashMemoryKiB + 1
	if _, err := hashPassword("password", parameters, strings.NewReader("0123456789abcdef")); !errors.Is(err, ErrPasswordHashInvalid) {
		t.Fatalf("hashPassword(unsafe) error = %v, want ErrPasswordHashInvalid", err)
	}
}

func BenchmarkPasswordHash(b *testing.B) {
	b.ReportAllocs()
	b.ReportMetric(float64(currentPasswordHashParameters.MemoryKiB)/1024, "MiB/op")
	for i := 0; i < b.N; i++ {
		if _, err := HashPassword("representative benchmark password"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPasswordVerificationKnown(b *testing.B) {
	encoded, err := HashPassword("representative benchmark password")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(currentPasswordHashParameters.MemoryKiB)/1024, "MiB/op")
	for i := 0; i < b.N; i++ {
		matches, replacement, err := VerifyAndRehashPassword(encoded, "representative benchmark password")
		if err != nil || !matches || replacement != "" {
			b.Fatalf("verification = matches:%t replacement:%q error:%v", matches, replacement, err)
		}
	}
}

func BenchmarkPasswordVerificationUnknown(b *testing.B) {
	b.ReportAllocs()
	b.ReportMetric(float64(currentPasswordHashParameters.MemoryKiB)/1024, "MiB/op")
	for i := 0; i < b.N; i++ {
		matches, replacement, err := VerifyAndRehashPassword("", "representative benchmark password")
		if err != nil || matches || replacement != "" {
			b.Fatalf("verification = matches:%t replacement:%q error:%v", matches, replacement, err)
		}
	}
}

type failingPasswordRandom struct {
	err error
}

func (random failingPasswordRandom) Read([]byte) (int, error) {
	return 0, random.err
}

func replacePasswordHashField(encoded string, field int, replacement string) string {
	parts := strings.Split(encoded, "$")
	if field < 0 || field >= len(parts) {
		panic(fmt.Sprintf("password hash field %d out of range", field))
	}
	parts[field] = replacement
	return strings.Join(parts, "$")
}
