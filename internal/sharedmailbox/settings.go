package sharedmailbox

import "strings"

// Settings are the user's choices for shared mailboxes. They are kept with
// the user's interface settings, under the keys below.
type Settings struct {
	// ShowAddress shows each shared mailbox's address under its name in
	// the sidebar.
	ShowAddress bool `json:"show_address"`
	// AutoIdentities makes a sender identity for each shared mailbox found.
	AutoIdentities bool `json:"auto_identities"`
	// DisableRemoved puts aside the identity of a mailbox no longer shared
	// with the account; it is never deleted.
	DisableRemoved bool `json:"disable_removed"`
	// VerifySender asks the server whether it accepts a new identity's
	// address as a sender (MAIL FROM, then RSET; nothing is sent).
	VerifySender bool `json:"verify_sender"`
	// PrefixMode is PrefixDomain, PrefixNone or PrefixCustom, and
	// CustomPrefix the text PrefixCustom uses.
	PrefixMode   string `json:"name_prefix_mode"`
	CustomPrefix string `json:"name_prefix"`
	// InUnified shows the shared mailboxes' mail in the unified folders,
	// with the account's own.
	InUnified bool `json:"in_unified"`
}

const (
	KeyShowAddress    = "shared_mailboxes_show_address"
	KeyAutoIdentities = "shared_mailboxes_auto_identities"
	KeyDisableRemoved = "shared_mailboxes_disable_removed"
	KeyVerifySender   = "shared_mailboxes_verify_sender"
	KeyPrefixMode     = "shared_mailboxes_name_prefix_mode"
	KeyCustomPrefix   = "shared_mailboxes_name_prefix"
	KeyInUnified      = "shared_mailboxes_in_unified"
)

// DefaultSettings has everything on that makes shared mailboxes work by
// themselves; nothing is ever deleted.
func DefaultSettings() Settings {
	return Settings{ShowAddress: true, AutoIdentities: true, DisableRemoved: true, VerifySender: true, PrefixMode: PrefixServer, InUnified: true}
}

// SettingsFrom reads the settings from stored values; a missing or unknown
// value keeps the default.
func SettingsFrom(values map[string]string) Settings {
	s := DefaultSettings()
	flag := func(key string, into *bool) {
		switch strings.TrimSpace(values[key]) {
		case "true", "1", "on":
			*into = true
		case "false", "0", "off":
			*into = false
		}
	}
	flag(KeyShowAddress, &s.ShowAddress)
	flag(KeyAutoIdentities, &s.AutoIdentities)
	flag(KeyDisableRemoved, &s.DisableRemoved)
	flag(KeyVerifySender, &s.VerifySender)
	flag(KeyInUnified, &s.InUnified)
	switch mode := strings.TrimSpace(values[KeyPrefixMode]); mode {
	case PrefixDomain, PrefixNone, PrefixCustom, PrefixServer:
		s.PrefixMode = mode
	}
	s.CustomPrefix = strings.TrimSpace(values[KeyCustomPrefix])
	if s.PrefixMode == PrefixCustom && s.CustomPrefix == "" {
		s.PrefixMode = PrefixDomain
	}
	return s
}

// IdentityName is the name for the shared mailbox at address under s.
func (s Settings) IdentityName(address string) string {
	return s.IdentityNameFrom(address, "")
}

// IdentityNameFrom is IdentityName for a mailbox the server names
// serverName, which PrefixServer takes when there is one.
func (s Settings) IdentityNameFrom(address, serverName string) string {
	mode := s.PrefixMode
	if mode == PrefixServer {
		if name := strings.TrimSpace(serverName); name != "" {
			return name
		}
		mode = PrefixDomain
	}
	return IdentityName(address, mode, s.CustomPrefix)
}

// MailboxName is what Gofer calls a shared mailbox, in the sidebar and
// elsewhere: the name the user gave it, or else the server's, or else its
// address's local part in words ("Support").
func MailboxName(address, serverName, customName string) string {
	if name := strings.TrimSpace(customName); name != "" {
		return name
	}
	if name := strings.TrimSpace(serverName); name != "" {
		return name
	}
	return IdentityName(address, PrefixNone, "")
}
