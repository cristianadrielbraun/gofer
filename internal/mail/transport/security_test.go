package transport

import "testing"

func TestRequireTLSMode(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "implicit TLS", value: "tls", want: TLSModeImplicit, ok: true},
		{name: "STARTTLS", value: " STARTTLS ", want: TLSModeStartTLS, ok: true},
		{name: "plaintext", value: "none"},
		{name: "empty", value: ""},
		{name: "unknown", value: "optional"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RequireTLSMode("imap", tt.value)
			if tt.ok && err != nil {
				t.Fatalf("RequireTLSMode() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("RequireTLSMode() = %q, want error", got)
			}
			if got != tt.want {
				t.Fatalf("RequireTLSMode() = %q, want %q", got, tt.want)
			}
		})
	}
}
