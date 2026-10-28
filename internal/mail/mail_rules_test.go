package mail

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// startRulesIMAPServer serves an in-memory mailbox with an Inbox, a Trash and
// a Junk folder, and returns a client to put mail in it.
func startRulesIMAPServer(t *testing.T) (host string, port int, seed *imap.Client) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user@example.com", "secret")
	for _, folder := range []string{"INBOX", "Trash", "Junk"} {
		if err := user.Create(folder, nil); err != nil {
			t.Fatalf("create %s: %v", folder, err)
		}
	}
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	host, portString, _ := net.SplitHostPort(listener.Addr().String())
	port, _ = strconv.Atoi(portString)
	seed, err = imap.NewClient(context.Background(), &models.AccountConfig{
		AccountID: "seed", IMAPHost: host, IMAPPort: port,
		IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true, IMAPAllowPrivate: true,
		AuthMethod: "plain", Username: "user@example.com",
	}, "secret")
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	return host, port, seed
}

func appendRulesTestMail(t *testing.T, seed *imap.Client, id, from, subject, extraHeaders string) {
	t.Helper()
	raw := fmt.Sprintf("From: %s\r\nTo: user@example.com\r\nMessage-ID: <%s@example.test>\r\nSubject: %s\r\n%s\r\nHello", from, id, subject, extraHeaders)
	if _, err := seed.AppendMessage(context.Background(), "INBOX", []byte(raw), nil, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// Mail rules run on the mail an IMAP sync finds arriving, not on what the
// first sync imports; what they move away isn't announced, and the queued
// changes are sent at once.
func TestSyncAccountRunsMailRulesOnIMAPArrivals(t *testing.T) {
	ctx := context.Background()
	host, port, seed := startRulesIMAPServer(t)
	db := newLabelSyncTestDB(t)
	if err := db.AddPlaintextTransportException(ctx, "imap", host, port, "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(ctx, "imap", host, port, "default"); err != nil {
		t.Fatal(err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		EmailAddress: "user@example.com", IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext",
		SMTPHost: "smtp.example.com", Username: "user@example.com", Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []models.MailRule{
		{Name: "Flyers", Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldListID, Operator: models.MailRuleOpContains, Value: "offers.corner-shop.example"}},
			Actions: []models.MailRuleAction{{Kind: models.MailRuleActionTrash}}},
		{Name: "Prizes", Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldSubject, Operator: models.MailRuleOpContains, Value: "you won"}},
			Actions: []models.MailRuleAction{{Kind: models.MailRuleActionSpam}}},
	} {
		rule.Enabled = true
		if _, err := db.SaveMailRuleGuarded(ctx, "default", rule, nil); err != nil {
			t.Fatal(err)
		}
	}
	listID := "List-Id: Corner Shop offers\r\n <offers.corner-shop.example>\r\n"
	appendRulesTestMail(t, seed, "old-flyer", "Corner Shop <flyers@corner-shop.example>", "Last week's offers", listID)

	orchestrator := NewSyncOrchestrator(db, accountStore, nil, nil)
	woken := 0
	orchestrator.SetMutationWake(func() { woken++ })
	events := orchestrator.Events().Subscribe()
	defer orchestrator.Events().Unsubscribe(events)
	if err := orchestrator.syncAccount(ctx, account.ID, true); err != nil {
		t.Fatalf("first syncAccount() error = %v", err)
	}
	for len(events) > 0 {
		<-events
	}

	appendRulesTestMail(t, seed, "new-flyer", "Corner Shop <flyers@corner-shop.example>", "This week's offers", listID)
	appendRulesTestMail(t, seed, "prize", "Lucky Draw <draw@prizes.example>", "You won a boat", "")
	appendRulesTestMail(t, seed, "order", "Corner Shop <orders@corner-shop.example>", "Your order is ready", "")
	if err := orchestrator.syncAccount(ctx, account.ID, true); err != nil {
		t.Fatalf("second syncAccount() error = %v", err)
	}
	var announced []Event
	for len(events) > 0 {
		if event := <-events; event.Type == EventNewMail {
			announced = append(announced, event)
		}
	}
	if len(announced) != 1 || announced[0].Payload["count"] != 1 || announced[0].Payload["subject"] != "Your order is ready" {
		t.Fatalf("new mail events = %+v, want the order alone", announced)
	}
	if woken == 0 {
		t.Fatal("the queued-changes worker wasn't woken")
	}

	roles := map[string]string{}
	rows, err := db.Read().QueryContext(ctx, `SELECT m.subject, f.role FROM messages m JOIN message_folder_state s ON s.message_id = m.id JOIN folders f ON f.id = s.folder_id WHERE s.is_deleted = 0`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var subject, role string
		if err := rows.Scan(&subject, &role); err != nil {
			t.Fatal(err)
		}
		roles[subject] = role
	}
	rows.Close()
	if roles["Last week's offers"] != "inbox" || roles["This week's offers"] != "trash" || roles["You won a boat"] != "junk" || roles["Your order is ready"] != "inbox" {
		t.Fatalf("folders = %v", roles)
	}
	var storedListID string
	if err := db.Read().QueryRowContext(ctx, `SELECT list_id FROM messages WHERE subject = 'Last week''s offers'`).Scan(&storedListID); err != nil || storedListID != "Corner Shop offers <offers.corner-shop.example>" {
		t.Fatalf("list_id = %q, %v", storedListID, err)
	}

	// The spam rule's training flag goes to the server as a flag, not a
	// label, and waits for the move to Junk, which the queued-changes worker
	// sends.
	pendingLabels := func() (count, attempts int) {
		t.Helper()
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(attempts), 0) FROM label_mutation_queue WHERE account_id = ?`, account.ID).Scan(&count, &attempts); err != nil {
			t.Fatal(err)
		}
		return count, attempts
	}
	orchestrator.replayIMAPLabelMutationQueue(ctx, account.ID)
	if count, attempts := pendingLabels(); count != 2 || attempts != 0 {
		t.Fatalf("label changes before the move = %d with %d attempts, want 2 waiting", count, attempts)
	}
	moveIMAPTestMail(t, host, port, "INBOX", "Junk", "You won a boat")
	if _, err := db.Write().ExecContext(ctx, `UPDATE message_mutations SET status = 'applied' WHERE account_id = ? AND kind = 'move'`, account.ID); err != nil {
		t.Fatal(err)
	}
	orchestrator.replayIMAPLabelMutationQueue(ctx, account.ID)
	if count, _ := pendingLabels(); count != 0 {
		t.Fatalf("label changes still pending = %d", count)
	}
	flags := imapTestFlags(t, host, port, "Junk", "You won a boat")
	if !slices.Contains(flags, goimap.Flag("$Junk")) || slices.Contains(flags, goimap.Flag("$NotJunk")) {
		t.Fatalf("flags on the prize mail = %v, want $Junk", flags)
	}
	var labels int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE lower(l.name) LIKE '%junk%'`).Scan(&labels); err != nil || labels != 0 {
		t.Fatalf("junk labels = %d, %v", labels, err)
	}
}

