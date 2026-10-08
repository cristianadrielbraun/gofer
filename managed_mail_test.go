package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
)

type managedLiteral struct {
	*strings.Reader
	size int64
}

func (r managedLiteral) Size() int64 { return r.size }

type managedSMTPBackend struct {
	mu       sync.Mutex
	accepted map[string][]string
	users    map[string]bool
}
type managedSMTPSession struct {
	backend *managedSMTPBackend
	owner   string
}

func (b *managedSMTPBackend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &managedSMTPSession{backend: b}, nil
}
func (s *managedSMTPSession) Reset()        {}
func (s *managedSMTPSession) Logout() error { return nil }
func (s *managedSMTPSession) Mail(from string, _ *smtp.MailOptions) error {
	if from != s.owner+"@example.test" {
		return errors.New("sender does not match authenticated mailbox")
	}
	return nil
}
func (s *managedSMTPSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *managedSMTPSession) Data(r io.Reader) error {
	wire, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.accepted[s.owner] = append(s.backend.accepted[s.owner], string(wire))
	return nil
}
func (s *managedSMTPSession) AuthMechanisms() []string { return []string{"PLAIN"} }
func (s *managedSMTPSession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != "PLAIN" {
		return nil, errors.New("unsupported fixture authentication")
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		if !s.backend.users[username] || password != "synthetic-only" {
			return errors.New("invalid fixture credentials")
		}
		s.owner = username
		return nil
	}), nil
}

// Wrap the library backend only to observe real, authenticated IDLE sessions.
// Protocol responses and mailbox updates remain the library implementation.
type managedIMAPActivity struct {
	mu   sync.Mutex
	idle map[string]int
}

type managedIMAPSession struct {
	*imapmemserver.UserSession
	users    map[string]*imapmemserver.User
	activity *managedIMAPActivity
	owner    string
}

func (s *managedIMAPSession) Login(username, password string) error {
	user := s.users[username]
	if user == nil {
		return imapserver.ErrAuthFailed
	}
	if err := user.Login(username, password); err != nil {
		return err
	}
	s.owner = username
	s.UserSession = imapmemserver.NewUserSession(user)
	return nil
}

func (s *managedIMAPSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	s.activity.mu.Lock()
	s.activity.idle[s.owner]++
	s.activity.mu.Unlock()
	defer func() {
		s.activity.mu.Lock()
		s.activity.idle[s.owner]--
		s.activity.mu.Unlock()
	}()
	return s.UserSession.Idle(w, stop)
}

func (a *managedIMAPActivity) idleCount(owner string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.idle[owner]
}

type managedMailFixture struct {
	app              *managedApplication
	path             string
	accounts, tokens map[string]string
	remote           map[string]*imapmemserver.User
	smtp             *managedSMTPBackend
	activity         *managedIMAPActivity
	initialMail      map[string][]managedSeedMail
}

type managedSeedMail struct {
	wire  string
	flags []imap.Flag
}

type managedMailOptions struct {
	Owners          []string
	Messages        int
	MessageCounts   map[string]int
	SkipApplication bool
}

func newManagedMailFixture(t *testing.T, configureSource ...func(*storage.DB, *config.AccountStore, map[string]string)) *managedMailFixture {
	t.Helper()
	return newManagedMailFixtureOptions(t, managedMailOptions{Owners: []string{"alice", "bob"}, Messages: 1}, configureSource...)
}

