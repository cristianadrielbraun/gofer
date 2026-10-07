package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

// All selected handlers below perform only local database/file work. Their
// response, events and queue wakes are flushed after releasing the user lease.
func (h *Handler) userComposeHandler(selectHandler func(*Handler) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		stop := context.AfterFunc(h.userStorageContext, cancel)
		defer stop()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, 2*composeAttachmentMaxBytes+1<<20)
		// Parse request data outside the storage lease. Retry JSON is parsed by
		// its handler; compose uses URL-encoded forms, uploads use their own route.
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/compose") {
			if err := r.ParseForm(); err != nil {
				writeComposeJSONError(w, 400, "invalid form data")
				return
			}
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/outgoing-sends/") {
			// A slow request body must not hold the owner's cache lease.
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
			if err != nil {
				writeOutgoingSendError(w, 400, "invalid outgoing request", false)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		state := &userMessageMutationState{owner: h.userID(ctx), routing: h.userStorage, checked: make(map[string]bool), wake: make(map[string]bool), credentials: h.userCredentials != nil}
		response := &userMutationResponse{header: make(http.Header)}
		err := h.userAccounts.WithUser(ctx, state.owner, func(accounts *config.AccountStore, db *storage.DB) error {
			state.accounts = accounts
			defer func() { state.accounts = nil }()
			local := &Handler{db: db, accountStore: accounts, auth: h.auth, blobStore: h.blobStore, userMutationState: state}
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/outgoing-sends/") {
				summary, err := db.GetOutgoingSendSummaryForUser(ctx, state.owner, r.PathValue("id"))
				if err != nil || local.checkUserMessageMutation(ctx, &storage.MessageMutationInfo{AccountID: summary.AccountID, AccountProvider: "imap"}) != nil {
					http.NotFound(response, r)
					return nil
				}
			}
			selectHandler(local)(response, r)
			return nil
		})
		if err != nil {
			log.Printf("user compose unavailable: %v", err)
			http.Error(w, "mailbox unavailable", 503)
			return
		}
		for _, event := range state.events {
			h.userIMAP.Events().Publish(event)
			state.wake[event.AccountID] = true
		}
		for id := range state.wake {
			if err := h.userIMAP.WakeMutations(ctx, state.owner, id); err != nil {
				log.Printf("user compose saved; queue wake %s: %v", id, err)
				response.header.Set("X-Gofer-Mail-Delivery", "delayed")
			}
		}
		for key, values := range response.header {
			w.Header()[key] = append([]string(nil), values...)
		}
		if response.status == 0 {
			response.status = 200
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	}
}

