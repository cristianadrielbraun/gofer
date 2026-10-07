package mail

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/retry"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

const userAttachmentMaxBytes = 64 << 20

// EnsureAttachment shares the message/body gate, bounded sessions and account
// cancellation. A cached file remains a local fast path; HTTP never holds a
// user-store lease. The returned metadata retains the requested attachment ID.
func (s *UserIMAP) EnsureAttachment(ctx context.Context, owner string, id int64) (*storage.AttachmentFetchInfo, error) {
	release, err := s.blobs.PinUserFiles(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	var initial *storage.AttachmentRecovery
	err = s.Routing().WithUser(ctx, owner, func(db *storage.DB) error {
		var err error
		initial, err = db.GetAttachmentRecoveryForUser(ctx, owner, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if initial == nil {
		return nil, sql.ErrNoRows
	}
	var result *storage.AttachmentFetchInfo
	err = s.operation(ctx, owner, initial.Info.AccountID, initial.Info.MessageID, 2*time.Minute, func(ctx context.Context) error {
		var snapshot *storage.AttachmentRecovery
		err := s.Routing().WithAccountForUser(ctx, owner, initial.Info.AccountID, func(db *storage.DB) error {
			var err error
			snapshot, err = db.GetAttachmentRecoveryForUser(ctx, owner, id)
			return err
		})
		if err != nil {
			return err
		}
		if snapshot == nil || snapshot.Info.AccountID != initial.Info.AccountID || snapshot.Info.MessageID != initial.Info.MessageID {
			return sql.ErrNoRows
		}
		if stat, err := os.Stat(snapshot.Info.StoragePath); err == nil && stat.Mode().IsRegular() {
			copy := snapshot.Info
			result = &copy
			return nil
		}
		scope, err := s.snapshot(ctx, owner, snapshot.Info.AccountID)
		if err != nil {
			return err
		}
		if scope.config.Provider != snapshot.Info.AccountProvider || scope.config.ProviderAccountID != snapshot.MailboxSubject || scope.config.AuthMethod != snapshot.AuthMethod {
			return storage.ErrMessageMutationSuperseded
		}
		candidate, err := s.blobs.NewMessageVersion()
		if err != nil {
			return err
		}
		published := false
		defer func() {
			if !published && candidate != nil {
				_ = candidate.DeleteMessage(snapshot.Info.AccountID, snapshot.Info.MessageID)
			}
		}()
		var raw, content []byte
		localMIME := false
		if snapshot.RawPath != "" {
			file, err := os.Open(snapshot.RawPath)
			if err == nil {
				raw, err = io.ReadAll(io.LimitReader(file, userAttachmentMaxBytes+1))
				_ = file.Close()
				if err != nil && !snapshot.NetworkAllowed {
					return err
				}
				if len(raw) > userAttachmentMaxBytes && !snapshot.NetworkAllowed {
					return errors.New("saved attachment MIME exceeds the size limit")
				}
				if err != nil || len(raw) > userAttachmentMaxBytes {
					raw = nil
				}
				localMIME = len(raw) > 0
			}
		}
		providerID := snapshot.Info.ProviderMessageID
		loadProvider := func() error {
			raw, content, localMIME = nil, nil, false
			if !snapshot.NetworkAllowed || scope.tokens == nil || (scope.config.Provider != "gmail" && scope.config.Provider != "outlook") {
				return errors.New("attachment has no available recovery source")
			}
			if providerID == "" {
				request, err := userProviderMutationRequest(ctx, scope)
				if err != nil {
					return err
				}
				providerID, err = resolveUserProviderMutationID(ctx, scope.config.Provider, snapshot.InternetMessageID, request)
				if err != nil {
					return err
				}
			}
			if snapshot.Info.ProviderAttachmentID != "" {
				content, err = s.fetchUserAttachment(ctx, scope, providerID, snapshot.Info.ProviderAttachmentID)
			} else {
				raw, err = s.fetchUserAttachmentMIME(ctx, scope, providerID)
			}
			if err != nil {
				return err
			}
			return nil
		}
		if !localMIME {
			if err := loadProvider(); err != nil {
				return err
			}
		}
		path, err := prepareUserRecoveredAttachment(ctx, candidate, *snapshot, raw, content)
		if err != nil && localMIME && snapshot.NetworkAllowed {
			// A broken saved MIME copy must not prevent verified remote recovery.
			// Pending local drafts exclude this fallback so old provider bytes
			// cannot replace a newer unpublished attachment.
			_ = candidate.DeleteMessage(snapshot.Info.AccountID, snapshot.Info.MessageID)
			candidate, err = s.blobs.NewMessageVersion()
			if err != nil {
				return err
			}
			if err := loadProvider(); err != nil {
				return err
			}
			path, err = prepareUserRecoveredAttachment(ctx, candidate, *snapshot, raw, content)
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := scope.call(ctx, func(db *storage.DB) error {
			return db.PublishRecoveredAttachment(ctx, owner, *snapshot, providerID, path, localMIME)
		}); err != nil {
			return err
		}
		published = true
		copy := snapshot.Info
		copy.StoragePath = path
		copy.ProviderMessageID = providerID
		result = &copy
		return nil
	})
	return result, err
}

func prepareUserRecoveredAttachment(ctx context.Context, candidate *store.BlobStore, s storage.AttachmentRecovery, raw, content []byte) (string, error) {
	if raw == nil {
		if s.Info.SizeBytes > 0 && int64(len(content)) != s.Info.SizeBytes {
			return "", errors.New("provider attachment size changed")
		}
		return candidate.StoreAttachment(ctx, s.Info.AccountID, s.Info.MessageID, s.Info.ID, s.Info.Filename, bytes.NewReader(content))
	}
	parsed, err := message.ParseMessage(ctx, bytes.NewReader(raw), candidate, s.Info.AccountID, s.Info.MessageID)
	if err != nil {
		return "", err
	}
	if parsed.ParseError != nil {
		return "", parsed.ParseError
	}
	if s.InternetMessageID != "" && message.NormalizeMessageID(parsed.MessageID) != message.NormalizeMessageID(s.InternetMessageID) {
		return "", errors.New("attachment MIME message identity changed")
	}
	index, err := userAttachmentMIMEPart(s, parsed.Attachments)
	if err != nil {
		return "", err
	}
	path := parsed.Attachments[index].BlobPath
	// Only the chosen private path is published; raw/sibling copies are disposable.
	_ = os.Remove(parsed.RawPath)
	for i, part := range parsed.Attachments {
		if i != index {
			_ = os.Remove(part.BlobPath)
		}
	}
	return path, nil
}

func attachmentMediaType(value string) string {
	if typ, _, err := mime.ParseMediaType(value); err == nil {
		return strings.ToLower(typ)
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func attachmentPartMatches(expected storage.AttachmentRecoveryPart, actual message.AttachmentMeta) bool {
	return expected.Filename == actual.Filename && attachmentMediaType(expected.ContentType) == attachmentMediaType(actual.ContentType) && strings.Trim(strings.TrimSpace(expected.ContentID), "<>") == strings.Trim(strings.TrimSpace(actual.ContentID), "<>") && expected.Inline == actual.Inline && (expected.SizeBytes <= 0 || expected.SizeBytes == actual.Size)
}

func userAttachmentMIMEPart(s storage.AttachmentRecovery, parts []message.AttachmentMeta) (int, error) {
	if s.PartIndex < 0 || s.PartIndex >= len(s.Parts) {
		return 0, errors.New("local attachment part is unavailable")
	}
	expected := s.Parts[s.PartIndex]
	matches := []int{}
	for i, p := range parts {
		if attachmentPartMatches(expected, p) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	// Identical metadata requires the full stored insertion order to match
	// MIME order. Never pick an arbitrary same-name sibling.
	if len(matches) > 1 && len(parts) == len(s.Parts) {
		for i, p := range parts {
			if !attachmentPartMatches(s.Parts[i], p) {
				return 0, errors.New("attachment MIME layout changed")
			}
		}
		return s.PartIndex, nil
	}
	return 0, errors.New("attachment MIME part is missing or ambiguous")
}

func (s *UserIMAP) withUserAttachmentToken(ctx context.Context, scope *userIMAPScope, fetch func(string) ([]byte, error)) ([]byte, error) {
	o := NewSyncOrchestrator(nil, nil, nil, scope.tokens)
	o.imapScope = scope
	var token string
	var err error
	if scope.config.Provider == "outlook" {
		graph, ok := scope.tokens.(graphMailTokenProvider)
		if !ok {
			return nil, errors.New("Graph attachment credentials are unavailable")
		}
		token, err = graph.GetMicrosoftGraphMailTokenForAccount(ctx, scope.id)
	} else {
		token, err = scope.tokens.GetOAuthTokenForAccount(ctx, scope.id)
	}
	if err != nil {
		return nil, o.recordOutlookRetry(ctx, err)
	}
	var content []byte
	err = o.outlookRequest(ctx, token, func(access string) error {
		if err := scope.call(ctx, func(*storage.DB) error { return nil }); err != nil {
			return err
		}
		var err error
		content, err = fetch(access)
		return err
	})
	return content, err
}

func (s *UserIMAP) fetchUserAttachmentMIME(ctx context.Context, scope *userIMAPScope, id string) ([]byte, error) {
	return s.withUserAttachmentToken(ctx, scope, func(token string) ([]byte, error) {
		if scope.config.Provider == "gmail" {
			return getUserGmailRaw(ctx, token, id)
		}
		return getUserOutlookRaw(ctx, token, id)
	})
}

func (s *UserIMAP) fetchUserAttachment(ctx context.Context, scope *userIMAPScope, messageID, attachmentID string) ([]byte, error) {
	return s.withUserAttachmentToken(ctx, scope, func(token string) ([]byte, error) {
		endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(attachmentID)
		limit := int64((userAttachmentMaxBytes+2)/3*4 + 4096)
		if scope.config.Provider == "outlook" {
			endpoint = outlookGraphBaseURL + "/me/messages/" + url.PathEscape(messageID) + "/attachments/" + url.PathEscape(attachmentID) + "/$value"
			limit = userAttachmentMaxBytes
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if scope.config.Provider == "outlook" {
			req.Header.Set("Prefer", `IdType="ImmutableId"`)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			at, _ := retry.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now().UTC())
			return nil, &providerAPIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body)), RetryAt: at}
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, errors.New("provider attachment exceeds the size limit")
		}
		if scope.config.Provider == "outlook" {
			return data, nil
		}
		var response struct {
			Data *string
			Size int64
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		if response.Data == nil || response.Size < 0 {
			return nil, errors.New("Gmail attachment response is incomplete")
		}
		raw, err := base64.RawURLEncoding.DecodeString(*response.Data)
		if err != nil {
			raw, err = base64.URLEncoding.DecodeString(*response.Data)
		}
		if err != nil {
			return nil, fmt.Errorf("decode Gmail attachment: %w", err)
		}
		if len(raw) > userAttachmentMaxBytes || (response.Size > 0 && response.Size != int64(len(raw))) {
			return nil, errors.New("Gmail attachment has an invalid size")
		}
		return raw, nil
	})
}
