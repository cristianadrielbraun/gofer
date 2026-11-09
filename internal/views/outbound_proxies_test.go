package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A managed installation's administrator sees who each proxy is for and the
// switch for people's own proxies; a person's own panel has neither, nor the
// default.
func TestOutboundProxiesPanelForAdministratorsAndPeople(t *testing.T) {
	proxies := []models.OutboundProxyView{
		{ID: "office", Name: "Office", Type: "http", Host: "proxy.example.test", Port: 3128, Everyone: true, IsDefault: true},
		{ID: "lab", Name: "Lab", Type: "socks5", Host: "lab.example.test", Port: 1080,
			People:     []models.AdminWebmailUserOption{{ID: "ana", Username: "ana"}},
			Candidates: []models.AdminWebmailUserOption{{ID: "ben", Username: "ben"}}},
	}
	admin := renderForTest(t, func(out *bytes.Buffer) error {
		return OutboundProxiesPanel(models.MailSecurityAdminData{ActionBase: "/admin/security", Proxies: proxies, ProxyPeople: true, CSRFTokens: map[string]string{}}).Render(context.Background(), out)
	})
	for _, want := range []string{`action="/admin/security/proxies/policy"`, "Let people add their own", `action="/admin/security/proxies/office/everyone"`,
		"For everyone", "Limit to chosen people", `action="/admin/security/proxies/lab/people"`, `name="chosen" value="false"`, ">ana<", "ben", `name="default"`} {
		if !strings.Contains(admin, want) {
			t.Fatalf("administrator's panel missing %q", want)
		}
	}
	own := renderForTest(t, func(out *bytes.Buffer) error {
		return OutboundProxiesPanel(models.MailSecurityAdminData{ActionBase: "/settings/security", Proxies: proxies[:1], OwnProxies: true, CSRFTokens: map[string]string{}}).Render(context.Background(), out)
	})
	if !strings.Contains(own, "Your proxies") || !strings.Contains(own, `action="/settings/security/proxies"`) {
		t.Fatal("a person's panel isn't theirs")
	}
	for _, unwanted := range []string{`name="default"`, "/proxies/policy", "/everyone", "/people"} {
		if strings.Contains(own, unwanted) {
			t.Fatalf("a person's panel has %q", unwanted)
		}
	}
}

// Adding a proxy changes nothing by itself: the form doesn't offer it as the
// default unless asked to, even when it's the first.
func TestNewOutboundProxyIsNotTheDefault(t *testing.T) {
	html := renderForTest(t, func(out *bytes.Buffer) error {
		return OutboundProxiesPanel(models.MailSecurityAdminData{ActionBase: "/admin/security", CSRFTokens: map[string]string{}}).Render(context.Background(), out)
	})
	if !strings.Contains(html, `name="default"`) || strings.Contains(html, `name="default" value="yes" checked`) {
		t.Fatal("the first proxy's form makes it the default unasked")
	}
}
