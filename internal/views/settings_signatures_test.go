package views

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func renderSignaturesManager(t *testing.T, accounts []models.AccountSignatureData, selectedAccountID ...string) string {
	t.Helper()
	selected := ""
	if len(selectedAccountID) > 0 {
		selected = selectedAccountID[0]
	}
	var out bytes.Buffer
	if err := ComposeSignaturesManager(accounts, selected).Render(context.Background(), &out); err != nil {
		t.Fatalf("ComposeSignaturesManager.Render() error = %v", err)
	}
	return out.String()
}

func TestSignaturesDialogListsSignaturesAndAccountChoices(t *testing.T) {
	signatures := []models.Signature{
		{ID: "sig-work", Name: "Alex Example, Head of Long Signature Names at Example Corporation", HTMLBody: "<p>Alex</p>"},
		{ID: "sig-lunch", Name: "Lunch on Friday?", HTMLBody: "<p>See you</p>"},
	}
	accounts := []models.AccountSignatureData{
		{
			Account:    models.Account{ID: "acct-alex", Name: "Alex", Email: "alex@example.com"},
			Signatures: signatures,
			Settings:   models.AccountSignatureSettings{NewSignatureID: "sig-work", NewEnabled: true, ReplySignatureID: "sig-lunch", ReplyEnabled: true, ReplyPlacement: "after"},
		},
		{
			Account:    models.Account{ID: "acct-sam", Name: "Sam", Email: "sam@example.com"},
			Signatures: signatures,
		},
	}
	html := renderSignaturesManager(t, accounts)

	// Every account's choices share one grid cell. The cell's column must be
	// allowed to shrink, or a long signature name in a choice pushes the
	// rows out of the dialog (#163).
	if !regexp.MustCompile(`class="[^"]*grid-cols-\[minmax\(0,1fr\)\][^"]*" data-signature-account-panels`).MatchString(html) {
		t.Fatal("account choices grid can't shrink below its content")
	}
	for _, want := range []string{
		`data-signature-row="sig-work"`,
		`data-signature-row="sig-lunch"`,
		`data-signature-new`,
		`data-account-id="acct-alex"`,
		`data-account-id="acct-sam"`,
		`name="new_signature_id"`,
		`name="reply_placement"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("signatures dialog missing %s", want)
		}
	}
	// A name too long for the list shows in full in a tooltip, which opens
	// only while the name is cut short.
	if !regexp.MustCompile(`data-tooltip-when-cut[^>]*>\s*<button[^>]*data-signature-row="sig-work"`).MatchString(html) {
		t.Error("signature rows have no cut-name tooltip")
	}
	// Picking "No signature" is how a signature is turned off, so there's no
	// separate on/off field.
	if strings.Contains(html, `_enabled"`) {
		t.Error("signatures dialog still has an on/off field")
	}
	// The place for forwards is hidden while forwards have no signature, and
	// shown for replies, which have one.
	alex := html[strings.Index(html, `data-account-id="acct-alex"`):strings.Index(html, `data-account-id="acct-sam"`)]
	places := regexp.MustCompile(`<div class="([^"]*)" data-signature-placement>`).FindAllStringSubmatch(alex, -1)
	if len(places) != 2 || strings.Contains(places[0][1], "hidden") || !strings.Contains(places[1][1], "hidden") {
		t.Errorf("reply place should show and forward place hide, got %v", places)
	}
}

// Everything in the dialog saves by itself, so its one footer says so and
// has only Close.
func TestSignaturesDialogFooterSaysChangesSaveThemselves(t *testing.T) {
	accounts := []models.AccountSignatureData{{Account: models.Account{ID: "acct-alex", Name: "Alex", Email: "alex@example.com"}}}
	html := renderSignaturesManager(t, accounts)
	if strings.Count(html, "data-data-dialog-footer") != 1 {
		t.Fatalf("want one footer, got %d", strings.Count(html, "data-data-dialog-footer"))
	}
	if !strings.Contains(html, ">Changes save automatically</span>") {
		t.Error("footer doesn't say changes save automatically")
	}
	if strings.Contains(html, `type="submit"`) || strings.Contains(html, "data-signatures-save") {
		t.Error("signatures dialog still has a Save button")
	}
}

// The dialog refreshes its lists after a signature is added or renamed, and
// the Accounts tab stays on the account it was showing.
func TestSignaturesDialogOpensAccountsOnRequestedAccount(t *testing.T) {
	accounts := []models.AccountSignatureData{
		{Account: models.Account{ID: "acct-alex", Name: "Alex", Email: "alex@example.com"}},
		{Account: models.Account{ID: "acct-sam", Name: "Sam", Email: "sam@example.com"}},
	}
	visible := func(html, id string) bool {
		form := regexp.MustCompile(`<form class="([^"]*)" data-account-signature-settings data-account-id="` + id + `"`).FindStringSubmatch(html)
		return form != nil && !strings.Contains(form[1], "invisible")
	}
	if html := renderSignaturesManager(t, accounts, "acct-sam"); !visible(html, "acct-sam") || visible(html, "acct-alex") || !strings.Contains(html, "Sam &lt;sam@example.com&gt;") {
		t.Error("Accounts tab not on the requested account")
	}
	if html := renderSignaturesManager(t, accounts, "acct-gone"); !visible(html, "acct-alex") {
		t.Error("an unknown account should fall back to the first")
	}
}
