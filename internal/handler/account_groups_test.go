package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

func TestAccountGroupsHandlersOwnershipProxyAndContainerOrder(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()

	proxyID, err := h.accountStore.SaveOutboundProxy(ctx, config.OutboundProxyInput{
		Name: "GroupSOCKS", Type: "socks5", Host: "198.51.100.20", Port: 1080,
	}, "owner")
	if err != nil {
		t.Fatal(err)
	}

	asUser := func(userID string, r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: userID, Username: userID}))
	}

	// Create top-level group with proxy.
	createReq := httptest.NewRequest(http.MethodPost, "/api/account-groups", strings.NewReader(url.Values{
		"name":  {"Work"},
		"proxy": {proxyID},
	}.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createRec := httptest.NewRecorder()
	h.handleCreateAccountGroup(createRec, asUser("owner", createReq))
	if createRec.Code != http.StatusOK || createRec.Header().Get("HX-Retarget") != "#settings-content #settings-accounts-section" {
		t.Fatalf("create group status = %d retarget = %q, body = %s", createRec.Code, createRec.Header().Get("HX-Retarget"), createRec.Body.String())
	}

	groups, err := db.ListAccountGroups(ctx, "owner")
	if err != nil || len(groups) != 1 {
		t.Fatalf("owner groups = %+v, %v", groups, err)
	}
	workID := groups[0].ID

	// Duplicate group name returns 200 form error panel without HX-Retarget so HTMX swaps inside the dialog.
	dupReq := httptest.NewRequest(http.MethodPost, "/api/account-groups", strings.NewReader(url.Values{
		"name": {"work"},
	}.Encode()))
	dupReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dupRec := httptest.NewRecorder()
	h.handleCreateAccountGroup(dupRec, asUser("owner", dupReq))
	if dupRec.Code != http.StatusOK || dupRec.Header().Get("HX-Retarget") != "" || !strings.Contains(dupRec.Body.String(), `role="alert"`) {
		t.Fatalf("duplicate group = %d retarget=%q body=%q, want 200 alert panel", dupRec.Code, dupRec.Header().Get("HX-Retarget"), dupRec.Body.String())
	}

	// Foreign user gets 404 on edit, delete, group proxy, and moving into owner's group.
	foreignEdit := httptest.NewRecorder()
	h.handleEditAccountGroup(foreignEdit, asUser("attacker", withPath(httptest.NewRequest(http.MethodGet, "/api/account-groups/"+workID+"/edit", nil), "id", workID)))
	if foreignEdit.Code != http.StatusNotFound {
		t.Fatalf("foreign edit status = %d, want 404", foreignEdit.Code)
	}
	foreignProxy := httptest.NewRecorder()
	h.handleManageAccountGroupProxy(foreignProxy, asUser("attacker", withPath(httptest.NewRequest(http.MethodGet, "/api/account-groups/"+workID+"/proxy/manage", nil), "id", workID)))
	if foreignProxy.Code != http.StatusNotFound {
		t.Fatalf("foreign group proxy status = %d, want 404", foreignProxy.Code)
	}
	foreignMoveReq := httptest.NewRequest(http.MethodPost, "/api/accounts/attacker-account/group", strings.NewReader(url.Values{"group_id": {workID}}.Encode()))
	foreignMoveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	foreignMove := httptest.NewRecorder()
	h.handleSaveAccountGroupMembership(foreignMove, asUser("attacker", withPath(foreignMoveReq, "id", "attacker-account")))
	if foreignMove.Code != http.StatusNotFound {
		t.Fatalf("foreign move to owner group status = %d, want 404", foreignMove.Code)
	}

	// HTMX move to a deleted/missing group renders an inline alert inside the dialog panel instead of 404.
	staleMoveReq := httptest.NewRequest(http.MethodPost, "/api/accounts/victim-account/group", strings.NewReader(url.Values{"group_id": {"missing-group"}}.Encode()))
	staleMoveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	staleMoveReq.Header.Set("HX-Request", "true")
	staleMoveRec := httptest.NewRecorder()
	h.handleSaveAccountGroupMembership(staleMoveRec, asUser("owner", withPath(staleMoveReq, "id", "victim-account")))
	if staleMoveRec.Code != http.StatusOK || staleMoveRec.Header().Get("HX-Retarget") != "" || !strings.Contains(staleMoveRec.Body.String(), `role="alert"`) {
		t.Fatalf("stale HTMX move = %d retarget=%q body=%q, want 200 alert panel", staleMoveRec.Code, staleMoveRec.Header().Get("HX-Retarget"), staleMoveRec.Body.String())
	}

	// Owner moves victim-account into Work group.
	moveReq := httptest.NewRequest(http.MethodPost, "/api/accounts/victim-account/group", strings.NewReader(url.Values{"group_id": {workID}}.Encode()))
	moveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	moveRec := httptest.NewRecorder()
	h.handleSaveAccountGroupMembership(moveRec, asUser("owner", withPath(moveReq, "id", "victim-account")))
	if moveRec.Code != http.StatusOK || moveRec.Header().Get("HX-Retarget") != "#settings-content #settings-accounts-section" {
		t.Fatalf("move account status = %d retarget=%q, body = %s", moveRec.Code, moveRec.Header().Get("HX-Retarget"), moveRec.Body.String())
	}

	// Account proxy dialog shows inherited group proxy.
	proxyDialog := httptest.NewRecorder()
	h.handleManageAccountProxy(proxyDialog, asUser("owner", withPath(httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/proxy/manage", nil), "id", "victim-account")))
	if proxyDialog.Code != http.StatusOK || !strings.Contains(proxyDialog.Body.String(), "Group (Work: GroupSOCKS)") {
		t.Fatalf("account proxy dialog = %d %q, want inherited group proxy label", proxyDialog.Code, proxyDialog.Body.String())
	}

	// Create child group and reorder inside Work.
	sub, err := db.CreateAccountGroup(ctx, "owner", workID, "Sub", "")
	if err != nil {
		t.Fatal(err)
	}
	// Proxy choice endpoint for a child/account inheriting from Work offers the inherited group proxy.
	choiceRec := httptest.NewRecorder()
	h.handleAccountProxyChoice(choiceRec, asUser("owner", httptest.NewRequest(http.MethodGet, "/api/accounts/proxy-choice?group="+url.QueryEscape(sub.ID), nil)))
	if choiceRec.Code != http.StatusOK || !strings.Contains(choiceRec.Body.String(), "Group (Work: GroupSOCKS)") {
		t.Fatalf("proxy-choice for sub group = %d %q, want inherited Work proxy label", choiceRec.Code, choiceRec.Body.String())
	}
	proxyCtx, err := h.proxyChoiceContext(ctx, "owner", "", sub.ID)
	if err != nil {
		t.Fatalf("proxyChoiceContext(sub) error = %v", err)
	}
	if got := netguard.ProxyFrom(proxyCtx); got == nil || got.Host != "198.51.100.20" {
		t.Fatalf("proxyChoiceContext(sub) proxy = %+v, want 198.51.100.20", got)
	}

	orderRec := httptest.NewRecorder()
	h.handleAccountOrder(orderRec, contactSelectionRequest(http.MethodPost, "/api/accounts/order", "owner", url.Values{
		"group_id": {workID},
		"item":     {"group:" + sub.ID, "account:victim-account"},
	}))
	if orderRec.Code != http.StatusNoContent {
		t.Fatalf("container order status = %d, want 204", orderRec.Code)
	}

	// Tree picker dialog lists owner's nested groups/accounts and rejects foreign access.
	foreignPicker := httptest.NewRecorder()
	h.handleManageAccountGroupAccounts(foreignPicker, asUser("attacker", withPath(httptest.NewRequest(http.MethodGet, "/api/account-groups/"+sub.ID+"/accounts/manage", nil), "id", sub.ID)))
	if foreignPicker.Code != http.StatusNotFound {
		t.Fatalf("foreign group accounts picker status = %d, want 404", foreignPicker.Code)
	}

	pickerRec := httptest.NewRecorder()
	h.handleManageAccountGroupAccounts(pickerRec, asUser("owner", withPath(httptest.NewRequest(http.MethodGet, "/api/account-groups/"+sub.ID+"/accounts/manage", nil), "id", sub.ID)))
	if pickerRec.Code != http.StatusOK || !strings.Contains(pickerRec.Body.String(), `value="victim-account"`) || !strings.Contains(pickerRec.Body.String(), "Work") {
		t.Fatalf("picker dialog = %d %q, want victim-account inside Work tree", pickerRec.Code, pickerRec.Body.String())
	}

	// Batch add existing accounts moves victim-account from Work into Sub.
	batchReq := httptest.NewRequest(http.MethodPost, "/api/account-groups/"+sub.ID+"/accounts", strings.NewReader(url.Values{
		"account_id": {"victim-account"},
	}.Encode()))
	batchReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	batchRec := httptest.NewRecorder()
	h.handleSaveAccountGroupAccounts(batchRec, asUser("owner", withPath(batchReq, "id", sub.ID)))
	if batchRec.Code != http.StatusOK || batchRec.Header().Get("HX-Retarget") != "#settings-content #settings-accounts-section" {
		t.Fatalf("batch add accounts = %d retarget=%q body=%q", batchRec.Code, batchRec.Header().Get("HX-Retarget"), batchRec.Body.String())
	}
	ownerAccounts, err := db.GetAccounts(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	var movedGroup string
	for _, a := range ownerAccounts {
		if a.ID == "victim-account" {
			movedGroup = a.GroupID
		}
	}
	if movedGroup != sub.ID {
		t.Fatalf("victim-account GroupID = %q, want %q", movedGroup, sub.ID)
	}

	// Admin diagnostics resolve inherited group proxy even when accountStore is nil on the admin handler.
	adminOnly := &Handler{db: db}
	mailboxes, has, err := adminOnly.adminUserMailboxes(ctx, &auth.User{ID: "admin", IsAdmin: true}, auth.AdministratorUserSummary{
		ID:       "owner",
		UserType: auth.UserTypeWebmail,
	})
	if err != nil || !has || len(mailboxes) == 0 {
		t.Fatalf("adminUserMailboxes = %+v, has=%v, err=%v", mailboxes, has, err)
	}
	if !strings.Contains(mailboxes[0].Proxy, "GroupSOCKS") {
		t.Fatalf("admin mailbox proxy = %q, want GroupSOCKS inherited from Work->Sub", mailboxes[0].Proxy)
	}
}

func TestSingleDBOAuthReconnectUsesInheritedGroupProxy(t *testing.T) {
	h, mailCredentials, db := newAccountOAuthFlowTestHandler(t)
	accountStore, err := config.NewAccountStore(db, testMailboxCredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	h.accountStore = accountStore
	ctx := t.Context()

	if err := db.SetPeopleMayAddOutboundProxies(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, name) VALUES ('user', 'user', 'user', 'User')`); err != nil {
		t.Fatal(err)
	}
	proxyID, err := accountStore.SaveOutboundProxy(ctx, config.OutboundProxyInput{
		Name: "OAuthGroupProxy", Type: "socks5", Host: "198.51.100.44", Port: 1080,
	}, "user")
	if err != nil {
		t.Fatal(err)
	}
	grp, err := db.CreateAccountGroup(ctx, "user", "", "OAuthWork", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, imap_host, imap_port, smtp_host, smtp_port, username, encrypted_password, auth_method, group_id)
		VALUES ('gmail-acc', 'user', 'gmail', 'user@gmail.com', 'User Gmail', 'imap.gmail.com', 993, 'smtp.gmail.com', 465, '', '', 'oauth2', ?)
	`, grp.ID); err != nil {
		t.Fatal(err)
	}

	session, err := h.auth.CreateAuthenticatedSession(ctx, "user", "Browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	const authorizePath = "/api/accounts/oauth2/authorize"
	form := url.Values{
		"provider":             {providers.ProviderGmail},
		"email_address":        {"user@gmail.com"},
		"flow_action":          {"reconnect"},
		auth.CSRFFormFieldName: {csrfProofForSession(t, h.auth, session.Token, authorizePath)},
	}
	req := httptest.NewRequest(http.MethodPost, authorizePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	rec := httptest.NewRecorder()
	h.auth.Middleware(http.HandlerFunc(h.handleAccountOAuthAuthorize)).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %q, want 303", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	flow, err := mailCredentials.ConsumeAccountOAuthFlow(ctx, loc.Query().Get("state"), "user", session.Token, providers.ProviderGmail)
	if err != nil {
		t.Fatal(err)
	}
	if flow.FormData["account_id"] != "gmail-acc" {
		t.Fatalf("reconnect flow account_id = %q, want gmail-acc", flow.FormData["account_id"])
	}
	proxyCtx, err := h.oauthFlowProxyContext(ctx, "user", flow.FormData)
	if err != nil {
		t.Fatal(err)
	}
	if got := netguard.ProxyFrom(proxyCtx); got == nil || got.Host != "198.51.100.44" {
		t.Fatalf("reconnect proxy = %+v, want 198.51.100.44", got)
	}
}
