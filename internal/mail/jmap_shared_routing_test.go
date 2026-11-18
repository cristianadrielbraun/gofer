package mail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
)

// accountsJMAPServer keeps each account's emails apart, as a real server
// does, which a move between two accounts needs. It takes one call per
// request.
type accountsJMAPServer struct {
	*httptest.Server
	mu sync.Mutex
	// emails holds, per account, each email's mailboxes and Message-ID.
	emails map[string]map[string]accountsJMAPEmail
	copies int
	// refuseDestroys answers that many Email/set destroys with forbidden.
	refuseDestroys int
	next           int
	// identities holds, per account, the addresses it may send as.
	identities map[string][]string
}

type accountsJMAPEmail struct {
	mailboxes map[string]bool
	messageID string
}

func newAccountsJMAPServer(t *testing.T) *accountsJMAPServer {
	f := &accountsJMAPServer{emails: map[string]map[string]accountsJMAPEmail{"a1": {}, "a2": {}}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *accountsJMAPServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/jmap" {
		fmt.Fprintf(w, `{"capabilities":{"urn:ietf:params:jmap:core":{"maxCallsInRequest":1},"urn:ietf:params:jmap:mail":{}},
			"accounts":{"a1":{"name":"ana@example.com","isPersonal":true,"accountCapabilities":{"urn:ietf:params:jmap:mail":{}}},
			"a2":{"name":"support@example.com","isPersonal":false,"accountCapabilities":{"urn:ietf:params:jmap:mail":{}}}},
			"primaryAccounts":{"urn:ietf:params:jmap:mail":"a1"},"apiUrl":%q,"state":"s"}`, f.URL+"/api")
		return
	}
	var req struct {
		MethodCalls [][3]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.MethodCalls) != 1 {
		http.Error(w, "one call per request", http.StatusBadRequest)
		return
	}
	var name, callID string
	_ = json.Unmarshal(req.MethodCalls[0][0], &name)
	_ = json.Unmarshal(req.MethodCalls[0][2], &callID)
	var args map[string]json.RawMessage
	_ = json.Unmarshal(req.MethodCalls[0][1], &args)
	str := func(key string) string {
		var v string
		_ = json.Unmarshal(args[key], &v)
		return v
	}
	f.mu.Lock()
	result := f.call(name, args, str)
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"methodResponses": [][]any{{name, result, callID}}, "sessionState": "s"})
}

// call answers one method; the caller holds f.mu.
func (f *accountsJMAPServer) call(name string, args map[string]json.RawMessage, str func(string) string) map[string]any {
	account := str("accountId")
	emails := f.emails[account]
	view := func(id string, e accountsJMAPEmail) map[string]any {
		return map[string]any{"id": id, "mailboxIds": e.mailboxes, "keywords": map[string]bool{}, "messageId": []string{e.messageID}, "receivedAt": "2026-11-01T09:00:00Z"}
	}
	switch name {
	case "Identity/get":
		list := []any{}
		for i, address := range f.identities[account] {
			list = append(list, map[string]string{"id": fmt.Sprintf("%s-id%d", account, i), "email": address, "name": ""})
		}
		return map[string]any{"accountId": account, "state": "i", "list": list}
	case "Email/get":
		var ids []string
		_ = json.Unmarshal(args["ids"], &ids)
		list := []any{}
		for _, id := range ids {
			if e, ok := emails[id]; ok {
				list = append(list, view(id, e))
			}
		}
		return map[string]any{"accountId": account, "state": "e", "list": list, "notFound": []string{}}
	case "Email/query":
		var filter jmap.EmailFilter
		_ = json.Unmarshal(args["filter"], &filter)
		ids := []string{}
		for id, e := range emails {
			if e.mailboxes[filter.InMailbox] && (len(filter.Header) < 2 || strings.Contains(e.messageID, filter.Header[1])) {
				ids = append(ids, id)
			}
		}
		return map[string]any{"accountId": account, "queryState": "q", "ids": ids, "position": 0}
	case "Email/copy":
		var create map[string]struct {
			ID         string          `json:"id"`
			MailboxIDs map[string]bool `json:"mailboxIds"`
		}
		_ = json.Unmarshal(args["create"], &create)
		created := map[string]any{}
		for key, c := range create {
			f.copies++
			f.next++
			id := fmt.Sprintf("copy%d", f.next)
			emails[id] = accountsJMAPEmail{mailboxes: c.MailboxIDs, messageID: f.emails[str("fromAccountId")][c.ID].messageID}
			created[key] = map[string]string{"id": id}
		}
		return map[string]any{"accountId": account, "created": created}
	case "Email/set":
		var destroy []string
		_ = json.Unmarshal(args["destroy"], &destroy)
		destroyed, notDestroyed := []string{}, map[string]any{}
		for _, id := range destroy {
			if f.refuseDestroys > 0 {
				f.refuseDestroys--
				notDestroyed[id] = map[string]string{"type": "forbidden"}
				continue
			}
			delete(emails, id)
			destroyed = append(destroyed, id)
		}
		return map[string]any{"accountId": account, "newState": "e2", "destroyed": destroyed, "notDestroyed": notDestroyed}
	}
	return map[string]any{}
}

