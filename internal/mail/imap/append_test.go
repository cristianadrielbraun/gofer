package imap

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAppendCommandErrorOnlyTreatsUnknownOutcomesAsAmbiguous(t *testing.T) {
	definitive := appendCommandError("Sent", "", &goimap.Error{Type: goimap.StatusResponseTypeNo, Text: "mailbox unavailable"})
	if IsAppendAmbiguous(definitive) {
		t.Fatalf("explicit IMAP NO response marked ambiguous: %v", definitive)
	}
	unknown := appendCommandError("Sent", "", errors.New("connection reset"))
	if !IsAppendAmbiguous(unknown) {
		t.Fatalf("connection loss not marked ambiguous: %v", unknown)
	}
}

func TestAppendMessageStoresSeenMessageAndReturnsUID(t *testing.T) {
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser("user@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	if err := user.Create("Sent", &goimap.CreateOptions{SpecialUse: []goimap.MailboxAttr{goimap.MailboxAttrSent}}); err != nil {
		t.Fatalf("create Sent: %v", err)
	}
	if err := user.Create("Drafts", &goimap.CreateOptions{SpecialUse: []goimap.MailboxAttr{goimap.MailboxAttrDrafts}}); err != nil {
		t.Fatalf("create Drafts: %v", err)
	}
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1: {},
			goimap.CapIMAP4rev2: {},
		},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	host, portString, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	client, err := NewClient(context.Background(), &models.AccountConfig{
		AccountID:          "acc",
		IMAPHost:           host,
		IMAPPort:           port,
		IMAPTLSMode:        "plaintext",
		IMAPAllowPlaintext: true,
		AuthMethod:         "plain",
		Username:           "user@example.com",
	}, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	raw := []byte("From: user@example.com\r\nTo: friend@example.com\r\nMessage-ID: <append@example.com>\r\nSubject: Appended\r\n\r\nBody")
	date := time.Date(2026, 7, 11, 10, 30, 0, 0, time.UTC)
	result, err := client.AppendMessage(context.Background(), "Sent", raw, []goimap.Flag{goimap.FlagSeen}, date)
	if err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	if result.UID == 0 || result.UIDValidity == 0 {
		t.Fatalf("AppendMessage() result = %#v, want UIDPLUS identity", result)
	}
	uid, err := client.FindUIDByMessageID(context.Background(), "Sent", "<append@example.com>")
	if err != nil {
		t.Fatalf("FindUIDByMessageID() error = %v", err)
	}
	if uid != result.UID {
		t.Fatalf("found UID = %d, want %d", uid, result.UID)
	}
	flags, _, _, err := client.FetchFlags(context.Background(), "Sent", []uint32{uid}, result.UIDValidity)
	if err != nil {
		t.Fatalf("FetchFlags() error = %v", err)
	}
	if len(flags) != 1 || !flags[0].IsRead {
		t.Fatalf("flags = %#v, want appended message marked seen", flags)
	}

	draftRaw := []byte("From: user@example.com\r\nTo: friend@example.com\r\nMessage-ID: <draft@example.com>\r\nX-Gofer-Draft-Revision: revision-1\r\nSubject: Draft\r\n\r\nBody")
	draftResult, err := client.AppendMessage(context.Background(), "Drafts", draftRaw, []goimap.Flag{goimap.FlagSeen, goimap.FlagDraft}, date)
	if err != nil {
		t.Fatalf("AppendMessage(Drafts) error = %v", err)
	}
	draftUID, draftUIDValidity, err := client.FindUIDByHeaderWithValidity(context.Background(), "Drafts", "X-Gofer-Draft-Revision", "revision-1")
	if err != nil || draftUID != draftResult.UID || draftUIDValidity != draftResult.UIDValidity {
		t.Fatalf("find draft revision UID=%d validity=%d result=%#v error=%v", draftUID, draftUIDValidity, draftResult, err)
	}
	var syncedDraft bool
	if _, err := client.SyncFolder(context.Background(), "drafts", "Drafts", FolderSyncOptions{ChunkSize: 10}, func(messages []storage.SyncMessage) error {
		for _, synced := range messages {
			if synced.RemoteUID == draftUID {
				syncedDraft = synced.IsDraft
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("SyncFolder(Drafts) error = %v", err)
	}
	if !syncedDraft {
		t.Fatal("appended draft did not sync with the draft flag")
	}
	validityChanged, err := client.DeleteMessagesIfUIDValidity(context.Background(), "Drafts", []uint32{draftUID}, draftUIDValidity+1)
	if err != nil || !validityChanged {
		t.Fatalf("delete with stale UID validity changed=%v error=%v", validityChanged, err)
	}
	if uid, _, err := client.FindUIDByHeaderWithValidity(context.Background(), "Drafts", "X-Gofer-Draft-Revision", "revision-1"); err != nil || uid != draftUID {
		t.Fatalf("draft deleted despite stale UID validity: UID=%d error=%v", uid, err)
	}
	validityChanged, err = client.DeleteMessagesIfUIDValidity(context.Background(), "Drafts", []uint32{draftUID}, draftUIDValidity)
	if err != nil || validityChanged {
		t.Fatalf("delete draft changed=%v error=%v", validityChanged, err)
	}
	if uid, _, err := client.FindUIDByHeaderWithValidity(context.Background(), "Drafts", "X-Gofer-Draft-Revision", "revision-1"); err != nil || uid != 0 {
		t.Fatalf("deleted draft still found: UID=%d error=%v", uid, err)
	}
}
