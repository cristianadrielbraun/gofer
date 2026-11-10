package imap

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// imap.139.com and imap.163.com leave UIDNEXT out of the SELECT reply. New
// mail must still reach an incremental sync there, and nothing already seen
// may come back.
func TestSyncFolderIncrementalFetchesNewMailWithoutUIDNext(t *testing.T) {
	const seeded = 3
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprintf("omit UIDNEXT %v", omit), func(t *testing.T) {
			// "n:*" past the last UID returns the last message on RFC 3501
			// servers, 139.com and 163.com among them, but nothing on
			// imapmemserver, so the proxy answers it until mail arrives.
			var lastUID atomic.Uint32
			intercept := func(tag, command string) (string, bool) {
				last := lastUID.Load()
				if last == 0 || !strings.HasPrefix(strings.ToUpper(command), fmt.Sprintf("UID FETCH %d:* ", last+1)) {
					return "", false
				}
				return fmt.Sprintf("* %d FETCH (UID %d)\r\n%s OK FETCH completed\r\n", seeded, last, tag), true
			}
			cfg, uids, _ := startInboxProxy(t, seeded, intercept, func(line string) bool {
				return omit && strings.Contains(strings.ToUpper(line), "[UIDNEXT ")
			})
			highest := uids[len(uids)-1]
			lastUID.Store(highest)
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			defer client.Close()
			validity := inboxUIDValidity(t, client)

			sync := func() ([]uint32, uint32) {
				t.Helper()
				var fetched []uint32
				result, err := client.SyncFolderIncremental(context.Background(), "inbox", "INBOX", highest, validity, func(msgs []storage.SyncMessage) error {
					for _, msg := range msgs {
						fetched = append(fetched, msg.RemoteUID)
					}
					return nil
				})
				if err != nil {
					t.Fatalf("SyncFolderIncremental() error = %v", err)
				}
				return fetched, result.HighestUID
			}

			if fetched, got := sync(); len(fetched) != 0 || got != highest {
				t.Fatalf("no new mail: fetched %v, highest %d; want none, %d", fetched, got, highest)
			}

			lastUID.Store(0)
			var arrived []uint32
			for i := range 2 {
				raw := []byte(fmt.Sprintf("From: sender@example.com\r\nMessage-ID: <new-%d@example.com>\r\nSubject: New %d\r\n\r\nBody", i, i))
				appended, err := client.AppendMessage(context.Background(), "INBOX", raw, nil, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("append %d: %v", i, err)
				}
				arrived = append(arrived, appended.UID)
			}
			if fetched, got := sync(); !slices.Equal(fetched, arrived) || got != arrived[len(arrived)-1] {
				t.Fatalf("new mail: fetched %v, highest %d; want %v, %d", fetched, got, arrived, arrived[len(arrived)-1])
			}
		})
	}
}
