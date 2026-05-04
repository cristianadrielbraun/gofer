package imap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"gofer.email/internal/storage"
)

type SyncResult struct {
	TotalFetched int
	HighestUID   uint32
	UIDValidity  uint32
	NumMessages  uint32
}

func (c *Client) SyncFolder(ctx context.Context, folderID, remoteName string, chunkSize int, fn func([]storage.SyncMessage) error) (*SyncResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()

	if selectData.NumMessages == 0 {
		return &SyncResult{
			UIDValidity: uint32(selectData.UIDValidity),
			NumMessages: 0,
		}, nil
	}

	result := &SyncResult{
		UIDValidity: uint32(selectData.UIDValidity),
		NumMessages: selectData.NumMessages,
	}

	fetchOpts := &imap.FetchOptions{
		UID:          true,
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
	}

	var allMsgs []storage.SyncMessage
	uidNext := uint32(selectData.UIDNext)
	startUID := uint32(1)

	for startUID < uidNext {
		endUID := startUID + uint32(chunkSize) - 1
		if endUID >= uidNext {
			endUID = uidNext - 1
		}

		var uidSet imap.UIDSet
		uidSet.AddRange(imap.UID(startUID), imap.UID(endUID))

		cmd := c.client.Fetch(uidSet, fetchOpts)

		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}

			syncMsg := storage.SyncMessage{
				AccountID: c.accountID,
				FolderID:  folderID,
			}

			for {
				item := msg.Next()
				if item == nil {
					break
				}
				switch item := item.(type) {
				case imapclient.FetchItemDataUID:
					syncMsg.RemoteUID = uint32(item.UID)
				case imapclient.FetchItemDataEnvelope:
					if item.Envelope != nil {
						syncMsg.Subject = item.Envelope.Subject
						syncMsg.MessageID = item.Envelope.MessageID
						if len(item.Envelope.From) > 0 {
							syncMsg.FromName = item.Envelope.From[0].Name
							syncMsg.FromEmail = item.Envelope.From[0].Addr()
						}
						if !item.Envelope.Date.IsZero() {
							syncMsg.DateSent = item.Envelope.Date
						} else {
							syncMsg.DateSent = time.Now()
						}
						for _, addr := range item.Envelope.To {
							if email := addr.Addr(); email != "" {
								syncMsg.ToRecipients = append(syncMsg.ToRecipients, storage.Recipient{
									Name:  addr.Name,
									Email: email,
								})
							}
						}
						for _, addr := range item.Envelope.Cc {
							if email := addr.Addr(); email != "" {
								syncMsg.CCRecipients = append(syncMsg.CCRecipients, storage.Recipient{
									Name:  addr.Name,
									Email: email,
								})
							}
						}
					}
				case imapclient.FetchItemDataFlags:
					for _, flag := range item.Flags {
						switch flag {
						case imap.FlagSeen:
							syncMsg.IsRead = true
						case imap.FlagFlagged:
							syncMsg.IsStarred = true
						}
					}
				case imapclient.FetchItemDataInternalDate:
					if syncMsg.DateSent.IsZero() {
						syncMsg.DateSent = item.Time
					}
				}
			}

			if syncMsg.MessageID == "" {
				syncMsg.MessageID = fmt.Sprintf("<%d@sync.gofer>", syncMsg.RemoteUID)
			}

			if syncMsg.Subject == "" {
				syncMsg.Subject = "(no subject)"
			}

			syncMsg.Snippet = truncate(syncMsg.Subject, 200)

			allMsgs = append(allMsgs, syncMsg)
			if syncMsg.RemoteUID > result.HighestUID {
				result.HighestUID = syncMsg.RemoteUID
			}
			result.TotalFetched++
		}

		if err := cmd.Close(); err != nil {
			return result, fmt.Errorf("fetch %s %d-%d: %w", remoteName, startUID, endUID, err)
		}

		if len(allMsgs) >= chunkSize {
			if err := fn(allMsgs); err != nil {
				return result, fmt.Errorf("callback: %w", err)
			}
			allMsgs = allMsgs[:0]
		}

		startUID = endUID + 1
	}

	if len(allMsgs) > 0 {
		if err := fn(allMsgs); err != nil {
			return result, fmt.Errorf("callback: %w", err)
		}
	}

	return result, nil
}

