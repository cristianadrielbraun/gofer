package imap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type SyncResult struct {
	TotalFetched       int
	HighestUID         uint32
	UIDValidity        uint32
	UIDValidityChanged bool
	NumMessages        uint32
}

// FolderSyncOptions controls a full mailbox sync. OnTotal is called after
// the server's UID SEARCH has produced the real mailbox size and before the
// first fetch starts. It is intentionally separate from the message-batch
// callback so callers do not have to infer totals from an empty batch.
type FolderSyncOptions struct {
	ChunkSize int
	OnTotal   func(total int)
}

type messageDateCandidates struct {
	envelopeDate time.Time
	internalDate time.Time
}

func (c messageDateCandidates) resolve(fallback time.Time) (time.Time, bool) {
	if !c.envelopeDate.IsZero() {
		return c.envelopeDate.UTC(), false
	}
	if !c.internalDate.IsZero() {
		return c.internalDate.UTC(), false
	}
	if fallback.IsZero() {
		fallback = time.Now().UTC()
	}
	return fallback.UTC(), true
}

func finalizeSyncMessage(syncMsg *storage.SyncMessage, folderID string, dates messageDateCandidates, fallback time.Time) {
	syncMsg.DateSent, syncMsg.DateSentFallback = dates.resolve(fallback)
	if syncMsg.MessageID == "" {
		syncMsg.MessageID = fmt.Sprintf("<%s-%d@sync.gofer>", folderID, syncMsg.RemoteUID)
	}
	if syncMsg.Subject == "" {
		syncMsg.Subject = "(no subject)"
	}
	syncMsg.Snippet = truncate(syncMsg.Subject, 200)
}

func (c *Client) SyncFolder(ctx context.Context, folderID, remoteName string, options FolderSyncOptions, fn func([]storage.SyncMessage) error) (*SyncResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	chunkSize := options.ChunkSize
	if chunkSize <= 0 {
		return nil, fmt.Errorf("chunk size must be positive")
	}
	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()

	if selectData.NumMessages == 0 {
		if options.OnTotal != nil {
			options.OnTotal(0)
		}
		return &SyncResult{
			UIDValidity: uint32(selectData.UIDValidity),
			NumMessages: 0,
		}, nil
	}

	result := &SyncResult{
		UIDValidity: uint32(selectData.UIDValidity),
	}
	searchData, err := c.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return result, fmt.Errorf("uid search %s: %w", remoteName, err)
	}
	uids := searchData.AllUIDs()
	result.NumMessages = uint32(len(uids))
	if options.OnTotal != nil {
		options.OnTotal(len(uids))
	}
	if len(uids) == 0 {
		return result, nil
	}

	fetchOpts := &imap.FetchOptions{
		UID:          true,
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"References", "In-Reply-To"},
			Peek:         true,
		}},
	}

	var allMsgs []storage.SyncMessage
	for start := 0; start < len(uids); start += chunkSize {
		end := min(start+chunkSize, len(uids))
		var uidSet imap.UIDSet
		uidSet.AddNum(uids[start:end]...)

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
			var dates messageDateCandidates

			for {
				item := msg.Next()
				if item == nil {
					break
				}
				switch item := item.(type) {
				case imapclient.FetchItemDataUID:
					syncMsg.RemoteUID = uint32(item.UID)
				case imapclient.FetchItemDataBodySection:
					body, err := io.ReadAll(item.Literal)
					if err == nil {
						inReplyTo, references := message.ParseThreadHeaders(body)
						if inReplyTo != "" {
							syncMsg.InReplyTo = inReplyTo
						}
						syncMsg.References = references
					}
				case imapclient.FetchItemDataEnvelope:
					if item.Envelope != nil {
						syncMsg.Subject = message.DecodeHeader(item.Envelope.Subject)
						syncMsg.MessageID = item.Envelope.MessageID
						if len(item.Envelope.InReplyTo) > 0 && syncMsg.InReplyTo == "" {
							syncMsg.InReplyTo = item.Envelope.InReplyTo[0]
						}
						if len(item.Envelope.From) > 0 {
							syncMsg.FromName = message.DecodeHeader(item.Envelope.From[0].Name)
							syncMsg.FromEmail = item.Envelope.From[0].Addr()
						}
						if !item.Envelope.Date.IsZero() {
							dates.envelopeDate = item.Envelope.Date
						}
						for _, addr := range item.Envelope.To {
							if email := addr.Addr(); email != "" {
								syncMsg.ToRecipients = append(syncMsg.ToRecipients, storage.Recipient{
									Name:  message.DecodeHeader(addr.Name),
									Email: email,
								})
							}
						}
						for _, addr := range item.Envelope.Cc {
							if email := addr.Addr(); email != "" {
								syncMsg.CCRecipients = append(syncMsg.CCRecipients, storage.Recipient{
									Name:  message.DecodeHeader(addr.Name),
									Email: email,
								})
							}
						}
					}
				case imapclient.FetchItemDataFlags:
					syncMsg.Labels = labelsFromFlags(item.Flags)
					syncMsg.LabelsKnown = true
					syncMsg.LabelProvider = storage.LabelProviderIMAPKeyword
					for _, flag := range item.Flags {
						switch flag {
						case imap.FlagSeen:
							syncMsg.IsRead = true
						case imap.FlagFlagged:
							syncMsg.IsStarred = true
						case imap.FlagDraft:
							syncMsg.IsDraft = true
						}
					}
				case imapclient.FetchItemDataInternalDate:
					if !item.Time.IsZero() {
						dates.internalDate = item.Time
					}
				}
			}

			finalizeSyncMessage(&syncMsg, folderID, dates, time.Now().UTC())

			allMsgs = append(allMsgs, syncMsg)
			if syncMsg.RemoteUID > result.HighestUID {
				result.HighestUID = syncMsg.RemoteUID
			}
			result.TotalFetched++
		}

		if err := cmd.Close(); err != nil {
			return result, fmt.Errorf("fetch %s UID batch %d: %w", remoteName, start/chunkSize+1, err)
		}

		if len(allMsgs) >= chunkSize {
			if err := fn(allMsgs); err != nil {
				return result, fmt.Errorf("callback: %w", err)
			}
			allMsgs = allMsgs[:0]
		}
	}

	if len(allMsgs) > 0 {
		if err := fn(allMsgs); err != nil {
			return result, fmt.Errorf("callback: %w", err)
		}
	}

	return result, nil
}