// openIMAPTestMail selects folder and finds the message with subject in it.
func openIMAPTestMail(t *testing.T, host string, port int, folder, subject string) (*imapclient.Client, goimap.UIDSet) {
	t.Helper()
	client, err := imapclient.DialInsecure(net.JoinHostPort(host, strconv.Itoa(port)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Login("user@example.com", "secret").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	found, err := client.UIDSearch(&goimap.SearchCriteria{Header: []goimap.SearchCriteriaHeaderField{{Key: "Subject", Value: subject}}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	uids := found.AllUIDs()
	if len(uids) != 1 {
		t.Fatalf("messages with subject %q in %s = %d", subject, folder, len(uids))
	}
	return client, goimap.UIDSetNum(uids...)
}

func moveIMAPTestMail(t *testing.T, host string, port int, from, to, subject string) {
	t.Helper()
	client, uids := openIMAPTestMail(t, host, port, from, subject)
	if _, err := client.Move(uids, to).Wait(); err != nil {
		t.Fatal(err)
	}
}

// imapTestFlags reads the flags of the message with subject in folder.
func imapTestFlags(t *testing.T, host string, port int, folder, subject string) []goimap.Flag {
	t.Helper()
	client, uids := openIMAPTestMail(t, host, port, folder, subject)
	messages, err := client.Fetch(uids, &goimap.FetchOptions{Flags: true}).Collect()
	if err != nil || len(messages) != 1 {
		t.Fatalf("fetch flags: %v", err)
	}
	return messages[0].Flags
}