func (h *Handler) saveUserComposeDraft(ctx context.Context, r *http.Request) (composeDraftSaveResult, *composeRequestError) {
	fail := func(status int, text string) (composeDraftSaveResult, *composeRequestError) {
		return composeDraftSaveResult{}, &composeRequestError{status: status, message: text}
	}
	accountID := r.FormValue("account_id")
	if accountID == "" {
		accountID = h.accountStore.GetFirstAccountID(ctx, h.userID(ctx))
	}
	account, err := h.ownedAccount(ctx, accountID)
	if err != nil || account == nil {
		return fail(404, "account not found or provider unavailable")
	}
	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return fail(404, "account not found")
	}
	folder, remote, err := h.db.GetFolderIDByRole(ctx, accountID, "drafts")
	if err != nil || folder == "" || remote == "" {
		return fail(400, "drafts folder not available")
	}
	draftID := strings.TrimSpace(r.FormValue("draft_id"))
	if draftID == "" {
		draftID = message.NewMessageID()
	}
	body, html := r.FormValue("body"), strings.TrimSpace(r.FormValue("html_body"))
	if html != "" {
		html = string(message.SanitizeHTML([]byte(html)))
	}
	attachments, _, err := h.collectComposeAttachments(r)
	if err != nil {
		return fail(404, err.Error())
	}
	if err := validateComposeMessageSize(attachments, body, html); err != nil {
		return fail(400, err.Error())
	}
	to, _ := message.ParseAddressList(r.FormValue("to"))
	cc, _ := message.ParseAddressList(r.FormValue("cc"))
	bcc, _ := message.ParseAddressList(r.FormValue("bcc"))
	msg := &message.OutgoingMessage{FromName: account.Name, FromEmail: account.Email, To: to, CC: cc, Bcc: bcc, Subject: r.FormValue("subject"), TextBody: body, HTMLBody: html, InReplyTo: r.FormValue("in_reply_to"), References: r.FormValue("references"), MessageID: draftID, Date: time.Now().UTC(), Attachments: attachments}
	reply, references := h.validComposeThreadHeaders(ctx, accountID, msg.Subject, msg.InReplyTo, msg.References)
	var pending *storage.OutgoingSend
	if localID, e := h.db.GetMessageLocalIDByInternetIDInternal(ctx, accountID, draftID); e == nil && localID > 0 {
		pending, err = h.db.OutgoingSendForMessageInternal(ctx, localID)
		if err != nil {
			return fail(500, "failed to load queued message")
		}
	}
	candidate, err := h.blobStore.NewMessageVersion()
	if err != nil {
		return fail(500, "failed to stage draft")
	}
	var stagedID int64
	success := false
	defer func() {
		if !success && stagedID > 0 {
			_ = candidate.DeleteMessage(accountID, stagedID)
		}
	}()
	draft := storage.DraftMessageInput{AccountID: accountID, FolderID: folder, InternetMessageID: draftID, InReplyTo: msg.InReplyTo, References: msg.References, Subject: msg.Subject, FromName: msg.FromName, FromEmail: msg.FromEmail, Snippet: sentSnippet(body, msg.Subject), ToRecipients: parseDraftRecipients(r.FormValue("to")), CCRecipients: parseDraftRecipients(r.FormValue("cc")), BCCRecipients: parseDraftRecipients(r.FormValue("bcc")), Date: msg.Date}
	id, err := h.db.PublishDraft(ctx, draft, func(id int64) (storage.DraftPublication, error) {
		stagedID = id
		c := storage.DraftPublication{}
		var err error
		if body != "" {
			c.TextPath, err = candidate.StoreBodyText(ctx, accountID, id, []byte(body))
			if err != nil {
				return c, err
			}
		}
		if html != "" {
			c.HTMLPath, err = candidate.StoreBodyHTML(ctx, accountID, id, []byte(html))
			if err != nil {
				return c, err
			}
		}
		msg.Attachments = append([]message.OutgoingAttachment(nil), attachments...)
		for i, a := range attachments {
			file, err := os.Open(a.Path)
			if err != nil {
				return c, err
			}
			path, copyErr := candidate.StoreAttachment(ctx, accountID, id, int64(i+1), a.Filename, file)
			closeErr := file.Close()
			if copyErr != nil {
				return c, copyErr
			}
			if closeErr != nil {
				return c, closeErr
			}
			stat, err := os.Stat(path)
			if err != nil {
				return c, err
			}
			msg.Attachments[i].Path = path
			msg.Attachments[i].Size = stat.Size()
			c.Attachments = append(c.Attachments, storage.AttachmentRow{Filename: a.Filename, ContentType: a.ContentType, SizeBytes: stat.Size(), ContentID: a.ContentID, Inline: a.Inline, StoragePath: path})
		}
		revision := uuid.NewString()
		raw, err := message.BuildMIMEMessageForIMAPDraft(msg, revision)
		if err != nil {
			return c, err
		}
		c.RawPath, err = candidate.StoreRaw(ctx, accountID, id, raw)
		if err != nil {
			return c, err
		}
		if account.Provider == "imap" {
			c.Sync = storage.QueueIMAPDraftUpsertInput{State: storage.IMAPDraftState{AccountID: accountID, DraftKey: draftID, FolderID: folder, FolderRemoteName: remote}, RevisionToken: revision, MIMEData: raw, MessageDate: msg.Date}
		} else {
			c.ProviderSync = &storage.QueueUserProviderDraftInput{State: storage.UserProviderDraftState{AccountID: accountID, DraftKey: draftID, FolderID: folder, Provider: cfg.Provider, MailboxSubject: cfg.ProviderAccountID}, RevisionToken: revision, MIMEData: raw, MessageDate: msg.Date}
		}
		if pending != nil && pending.Status == storage.OutgoingSendPending {
			var previous outgoingMessageSnapshot
			if err := json.Unmarshal(pending.MessageJSON, &previous); err != nil {
				return c, err
			}
			delivery := *msg
			delivery.MessageID = previous.MessageID
			delivery.Date = previous.Date
			if delivery.HTMLBody != "" && !strings.Contains(strings.ToLower(delivery.HTMLBody), "<html") {
				delivery.HTMLBody = "<html><body>" + delivery.HTMLBody + "</body></html>"
			}
			delivery.InReplyTo, delivery.References = reply, references
			if len(message.AllRecipients(&delivery)) == 0 {
				return c, fmt.Errorf("queued draft needs a recipient")
			}
			mime, err := message.BuildMIMEMessage(&delivery)
			if err != nil {
				return c, err
			}
			snapshot, err := json.Marshal(snapshotOutgoingMessage(&delivery))
			if err != nil {
				return c, err
			}
			c.Pending = &storage.QueueOutgoingSendInput{ID: pending.ID, AccountID: accountID, DraftID: draftID, EnvelopeFrom: msg.FromEmail, EnvelopeRecipients: message.AllRecipients(&delivery), MIMEData: mime, MessageJSON: snapshot}
		}
		return c, nil
	})
	if err != nil {
		log.Printf("publish user draft: %v", err)
		return fail(500, "failed to save draft")
	}
	success = true
	h.publishMutation(accountID, folder)
	return composeDraftSaveResult{AccountID: accountID, DraftID: draftID, MessageID: id, DraftFolderID: folder}, nil
}