func (c *Client) SyncFolderIncremental(ctx context.Context, folderID, remoteName string, highestUID, expectedUIDValidity uint32, fn func([]storage.SyncMessage) error) (*SyncResult, error) {
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
	if uidStateNeedsReset(expectedUIDValidity, result.UIDValidity, highestUID) {
		result.UIDValidityChanged = true
		return result, nil
	}
	result.HighestUID = highestUID

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
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"References", "In-Reply-To"},
			Peek:         true,
		}},
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
		var dates messageDateCandidates

		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				syncMsg.RemoteUID = uint32(item.UID)
			case imapclient.FetchItemDataBodySection:
				body, err := io.ReadAll(item.Literal)
				if err == nil {
					inReplyTo, references := message.ParseThreadHeaders(body)
					if inReplyTo != "" {
						syncMsg.InReplyTo = inReplyTo
					}
					syncMsg.References = references
				}
			case imapclient.FetchItemDataEnvelope:
				if item.Envelope != nil {
					syncMsg.Subject = message.DecodeHeader(item.Envelope.Subject)
					syncMsg.MessageID = item.Envelope.MessageID
					if len(item.Envelope.InReplyTo) > 0 && syncMsg.InReplyTo == "" {
						syncMsg.InReplyTo = item.Envelope.InReplyTo[0]
					}
					if len(item.Envelope.From) > 0 {
						syncMsg.FromName = message.DecodeHeader(item.Envelope.From[0].Name)
						syncMsg.FromEmail = item.Envelope.From[0].Addr()
					}
					if !item.Envelope.Date.IsZero() {
						dates.envelopeDate = item.Envelope.Date
					}
					for _, addr := range item.Envelope.To {
						if email := addr.Addr(); email != "" {
							syncMsg.ToRecipients = append(syncMsg.ToRecipients, storage.Recipient{
								Name:  message.DecodeHeader(addr.Name),
								Email: email,
							})
						}
					}
					for _, addr := range item.Envelope.Cc {
						if email := addr.Addr(); email != "" {
							syncMsg.CCRecipients = append(syncMsg.CCRecipients, storage.Recipient{
								Name:  message.DecodeHeader(addr.Name),
								Email: email,
							})
						}
					}
				}
			case imapclient.FetchItemDataFlags:
				syncMsg.Labels = labelsFromFlags(item.Flags)
				syncMsg.LabelsKnown = true
				syncMsg.LabelProvider = storage.LabelProviderIMAPKeyword
				for _, flag := range item.Flags {
					switch flag {
					case imap.FlagSeen:
						syncMsg.IsRead = true
					case imap.FlagFlagged:
						syncMsg.IsStarred = true
					case imap.FlagDraft:
						syncMsg.IsDraft = true
					}
				}
			case imapclient.FetchItemDataInternalDate:
				if !item.Time.IsZero() {
					dates.internalDate = item.Time
				}
			}
		}

		finalizeSyncMessage(&syncMsg, folderID, dates, time.Now().UTC())

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

