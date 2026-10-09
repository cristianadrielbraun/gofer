package server

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func provisionInitialSetupToken(ctx context.Context, manager *auth.Manager, configuredToken string, console io.Writer) error {
	if !manager.IsEnabled() {
		return nil
	}
	provision, err := manager.EnsureSetupToken(ctx, configuredToken)
	if err != nil {
		return err
	}
	if provision.State.Initialized {
		return nil
	}
	var notice strings.Builder
	fmt.Fprintf(&notice, "\n────────────────────────────────────────────────────────────\n %s SETUP REQUIRED\n\n Open: %s/setup\n", strings.ToUpper(string(manager.Config().AuthenticationMode())), strings.TrimRight(manager.Config().BaseURL, "/"))
	if provision.Created && !provision.Configured {
		if provision.Token == "" || provision.State.TokenExpiresAt == nil {
			return fmt.Errorf("generated setup token is incomplete")
		}
		fmt.Fprintf(&notice, "\n Token (shown once):\n setup_token: %s\n", provision.Token)
	} else if provision.Configured {
		notice.WriteString("\n Use the token you supplied in GOFER_SETUP_TOKEN.\n")
	} else {
		notice.WriteString("\n The setup token was issued earlier and cannot be displayed again.\n")
	}
	if provision.State.TokenExpiresAt != nil {
		fmt.Fprintf(&notice, " Expires: %s (server local time)\n", provision.State.TokenExpiresAt.Local().Format("2006-01-02 15:04:05 MST (UTC-07:00)"))
	}
	notice.WriteString("\n Lost or expired token? Stop Gofer, then run:\n ./gofer auth setup-token rotate\n Use the same GOFER_DB_PATH, then restart Gofer.\n")
	if err := writeLocalProfileNotice(ctx, manager, &notice); err != nil {
		return err
	}
	notice.WriteString("────────────────────────────────────────────────────────────\n\n")
	if _, err := io.WriteString(console, notice.String()); err != nil {
		return fmt.Errorf("write setup notice to local console: %w", err)
	}
	return nil
}

// A converted open or personal installation keeps its profile as a regular
// user. Setup creates a separate administrator; a profile without credentials
// gets a fresh link to set its first password on each start until setup ends.
func writeLocalProfileNotice(ctx context.Context, manager *auth.Manager, notice *strings.Builder) error {
	profile, err := manager.PendingLocalProfile(ctx)
	if err != nil || profile == nil {
		return err
	}
	base := strings.TrimRight(manager.Config().BaseURL, "/")
	notice.WriteString("\n This installation was converted from open or personal mode.\n Setup creates a new administrator account. Your existing profile\n")
	fmt.Fprintf(notice, " %q keeps its mailboxes and becomes a regular user.\n", profile.Username)
	if profile.HasCredentials {
		notice.WriteString(" It signs in with the same credentials as before.\n")
		return nil
	}
	token, err := manager.IssueLocalProfilePasswordLink(ctx)
	if err != nil {
		return fmt.Errorf("issue local profile password token: %w", err)
	}
	fmt.Fprintf(notice, "\n It has no password yet. To set one, open: %s/account/enroll\n reset_token: %s\n Expires: %s (server local time)\n",
		base, token.Token, token.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST (UTC-07:00)"))
	notice.WriteString(" Restarting Gofer before setup issues a new token; afterwards, an\n administrator can send one from the Users page.\n")
	return nil
}
