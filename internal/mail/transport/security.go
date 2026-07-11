package transport

import (
	"fmt"
	"strings"
)

const (
	TLSModeImplicit  = "tls"
	TLSModeStartTLS  = "starttls"
	TLSModePlaintext = "plaintext"
)

func RequireTLSMode(protocol, value string) (string, error) {
	return RequireTLSModeWithPlaintext(protocol, value, false)
}

func RequireTLSModeWithPlaintext(protocol, value string, allowPlaintext bool) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(value))
	switch mode {
	case TLSModeImplicit, TLSModeStartTLS:
		return mode, nil
	case TLSModePlaintext:
		if allowPlaintext {
			return mode, nil
		}
		return "", fmt.Errorf("%s plaintext connections require an admin-approved server exception", protocolLabel(protocol))
	default:
		return "", fmt.Errorf("%s requires an encrypted connection; use TLS or STARTTLS", protocolLabel(protocol))
	}
}

func protocolLabel(protocol string) string {
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	if protocol == "" {
		return "MAIL"
	}
	return protocol
}