func uidValidityChanged(expected, current uint32) bool {
	return expected > 0 && current > 0 && expected != current
}

func uidStateNeedsReset(expected, current, highestUID uint32) bool {
	return uidValidityChanged(expected, current) || expected == 0 && current > 0 && highestUID > 0
}

func (c *Client) FetchAllUIDs(ctx context.Context, remoteName string, expectedUIDValidity uint32) ([]uint32, uint32, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, 0, false, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return nil, 0, false, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()
	currentUIDValidity := uint32(selectData.UIDValidity)
	if uidValidityChanged(expectedUIDValidity, currentUIDValidity) {
		return nil, currentUIDValidity, true, nil
	}

	searchCmd := c.client.UIDSearch(&imap.SearchCriteria{}, nil)
	searchData, err := searchCmd.Wait()
	if err != nil {
		return nil, currentUIDValidity, false, fmt.Errorf("uid search %s: %w", remoteName, err)
	}

	imapUIDs := searchData.AllUIDs()
	uids := make([]uint32, len(imapUIDs))
	for i, uid := range imapUIDs {
		uids[i] = uint32(uid)
	}
	return uids, currentUIDValidity, false, nil
}

func (c *Client) FindUIDByMessageID(ctx context.Context, remoteName, messageID string) (uint32, error) {
	uid, _, err := c.FindUIDByMessageIDWithValidity(ctx, remoteName, messageID)
	return uid, err
}

func (c *Client) FindUIDByMessageIDWithValidity(ctx context.Context, remoteName, messageID string) (uint32, uint32, error) {
	return c.FindUIDByHeaderWithValidity(ctx, remoteName, "Message-ID", messageID)
}

func (c *Client) FindUIDByHeaderWithValidity(ctx context.Context, remoteName, headerName, headerValue string) (uint32, uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	headerName = strings.TrimSpace(headerName)
	headerValue = strings.TrimSpace(headerValue)
	if headerName == "" || headerValue == "" {
		return 0, 0, nil
	}
	if c.closed {
		return 0, 0, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(remoteName, nil).Wait()
	if err != nil {
		return 0, 0, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()
	uidValidity := uint32(selectData.UIDValidity)

	searchCmd := c.client.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   headerName,
			Value: headerValue,
		}},
	}, nil)
	searchData, err := searchCmd.Wait()
	if err != nil {
		return 0, uidValidity, fmt.Errorf("uid search %s %s %s: %w", remoteName, headerName, headerValue, err)
	}

	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return 0, uidValidity, nil
	}
	return uint32(uids[0]), uidValidity, nil
}

type FlagUpdate struct {
	UID       uint32
	IsRead    bool
	IsStarred bool
	Labels    []storage.LabelInput
}

type FlagSyncResult struct {
	Updates            []FlagUpdate
	UIDValidity        uint32
	UIDValidityChanged bool
	HighestModSeq      uint64
	CheckpointValid    bool
	UsedCondStore      bool
}

func (c *Client) FetchFlags(ctx context.Context, remoteName string, uids []uint32, expectedUIDValidity uint32) ([]FlagUpdate, uint32, bool, error) {
	result, err := c.FetchFlagChanges(ctx, remoteName, uids, expectedUIDValidity, 0)
	return result.Updates, result.UIDValidity, result.UIDValidityChanged, err
}

