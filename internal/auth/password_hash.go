package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

var (
	ErrPasswordHashInvalid     = errors.New("password hash is invalid")
	ErrPasswordHashUnsupported = errors.New("password hash is unsupported")
)

const (
	passwordHashAlgorithm = "argon2id"

	// RFC 9106's second recommended Argon2id profile is intended for
	// memory-constrained environments and remains practical for self-hosted
	// interactive authentication.
	passwordHashMemoryKiB   uint32 = 64 * 1024
	passwordHashIterations  uint32 = 3
	passwordHashParallelism uint8  = 4
	passwordHashSaltLength         = 16
	passwordHashKeyLength          = 32

	maxPasswordHashMemoryKiB   uint32 = 256 * 1024
	maxPasswordHashIterations  uint32 = 10
	maxPasswordHashParallelism uint8  = 16
	minPasswordHashSaltLength         = 8
	maxPasswordHashSaltLength         = 64
	minPasswordHashKeyLength          = 16
	maxPasswordHashKeyLength          = 64
	maxPasswordHashPHCLength          = 512
)

type passwordHashParameters struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  int
	KeyLength   int
}

var currentPasswordHashParameters = passwordHashParameters{
	MemoryKiB:   passwordHashMemoryKiB,
	Iterations:  passwordHashIterations,
	Parallelism: passwordHashParallelism,
	SaltLength:  passwordHashSaltLength,
	KeyLength:   passwordHashKeyLength,
}

var dummyPasswordHash = encodePasswordHash(
	currentPasswordHashParameters,
	[]byte("gofer-auth-dummy"),
	make([]byte, passwordHashKeyLength),
)

// HashPassword returns a self-describing Argon2id PHC string with a fresh
// random salt. Password policy is intentionally enforced by the caller.
func HashPassword(password string) (string, error) {
	return hashPassword(password, currentPasswordHashParameters, rand.Reader)
}

// VerifyPassword checks an encoded password hash in constant time. A matching
// hash reports whether its parameters should be upgraded after authentication.
func VerifyPassword(encodedHash, password string) (matches, needsRehash bool, err error) {
	password, err = normalizePasswordForHashing(password)
	if err != nil {
		return false, false, err
	}
	parameters, salt, expected, err := parsePasswordHash(encodedHash)
	if err != nil {
		return false, false, err
	}
	defer clear(expected)

	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	actual := argon2.IDKey(
		passwordBytes,
		salt,
		parameters.Iterations,
		parameters.MemoryKiB,
		parameters.Parallelism,
		uint32(parameters.KeyLength),
	)
	defer clear(actual)

	matches = subtle.ConstantTimeCompare(actual, expected) == 1
	return matches, matches && parameters != currentPasswordHashParameters, nil
}

// VerifyAndRehashPassword performs the same fixed-cost Argon2id work for an
// unknown identifier, preventing the missing-credential path from becoming a
// cheap username oracle. When a real credential matches stale parameters, it
// returns a replacement PHC string for the caller to persist atomically.
func VerifyAndRehashPassword(encodedHash, password string) (matches bool, replacementHash string, err error) {
	unknownCredential := encodedHash == ""
	if unknownCredential {
		encodedHash = dummyPasswordHash
	}

	matches, needsRehash, err := VerifyPassword(encodedHash, password)
	if err != nil {
		return false, "", err
	}
	if unknownCredential || !matches {
		return false, "", nil
	}
	if !needsRehash {
		return true, "", nil
	}

	replacementHash, err = HashPassword(password)
	if err != nil {
		return false, "", fmt.Errorf("rehash password: %w", err)
	}
	return true, replacementHash, nil
}

func hashPassword(password string, parameters passwordHashParameters, random io.Reader) (string, error) {
	password, err := normalizePasswordForHashing(password)
	if err != nil {
		return "", err
	}
	if err := validatePasswordHashParameters(parameters); err != nil {
		return "", err
	}
	if random == nil {
		return "", fmt.Errorf("generate password salt: random source is required")
	}

	salt := make([]byte, parameters.SaltLength)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	key := argon2.IDKey(
		passwordBytes,
		salt,
		parameters.Iterations,
		parameters.MemoryKiB,
		parameters.Parallelism,
		uint32(parameters.KeyLength),
	)
	defer clear(key)

	return encodePasswordHash(parameters, salt, key), nil
}