func (h *Handler) handleUserDiscardDraft(w http.ResponseWriter, r *http.Request) {
	accountID, draftID := r.FormValue("account_id"), r.FormValue("draft_id")
	if r.Method == http.MethodDelete {
		email, err := h.db.GetEmailByIDForUser(r.Context(), r.PathValue("id"), h.userID(r.Context()))
		if err != nil || email == nil || !email.IsDraft {
			http.NotFound(w, r)
			return
		}
		accountID, draftID = email.AccountID, email.InternetMessageID
	}
	account, err := h.ownedAccount(r.Context(), accountID)
	if err != nil || account == nil {
		http.NotFound(w, r)
		return
	}
	if draftID == "" {
		writeComposeJSONError(w, 400, "draft id is required")
		return
	}
	folder, err := h.db.DiscardIMAPDraft(r.Context(), accountID, draftID)
	if err != nil {
		writeComposeJSONError(w, 500, "failed to discard draft")
		return
	}
	if folder != "" {
		h.publishMutation(accountID, folder)
	}
	writeOutgoingSendJSON(w, 200, map[string]string{"status": "discarded"})
}

func (h *Handler) registerUserCompose(private func(string, http.HandlerFunc)) {
	for _, route := range []struct {
		pattern string
		handler func(*Handler) http.HandlerFunc
	}{
		{"GET /compose/pane", func(h *Handler) http.HandlerFunc { return h.handleComposePane }},
		{"GET /api/compose/source", func(h *Handler) http.HandlerFunc { return h.handleComposeSource }},
		{"POST /compose", func(h *Handler) http.HandlerFunc { return h.handleCompose }},
		{"POST /compose/schedule", func(h *Handler) http.HandlerFunc { return h.handleComposeSchedule }},
		{"POST /compose/draft", func(h *Handler) http.HandlerFunc { return h.handleComposeDraft }},
		{"POST /compose/draft/discard", func(h *Handler) http.HandlerFunc { return h.handleUserDiscardDraft }},
		{"GET /api/drafts/{id}", func(h *Handler) http.HandlerFunc { return h.handleGetDraft }},
		{"DELETE /api/drafts/{id}", func(h *Handler) http.HandlerFunc { return h.handleUserDiscardDraft }},
		{"GET /api/outgoing-sends/active", func(h *Handler) http.HandlerFunc { return h.handleOutgoingSendList }},
		{"GET /api/outgoing-sends/{id}", func(h *Handler) http.HandlerFunc { return h.handleOutgoingSendGet }},
		{"POST /api/outgoing-sends/{id}/retry", func(h *Handler) http.HandlerFunc { return h.handleOutgoingSendRetry }},
		{"POST /api/outgoing-sends/{id}/retry-now", func(h *Handler) http.HandlerFunc { return h.handleOutgoingSendRetryNow }},
		{"POST /api/outgoing-sends/{id}/cancel", func(h *Handler) http.HandlerFunc { return h.handleOutgoingSendCancel }},
	} {
		private(route.pattern, h.userComposeHandler(route.handler))
	}
	private("POST /compose/attachments", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, composeAttachmentMaxBytes+1<<20)
		local := &Handler{auth: h.auth, blobStore: h.blobStore, userMutationState: &userMessageMutationState{}}
		local.handleComposeAttachmentUpload(w, r)
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	})
	local := &Handler{auth: h.auth, blobStore: h.blobStore}
	private("GET /compose/attachments/{id}/preview", local.handleComposeAttachmentPreview)
	private("DELETE /compose/attachments/{id}", local.handleComposeAttachmentDelete)
}
