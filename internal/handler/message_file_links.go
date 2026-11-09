package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

// A message is shown in a sandboxed frame that the browser treats as another
// site, so the images it loads from Gofer carry no session cookie and would be
// refused. Gofer therefore signs each image address when it builds the message
// page: the signature names the owner, the message and the file, and expires.
// The signed route serves only that file, as its owner, without a session.
// The signing key is random for each server start, so a restart only means
// opening the message again. A link also names the session that opened the
// message and stops working when that session ends, such as at sign-out.
//
// Remote content the reader chose to load is signed the same way, as a "web"
// file named by its address: Gofer downloads it and passes it on, so the
// sender never sees the reader's IP address or browser, and browsers that
// block known trackers inside other sites still show the images.

const (
	messageFileRoute        = "/api/message-file/"
	messageFileLinkLifetime = 12 * time.Hour
	messageFileInline       = "inline"
	messageFileRemote       = "remote"
	messageFileWeb          = "web"
)

func newMessageFileKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

func (h *Handler) messageFileSignature(kind, owner, session string, messageID int64, name string, expires int64) string {
	mac := hmac.New(sha256.New, h.messageFileKey)
	for _, part := range []string{"gofer-message-file/v2", kind, owner, session, strconv.FormatInt(messageID, 10), name, strconv.FormatInt(expires, 10)} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

var messageFilePath = regexp.MustCompile(`/api/(inline-content|remote-assets)/(\d+)/([^"'\s)<>&?#]+)`)

// signMessageFileLinks rewrites the message's own inline parts and saved
// remote images in a body document to signed addresses. Paths that name
// another message are left alone, so a message can't borrow another one's
// files.
func (h *Handler) signMessageFileLinks(ctx context.Context, body []byte, owner string, messageID int64) []byte {
	if len(h.messageFileKey) == 0 || owner == "" {
		return body
	}
	session := messageFileSession(ctx)
	expires := messageFileLinkExpiry()
	return messageFilePath.ReplaceAllFunc(body, func(match []byte) []byte {
		parts := messageFilePath.FindSubmatch(match)
		id, err := strconv.ParseInt(string(parts[2]), 10, 64)
		if err != nil || id != messageID {
			return match
		}
		name, err := url.PathUnescape(string(parts[3]))
		if err != nil || name == "" {
			return match
		}
		kind := messageFileInline
		if string(parts[1]) == "remote-assets" {
			kind = messageFileRemote
		}
		query := url.Values{
			"u": {owner},
			"e": {strconv.FormatInt(expires, 10)},
			"s": {h.messageFileSignature(kind, owner, session, id, name, expires)},
		}
		if session != "" {
			query.Set("a", session)
		}
		return []byte(messageFileRoute + kind + "/" + string(parts[2]) + "/" + url.PathEscape(name) + "?" + query.Encode())
	})
}

// messageFileLinkExpiry ends links on the hour, so a message opened again
// within the hour gets the same addresses and the browser can reuse what it
// already loaded.
func messageFileLinkExpiry() int64 {
	return time.Now().Truncate(time.Hour).Add(messageFileLinkLifetime + time.Hour).Unix()
}

func messageFileWebName(remoteURL string) string {
	sum := sha256.Sum256([]byte(remoteURL))
	return hex.EncodeToString(sum[:8])
}

// remoteContentLinks returns the loader that RestoreRemoteContent passes each
// remote address of the message through, giving a signed address on Gofer.
func (h *Handler) remoteContentLinks(ctx context.Context, owner string, messageID int64) func(string) string {
	expires := messageFileLinkExpiry()
	session := messageFileSession(ctx)
	return func(remoteURL string) string {
		if len(h.messageFileKey) == 0 || owner == "" || remoteURL == "" {
			return ""
		}
		query := url.Values{
			"u": {owner},
			"e": {strconv.FormatInt(expires, 10)},
			"r": {remoteURL},
			"s": {h.messageFileSignature(messageFileWeb, owner, session, messageID, remoteURL, expires)},
		}
		if session != "" {
			query.Set("a", session)
		}
		return messageFileRoute + messageFileWeb + "/" + strconv.FormatInt(messageID, 10) + "/" + messageFileWebName(remoteURL) + "?" + query.Encode()
	}
}

// verifiedMessageFileRequest checks a signed address and returns the request
// as its owner would make it to the file's usual route, or false.
func (h *Handler) verifiedMessageFileRequest(r *http.Request) (*http.Request, string, bool) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	id, err := strconv.ParseInt(r.PathValue("messageID"), 10, 64)
	query := r.URL.Query()
	owner := query.Get("u")
	expires, expErr := strconv.ParseInt(query.Get("e"), 10, 64)
	if len(h.messageFileKey) == 0 || err != nil || id <= 0 || expErr != nil || owner == "" || name == "" ||
		(kind != messageFileInline && kind != messageFileRemote && kind != messageFileWeb) || time.Now().Unix() > expires {
		return nil, "", false
	}
	signed := name
	if kind == messageFileWeb {
		signed = query.Get("r")
		if signed == "" || name != messageFileWebName(signed) {
			return nil, "", false
		}
	}
	session := query.Get("a")
	want := h.messageFileSignature(kind, owner, session, id, signed, expires)
	if !hmac.Equal([]byte(strings.ToLower(query.Get("s"))), []byte(want)) {
		return nil, "", false
	}
	if !h.messageFileSessionActive(r.Context(), session, owner) {
		return nil, "", false
	}
	ownerRequest := r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: owner, Username: owner}))
	ownerRequest.SetPathValue("messageID", strconv.FormatInt(id, 10))
	ownerRequest.SetPathValue("contentID", name)
	ownerRequest.SetPathValue("filename", name)
	return ownerRequest, kind, true
}