func (c *Client) SyncFolderIncremental(ctx context.Context, folderID, remoteName string, highestUID uint32, fn func([]storage.SyncMessage) error) (*SyncResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()

	result := &SyncResult{
		UIDValidity: uint32(selectData.UIDValidity),
		NumMessages: selectData.NumMessages,
	}

	uidNext := uint32(selectData.UIDNext)
	if highestUID+1 >= uidNext {
		return result, nil
	}

	var uidSet imap.UIDSet
	uidSet.AddRange(imap.UID(highestUID+1), imap.UID(uidNext-1))

	fetchOpts := &imap.FetchOptions{
		UID:          true,
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
	}

	cmd := c.client.Fetch(uidSet, fetchOpts)

	var msgs []storage.SyncMessage

	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}

		syncMsg := storage.SyncMessage{
			AccountID: c.accountID,
			FolderID:  folderID,
		}

		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				syncMsg.RemoteUID = uint32(item.UID)
			case imapclient.FetchItemDataEnvelope:
				if item.Envelope != nil {
					syncMsg.Subject = item.Envelope.Subject
					syncMsg.MessageID = item.Envelope.MessageID
					if len(item.Envelope.From) > 0 {
						syncMsg.FromName = item.Envelope.From[0].Name
						syncMsg.FromEmail = item.Envelope.From[0].Addr()
					}
					if !item.Envelope.Date.IsZero() {
						syncMsg.DateSent = item.Envelope.Date
					} else {
						syncMsg.DateSent = time.Now()
					}
					for _, addr := range item.Envelope.To {
						if email := addr.Addr(); email != "" {
							syncMsg.ToRecipients = append(syncMsg.ToRecipients, storage.Recipient{
								Name:  addr.Name,
								Email: email,
							})
						}
					}
					for _, addr := range item.Envelope.Cc {
						if email := addr.Addr(); email != "" {
							syncMsg.CCRecipients = append(syncMsg.CCRecipients, storage.Recipient{
								Name:  addr.Name,
								Email: email,
							})
						}
					}
				}
			case imapclient.FetchItemDataFlags:
				for _, flag := range item.Flags {
					switch flag {
					case imap.FlagSeen:
						syncMsg.IsRead = true
					case imap.FlagFlagged:
						syncMsg.IsStarred = true
					}
				}
			case imapclient.FetchItemDataInternalDate:
				if syncMsg.DateSent.IsZero() {
					syncMsg.DateSent = item.Time
				}
			}
		}

		if syncMsg.MessageID == "" {
			syncMsg.MessageID = fmt.Sprintf("<%d@sync.gofer>", syncMsg.RemoteUID)
		}

		if syncMsg.Subject == "" {
			syncMsg.Subject = "(no subject)"
		}

		syncMsg.Snippet = truncate(syncMsg.Subject, 200)

		msgs = append(msgs, syncMsg)
		if syncMsg.RemoteUID > result.HighestUID {
			result.HighestUID = syncMsg.RemoteUID
		}
		result.TotalFetched++
	}

	if err := cmd.Close(); err != nil {
		return result, fmt.Errorf("fetch incremental %s: %w", remoteName, err)
	}

	if len(msgs) > 0 {
		if err := fn(msgs); err != nil {
			return result, fmt.Errorf("callback: %w", err)
		}
	}

	return result, nil
}

func (c *Client) FetchAllUIDs(ctx context.Context, remoteName string) ([]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	_, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()

	searchCmd := c.client.UIDSearch(&imap.SearchCriteria{}, nil)
	searchData, err := searchCmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("uid search %s: %w", remoteName, err)
	}

	imapUIDs := searchData.AllUIDs()
	uids := make([]uint32, len(imapUIDs))
	for i, uid := range imapUIDs {
		uids[i] = uint32(uid)
	}
	return uids, nil
}

type FlagUpdate struct {
	UID       uint32
	IsRead    bool
	IsStarred bool
}

func (c *Client) FetchFlags(ctx context.Context, remoteName string, uids []uint32) ([]FlagUpdate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	if len(uids) == 0 {
		return nil, nil
	}

	_, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()

	var allUpdates []FlagUpdate
	chunkSize := 500

	for i := 0; i < len(uids); i += chunkSize {
		end := i + chunkSize
		if end > len(uids) {
			end = len(uids)
		}
		chunk := uids[i:end]

		var uidSet imap.UIDSet
		for _, uid := range chunk {
			uidSet.AddNum(imap.UID(uid))
		}

		fetchOpts := &imap.FetchOptions{UID: true, Flags: true}
		cmd := c.client.Fetch(uidSet, fetchOpts)

		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}

			var update FlagUpdate
			for {
				item := msg.Next()
				if item == nil {
					break
				}
				switch item := item.(type) {
				case imapclient.FetchItemDataUID:
					update.UID = uint32(item.UID)
				case imapclient.FetchItemDataFlags:
					for _, flag := range item.Flags {
						switch flag {
						case imap.FlagSeen:
							update.IsRead = true
						case imap.FlagFlagged:
							update.IsStarred = true
						}
					}
				}
			}

			if update.UID > 0 {
				allUpdates = append(allUpdates, update)
			}
		}

		if err := cmd.Close(); err != nil {
			return allUpdates, fmt.Errorf("fetch flags %s: %w", remoteName, err)
		}
	}

	return allUpdates, nil
}

func (c *Client) CheckUIDValidity(ctx context.Context, remoteName string) (uint32, uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, 0, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return 0, 0, fmt.Errorf("select %s: %w", remoteName, err)
	}
	c.client.Unselect()

	return uint32(selectData.UIDValidity), uint32(selectData.UIDNext), nil
}

func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
