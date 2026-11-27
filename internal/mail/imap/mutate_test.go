package imap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

func TestStoreFlagsBatchIfUIDValidityMarksEveryMessageInChunks(t *testing.T) {
	cfg, recorder := startRecordedIMAPServer(t, goimap.CapSet{goimap.CapIMAP4rev1: {}})
	client, err := NewClient(context.Background(), cfg, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()
	// More than one STORE command's worth.
	const count = storeFlagsChunk + 20
	uids := make([]uint32, 0, count)
	for i := range count {
		raw := fmt.Sprintf("From: Ines Park <ines@example.com>\r\nSubject: Allotment note %d\r\n\r\nWater the beans.\r\n", i)
		appendCmd := client.client.Append("INBOX", int64(len(raw)), nil)
		if _, err := appendCmd.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := appendCmd.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := appendCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, uint32(data.UID))
	}
	selected, err := client.client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("select INBOX: %v", err)
	}
	validity := uint32(selected.UIDValidity)
	client.unselect()

	if err := client.StoreFlagsBatchIfUIDValidity(context.Background(), "INBOX", uids, goimap.StoreFlagsAdd, []goimap.Flag{goimap.FlagSeen}, validity+1); !errors.Is(err, ErrMutationUIDValidityChanged) {
		t.Fatalf("StoreFlagsBatchIfUIDValidity(stale validity) error = %v, want ErrMutationUIDValidityChanged", err)
	}
	before := strings.Count(strings.ToUpper(recorder.String()), " UID STORE ")
	if before != 0 {
		t.Fatalf("a stale UIDVALIDITY still sent %d STORE commands", before)
	}

	if err := client.StoreFlagsBatchIfUIDValidity(context.Background(), "INBOX", uids, goimap.StoreFlagsAdd, []goimap.Flag{goimap.FlagSeen}, validity); err != nil {
		t.Fatalf("StoreFlagsBatchIfUIDValidity() error = %v", err)
	}
	// The client sends what the proxy records once the next command goes out.
	if _, err := client.client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select INBOX: %v", err)
	}
	unseen, err := client.client.UIDSearch(&goimap.SearchCriteria{NotFlag: []goimap.Flag{goimap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatalf("search unseen: %v", err)
	}
	if left := unseen.AllUIDs(); len(left) != 0 {
		t.Fatalf("%d messages still unseen", len(left))
	}
	if stores := strings.Count(strings.ToUpper(recorder.String()), " UID STORE "); stores != 2 {
		t.Fatalf("UID STORE commands = %d, want 2 for %d messages", stores, count)
	}
}

func TestFlagColorStoreKeepsUnrelatedFlagsAndRejectsAnOldUIDEpoch(t *testing.T) {
	cfg, recorder := startRecordedIMAPServer(t, goimap.CapSet{goimap.CapIMAP4rev1: {}})
	ctx := t.Context()
	client, err := NewClient(ctx, cfg, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw := "From: ida@example.test\r\nSubject: Garden notes\r\n\r\nPlant the beans.\r\n"
	keep := []goimap.Flag{goimap.FlagSeen, "$hasmemo", "$VendorKeyword"}
	added := client.client.Append("INBOX", int64(len(raw)), &goimap.AppendOptions{
		Flags: append(keep, goimap.FlagFlagged, "$MailFlagBit0"),
	})
	if _, err := added.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := added.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := added.Wait()
	if err != nil {
		t.Fatal(err)
	}
	validity := data.UIDValidity
	uids := []uint32{uint32(data.UID)}
	if err := client.StoreFlagColorBatchIfUIDValidity(ctx, "INBOX", uids, true, 4, validity+1); !errors.Is(err, ErrMutationUIDValidityChanged) {
		t.Fatalf("STORE with stale UIDVALIDITY = %v", err)
	}
	if strings.Contains(strings.ToUpper(recorder.String()), " UID STORE ") {
		t.Fatal("stale UIDVALIDITY sent STORE")
	}
	assertFlags := func(on, off []goimap.Flag) {
		t.Helper()
		if _, err := client.client.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		defer client.unselect()
		result, err := client.client.UIDSearch(&goimap.SearchCriteria{Flag: on, NotFlag: off}, nil).Wait()
		if err != nil || len(result.AllUIDs()) != 1 || result.AllUIDs()[0] != data.UID {
			t.Fatalf("required %v and absent %v: result %v (%v)", on, off, result, err)
		}
	}
	// A repeat is safe after an uncertain previous response.
	for range 2 {
		if err := client.StoreFlagColorBatchIfUIDValidity(ctx, "INBOX", uids, true, 4, validity); err != nil {
			t.Fatal(err)
		}
	}
	assertFlags(append(keep, goimap.FlagFlagged, "$MailFlagBit2"), []goimap.Flag{"$MailFlagBit0", "$MailFlagBit1"})
	if err := client.StoreFlagColorBatchIfUIDValidity(ctx, "INBOX", uids, false, 0, validity); err != nil {
		t.Fatal(err)
	}
	assertFlags(keep, []goimap.Flag{goimap.FlagFlagged, "$MailFlagBit0", "$MailFlagBit1", "$MailFlagBit2"})
}
