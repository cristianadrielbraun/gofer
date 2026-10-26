package handler

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// AvatarProvidersFromEnv reads GOFER_AVATAR_PROVIDERS, the comma-separated
// sender picture sources Gofer may look up. Unset means all of them; "none"
// turns lookups off. bimi and domain_icon contact servers chosen by the
// sender, so this is how to keep Gofer from reaching out to them.
func AvatarProvidersFromEnv() (map[string]bool, error) {
	return parseAvatarProviders(os.Getenv("GOFER_AVATAR_PROVIDERS"))
}

func parseAvatarProviders(raw string) (map[string]bool, error) {
	known := avatarProviderNames()
	enabled := make(map[string]bool, len(known))
	raw = strings.TrimSpace(raw)
	if raw == "" {
		for _, name := range known {
			enabled[name] = true
		}
		return enabled, nil
	}
	if strings.EqualFold(raw, "none") {
		return enabled, nil
	}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("GOFER_AVATAR_PROVIDERS: unknown provider %q; use a comma-separated list of %s, or none", name, strings.Join(known, ", "))
		}
		enabled[name] = true
	}
	return enabled, nil
}

// avatarProviderEnabled reports whether GOFER_AVATAR_PROVIDERS allows a
// provider. A Handler built without a list allows all of them.
func (h *Handler) avatarProviderEnabled(name string) bool {
	return h.avatarProviders == nil || h.avatarProviders[name]
}

func avatarProviderOffAttemptMessage(name string) string {
	return fmt.Sprintf("Skipped %s lookup: turned off by GOFER_AVATAR_PROVIDERS", name)
}
