package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/translation"
)

func (h *Handler) handleUserTranslateMessage(w http.ResponseWriter, r *http.Request) {
	h.handleUserTranslation(w, r, false)
}

func (h *Handler) handleUserTranslatedEmailBody(w http.ResponseWriter, r *http.Request) {
	h.handleUserTranslation(w, r, true)
}

func (h *Handler) handleUserTranslation(w http.ResponseWriter, r *http.Request, document bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		if document {
			http.NotFound(w, r)
		} else {
			http.Error(w, "invalid message id", 400)
		}
		return
	}
	var request translateMessageRequest
	if document {
		request = translateMessageRequest{Provider: r.URL.Query().Get("provider"), SourceLanguage: r.URL.Query().Get("source_language"), TargetLanguage: r.URL.Query().Get("target_language")}
	} else if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil {
			// Empty and malformed optional preferences keep the previous defaults.
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "translation request is too large", 413)
				return
			}
		}
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	if err := h.userIMAP.EnsureBody(ctx, owner, id); err != nil {
		userAccountError(w, r, err)
		return
	}
	var result translation.Result
	var text, translatedHTML string
	remote := r.URL.Query().Get("remote") == "true"
	status := http.StatusServiceUnavailable
	err = h.userIMAP.RunMessageWork(ctx, owner, id, func(ctx context.Context, account string) error {
		var settings map[string]string
		var sourceHTML string
		var before *storage.MessageContentSnapshot
		err := h.userStorage.WithAccountForUser(ctx, owner, account, func(db *storage.DB) error {
			var err error
			before, err = db.GetMessageContentSnapshotForUser(ctx, owner, id)
			if err != nil {
				return err
			}
			settings = db.GetUISettings(ctx, owner)
			email, err := db.GetEmailByIDForUser(ctx, strconv.FormatInt(id, 10), owner)
			if err != nil {
				return err
			}
			if email == nil {
				return sql.ErrNoRows
			}
			body, err := db.GetEmailBodyForUser(ctx, strconv.FormatInt(id, 10), owner)
			if err != nil {
				return err
			}
			sourceHTML = string(body)
			text = strings.TrimSpace(email.TextBody)
			if text == "" {
				text = plainTextFromHTML(sourceHTML)
			}
			if text == "" {
				text = mailmessage.PreviewFromText(email.Preview)
				sourceHTML = ""
			}
			if text == "" {
				return fmt.Errorf("message body is empty")
			}
			return nil
		})
		if err != nil {
			return err
		}
		provider, ok := h.translationProvider(normalizeTranslationProvider(firstNonEmptyString(request.Provider, settings["translation_provider"])))
		if !ok {
			status = 400
			return fmt.Errorf("translation provider is not configured")
		}
		status = http.StatusBadGateway
		result, translatedHTML, err = translateMessageBody(ctx, provider, normalizeTranslationLanguage(request.SourceLanguage, "auto"), normalizeTranslationLanguage(firstNonEmptyString(request.TargetLanguage, settings["translation_target_language"]), "en"), text, sourceHTML)
		if err != nil {
			return err
		}
		status = http.StatusConflict
		// Re-read consent after network work, and deny results for replaced content.
		return h.userStorage.WithAccountForUser(ctx, owner, account, func(db *storage.DB) error {
			after, err := db.GetMessageContentSnapshotForUser(ctx, owner, id)
			if err != nil {
				return err
			}
			if *before != *after {
				return storage.ErrMessageContentChanged
			}
			if !remote {
				remote = db.IsRemoteContentAllowedForMessageForUser(ctx, id, owner) || db.IsRemoteContentAllowedForSenderForUser(ctx, after.SenderEmail, owner)
			}
			return nil
		})
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrAccountRoute) {
			userAccountError(w, r, err)
			return
		}
		if document {
			h.writeTranslatedEmailError(w, r.PathValue("id"), err.Error(), status)
		} else {
			http.Error(w, err.Error(), status)
		}
		return
	}
	if document {
		h.writeTranslatedBody(w, r, result, translatedHTML, remote)
		return
	}
	translatedText := result.Text
	if translatedHTML != "" {
		translatedText = plainTextFromHTML(translatedHTML)
	}
	h.writeTranslationResult(w, result, text, translatedText, translatedHTML)
}
