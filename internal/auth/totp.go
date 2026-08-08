package auth

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

const (
	totpIssuer           = "Gofer"
	totpAlgorithm        = "SHA1"
	totpDigits           = 6
	totpPeriodSeconds    = 30
	totpAcceptedSkew     = 1
	totpSecretByteCount  = 20
	totpQRCodeDimensions = 256
)

var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

type SetupTOTPEnrollment struct {
	ManualKey string
	QRPNG     []byte
	Confirmed bool
	Algorithm string
	Digits    int
	Period    int
}

func newTOTPKey(accountName, randomMaterial string) (*otp.Key, error) {
	if strings.TrimSpace(randomMaterial) == "" {
		return nil, fmt.Errorf("generate TOTP secret: random material is empty")
	}
	digest := sha256.Sum256([]byte(randomMaterial))
	return totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: accountName,
		Period:      totpPeriodSeconds,
		Secret:      digest[:totpSecretByteCount],
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
}

func restoreTOTPKey(accountName, secret string) (*otp.Key, error) {
	decoded, err := totpBase32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(decoded) != totpSecretByteCount {
		return nil, fmt.Errorf("decode TOTP secret")
	}
	return totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: accountName,
		Period:      totpPeriodSeconds,
		Secret:      decoded,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
}

func setupTOTPEnrollment(accountName, secret string, confirmed bool) (*SetupTOTPEnrollment, error) {
	key, err := restoreTOTPKey(accountName, secret)
	if err != nil {
		return nil, err
	}
	image, err := key.Image(totpQRCodeDimensions, totpQRCodeDimensions)
	if err != nil {
		return nil, fmt.Errorf("render TOTP QR code: %w", err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image); err != nil {
		return nil, fmt.Errorf("encode TOTP QR code: %w", err)
	}
	return &SetupTOTPEnrollment{
		ManualKey: formatTOTPManualKey(key.Secret()),
		QRPNG:     encoded.Bytes(),
		Confirmed: confirmed,
		Algorithm: totpAlgorithm,
		Digits:    totpDigits,
		Period:    totpPeriodSeconds,
	}, nil
}

func matchTOTPCode(secret, code string, now time.Time) (int64, bool, error) {
	code = strings.TrimSpace(code)
	validShape := len(code) == totpDigits
	for _, character := range code {
		if character < '0' || character > '9' {
			validShape = false
		}
	}
	currentStep := now.UTC().Unix() / totpPeriodSeconds
	matchedStep := int64(-1)
	for _, offset := range []int64{0, -totpAcceptedSkew, totpAcceptedSkew} {
		step := currentStep + offset
		if step < 0 {
			continue
		}
		expected, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriodSeconds, 0).UTC(), totp.ValidateOpts{
			Period:    totpPeriodSeconds,
			Skew:      0,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, false, fmt.Errorf("generate expected TOTP code: %w", err)
		}
		if validShape && subtle.ConstantTimeCompare([]byte(code), []byte(expected)) == 1 && matchedStep < 0 {
			matchedStep = step
		}
	}
	return matchedStep, matchedStep >= 0, nil
}

func formatTOTPManualKey(secret string) string {
	secret = strings.ToUpper(strings.TrimSpace(secret))
	groups := make([]string, 0, (len(secret)+3)/4)
	for len(secret) > 4 {
		groups = append(groups, secret[:4])
		secret = secret[4:]
	}
	if secret != "" {
		groups = append(groups, secret)
	}
	return strings.Join(groups, " ")
}
