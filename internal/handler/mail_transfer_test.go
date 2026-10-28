package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// addMailTransferTarget adds a second account on the fake server, with an
// Inbox that already has one of the first account's messages.
func (f *mailExportFixture) addMailTransferTarget(t *testing.T) (models.Account, string) {
	t.Helper()
	ctx := t.Context()
	user := imapmemserver.NewUser("sky@example.test", "other-secret")
	f.mem.AddUser(user)
	client, err := imapclient.DialInsecure(f.imapAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Login("sky@example.test", "other-secret").Wait(); err != nil {
		t.Fatal(err)
	}
	_ = client.Create("INBOX", nil).Wait()
	raw := mailExportTestMessage("Garden plans", "The tomatoes go by the fence.")
	command := client.Append("INBOX", int64(len(raw)), nil)
	_, _ = command.Write([]byte(raw))
	_ = command.Close()
	if _, err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	created, err := f.h.accountStore.CreateAccount(ctx, "owner", &models.CreateAccountRequest{
		Provider: "imap", AuthMethod: "plain", EmailAddress: "sky@example.test", DisplayName: "Sky Example",
		IMAPHost: "127.0.0.1", IMAPPort: f.port, IMAPTLSMode: "plaintext",
		SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPTLSMode: "plaintext",
		Username: "sky@example.test", Password: "other-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox := created.ID + "-inbox"
	if _, err := f.db.Write().ExecContext(ctx, `INSERT INTO folders (id, account_id, remote_id, name, role, selectable) VALUES (?, ?, 'INBOX', 'Inbox', 'inbox', 1)`, inbox, created.ID); err != nil {
		t.Fatal(err)
	}
	result, err := f.db.Write().ExecContext(ctx, `INSERT INTO messages (account_id, subject, from_email, internet_message_id) VALUES (?, 'Garden plans', 'juno@example.test', '<garden-plans@example.test>')`, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if _, err := f.db.Write().ExecContext(ctx, `INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read) VALUES (?, ?, 1, 1)`, id, inbox); err != nil {
		t.Fatal(err)
	}
	account, found, err := f.h.mailExportAccount(ctx, "owner", created.ID)
	if err != nil || !found {
		t.Fatalf("second account not listed: %v", err)
	}
	return account, inbox
}

// holdBackgroundSync keeps the sync a transfer starts when it ends from
// running, so it doesn't rebuild the fixture's folders, which have no sync
// state, while the test looks at what the transfer did.
func (f *mailExportFixture) holdBackgroundSync(t *testing.T) {
	t.Helper()
	if _, err := f.db.Write().ExecContext(t.Context(), `UPDATE accounts SET email_sync_enabled = 0`); err != nil {
		t.Fatal(err)
	}
}

func (f *mailExportFixture) waitTransferFinished(t *testing.T, id string) *mailTransferJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := f.h.mailTransfers.load(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != mailJobRunning && !f.h.mailTransfers.isRunning(id) {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("transfer did not finish")
	return nil
}

func (f *mailExportFixture) serverMessagesAs(t *testing.T, user, password, mailbox string) []serverMessage {
	t.Helper()
	client, err := imapclient.DialInsecure(f.imapAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Login(user, password).Wait(); err != nil {
		t.Fatal(err)
	}
	selected, err := client.Select(mailbox, nil).Wait()
	if err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	if selected.NumMessages == 0 {
		return nil
	}
	var all goimap.SeqSet
	all.AddRange(1, selected.NumMessages)
	section := &goimap.FetchItemBodySection{Peek: true}
	fetched, err := client.Fetch(all, &goimap.FetchOptions{Flags: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var messages []serverMessage
	for _, message := range fetched {
		messages = append(messages, serverMessage{flags: message.Flags, raw: string(message.FindBodySection(section))})
	}
	return messages
}

// Moving a folder copies each message into the other account, keeps its
// state, skips the ones already there, and then deletes from the first
// account only the messages whose copies are saved.
func TestMailTransferMovesMessagesBetweenAccounts(t *testing.T) {
	f := newMailExportFixture(t)
	f.h.syncer = mail.NewSyncOrchestrator(f.db, f.h.accountStore, f.h.blobStore, nil)
	target, targetInbox := f.addMailTransferTarget(t)
	f.holdBackgroundSync(t)

	req := ownerRequest(httptest.NewRequest(http.MethodGet, "/api/mail/transfers/new?from="+f.account.ID+"&to="+target.ID, nil))
	rec := httptest.NewRecorder()
	f.h.handleNewMailTransfer(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `name="target:`+f.folders["inbox"]+`"`) || !strings.Contains(body, `data-tui-selectbox-value="`+targetInbox+`" data-tui-selectbox-selected="true"`) {
		t.Fatalf("form doesn't suggest the other Inbox:\n%s", body)
	}

	form := url.Values{
		"from": {f.account.ID}, "to": {target.ID}, "mode": {"move"},
		"folder":                              {f.folders["inbox"], f.folders["archive-2024"]},
		"target:" + f.folders["inbox"]:        {targetInbox},
		"target:" + f.folders["archive-2024"]: {mailImportTargetNew},
		"new:" + f.folders["archive-2024"]:    {"Old/2024"},
	}
	start := ownerRequest(httptest.NewRequest(http.MethodPost, "/api/mail/transfers", strings.NewReader(form.Encode())))
	start.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	started := httptest.NewRecorder()
	f.h.handleStartMailTransfer(started, start)
	if !strings.Contains(started.Header().Get("HX-Trigger"), `"mail-jobs-changed":true`) || !strings.Contains(started.Body.String(), "data-mail-transfer-form") {
		t.Fatalf("start = %s", started.Body.String())
	}
	jobs, err := f.h.mailTransfers.jobs("owner")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d, %v", len(jobs), err)
	}
	job := f.waitTransferFinished(t, jobs[0].ID)
	if job.State != mailJobDone {
		t.Fatalf("state = %s (%s)", job.State, job.Error)
	}
	_, _, copied, duplicates, failed, deleted := job.totals()
	// Inbox: two copied, Garden plans already there, one gone from the
	// server. Archive/2024: one copied.
	if copied != 3 || duplicates != 1 || failed != 1 || deleted != 4 {
		t.Fatalf("copied %d, duplicates %d, failed %d, deleted %d", copied, duplicates, failed, deleted)
	}

	statusURL := "/api/mail/transfers/status?id=" + job.ID + "&id=missing"
	status := httptest.NewRecorder()
	f.h.handleMailTransferStatus(status, ownerRequest(httptest.NewRequest(http.MethodGet, statusURL, nil)))
	var statuses []mailTransferStatus
	if err := json.Unmarshal(status.Body.Bytes(), &statuses); err != nil || len(statuses) != 1 || statuses[0].State != mailJobDone || statuses[0].Copied != 3 || statuses[0].Deleted != 4 {
		t.Fatalf("status = %s (%v)", status.Body.String(), err)
	}
	status = httptest.NewRecorder()
	f.h.handleMailTransferStatus(status, attackerRequest(httptest.NewRequest(http.MethodGet, statusURL, nil)))
	if strings.TrimSpace(status.Body.String()) != "[]" {
		t.Fatalf("another person sees the status: %s", status.Body.String())
	}

	inbox := f.serverMessagesAs(t, "sky@example.test", "other-secret", "INBOX")
	if len(inbox) != 3 {
		t.Fatalf("other Inbox has %d messages", len(inbox))
	}
	for _, message := range inbox {
		if strings.Contains(message.raw, "Subject: Quoted line") && (hasFlag(message.flags, goimap.FlagSeen) || !hasFlag(message.flags, goimap.FlagFlagged)) {
			t.Fatalf("Quoted line flags = %v", message.flags)
		}
	}
	if old := f.serverMessagesAs(t, "sky@example.test", "other-secret", "Old/2024"); len(old) != 1 || !strings.Contains(old[0].raw, "Paid in full.") {
		t.Fatalf("Old/2024 = %+v", old)
	}

	// The moved messages are in the first account's Trash now; the one that
	// couldn't be copied stayed in its Inbox.
	where := map[int64]string{}
	rows, err := f.db.Read().QueryContext(t.Context(), `SELECT message_id, folder_id FROM message_folder_state WHERE is_deleted = 0`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var folder string
		if err := rows.Scan(&id, &folder); err != nil {
			t.Fatal(err)
		}
		where[id] = folder
	}
	_ = rows.Close()
	for _, key := range []string{"garden", "quoted", "saved", "receipt"} {
		if where[f.ids[key]] != f.folders["trash"] {
			t.Fatalf("%s is in %q, want Trash", key, where[f.ids[key]])
		}
	}
	if where[f.ids["missing"]] != f.folders["inbox"] {
		t.Fatalf("the message that wasn't copied is in %q", where[f.ids["missing"]])
	}

	if views, err := f.h.mailTransferJobViews("attacker"); err != nil || len(views) != 0 {
		t.Fatalf("another person sees %d transfers (%v)", len(views), err)
	}
	attack := attackerRequest(httptest.NewRequest(http.MethodPost, "/api/mail/transfers", strings.NewReader(form.Encode())))
	attack.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attacked := httptest.NewRecorder()
	f.h.handleStartMailTransfer(attacked, attack)
	if all, err := f.h.mailTransfers.jobs(""); err != nil || len(all) != 1 || attacked.Header().Get("HX-Trigger") != "" {
		t.Fatalf("another person started a transfer of someone else's accounts (%d transfers, %v)", len(all), err)
	}
}

func TestMailTransferCopyLeavesTheOriginals(t *testing.T) {
	f := newMailExportFixture(t)
	target, targetInbox := f.addMailTransferTarget(t)
	form := url.Values{"folder": {f.folders["archive-2024"]}, "target:" + f.folders["archive-2024"]: {targetInbox}}
	job, err := f.h.createMailTransfer(t.Context(), "owner", f.account, target, false, form)
	if err != nil {
		t.Fatal(err)
	}
	job = f.waitTransferFinished(t, job.ID)
	_, _, copied, _, _, deleted := job.totals()
	if job.State != mailJobDone || copied != 1 || deleted != 0 {
		t.Fatalf("state %s, copied %d, deleted %d", job.State, copied, deleted)
	}
	var folder string
	if err := f.db.Read().QueryRow(`SELECT folder_id FROM message_folder_state WHERE message_id = ?`, f.ids["receipt"]).Scan(&folder); err != nil || folder != f.folders["archive-2024"] {
		t.Fatalf("copied message moved to %q (%v)", folder, err)
	}
	if _, err := f.h.createMailTransfer(t.Context(), "owner", f.account, f.account, false, form); err != errMailTransferSameAccount {
		t.Fatalf("transfer into the same account = %v", err)
	}
}

// Emails chosen in the message list go into another account's folder after
// the dialog names both accounts; only those emails move.
func TestMessagesMoveToAnotherAccount(t *testing.T) {
	f := newMailExportFixture(t)
	f.h.syncer = mail.NewSyncOrchestrator(f.db, f.h.accountStore, f.h.blobStore, nil)
	_, targetInbox := f.addMailTransferTarget(t)
	f.holdBackgroundSync(t)

	query := url.Values{"folder_id": {targetInbox}, "accounts": {f.account.ID}, "count": {"2"}}
	req := ownerRequest(httptest.NewRequest(http.MethodGet, "/api/messages/to-account?"+query.Encode(), nil))
	rec := httptest.NewRecorder()
	f.h.handleMessagesToAccountDialog(rec, req)
	for _, want := range []string{"Move to another account?", "2 emails will go from robin@example.test to Inbox in sky@example.test, a different account.", "Copy instead", "data-mail-to-account-move"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dialog lacks %q:\n%s", want, rec.Body.String())
		}
	}

	post := func(request func(*http.Request) *http.Request, body string) *httptest.ResponseRecorder {
		req := request(httptest.NewRequest(http.MethodPost, "/api/messages/to-account", strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.handleMessagesToAccount(rec, req)
		return rec
	}
	body := `{"targets":[{"id":"` + strconv.FormatInt(f.ids["quoted"], 10) + `"},{"id":"` + strconv.FormatInt(f.ids["saved"], 10) + `"}],"folder_id":"` + targetInbox + `","source_folder_id":"` + f.folders["inbox"] + `","mode":"move"}`
	if rec := post(attackerRequest, body); rec.Code == http.StatusOK {
		t.Fatalf("another person moved the emails: %s", rec.Body.String())
	}
	rec = post(ownerRequest, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"messages":2`) {
		t.Fatalf("move = %d %s", rec.Code, rec.Body.String())
	}
	jobs, err := f.h.mailTransfers.jobs("owner")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("transfers = %d, %v", len(jobs), err)
	}
	job := f.waitTransferFinished(t, jobs[0].ID)
	_, total, copied, _, failed, deleted := job.totals()
	if job.State != mailJobDone || total != 2 || copied != 2 || failed != 0 || deleted != 2 {
		t.Fatalf("state %s: total %d, copied %d, failed %d, deleted %d (%s)", job.State, total, copied, failed, deleted, job.Error)
	}
	if inbox := f.serverMessagesAs(t, "sky@example.test", "other-secret", "INBOX"); len(inbox) != 3 {
		t.Fatalf("other Inbox has %d messages", len(inbox))
	}
	var garden, quoted string
	_ = f.db.Read().QueryRow(`SELECT folder_id FROM message_folder_state WHERE message_id = ? AND is_deleted = 0`, f.ids["garden"]).Scan(&garden)
	_ = f.db.Read().QueryRow(`SELECT folder_id FROM message_folder_state WHERE message_id = ? AND is_deleted = 0`, f.ids["quoted"]).Scan(&quoted)
	if garden != f.folders["inbox"] || quoted != f.folders["trash"] {
		t.Fatalf("garden in %q, quoted in %q", garden, quoted)
	}

	// Emails already in the destination account aren't moved this way.
	var targetMessage int64
	_ = f.db.Read().QueryRow(`SELECT message_id FROM message_folder_state WHERE folder_id = ?`, targetInbox).Scan(&targetMessage)
	mixed := `{"targets":[{"id":"` + strconv.FormatInt(targetMessage, 10) + `"}],"folder_id":"` + targetInbox + `","mode":"move"}`
	if rec := post(ownerRequest, mixed); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already in sky@example.test") {
		t.Fatalf("emails already there = %d %s", rec.Code, rec.Body.String())
	}
}
