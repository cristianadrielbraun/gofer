package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

func TestComposeOneTimeFromRoutes(t *testing.T) {
	gmail := models.Account{ID: "mail", Provider: providers.ProviderGmail, Name: "Robin", Email: "robin@example.com", FromRule: providers.AccountFromRule(providers.ProviderGmail, "smtp.gmail.com", nil),
		Identities: []models.Identity{
			{ID: "shop", AccountID: "mail", Email: "shop@example.org", SMTPHost: "smtp.example.org", SMTPPort: 587, SMTPUsername: "shop"},
			{ID: "deals", AccountID: "mail", Email: "deals@example.org", SMTPHost: "SMTP.example.org", SMTPPort: 587, SMTPUsername: "shop"},
			{ID: "alias", AccountID: "mail", Email: "alias@example.com"},
		}}
	routes := composeFromRoutes(gmail)
	if len(routes) != 2 {
		t.Fatalf("routes = %+v, want the account's and one shared identity server", routes)
	}
	if routes[0].Reason == "" || routes[1].IdentityID != "shop" || routes[1].Reason != "" || !routes[1].Warns() {
		t.Fatalf("routes = %+v", routes)
	}

	jmap := models.Account{ID: "home", Provider: providers.ProviderJMAP, Email: "sam@example.net", FromRule: providers.AccountFromRule(providers.ProviderJMAP, "", []string{"example.net"})}
	if route := composeFromRoutes(jmap)[0]; route.Reason != "" || route.Warns() || route.Domains() != "example.net" || !strings.Contains(route.Note, "example.net") {
		t.Fatalf("JMAP route = %+v", route)
	}
	jmapListed := models.Account{ID: "other", Provider: providers.ProviderJMAP, Email: "kai@example.net", FromRule: providers.AccountFromRule(providers.ProviderJMAP, "", nil)}
	if composeHasOneTimeFrom([]models.Account{jmapListed}) {
		t.Fatal("an account that sends only from its listed addresses offers a one-time From")
	}

	var out bytes.Buffer
	if err := ComposeDialog([]models.Account{gmail, jmap}).Render(context.Background(), &out); err != nil {
		t.Fatalf("ComposeDialog.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Custom From…",
		`name="from_email"`,
		`name="send_via"`,
		`data-tui-selectbox-value="account:mail" data-tui-selectbox-selected="false" data-tui-selectbox-disabled="true"`,
		`data-tui-selectbox-value="identity:shop"`,
		`data-tui-selectbox-value="account:home"`,
		`data-from-domains="example.net"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("compose dialog lacks %q", want)
		}
	}
	if strings.Contains(html, "identity:deals") {
		t.Error("an identity on the same server and login is listed twice")
	}
}
