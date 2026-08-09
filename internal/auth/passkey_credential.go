package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	passkeyCredentialKeyVersion = 1
	passkeyCredentialKeyContext = "gofer/auth/passkey-credential/v1"
)

func (m *Manager) passkeyCredentialAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("passkey credential encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(passkeyCredentialKeyContext))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create passkey credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create passkey credential AEAD: %w", err)
	}
	return aead, nil
}

func (m *Manager) encryptPasskeyCredential(userID, rowID string, record []byte) ([]byte, error) {
	if len(record) == 0 {
		return nil, fmt.Errorf("passkey credential record is empty")
	}
	aead, err := m.passkeyCredentialAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate passkey credential nonce: %w", err)
	}
	payload := []byte{passkeyCredentialKeyVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, record, passkeyCredentialAAD(userID, rowID)), nil
}

func (m *Manager) decryptPasskeyCredential(userID, rowID string, payload []byte, keyVersion int) (*webauthn.Credential, error) {
	if keyVersion != passkeyCredentialKeyVersion {
		return nil, fmt.Errorf("unsupported passkey credential key version %d", keyVersion)
	}
	aead, err := m.passkeyCredentialAEAD()
	if err != nil {
		return nil, err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || int(payload[0]) != keyVersion {
		return nil, fmt.Errorf("passkey credential payload is invalid")
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], passkeyCredentialAAD(userID, rowID))
	if err != nil {
		return nil, fmt.Errorf("authenticate passkey credential: %w", err)
	}
	var credential webauthn.Credential
	remaining, err := credential.UnmarshalMsg(plaintext)
	if err != nil {
		return nil, fmt.Errorf("decode passkey credential: %w", err)
	}
	if len(remaining) != 0 {
		return nil, fmt.Errorf("decode passkey credential: trailing data")
	}
	if len(credential.ID) == 0 || len(credential.PublicKey) == 0 {
		return nil, fmt.Errorf("passkey credential record is incomplete")
	}
	return &credential, nil
}

func validatePasskeyCredentialBinding(credential *webauthn.Credential, credentialID, publicKey []byte) error {
	if credential == nil || !bytes.Equal(credential.ID, credentialID) || !bytes.Equal(credential.PublicKey, publicKey) {
		return fmt.Errorf("passkey credential record does not match lookup metadata")
	}
	return nil
}

func passkeyCredentialAAD(userID, rowID string) []byte {
	return []byte(userID + "\x00" + rowID)
}
