package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// mailImportFolderRemote is how an upload names a destination folder: the
// Gmail label, Outlook folder or JMAP mailbox behind it, or the IMAP folder
// name.
func mailImportFolderRemote(provider string, folder storage.MailExportFolder) string {
	if provider == providers.ProviderLocal {
		// The Local folders writer files mail by folder ID.
		return folder.ID
	}
	if provider == "gmail" || provider == "outlook" || provider == providers.ProviderJMAP {
		return folder.ProviderRemoteID
	}
	return folder.RemoteID
}

// errOutlookImportDraft reports a message Outlook kept as an unsent draft.
// The draft is deleted, so a failed upload leaves nothing behind.
var errOutlookImportDraft = errors.New("Outlook saved it as an unsent draft, so it was removed")

// outlookImportWriter uploads messages to Outlook through Microsoft Graph.
// Graph makes a message created from MIME an unsent draft, and a message's
// draft state can't change after it's created, so each message is created
// from its parts instead, with the MAPI properties of a delivered message:
// its flags say it isn't a draft, and its sent and received times are the
// original's. A message that still comes back as a draft is removed and
// reported.
type outlookImportWriter struct {
	mailImportCaller
}

func (w *outlookImportWriter) close() {}

// ensureFolder finds or creates the folder, one level of its path at a time.
func (w *outlookImportWriter) ensureFolder(ctx context.Context, name string) (string, error) {
	parent := ""
	for _, part := range strings.Split(name, "/") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		collection := outlookGraphBaseURL + "/me/mailFolders"
		if parent != "" {
			collection = outlookGraphBaseURL + "/me/mailFolders/" + url.PathEscape(parent) + "/childFolders"
		}
		var found struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		filter := url.Values{"$filter": {"displayName eq '" + strings.ReplaceAll(part, "'", "''") + "'"}}
		if err := w.call(ctx, http.MethodGet, collection+"?"+filter.Encode(), "", func() io.Reader { return nil }, &found); err != nil {
			return "", err
		}
		if len(found.Value) > 0 && found.Value[0].ID != "" {
			parent = found.Value[0].ID
			continue
		}
		payload, _ := json.Marshal(map[string]string{"displayName": part})
		var created struct {
			ID string `json:"id"`
		}
		if err := w.call(ctx, http.MethodPost, collection, "application/json", func() io.Reader { return bytes.NewReader(payload) }, &created); err != nil {
			return "", err
		}
		if created.ID == "" {
			return "", errors.New("Outlook did not create the folder")
		}
		parent = created.ID
	}
	if parent == "" {
		return "", errors.New("the folder has no name")
	}
	return parent, nil
}

func (w *outlookImportWriter) upload(ctx context.Context, folderID string, msg mailImportMessage) error {
	payload, later, err := outlookMessageJSON(msg)
	if err != nil {
		return err
	}
	var created struct {
		ID      string `json:"id"`
		IsDraft bool   `json:"isDraft"`
	}
	err = w.call(ctx, http.MethodPost, outlookGraphBaseURL+"/me/mailFolders/"+url.PathEscape(folderID)+"/messages", "application/json",
		func() io.Reader { return bytes.NewReader(payload) }, &created)
	if err != nil {
		return err
	}
	if created.ID == "" {
		return errors.New("Outlook did not return the new message")
	}
	message := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(created.ID)
	remove := func() { _ = w.call(ctx, http.MethodDelete, message, "", func() io.Reader { return nil }, nil) }
	if created.IsDraft {
		remove()
		return errOutlookImportDraft
	}
	// Attachments too big for one request follow the message. A message
	// that can't take one of them is removed, so no copy is left without
	// part of its content.
	for _, attachment := range later {
		if err := w.attach(ctx, message, attachment); err != nil {
			remove()
			return fmt.Errorf("attach %s: %w", attachment.Name, err)
		}
	}
	// The message is created read, the flag value known to keep it out of
	// Drafts, and then takes its own read state.
	state := map[string]any{"isRead": msg.Read}
	if msg.Flagged {
		state["flag"] = map[string]string{"flagStatus": "flagged"}
	}
	statePayload, _ := json.Marshal(state)
	return w.call(ctx, http.MethodPatch, message, "application/json", func() io.Reader { return bytes.NewReader(statePayload) }, nil)
}