// A move into a shared account whose original the server refuses to remove
// fails, and tried again it removes the original without a second copy.
func TestMoveJMAPEmailAcrossChecksTheOriginalWentAndCopiesOnce(t *testing.T) {
	server := newAccountsJMAPServer(t)
	server.emails["a1"]["e1"] = accountsJMAPEmail{mailboxes: map[string]bool{"own-inbox": true}, messageID: "ticket@example.com"}
	server.refuseDestroys = 1
	client := &jmap.Client{HTTP: server.Client()}
	session, err := client.Session(t.Context(), server.URL+"/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := moveJMAPEmailAcross(t.Context(), client, session, "a1", "a2", []string{"e1"}, "own-inbox", "support-inbox"); err == nil {
		t.Fatal("a move whose original stayed was reported done")
	}
	if _, ok := server.emails["a1"]["e1"]; !ok || server.copies != 1 {
		t.Fatalf("after the refused removal: original kept %v, copies %d", ok, server.copies)
	}

	gone, ref, err := moveJMAPEmailAcross(t.Context(), client, session, "a1", "a2", []string{"e1"}, "own-inbox", "support-inbox")
	if err != nil || gone {
		t.Fatalf("retry = %v, %v", gone, err)
	}
	if _, ok := server.emails["a1"]["e1"]; ok || server.copies != 1 || len(server.emails["a2"]) != 1 {
		t.Fatalf("after the retry: original kept %v, copies %d, in the shared account %d", ok, server.copies, len(server.emails["a2"]))
	}
	if ref != "a2:copy1" {
		t.Fatalf("moved email = %q, want the first copy", ref)
	}
}

// A shared mailbox's address sends from that mailbox's account when the
// server gives the login an identity there, and otherwise from the login's
// own, if it may send as the address.
func TestJMAPSendingIdentityFallsBackToTheLoginsAccount(t *testing.T) {
	server := newAccountsJMAPServer(t)
	client := &jmap.Client{HTTP: server.Client()}
	session, err := client.Session(t.Context(), server.URL+"/.well-known/jmap")
	if err != nil {
		t.Fatal(err)
	}
	session.Accounts["a2"].AccountCapabilities[jmap.CapabilitySubmission] = json.RawMessage(`{}`)
	cases := []struct {
		name        string
		identities  map[string][]string
		wantAccount string
		wantOK      bool
	}{
		{"identity in the shared account", map[string][]string{"a1": {"ana@example.com"}, "a2": {"support@example.com"}}, "a2", true},
		{"identity only in the login's", map[string][]string{"a1": {"ana@example.com", "support@example.com"}}, "a1", true},
		{"no identity anywhere", map[string][]string{"a1": {"ana@example.com"}}, "", false},
	}
	for _, c := range cases {
		server.identities = c.identities
		account, _, ok, err := jmapSendingIdentity(t.Context(), client, session, "support@example.com")
		if err != nil || ok != c.wantOK || account != c.wantAccount {
			t.Errorf("%s: account %q, ok %v, %v", c.name, account, ok, err)
		}
	}
}
