package transport

import (
	"fmt"
	"strings"
)

const (
	TLSModeImplicit = "tls"
	TLSModeStartTLS = "starttls"
)

func RequireTLSMode(protocol, value string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(value))
	switch mode {
	case TLSModeImplicit, TLSModeStartTLS:
		return mode, nil
	default:
		protocol = strings.ToUpper(strings.TrimSpace(protocol))
		if protocol == "" {
			protocol = "MAIL"
		}
		return "", fmt.Errorf("%s requires an encrypted connection; use TLS or STARTTLS", protocol)
	}
}
