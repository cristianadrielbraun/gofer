package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

const (
	totpCredentialKeyVersion = 1
	totpCredentialKeyContext = "gofer/auth/totp-credential/v1"
	setupTOTPIssuer          = "Gofer"
)

func (m *Manager) totpCredentialAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("TOTP credential encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(totpCredentialKeyContext))
	key := deriver.Sum(nil)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create TOTP credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create TOTP credential AEAD: %w", err)
	}
	return aead, nil
}

func (m *Manager) encryptTOTPSeed(userID, credentialID, seed string) ([]byte, error) {
	aead, err := m.totpCredentialAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate TOTP credential nonce: %w", err)
	}
	payload := []byte{totpCredentialKeyVersion}
	payload = append(payload, nonce...)
	payload = aead.Seal(payload, nonce, []byte(seed), totpCredentialAAD(userID, credentialID))
	return payload, nil
}

func (m *Manager) decryptTOTPSeed(userID, credentialID string, payload []byte, keyVersion int) (string, error) {
	if keyVersion != totpCredentialKeyVersion {
		return "", fmt.Errorf("unsupported TOTP credential key version %d", keyVersion)
	}
	aead, err := m.totpCredentialAEAD()
	if err != nil {
		return "", err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || int(payload[0]) != keyVersion {
		return "", fmt.Errorf("TOTP credential payload is invalid")
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], totpCredentialAAD(userID, credentialID))
	if err != nil {
		return "", fmt.Errorf("authenticate TOTP credential: %w", err)
	}
	if len(plaintext) == 0 {
		return "", fmt.Errorf("TOTP credential seed is empty")
	}
	return string(plaintext), nil
}

func totpCredentialAAD(userID, credentialID string) []byte {
	return []byte(userID + "\x00" + credentialID)
}