func newManagedMailFixtureOptions(t *testing.T, options managedMailOptions, configureSource ...func(*storage.DB, *config.AccountStore, map[string]string)) *managedMailFixture {
	t.Helper()
	if len(options.Owners) == 0 || options.Messages < 1 {
		t.Fatal("native fixture requires owners and mail")
	}
	managedTestEnvironment(t)
	t.Setenv("GOFER_SECRET_KEY", "")
	activity := &managedIMAPActivity{idle: make(map[string]int)}
	remote := make(map[string]*imapmemserver.User)
	initialMail := make(map[string][]managedSeedMail)
	for _, owner := range options.Owners {
		user := imapmemserver.NewUser(owner, "synthetic-only")
		for _, folder := range []string{"INBOX", "Sent", "Drafts", "Trash"} {
			if err := user.Create(folder, nil); err != nil {
				t.Fatal(err)
			}
		}
		remote[owner] = user
		count := options.Messages
		if configured, ok := options.MessageCounts[owner]; ok {
			count = configured
		}
		if count < 1 {
			t.Fatal("native fixture mailbox cannot be empty")
		}
		for index := 0; index < count; index++ {
			id := "same-remote-id"
			subject := owner + " native private message"
			date := time.Now().Add(-time.Duration(index) * time.Minute)
			body := owner + " native private body"
			if index > 0 {
				id = fmt.Sprintf("office-seed-%08d", index)
				subject += fmt.Sprintf(" #%08d", index)
				body += strings.Repeat(" project review invoice meeting", 32)
			}
			raw := fmt.Sprintf("From: Sender <sender@example.test>\r\nTo: %s@example.test\r\nDate: %s\r\nSubject: %s\r\nMessage-ID: <%s@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", owner, date.Format(time.RFC1123Z), subject, id, body)
			flags := []imap.Flag(nil)
			if index > 0 && index%3 == 0 {
				flags = append(flags, imap.FlagSeen)
			}
			if _, err := user.Append("INBOX", managedLiteral{strings.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{Flags: flags}); err != nil {
				t.Fatal(err)
			}
			initialMail[owner] = append(initialMail[owner], managedSeedMail{wire: raw, flags: flags})
		}
	}
	imapListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	imapServer := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}, imap.CapMove: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &managedIMAPSession{users: remote, activity: activity}, nil, nil
	}})
	imapDone := make(chan error, 1)
	go func() { imapDone <- imapServer.Serve(imapListener) }()
	t.Cleanup(func() { imapServer.Close(); <-imapDone })
	smtpBackend := &managedSMTPBackend{accepted: make(map[string][]string), users: make(map[string]bool)}
	for _, owner := range options.Owners {
		smtpBackend.users[owner] = true
	}
	smtpServer := smtp.NewServer(smtpBackend)
	smtpServer.AllowInsecureAuth = true
	smtpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	smtpDone := make(chan error, 1)
	go func() { smtpDone <- smtpServer.Serve(smtpListener) }()
	t.Cleanup(func() { smtpServer.Close(); <-smtpDone })
	_, imapPortText, _ := net.SplitHostPort(imapListener.Addr().String())
	imapPort, _ := strconv.Atoi(imapPortText)
	_, smtpPortText, _ := net.SplitHostPort(smtpListener.Addr().String())
	smtpPort, _ := strconv.Atoi(smtpPortText)
	root := t.TempDir()
	sourcePath := filepath.Join(root, "shared.db")
	destination := filepath.Join(root, "central.db")
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(root, "secret.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := storage.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Write().Exec(`INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('owner','owner','owner','management',1); UPDATE auth_system_state SET initialized=1,owner_user_id='owner',initialized_at=CURRENT_TIMESTAMP`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range options.Owners {
		if _, err := source.Write().Exec(`INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES(?,?,?,'webmail',0)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	for protocol, port := range map[string]int{"imap": imapPort, "smtp": smtpPort} {
		if err := source.AddPlaintextTransportException(t.Context(), protocol, "127.0.0.1", port, "owner"); err != nil {
			t.Fatal(err)
		}
		if err := source.AddPrivateTargetException(t.Context(), protocol, "127.0.0.1", port, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := config.NewAccountStore(source, key)
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]string)
	for _, owner := range options.Owners {
		account, err := accounts.CreateAccount(t.Context(), owner, &models.CreateAccountRequest{Provider: "imap", AuthMethod: "plain", Username: owner, Password: "synthetic-only", EmailAddress: owner + "@example.test", DisplayName: owner, IMAPHost: "127.0.0.1", IMAPPort: imapPort, IMAPTLSMode: "plaintext", SMTPHost: "127.0.0.1", SMTPPort: smtpPort, SMTPTLSMode: "plaintext"})
		if err != nil {
			t.Fatal(err)
		}
		ids[owner] = account.ID
		if err := source.SetUISettings(t.Context(), owner, map[string]string{"contacts_auto_create_observed": "false", "desktop_notifications": "false"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, configure := range configureSource {
		configure(source, accounts, ids)
	}

	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MigrateUserStorage(t.Context(), migrationOptions(sourcePath, destination, key)); err != nil {
		t.Fatal(err)
	}
	f := &managedMailFixture{path: destination, accounts: ids, remote: remote, smtp: smtpBackend, activity: activity, initialMail: initialMail, tokens: make(map[string]string)}
	t.Cleanup(func() {
		if f.app != nil {
			if err := f.app.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if options.SkipApplication {
		return f
	}
	app, err := newManagedApplication(t.Context(), destination, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.app = app
	for _, owner := range append(append([]string(nil), options.Owners...), "owner") {
		session, err := app.auth.CreateAuthenticatedSession(t.Context(), owner, "native protocols", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[owner] = session.Token
		if owner == "owner" {
			if ok, err := app.auth.RecordSessionStepUp(t.Context(), owner, session.ID, auth.AuthenticationMethodTOTP); err != nil || !ok {
				t.Fatal("fixture administrator step-up", ok, err)
			}
		}
	}
	return f
}

func (f *managedMailFixture) form(owner, method, path string, form url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(form.Encode()))
	request.Header.Set("Origin", "http://127.0.0.1:8090")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.tokens[owner]})
	response := httptest.NewRecorder()
	f.app.ServeHTTP(response, request)
	return response
}

func awaitManagedCondition(t *testing.T, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("application flow timed out")
		case <-ticker.C:
		}
	}
}

func TestManagedApplicationNativeIMAPReceiveBodySMTPAndSentCopy(t *testing.T) {
	f := newManagedMailFixture(t)
	messageIDs := make(map[string]int64)
	awaitManagedCondition(t, func() bool {
		for _, owner := range []string{"alice", "bob"} {
			var id int64
			err := f.app.storage.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
				return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND subject=?`, f.accounts[owner], owner+" native private message").Scan(&id)
			})
			if errors.Is(err, sql.ErrNoRows) {
				return false
			}
			if err != nil {
				t.Fatal(err)
			}
			messageIDs[owner] = id
		}
		return true
	})
	for _, owner := range []string{"alice", "bob"} {
		response := managedRequest(f.app, "GET", fmt.Sprintf("/email/%d/body", messageIDs[owner]), f.tokens[owner], "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), owner+" native private body") {
			t.Fatal("native body HTTP", owner, response.Code, response.Body.String())
		}
	}
	var sendIDs = make(map[string]string)
	for _, owner := range []string{"alice", "bob"} {
		response := f.form(owner, "POST", "/compose", url.Values{"account_id": {f.accounts[owner]}, "to": {"recipient@example.test"}, "subject": {owner + " native outgoing"}, "body": {owner + " outgoing body"}})
		var result map[string]any
		if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("compose HTTP", response.Code, response.Body.String())
		}
		id, ok := result["send_id"].(string)
		if !ok || id == "" {
			t.Fatal("missing send receipt", result)
		}
		sendIDs[owner] = id
	}
	awaitManagedCondition(t, func() bool {
		for _, owner := range []string{"alice", "bob"} {
			complete := false
			err := f.app.storage.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), sendIDs[owner])
				complete = err == nil && send.SentCopyStatus == storage.SentCopyComplete
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !complete {
				return false
			}
		}
		return true
	})
	f.smtp.mu.Lock()
	for _, owner := range []string{"alice", "bob"} {
		if len(f.smtp.accepted[owner]) != 1 || !strings.Contains(f.smtp.accepted[owner][0], owner+" native outgoing") {
			f.smtp.mu.Unlock()
			t.Fatal("SMTP acceptance crossed users or duplicated delivery", owner)
		}
		status, err := f.remote[owner].Status("Sent", &imap.StatusOptions{NumMessages: true})
		if err != nil || status.NumMessages == nil || *status.NumMessages != 1 {
			f.smtp.mu.Unlock()
			t.Fatal("native sent copy", owner, status, err)
		}
	}
	f.smtp.mu.Unlock()
	if response := f.form("bob", "GET", "/api/outgoing-sends/"+sendIDs["alice"], nil); response.Code != 404 {
		t.Fatal("foreign send receipt exposed", response.Code)
	}
	if err := storage.VerifyUserStorageRuntimeBoundary(t.Context(), f.path); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	app, err := newManagedApplication(t.Context(), f.path, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.app = app
	for _, owner := range []string{"alice", "bob"} {
		response := managedRequest(f.app, "GET", fmt.Sprintf("/email/%d/body", messageIDs[owner]), f.tokens[owner], "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), owner+" native private body") {
			t.Fatal("native body restart", owner, response.Code)
		}
		if err := f.app.storage.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			send, err := db.GetOutgoingSend(t.Context(), sendIDs[owner])
			if err != nil {
				return err
			}
			if send.SentCopyStatus != storage.SentCopyComplete {
				return errors.New("completed sent-copy receipt lost across restart")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	f.smtp.mu.Lock()
	defer f.smtp.mu.Unlock()
	for _, owner := range []string{"alice", "bob"} {
		if len(f.smtp.accepted[owner]) != 1 {
			t.Fatal("restart duplicated SMTP delivery", owner)
		}
	}
}

func (f *managedMailFixture) appendMail(t *testing.T, owner, subject string) {
	t.Helper()
	raw := fmt.Sprintf("From: sender@example.test\r\nTo: %s@example.test\r\nDate: %s\r\nSubject: %s\r\nMessage-ID: <%s-%d@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s live private body\r\n", owner, time.Now().Format(time.RFC1123Z), subject, owner, time.Now().UnixNano(), owner)
	if _, err := f.remote[owner].Append("INBOX", managedLiteral{strings.NewReader(raw), int64(len(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (f *managedMailFixture) messageCount(t *testing.T, owner, subject string) int {
	t.Helper()
	var count int
	if err := f.app.storage.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT count(*) FROM messages WHERE account_id=? AND subject=?`, f.accounts[owner], subject).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *managedMailFixture) setUserStatus(t *testing.T, owner, status string) {
	t.Helper()
	path := "/admin/users/" + owner + "/status"
	// Obtain the action-bound proof from the real authenticated administrator
	// context, then dispatch the mutation through the full application mux.
	probe := httptest.NewRequest("GET", "http://127.0.0.1:8090/admin/users", nil)
	probe.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.tokens["owner"]})
	proof := httptest.NewRecorder()
	f.app.auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, auth.CSRFToken(r.Context(), "POST", path))
	})).ServeHTTP(proof, probe)
	if proof.Code != http.StatusOK || proof.Body.Len() == 0 {
		t.Fatal("administrator CSRF proof", proof.Code)
	}
	response := f.form("owner", "POST", path, url.Values{"status": {status}, auth.CSRFFormFieldName: {proof.Body.String()}})
	if response.Code != http.StatusSeeOther {
		t.Fatal("administrator status change", status, response.Code)
	}
	user, err := f.app.auth.GetUserByID(t.Context(), owner)
	if err != nil || user == nil || string(user.Status) != status {
		t.Fatal("status change did not persist", user, err)
	}
}

func TestManagedApplicationNativeIDLEUserDisableAndCatchUp(t *testing.T) {
	f := newManagedMailFixture(t)
	awaitManagedCondition(t, func() bool {
		return f.activity.idleCount("alice") > 0 && f.activity.idleCount("bob") > 0 &&
			f.messageCount(t, "alice", "alice native private message") == 1 &&
			f.messageCount(t, "bob", "bob native private message") == 1
	})
	// Production polling is five minutes. Completion within this wait must come
	// from native mailbox updates, including after cache1 evicts the owner DB.
	f.appendMail(t, "alice", "alice IDLE arrival")
	awaitManagedCondition(t, func() bool { return f.messageCount(t, "alice", "alice IDLE arrival") == 1 })
	if f.messageCount(t, "bob", "alice IDLE arrival") != 0 {
		t.Fatal("IDLE crossed owner stores")
	}
	f.setUserStatus(t, "alice", "disabled")
	awaitManagedCondition(t, func() bool { return f.activity.idleCount("alice") == 0 && f.activity.idleCount("bob") > 0 })
	if response := managedRequest(f.app, "GET", "/api/settings/ui", f.tokens["alice"], ""); response.Code != http.StatusUnauthorized {
		t.Fatal("disabled user session remained usable", response.Code)
	}
	f.appendMail(t, "alice", "alice missed while disabled")
	f.appendMail(t, "bob", "bob continues during disable")
	awaitManagedCondition(t, func() bool { return f.messageCount(t, "bob", "bob continues during disable") == 1 })
	if f.activity.idleCount("alice") != 0 {
		t.Fatal("disabled user's connections restarted")
	}
	f.setUserStatus(t, "alice", "active")
	if response := managedRequest(f.app, "GET", "/api/settings/ui", f.tokens["alice"], ""); response.Code != http.StatusUnauthorized {
		t.Fatal("re-enable restored revoked session", response.Code)
	}
	session, err := f.app.auth.CreateAuthenticatedSession(t.Context(), "alice", "after re-enable", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	f.tokens["alice"] = session.Token
	awaitManagedCondition(t, func() bool {
		return f.activity.idleCount("alice") > 0 && f.messageCount(t, "alice", "alice missed while disabled") == 1
	})
	for _, check := range []struct{ owner, subject string }{{"alice", "alice IDLE arrival"}, {"alice", "alice missed while disabled"}, {"bob", "bob continues during disable"}} {
		var id int64
		if err := f.app.storage.routing.WithUser(t.Context(), check.owner, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND subject=?`, f.accounts[check.owner], check.subject).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		response := managedRequest(f.app, "GET", fmt.Sprintf("/email/%d/body", id), f.tokens[check.owner], "")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), check.owner+" live private body") {
			t.Fatal("live mailbox body", check.owner, response.Code)
		}
	}
	if err := storage.VerifyUserStorageRuntimeBoundary(t.Context(), f.path); err != nil {
		t.Fatal(err)
	}
}