// messageFileSession names the session a link is made for, or "" when the
// server signs nobody in.
func messageFileSession(ctx context.Context) string {
	if session := auth.SessionFromContext(ctx); session != nil {
		return session.ID
	}
	return ""
}

// messageFileSessionActive checks that the session a link was made for is
// still signed in. Links without one are accepted only where nobody signs in.
func (h *Handler) messageFileSessionActive(ctx context.Context, session, owner string) bool {
	if h.auth == nil || !h.auth.IsEnabled() {
		return true
	}
	if session == "" {
		return false
	}
	active, err := h.auth.SessionActive(ctx, session, owner)
	return err == nil && active
}

// The frame showing the message counts as another site, so these responses
// must be usable cross-origin. They stay sandboxed and are never sniffed.
func allowMessageFileEmbedding(w http.ResponseWriter) {
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}

// handleSignedMessageFile serves a signed message file on the shared server.
func (h *Handler) handleSignedMessageFile(w http.ResponseWriter, r *http.Request) {
	ownerRequest, kind, ok := h.verifiedMessageFileRequest(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	allowMessageFileEmbedding(w)
	switch kind {
	case messageFileWeb:
		h.relayRemoteContent(w, ownerRequest)
	case messageFileRemote:
		h.handleRemoteAsset(w, ownerRequest)
	default:
		h.handleInlineContent(w, ownerRequest)
	}
}

// relayRemoteContent downloads one remote file of a message for the reader
// and passes it on as a message file: images show in place, anything else
// is sent as bytes the page can't run.
func (h *Handler) relayRemoteContent(w http.ResponseWriter, r *http.Request) {
	download := h.remoteResourceDownloader
	if download == nil {
		download = newRemoteResourceDownloader(privateTargetAllowed(h.db), h.readerDefaultProxy)
	}
	data, err := download(r.Context(), r.URL.Query().Get("r"))
	if err != nil || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	serveMailFile(w, r, bytes.NewReader(data), "remote", relayedContentType(data), true)
}

// relayedContentType names relayed content by what it is, never by what the
// remote server says it is.
func relayedContentType(data []byte) string {
	contentType := http.DetectContentType(data)
	if strings.HasPrefix(contentType, "text/xml") || strings.HasPrefix(contentType, "text/plain") {
		head := bytes.ToLower(data[:min(len(data), 1024)])
		if bytes.Contains(head, []byte("<svg")) {
			return "image/svg+xml"
		}
	}
	return contentType
}