// Graph takes requests of up to 4 MB. A message is created with as many of
// its attachments as keep the request under outlookRequestBudget; the rest
// are added afterwards, those under outlookSmallAttachment in one request
// each and larger ones through an upload session, in pieces of
// outlookUploadChunk bytes (a multiple of 320 KiB, as Graph asks).
var (
	outlookRequestBudget   = 3 << 20
	outlookSmallAttachment = 3 << 20
	outlookUploadChunk     = 9 * 320 << 10
)

// outlookPendingAttachment is an attachment added after its message.
type outlookPendingAttachment struct {
	Name        string
	ContentType string
	Content     []byte
	Inline      bool
	ContentID   string
}

func (a outlookPendingAttachment) graph() outlookImportAttachment {
	return outlookFileAttachment(a.Name, a.ContentType, a.Content, a.Inline, a.ContentID)
}

func (w *outlookImportWriter) attach(ctx context.Context, message string, attachment outlookPendingAttachment) error {
	if len(attachment.Content) < outlookSmallAttachment {
		payload, err := json.Marshal(attachment.graph())
		if err != nil {
			return err
		}
		return w.call(ctx, http.MethodPost, message+"/attachments", "application/json", func() io.Reader { return bytes.NewReader(payload) }, nil)
	}
	item := map[string]any{
		"attachmentType": "file", "name": attachment.Name, "size": len(attachment.Content),
		"contentType": attachment.graph().ContentType, "isInline": attachment.Inline,
	}
	if attachment.Inline {
		item["contentId"] = attachment.ContentID
	}
	request, _ := json.Marshal(map[string]any{"AttachmentItem": item})
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := w.call(ctx, http.MethodPost, message+"/attachments/createUploadSession", "application/json", func() io.Reader { return bytes.NewReader(request) }, &session); err != nil {
		return err
	}
	if session.UploadURL == "" {
		return errors.New("Outlook did not start the upload")
	}
	total := len(attachment.Content)
	for start := 0; start < total; start += outlookUploadChunk {
		end := min(start+outlookUploadChunk, total)
		if err := outlookUploadRange(ctx, session.UploadURL, attachment.Content[start:end], start, total); err != nil {
			return err
		}
	}
	return nil
}

// outlookUploadRange sends one piece of an attachment to its upload
// session. The session's address carries its own authorization.
func outlookUploadRange(ctx context.Context, uploadURL string, piece []byte, start, total int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(piece))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(piece))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+len(piece)-1, total))
	resp, err := (&http.Client{Transport: netguard.APITransport, Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &mailImportAPIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return nil
}

type outlookImportAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type outlookImportRecipient struct {
	EmailAddress outlookImportAddress `json:"emailAddress"`
}

type outlookImportAttachment struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType,omitempty"`
	ContentBytes string `json:"contentBytes"`
	IsInline     bool   `json:"isInline,omitempty"`
	ContentID    string `json:"contentId,omitempty"`
}

type outlookImportProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type outlookImportMessage struct {
	Subject           string                    `json:"subject"`
	Body              outlookImportBody         `json:"body"`
	From              *outlookImportRecipient   `json:"from,omitempty"`
	Sender            *outlookImportRecipient   `json:"sender,omitempty"`
	To                []outlookImportRecipient  `json:"toRecipients,omitempty"`
	Cc                []outlookImportRecipient  `json:"ccRecipients,omitempty"`
	Bcc               []outlookImportRecipient  `json:"bccRecipients,omitempty"`
	ReplyTo           []outlookImportRecipient  `json:"replyTo,omitempty"`
	InternetMessageID string                    `json:"internetMessageId,omitempty"`
	Attachments       []outlookImportAttachment `json:"attachments,omitempty"`
	Extended          []outlookImportProperty   `json:"singleValueExtendedProperties"`
}

type outlookImportBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// outlookMessageJSON builds the Graph message for an original MIME message:
// its addresses, subject, body and attachments, and the MAPI properties of
// a delivered, read message dated when it was sent. Attachments that would
// make the request too big are returned to add afterwards.
func outlookMessageJSON(msg mailImportMessage) ([]byte, []outlookPendingAttachment, error) {
	reader, err := gomail.CreateReader(bytes.NewReader(msg.Raw))
	if err != nil {
		return nil, nil, fmt.Errorf("read the message: %w", err)
	}
	header := reader.Header
	out := outlookImportMessage{}
	out.Subject, _ = header.Subject()
	recipients := func(field string) []outlookImportRecipient {
		list, err := header.AddressList(field)
		if err != nil {
			return nil
		}
		var result []outlookImportRecipient
		for _, address := range list {
			if address.Address != "" {
				result = append(result, outlookImportRecipient{EmailAddress: outlookImportAddress{Name: mailmessage.DecodeHeader(address.Name), Address: address.Address}})
			}
		}
		return result
	}
	if from := recipients("From"); len(from) > 0 {
		out.From = &from[0]
		out.Sender = &from[0]
	}
	if sender := recipients("Sender"); len(sender) > 0 {
		out.Sender = &sender[0]
	}
	out.To, out.Cc, out.Bcc, out.ReplyTo = recipients("To"), recipients("Cc"), recipients("Bcc"), recipients("Reply-To")
	if id := strings.TrimSpace(header.Get("Message-Id")); id != "" {
		out.InternetMessageID = id
	}

	var text, html string
	var attachments []outlookPendingAttachment
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read the message: %w", err)
		}
		content, err := io.ReadAll(part.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("read the message: %w", err)
		}
		switch h := part.Header.(type) {
		case *gomail.InlineHeader:
			contentType, _, _ := h.ContentType()
			switch {
			case contentType == "text/html" && html == "":
				html = string(content)
				continue
			case strings.HasPrefix(contentType, "text/") && contentType != "text/calendar" && text == "":
				text = string(content)
				continue
			}
			id := strings.Trim(h.Get("Content-Id"), "<> ")
			name, _ := (&gomail.AttachmentHeader{Header: h.Header}).Filename()
			if name == "" {
				name = id
			}
			if name == "" {
				name = "attachment"
			}
			attachments = append(attachments, outlookPendingAttachment{Name: name, ContentType: contentType, Content: content, Inline: id != "", ContentID: id})
		case *gomail.AttachmentHeader:
			contentType, _, _ := h.ContentType()
			name, _ := h.Filename()
			if name == "" {
				name = fmt.Sprintf("attachment-%d", len(attachments)+1)
				if contentType == "message/rfc822" {
					name += ".eml"
				}
			}
			id := strings.Trim(h.Get("Content-Id"), "<> ")
			inline := strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Disposition"))), "inline") && id != ""
			attachments = append(attachments, outlookPendingAttachment{Name: name, ContentType: contentType, Content: content, Inline: inline, ContentID: id})
		}
	}
	if html != "" {
		out.Body = outlookImportBody{ContentType: "html", Content: html}
	} else {
		out.Body = outlookImportBody{ContentType: "text", Content: text}
	}

	date := msg.Date
	if date.IsZero() {
		date = time.Now()
	}
	when := date.UTC().Format(time.RFC3339)
	out.Extended = []outlookImportProperty{
		// PidTagMessageFlags: read, and not unsent.
		{ID: "Integer 0x0E07", Value: "1"},
		// PidTagClientSubmitTime and PidTagMessageDeliveryTime.
		{ID: "SystemTime 0x0039", Value: when},
		{ID: "SystemTime 0x0E06", Value: when},
	}
	// Smaller attachments go first into the message, while it stays within
	// the request budget.
	sort.SliceStable(attachments, func(i, k int) bool { return len(attachments[i].Content) < len(attachments[k].Content) })
	base, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	size := len(base)
	var later []outlookPendingAttachment
	for _, attachment := range attachments {
		encoded := base64.StdEncoding.EncodedLen(len(attachment.Content)) + len(attachment.Name) + len(attachment.ContentID) + 512
		if size+encoded <= outlookRequestBudget {
			out.Attachments = append(out.Attachments, attachment.graph())
			size += encoded
			continue
		}
		later = append(later, attachment)
	}
	payload, err := json.Marshal(out)
	return payload, later, err
}

func outlookFileAttachment(name, contentType string, content []byte, inline bool, id string) outlookImportAttachment {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	attachment := outlookImportAttachment{
		ODataType: "#microsoft.graph.fileAttachment", Name: name, ContentType: contentType,
		ContentBytes: base64.StdEncoding.EncodeToString(content), IsInline: inline,
	}
	if inline {
		attachment.ContentID = id
	}
	return attachment
}