func (c *Client) FetchFlagChanges(ctx context.Context, remoteName string, uids []uint32, expectedUIDValidity uint32, highestModSeq uint64) (FlagSyncResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return FlagSyncResult{}, err
	}
	if c.closed {
		return FlagSyncResult{}, fmt.Errorf("client is closed")
	}

	if len(uids) == 0 {
		return FlagSyncResult{UIDValidity: expectedUIDValidity}, nil
	}

	useCondStore := false
	if caps := c.client.Caps(); caps != nil {
		useCondStore = caps.Has(imap.CapCondStore)
	}
	var selectOptions *imap.SelectOptions
	if useCondStore {
		selectOptions = &imap.SelectOptions{CondStore: true}
	}
	selectData, err := c.client.Select(remoteName, selectOptions).Wait()
	if err != nil && useCondStore && isIMAPCommandRejection(err) {
		useCondStore = false
		selectData, err = c.client.Select(remoteName, nil).Wait()
	}
	if err != nil {
		return FlagSyncResult{}, fmt.Errorf("select %s: %w", remoteName, err)
	}
	defer c.client.Unselect()
	currentUIDValidity := uint32(selectData.UIDValidity)
	if uidValidityChanged(expectedUIDValidity, currentUIDValidity) {
		return FlagSyncResult{UIDValidity: currentUIDValidity, UIDValidityChanged: true}, nil
	}

	checkpointValid := useCondStore && currentUIDValidity > 0 && selectData.HighestModSeq > 0 && selectData.HighestModSeq <= math.MaxInt64
	changedSince := uint64(0)
	if checkpointValid && highestModSeq > 0 && highestModSeq <= selectData.HighestModSeq {
		changedSince = highestModSeq
	}
	updates, err := c.fetchFlagUpdatesLocked(remoteName, uids, changedSince, checkpointValid)
	if err != nil && checkpointValid && isIMAPCommandRejection(err) {
		updates, err = c.fetchFlagUpdatesLocked(remoteName, uids, 0, false)
		checkpointValid = false
		useCondStore = false
	}
	if err != nil {
		return FlagSyncResult{UIDValidity: currentUIDValidity}, err
	}
	result := FlagSyncResult{
		Updates:         updates,
		UIDValidity:     currentUIDValidity,
		CheckpointValid: checkpointValid,
		UsedCondStore:   useCondStore && changedSince > 0,
	}
	if checkpointValid {
		result.HighestModSeq = selectData.HighestModSeq
	}
	return result, nil
}

func isIMAPCommandRejection(err error) bool {
	var statusErr *imap.Error
	return errors.As(err, &statusErr) && (statusErr.Type == imap.StatusResponseTypeBad || statusErr.Type == imap.StatusResponseTypeNo)
}

func (c *Client) fetchFlagUpdatesLocked(remoteName string, uids []uint32, changedSince uint64, includeModSeq bool) ([]FlagUpdate, error) {
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

		fetchOpts := &imap.FetchOptions{UID: true, Flags: true, ModSeq: includeModSeq, ChangedSince: changedSince}
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
					update.Labels = labelsFromFlags(item.Flags)
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
			return nil, fmt.Errorf("fetch flags %s: %w", remoteName, err)
		}
	}

	return allUpdates, nil
}

func labelsFromFlags(flags []imap.Flag) []storage.LabelInput {
	labels := make([]storage.LabelInput, 0, len(flags))
	seen := map[string]bool{}
	for _, flag := range flags {
		label, ok := labelInputFromFlag(flag)
		if !ok {
			continue
		}
		key := strings.ToLower(label.ProviderID)
		if seen[key] {
			continue
		}
		seen[key] = true
		labels = append(labels, label)
	}
	return labels
}

func labelInputFromFlag(flag imap.Flag) (storage.LabelInput, bool) {
	keyword := strings.TrimSpace(string(flag))
	if keyword == "" || isSystemOrStatusFlag(keyword) {
		return storage.LabelInput{}, false
	}
	return storage.LabelInput{
		Name:         keyword,
		ProviderID:   keyword,
		ProviderType: storage.LabelProviderIMAPKeyword,
	}, true
}

func ValidateKeyword(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return "", fmt.Errorf("label is required")
	}
	if strings.HasPrefix(keyword, "\\") {
		return "", fmt.Errorf("label cannot be an IMAP system flag")
	}
	if isSystemOrStatusFlag(keyword) {
		return "", fmt.Errorf("label %q is an IMAP status keyword", keyword)
	}
	for _, r := range keyword {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '$' {
			continue
		}
		return "", fmt.Errorf("label %q is not a portable IMAP keyword", keyword)
	}
	return keyword, nil
}

func isSystemOrStatusFlag(flag string) bool {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "\\seen", "\\answered", "\\flagged", "\\deleted", "\\draft", "\\recent",
		"$junk", "$notjunk", "$nonjunk", "junk", "notjunk", "nonjunk", "non-junk",
		"$forwarded", "$mdnsent", "$phishing":
		return true
	default:
		return strings.HasPrefix(flag, "\\")
	}
}

func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