func encodePasswordHash(parameters passwordHashParameters, salt, key []byte) string {
	return fmt.Sprintf(
		"$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		passwordHashAlgorithm,
		argon2.Version,
		parameters.MemoryKiB,
		parameters.Iterations,
		parameters.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func parsePasswordHash(encodedHash string) (passwordHashParameters, []byte, []byte, error) {
	if encodedHash == "" || len(encodedHash) > maxPasswordHashPHCLength {
		return passwordHashParameters{}, nil, nil, ErrPasswordHashInvalid
	}
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "" {
		return passwordHashParameters{}, nil, nil, ErrPasswordHashInvalid
	}
	if parts[1] != passwordHashAlgorithm {
		return passwordHashParameters{}, nil, nil, ErrPasswordHashUnsupported
	}
	if !strings.HasPrefix(parts[2], "v=") {
		return passwordHashParameters{}, nil, nil, ErrPasswordHashInvalid
	}
	version, err := parsePasswordHashUint(strings.TrimPrefix(parts[2], "v="), 8)
	if err != nil {
		return passwordHashParameters{}, nil, nil, ErrPasswordHashInvalid
	}
	if version != argon2.Version {
		return passwordHashParameters{}, nil, nil, ErrPasswordHashUnsupported
	}

	parameters, err := parsePasswordHashParameters(parts[3])
	if err != nil {
		return passwordHashParameters{}, nil, nil, err
	}
	salt, err := decodePasswordHashField(parts[4], minPasswordHashSaltLength, maxPasswordHashSaltLength)
	if err != nil {
		return passwordHashParameters{}, nil, nil, err
	}
	key, err := decodePasswordHashField(parts[5], minPasswordHashKeyLength, maxPasswordHashKeyLength)
	if err != nil {
		return passwordHashParameters{}, nil, nil, err
	}
	parameters.SaltLength = len(salt)
	parameters.KeyLength = len(key)
	if err := validatePasswordHashParameters(parameters); err != nil {
		clear(key)
		return passwordHashParameters{}, nil, nil, err
	}
	return parameters, salt, key, nil
}

func parsePasswordHashParameters(encoded string) (passwordHashParameters, error) {
	var parameters passwordHashParameters
	seen := make(map[string]bool, 3)
	fields := strings.Split(encoded, ",")
	if len(fields) != 3 {
		return parameters, ErrPasswordHashInvalid
	}
	for _, field := range fields {
		name, value, ok := strings.Cut(field, "=")
		if !ok || name == "" || value == "" || seen[name] {
			return parameters, ErrPasswordHashInvalid
		}
		seen[name] = true
		switch name {
		case "m":
			parsed, err := parsePasswordHashUint(value, 32)
			if err != nil {
				return parameters, ErrPasswordHashInvalid
			}
			parameters.MemoryKiB = uint32(parsed)
		case "t":
			parsed, err := parsePasswordHashUint(value, 32)
			if err != nil {
				return parameters, ErrPasswordHashInvalid
			}
			parameters.Iterations = uint32(parsed)
		case "p":
			parsed, err := parsePasswordHashUint(value, 8)
			if err != nil {
				return parameters, ErrPasswordHashInvalid
			}
			parameters.Parallelism = uint8(parsed)
		default:
			return parameters, ErrPasswordHashInvalid
		}
	}
	if !seen["m"] || !seen["t"] || !seen["p"] {
		return parameters, ErrPasswordHashInvalid
	}
	return parameters, nil
}

func parsePasswordHashUint(value string, bits int) (uint64, error) {
	if value == "" {
		return 0, ErrPasswordHashInvalid
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, ErrPasswordHashInvalid
		}
	}
	return strconv.ParseUint(value, 10, bits)
}

func decodePasswordHashField(encoded string, minimumLength, maximumLength int) ([]byte, error) {
	if encoded == "" || len(encoded) > base64.RawStdEncoding.EncodedLen(maximumLength) {
		return nil, ErrPasswordHashInvalid
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) < minimumLength || len(decoded) > maximumLength {
		clear(decoded)
		return nil, ErrPasswordHashInvalid
	}
	return decoded, nil
}

func validatePasswordHashParameters(parameters passwordHashParameters) error {
	if parameters.Parallelism == 0 || parameters.Parallelism > maxPasswordHashParallelism {
		return ErrPasswordHashInvalid
	}
	minimumMemory := uint32(8) * uint32(parameters.Parallelism)
	if parameters.MemoryKiB < minimumMemory || parameters.MemoryKiB > maxPasswordHashMemoryKiB {
		return ErrPasswordHashInvalid
	}
	if parameters.Iterations == 0 || parameters.Iterations > maxPasswordHashIterations {
		return ErrPasswordHashInvalid
	}
	if parameters.SaltLength < minPasswordHashSaltLength || parameters.SaltLength > maxPasswordHashSaltLength {
		return ErrPasswordHashInvalid
	}
	if parameters.KeyLength < minPasswordHashKeyLength || parameters.KeyLength > maxPasswordHashKeyLength {
		return ErrPasswordHashInvalid
	}
	return nil
}
