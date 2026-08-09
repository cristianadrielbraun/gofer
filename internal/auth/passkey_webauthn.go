package auth

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const passkeyCeremonyLifetime = 5 * time.Minute

type passkeyRegistrationFactory func(origin string) (passkeyRegistrationCeremony, error)

type passkeyAssertionFactory func(origin string) (passkeyAssertionCeremony, error)

type passkeyRegistrationCeremony interface {
	Begin(passkeyUser) (creationJSON, sessionJSON []byte, err error)
	Finish(passkeyUser, []byte, []byte) (*passkeyCredentialRecord, error)
}

type passkeyAssertionUserLookup func(rawID, userHandle []byte) (passkeyUser, error)

type passkeyAssertionCeremony interface {
	Begin(*passkeyUser) (requestJSON, sessionJSON []byte, err error)
	Finish(passkeyUser, []byte, []byte) (*passkeyCredentialRecord, error)
	FinishDiscoverable(passkeyAssertionUserLookup, []byte, []byte) (*passkeyCredentialRecord, error)
}

type passkeyUser struct {
	ID          []byte
	Name        string
	DisplayName string
	Credentials []webauthn.Credential
}

func (user passkeyUser) WebAuthnID() []byte                         { return user.ID }
func (user passkeyUser) WebAuthnName() string                       { return user.Name }
func (user passkeyUser) WebAuthnDisplayName() string                { return user.DisplayName }
func (user passkeyUser) WebAuthnCredentials() []webauthn.Credential { return user.Credentials }

type passkeyCredentialRecord struct {
	CredentialID   []byte
	PublicKey      []byte
	SignCount      uint32
	AAGUID         []byte
	Transports     []string
	Attachment     string
	Flags          byte
	CloneWarning   bool
	BackupEligible bool
	BackupState    bool
	Record         []byte
}

type goWebAuthnRegistration struct {
	webAuthn *webauthn.WebAuthn
}

type goWebAuthnAssertion struct {
	webAuthn *webauthn.WebAuthn
}

func newPasskeyRegistrationCeremony(origin string) (passkeyRegistrationCeremony, error) {
	instance, err := newPasskeyWebAuthn(origin)
	if err != nil {
		return nil, err
	}
	return &goWebAuthnRegistration{webAuthn: instance}, nil
}

func newPasskeyAssertionCeremony(origin string) (passkeyAssertionCeremony, error) {
	instance, err := newPasskeyWebAuthn(origin)
	if err != nil {
		return nil, err
	}
	return &goWebAuthnAssertion{webAuthn: instance}, nil
}

func newPasskeyWebAuthn(origin string) (*webauthn.WebAuthn, error) {
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return nil, err
	}
	instance, err := webauthn.New(&webauthn.Config{
		RPDisplayName:         "Gofer",
		RPID:                  rpID,
		RPOrigins:             []string{canonicalOrigin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyLifetime},
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyLifetime},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure WebAuthn relying party: %w", err)
	}
	return instance, nil
}

func canonicalWebAuthnRelyingParty(origin string) (canonicalOrigin, rpID string, err error) {
	canonicalOrigin, err = canonicalAuthOrigin(origin)
	if err != nil {
		return "", "", err
	}
	parsed, err := url.Parse(canonicalOrigin)
	if err != nil {
		return "", "", fmt.Errorf("parse WebAuthn origin: %w", err)
	}
	rpID = strings.ToLower(parsed.Hostname())
	if rpID == "" {
		return "", "", fmt.Errorf("WebAuthn relying-party ID is empty")
	}
	if parsed.Scheme == "http" && !isWebAuthnLoopbackHost(rpID) {
		return "", "", fmt.Errorf("WebAuthn requires HTTPS outside loopback development origins")
	}
	return canonicalOrigin, rpID, nil
}

func isWebAuthnLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (registration *goWebAuthnRegistration) Begin(user passkeyUser) ([]byte, []byte, error) {
	creation, session, err := registration.webAuthn.BeginRegistration(
		user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
		webauthn.WithExclusions(webauthn.Credentials(user.Credentials).CredentialDescriptors()),
	)
	if err != nil {
		return nil, nil, err
	}
	creationJSON, err := json.Marshal(creation)
	if err != nil {
		return nil, nil, fmt.Errorf("encode WebAuthn creation options: %w", err)
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return nil, nil, fmt.Errorf("encode WebAuthn registration session: %w", err)
	}
	return creationJSON, sessionJSON, nil
}

func (registration *goWebAuthnRegistration) Finish(user passkeyUser, sessionJSON, responseJSON []byte) (*passkeyCredentialRecord, error) {
	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		return nil, fmt.Errorf("decode WebAuthn registration session: %w", err)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(responseJSON)
	if err != nil {
		return nil, err
	}
	credential, err := registration.webAuthn.CreateCredential(user, session, parsed)
	if err != nil {
		return nil, err
	}
	return passkeyCredentialRecordFromCredential(credential)
}

func (assertion *goWebAuthnAssertion) Begin(user *passkeyUser) ([]byte, []byte, error) {
	var (
		request *protocol.CredentialAssertion
		session *webauthn.SessionData
		err     error
	)
	if user == nil {
		request, session, err = assertion.webAuthn.BeginDiscoverableLogin(
			webauthn.WithUserVerification(protocol.VerificationRequired),
		)
	} else {
		request, session, err = assertion.webAuthn.BeginLogin(
			*user, webauthn.WithUserVerification(protocol.VerificationRequired),
		)
	}
	if err != nil {
		return nil, nil, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, nil, fmt.Errorf("encode WebAuthn assertion options: %w", err)
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return nil, nil, fmt.Errorf("encode WebAuthn assertion session: %w", err)
	}
	return requestJSON, sessionJSON, nil
}

func (assertion *goWebAuthnAssertion) Finish(user passkeyUser, sessionJSON, responseJSON []byte) (*passkeyCredentialRecord, error) {
	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		return nil, fmt.Errorf("decode WebAuthn assertion session: %w", err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(responseJSON)
	if err != nil {
		return nil, err
	}
	credential, err := assertion.webAuthn.ValidateLogin(user, session, parsed)
	if err != nil {
		return nil, err
	}
	return passkeyCredentialRecordFromCredential(credential)
}

func (assertion *goWebAuthnAssertion) FinishDiscoverable(lookup passkeyAssertionUserLookup, sessionJSON, responseJSON []byte) (*passkeyCredentialRecord, error) {
	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		return nil, fmt.Errorf("decode discoverable WebAuthn assertion session: %w", err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(responseJSON)
	if err != nil {
		return nil, err
	}
	_, credential, err := assertion.webAuthn.ValidatePasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			user, err := lookup(rawID, userHandle)
			if err != nil {
				return nil, err
			}
			return user, nil
		},
		session,
		parsed,
	)
	if err != nil {
		return nil, err
	}
	return passkeyCredentialRecordFromCredential(credential)
}

func passkeyCredentialRecordFromCredential(credential *webauthn.Credential) (*passkeyCredentialRecord, error) {
	if credential == nil {
		return nil, fmt.Errorf("WebAuthn credential is missing")
	}
	record, err := credential.MarshalMsg(nil)
	if err != nil {
		return nil, fmt.Errorf("encode WebAuthn credential record: %w", err)
	}
	transports := make([]string, len(credential.Transport))
	for index, transport := range credential.Transport {
		transports[index] = string(transport)
	}
	return &passkeyCredentialRecord{
		CredentialID:   append([]byte(nil), credential.ID...),
		PublicKey:      append([]byte(nil), credential.PublicKey...),
		SignCount:      credential.Authenticator.SignCount,
		AAGUID:         append([]byte(nil), credential.Authenticator.AAGUID...),
		Transports:     transports,
		Attachment:     string(credential.Authenticator.Attachment),
		Flags:          byte(credential.Flags.ProtocolValue()),
		CloneWarning:   credential.Authenticator.CloneWarning,
		BackupEligible: credential.Flags.BackupEligible,
		BackupState:    credential.Flags.BackupState,
		Record:         record,
	}, nil
}
