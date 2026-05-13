package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	mail "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const (
	avatarBackfillBatchSize     = 100
	avatarBackfillWorkers       = 4
	avatarBackfillStartInterval = 75 * time.Millisecond
	avatarBackfillRetryAttempts = 1
	avatarBackfillRetryDelay    = 250 * time.Millisecond
	avatarMissingTTL            = 24 * time.Hour
	avatarErrorRetryAfter       = 6 * time.Hour
)

type avatarBackfillResult struct {
	found bool
	err   error
}

func (h *Handler) StartAvatarBackfill(ctx context.Context) {
	go func() {
		h.startAvatarBackfill(ctx, false)

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.startAvatarBackfill(ctx, false)
			}
		}
	}()
}

func (h *Handler) startAvatarBackfill(ctx context.Context, force bool) bool {
	startedAt := time.Now()
	mode := "scheduled"
	if force {
		mode = "manual"
	}

	h.avatarBackfillMu.Lock()
	if h.avatarBackfillState.InProgress {
		h.avatarBackfillMu.Unlock()
		log.Printf("avatar: backfill %s request skipped because a run is already in progress", mode)
		return false
	}
	runCtx, cancel := context.WithCancel(ctx)
	h.avatarBackfillRunID++
	runID := h.avatarBackfillRunID
	h.avatarBackfillState = models.AvatarBackfillState{InProgress: true, Mode: mode, StartedAt: startedAt}
	h.avatarBackfillCancel = cancel
	h.avatarBackfillMu.Unlock()

	go h.runAvatarBackfill(runCtx, runID, force, startedAt, mode)
	return true
}

func (h *Handler) runAvatarBackfill(ctx context.Context, runID int64, force bool, startedAt time.Time, mode string) {
	log.Printf("avatar: %s backfill worker started", mode)
	defer h.clearAvatarBackfillCancel(runID)
	if force {
		h.avatar.ClearCache()
	}

	if _, err := h.db.EnsureSenderAvatarCandidates(ctx); err != nil {
		log.Printf("avatar: candidate scan failed: %v", err)
		state := finishAvatarBackfillCanceled(models.AvatarBackfillState{InProgress: true, Mode: mode, StartedAt: startedAt}, err)
		h.setAvatarBackfillState(state)
		return
	}

	stats, err := h.db.GetSenderAvatarStats(ctx)
	if err != nil {
		log.Printf("avatar: status count failed: %v", err)
		state := finishAvatarBackfillCanceled(models.AvatarBackfillState{InProgress: true, Mode: mode, StartedAt: startedAt}, err)
		h.setAvatarBackfillState(state)
		return
	}

	total := stats.Due
	if force {
		total = stats.Total
	}
	state := models.AvatarBackfillState{InProgress: true, Mode: mode, Total: total, StartedAt: startedAt}
	h.setAvatarBackfillState(state)

	offset := 0
	for {
		if err := ctx.Err(); err != nil {
			state = finishAvatarBackfillCanceled(state, err)
			h.setAvatarBackfillState(state)
			return
		}

		var candidates []storage.SenderAvatarCandidate
		if force {
			candidates, err = h.db.GetAllSenderAvatarCandidates(ctx, avatarBackfillBatchSize, offset)
		} else {
			candidates, err = h.db.GetDueSenderAvatarCandidates(ctx, avatarBackfillBatchSize)
		}
		if err != nil {
			state = finishAvatarBackfillCanceled(state, err)
			h.setAvatarBackfillState(state)
			log.Printf("avatar: candidate load failed: %v", err)
			return
		}
		if len(candidates) == 0 {
			break
		}
		if force {
			offset += len(candidates)
		}

		batchErr := h.runAvatarBackfillBatch(ctx, candidates, func(found bool, err error) {
			state.Processed++
			if err != nil {
				state.Errors++
				state.LastError = err.Error()
			} else if found {
				state.Found++
			} else {
				state.Missing++
			}
			h.setAvatarBackfillState(state)
		})
		if batchErr != nil {
			state = finishAvatarBackfillCanceled(state, batchErr)
			h.setAvatarBackfillState(state)
			return
		}
	}

	state.InProgress = false
	state.FinishedAt = time.Now()
	h.setAvatarBackfillState(state)
	log.Printf("avatar: %s backfill worker finished processed=%d found=%d missing=%d errors=%d", mode, state.Processed, state.Found, state.Missing, state.Errors)
}

func finishAvatarBackfillCanceled(state models.AvatarBackfillState, err error) models.AvatarBackfillState {
	state.InProgress = false
	state.CancelRequested = false
	state.FinishedAt = time.Now()
	if errors.Is(err, context.Canceled) {
		state.Canceled = true
		state.LastError = ""
		return state
	}
	state.LastError = err.Error()
	return state
}

func (h *Handler) runAvatarBackfillBatch(ctx context.Context, candidates []storage.SenderAvatarCandidate, handle func(found bool, err error)) error {
	if len(candidates) == 0 {
		return nil
	}
	workerCount := avatarBackfillWorkers
	if workerCount > len(candidates) {
		workerCount = len(candidates)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	jobs := make(chan storage.SenderAvatarCandidate)
	results := make(chan avatarBackfillResult)
	throttle := time.NewTicker(avatarBackfillStartInterval)
	defer throttle.Stop()

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				if err := waitForAvatarBackfillSlot(ctx, throttle); err != nil {
					return
				}
				_, found, err := h.fetchAndPersistAvatar(ctx, candidate.EmailHash, candidate.Email)
				results <- avatarBackfillResult{found: found, err: err}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, candidate := range candidates {
			select {
			case <-ctx.Done():
				return
			case jobs <- candidate:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		if ctx.Err() != nil || errors.Is(result.err, context.Canceled) {
			continue
		}
		handle(result.found, result.err)
	}
	return ctx.Err()
}

func waitForAvatarBackfillSlot(ctx context.Context, throttle *time.Ticker) error {
	if avatarBackfillStartInterval <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-throttle.C:
		return nil
	}
}

type avatarProviderSpec struct {
	name           string
	resolve        func(context.Context, *Handler, string, string) (avatarresolver.Image, bool, error)
	skip           func(string) (bool, string)
	missingMessage func(string, string) string
	errorMessage   func(string, string, error) string
}

func avatarProviderSpecs() []avatarProviderSpec {
	return []avatarProviderSpec{
		{
			name: "gravatar",
			resolve: func(ctx context.Context, h *Handler, hash, _ string) (avatarresolver.Image, bool, error) {
				return h.avatar.ResolveGravatar(ctx, hash)
			},
			missingMessage: func(hash, _ string) string { return gravatarMissingAttemptMessage(hash) },
			errorMessage:   func(hash, _ string, err error) string { return gravatarErrorAttemptMessage(hash, err) },
		},
		{
			name: "libravatar",
			resolve: func(ctx context.Context, h *Handler, hash, _ string) (avatarresolver.Image, bool, error) {
				return h.avatar.ResolveLibravatar(ctx, hash)
			},
			missingMessage: func(hash, _ string) string { return libravatarMissingAttemptMessage(hash) },
			errorMessage:   func(hash, _ string, err error) string { return libravatarErrorAttemptMessage(hash, err) },
		},
		{
			name: "bimi",
			skip: func(email string) (bool, string) {
				domain := avatarresolver.EmailDomain(email)
				if domain == "" || avatarresolver.IsPublicMailboxDomain(domain) {
					return true, bimiSkippedAttemptMessage(domain)
				}
				return false, ""
			},
			resolve: func(ctx context.Context, h *Handler, _, email string) (avatarresolver.Image, bool, error) {
				return h.avatar.ResolveBIMI(ctx, email)
			},
			missingMessage: func(_, email string) string { return bimiMissingAttemptMessage(avatarresolver.EmailDomain(email)) },
			errorMessage: func(_, email string, err error) string {
				return bimiErrorAttemptMessage(avatarresolver.EmailDomain(email), err)
			},
		},
	}
}

func avatarProviderNames() []string {
	providers := avatarProviderSpecs()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.name)
	}
	return names
}

func (h *Handler) fetchAndPersistAvatar(ctx context.Context, hash, email string) (avatarresolver.Image, bool, error) {
	providerStatuses := map[string]string{
		"gravatar":   "unchecked",
		"libravatar": "unchecked",
		"bimi":       "unchecked",
	}

	for _, provider := range avatarProviderSpecs() {
		if provider.skip != nil {
			skipped, message := provider.skip(email)
			if skipped {
				providerStatuses[provider.name] = "skipped"
				_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "skipped", message)
				continue
			}
		}

		image, found, err := resolveAvatarWithRetry(ctx, func(ctx context.Context) (avatarresolver.Image, bool, error) {
			return provider.resolve(ctx, h, hash, email)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return avatarresolver.Image{}, false, err
			}
			providerStatuses[provider.name] = "error"
			_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "error", provider.errorMessage(hash, email, err))
			_ = h.db.SaveSenderAvatarError(ctx, hash, email, provider.name, err.Error(), time.Now().Add(avatarErrorRetryAfter), avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi"))
			return avatarresolver.Image{}, false, err
		}
		if found {
			providerStatuses[provider.name] = "found"
			_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "found", avatarFoundAttemptMessage(image))
			return h.persistFoundAvatar(ctx, hash, email, image, avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi"))
		}

		providerStatuses[provider.name] = "missing"
		_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "missing", provider.missingMessage(hash, email))
	}

	if err := h.db.SaveSenderAvatarMissing(ctx, hash, email, "none", time.Now().Add(avatarMissingTTL), avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi")); err != nil {
		return avatarresolver.Image{}, false, err
	}
	return avatarresolver.Image{}, false, nil
}

func avatarStatus(statuses map[string]string, provider string) string {
	if status := statuses[provider]; status != "" {
		return status
	}
	return "unchecked"
}

func resolveAvatarWithRetry(ctx context.Context, resolve func(context.Context) (avatarresolver.Image, bool, error)) (avatarresolver.Image, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= avatarBackfillRetryAttempts; attempt++ {
		image, found, err := resolve(ctx)
		if err == nil {
			return image, found, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return avatarresolver.Image{}, false, ctxErr
		}
		lastErr = err
		if attempt == avatarBackfillRetryAttempts {
			break
		}

		timer := time.NewTimer(avatarBackfillRetryDelay * time.Duration(attempt+1))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return avatarresolver.Image{}, false, ctx.Err()
		case <-timer.C:
		}
	}
	return avatarresolver.Image{}, false, lastErr
}

func avatarFoundAttemptMessage(image avatarresolver.Image) string {
	detail := fmt.Sprintf("GET %s -> 200; content_type=%s; bytes=%d", avatarSourceURL(image), avatarContentType(image.ContentType), len(image.Data))
	if name := avatarSourceFile(image.SourceURL); name != "" {
		detail += "; file=" + name
	}
	if !image.ExpiresAt.IsZero() {
		detail += "; expires_at=" + image.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return detail
}

func gravatarMissingAttemptMessage(hash string) string {
	return fmt.Sprintf("GET %s -> 404; default=404; no Gravatar image", gravatarSourceURL(hash))
}

func gravatarErrorAttemptMessage(hash string, err error) string {
	return fmt.Sprintf("GET %s failed: %v", gravatarSourceURL(hash), err)
}

func libravatarMissingAttemptMessage(hash string) string {
	return fmt.Sprintf("GET %s -> 404; default=404; no Libravatar image", libravatarSourceURL(hash))
}

func libravatarErrorAttemptMessage(hash string, err error) string {
	return fmt.Sprintf("GET %s failed: %v", libravatarSourceURL(hash), err)
}

func bimiSkippedAttemptMessage(domain string) string {
	if domain == "" {
		return "Skipped BIMI lookup: sender email has no registrable domain"
	}
	return fmt.Sprintf("Skipped BIMI lookup for default._bimi.%s: public mailbox domain", domain)
}

func bimiMissingAttemptMessage(domain string) string {
	return fmt.Sprintf("TXT default._bimi.%s -> no usable BIMI logo URL", domain)
}

func bimiErrorAttemptMessage(domain string, err error) string {
	return fmt.Sprintf("BIMI lookup/fetch for default._bimi.%s failed: %v", domain, err)
}

func avatarSourceURL(image avatarresolver.Image) string {
	if image.SourceURL != "" {
		return image.SourceURL
	}
	if image.Source == "gravatar" {
		return "https://www.gravatar.com/avatar/<hash>?s=96&d=404&r=pg"
	}
	return image.Source
}

func gravatarSourceURL(hash string) string {
	return fmt.Sprintf("https://www.gravatar.com/avatar/%s?s=96&d=404&r=pg", hash)
}

func libravatarSourceURL(hash string) string {
	return fmt.Sprintf("https://seccdn.libravatar.org/avatar/%s?s=96&d=404", hash)
}

func avatarContentType(contentType string) string {
	if contentType == "" {
		return "unknown"
	}
	return contentType
}

func avatarSourceFile(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	name := path.Base(parsed.Path)
	if name == "." || name == "/" {
		return ""
	}
	return name
}

func (h *Handler) persistFoundAvatar(ctx context.Context, hash, email string, image avatarresolver.Image, gravatarStatus, bimiStatus string) (avatarresolver.Image, bool, error) {
	expiresAt := image.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(7 * 24 * time.Hour)
	}
	image.ExpiresAt = expiresAt
	storagePath, err := h.blobStore.StoreAvatar(hash, image.ContentType, image.Data)
	if err != nil {
		return avatarresolver.Image{}, false, err
	}
	if err := h.db.SaveSenderAvatarFound(ctx, hash, email, image.Source, image.ContentType, storagePath, nil, expiresAt, gravatarStatus, bimiStatus); err != nil {
		return avatarresolver.Image{}, false, err
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventAvatarUpdated, AvatarHash: hash, AvatarURL: storage.SenderAvatarURL(hash, expiresAt)})
	}
	return image, true, nil
}

func (h *Handler) handleAvatarStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.avatarStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get avatar status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (h *Handler) handleAvatarImage(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !avatarresolver.IsGravatarHash(hash) {
		http.NotFound(w, r)
		return
	}
	rec, err := h.db.GetSenderAvatarByHash(r.Context(), hash)
	if err != nil {
		http.Error(w, "failed to load avatar", http.StatusInternalServerError)
		return
	}
	if rec == nil || rec.Status != "found" || (rec.ExpiresAtValid && time.Now().After(rec.ExpiresAt)) {
		http.NotFound(w, r)
		return
	}

	data := rec.ImageData
	if rec.StoragePath != "" {
		fileData, err := h.blobStore.ReadAvatar(rec.StoragePath)
		if err != nil && len(data) == 0 {
			http.NotFound(w, r)
			return
		}
		if err == nil {
			data = fileData
		}
	}
	if len(data) == 0 {
		http.NotFound(w, r)
		return
	}

	contentType := rec.ContentType
	if contentType == "" {
		contentType = "image/jpeg"
	}
	etag := fmt.Sprintf(`"%s-%d-%d"`, rec.EmailHash, rec.ExpiresAt.Unix(), len(data))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isSVGAvatarContentType(contentType) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'none'; media-src 'none'; object-src 'none'; script-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	w.Header().Set("ETag", etag)
	if rec.ExpiresAtValid {
		w.Header().Set("Expires", rec.ExpiresAt.UTC().Format(http.TimeFormat))
	}
	_, _ = w.Write(data)
}

func isSVGAvatarContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return contentType == "image/svg+xml" || contentType == "application/svg+xml"
}

func (h *Handler) handleAvatarAttempts(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}
	errorsOnly := r.URL.Query().Get("kind") == "errors"
	logs, total, err := h.db.GetSenderAvatarAttemptLogs(r.Context(), storage.SenderAvatarAttemptLogFilter{
		ErrorsOnly: errorsOnly,
		Query:      r.URL.Query().Get("q"),
		Provider:   r.URL.Query().Get("provider"),
		Status:     r.URL.Query().Get("status"),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		http.Error(w, "failed to get avatar attempt logs", http.StatusInternalServerError)
		return
	}
	items := make([]models.AvatarAttemptLog, 0, len(logs))
	for _, entry := range logs {
		items = append(items, models.AvatarAttemptLog{
			Email:     entry.Email,
			Provider:  entry.Provider,
			Status:    entry.Status,
			Message:   entry.Message,
			CreatedAt: entry.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items":       items,
		"total_count": total,
		"next_offset": offset + len(items),
		"has_more":    offset+len(items) < total,
	})
}

func (h *Handler) handleAvatarSenders(w http.ResponseWriter, r *http.Request) {
	limit := 80
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}
	rows, total, err := h.db.GetSenderAvatarRows(r.Context(), storage.SenderAvatarRowFilter{
		Query:      r.URL.Query().Get("q"),
		Status:     r.URL.Query().Get("status"),
		Source:     r.URL.Query().Get("source"),
		Provider:   r.URL.Query().Get("provider"),
		ErrorsOnly: r.URL.Query().Get("errors") == "true",
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		http.Error(w, "failed to get avatar senders", http.StatusInternalServerError)
		return
	}
	providers, err := h.db.GetAvatarProviderNames(r.Context())
	if err != nil {
		http.Error(w, "failed to get avatar providers", http.StatusInternalServerError)
		return
	}
	items := make([]models.AvatarSenderRow, 0, len(rows))
	for _, row := range rows {
		item := models.AvatarSenderRow{
			Email:     row.Email,
			EmailHash: row.EmailHash,
			InUse: models.AvatarInUse{
				Status: row.Status,
				Source: row.Source,
				Error:  row.Error,
			},
			Status:    row.Status,
			Source:    row.Source,
			Error:     row.Error,
			UpdatedAt: row.UpdatedAt,
		}
		if row.Status == "found" && (row.StoragePath != "" || len(row.ImageData) > 0) {
			item.AvatarURL = storage.SenderAvatarURL(row.EmailHash, row.ExpiresAt)
			item.InUse.AvatarURL = item.AvatarURL
		}
		if row.FetchedAtValid {
			item.FetchedAt = row.FetchedAt
			item.InUse.FetchedAt = row.FetchedAt
		}
		if row.ExpiresAtValid {
			item.ExpiresAt = row.ExpiresAt
			item.InUse.ExpiresAt = row.ExpiresAt
		}
		if row.NextRetryAtValid {
			item.NextRetryAt = row.NextRetryAt
			item.InUse.NextRetryAt = row.NextRetryAt
		}
		for _, provider := range row.Providers {
			state := models.AvatarProviderState{Provider: provider.Provider, Status: provider.Status, Message: provider.Message}
			if provider.Checked {
				state.CheckedAt = provider.CheckedAt
			}
			item.Providers = append(item.Providers, state)
		}
		items = append(items, item)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items":       items,
		"providers":   providers,
		"total_count": total,
		"next_offset": offset + len(items),
		"has_more":    offset+len(items) < total,
	})
}

func (h *Handler) handleRecheckAvatarSender(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	rec, err := h.db.GetSenderAvatarByHash(r.Context(), hash)
	if err != nil || rec == nil || rec.Email == "" {
		http.NotFound(w, r)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _, _ = h.fetchAndPersistAvatar(ctx, rec.EmailHash, rec.Email)
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
}

func (h *Handler) handleForceAvatarBackfill(w http.ResponseWriter, r *http.Request) {
	started := h.startAvatarBackfill(context.WithoutCancel(r.Context()), true)
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		if !started {
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"started": started})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleCancelAvatarBackfill(w http.ResponseWriter, r *http.Request) {
	canceled := h.cancelAvatarBackfill()
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		if !canceled {
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"canceled": canceled})
		return
	}
	http.Redirect(w, r, "/admin/avatars/", http.StatusSeeOther)
}

func (h *Handler) avatarStatus(ctx context.Context) (models.AvatarStatus, error) {
	stats, err := h.db.GetSenderAvatarStats(ctx)
	if err != nil {
		return models.AvatarStatus{}, err
	}
	recent, err := h.db.GetRecentSenderAvatarAttemptLogs(ctx, 50)
	if err != nil {
		return models.AvatarStatus{}, err
	}
	recentErrors, err := h.db.GetRecentSenderAvatarErrorLogs(ctx, 50)
	if err != nil {
		return models.AvatarStatus{}, err
	}
	recentAttempts := make([]models.AvatarAttemptLog, 0, len(recent))
	for _, entry := range recent {
		recentAttempts = append(recentAttempts, models.AvatarAttemptLog{
			Email:     entry.Email,
			Provider:  entry.Provider,
			Status:    entry.Status,
			Message:   entry.Message,
			CreatedAt: entry.CreatedAt,
		})
	}
	recentErrorAttempts := make([]models.AvatarAttemptLog, 0, len(recentErrors))
	for _, entry := range recentErrors {
		recentErrorAttempts = append(recentErrorAttempts, models.AvatarAttemptLog{
			Email:     entry.Email,
			Provider:  entry.Provider,
			Status:    entry.Status,
			Message:   entry.Message,
			CreatedAt: entry.CreatedAt,
		})
	}
	return models.AvatarStatus{
		Backfill: h.getAvatarBackfillState(),
		Cache: models.AvatarCacheStats{
			Total:           stats.Total,
			Pending:         stats.Pending,
			Found:           stats.Found,
			Missing:         stats.Missing,
			Error:           stats.Error,
			Due:             stats.Due,
			GravatarChecked: stats.GravatarChecked,
			GravatarFound:   stats.GravatarFound,
			GravatarMissing: stats.GravatarMissing,
			GravatarError:   stats.GravatarError,
			BIMIChecked:     stats.BIMIChecked,
			BIMIFound:       stats.BIMIFound,
			BIMIMissing:     stats.BIMIMissing,
			BIMIError:       stats.BIMIError,
			BIMISkipped:     stats.BIMISkipped,
			OtherFound:      stats.OtherFound,
			ProviderStats:   avatarProviderStats(stats.ProviderStats),
		},
		RecentAttempts: recentAttempts,
		RecentErrors:   recentErrorAttempts,
	}, nil
}

func avatarProviderStats(stats []storage.SenderAvatarProviderStats) []models.AvatarProviderStats {
	byProvider := make(map[string]models.AvatarProviderStats, len(stats))
	for _, stat := range stats {
		byProvider[stat.Provider] = models.AvatarProviderStats{
			Provider: stat.Provider,
			InUse:    stat.InUse,
			Checked:  stat.Checked,
			Found:    stat.Found,
			Missing:  stat.Missing,
			Skipped:  stat.Skipped,
			Error:    stat.Error,
		}
	}

	ordered := make([]models.AvatarProviderStats, 0, len(byProvider)+len(avatarProviderNames()))
	seen := map[string]struct{}{}
	for _, provider := range avatarProviderNames() {
		stat := byProvider[provider]
		stat.Provider = provider
		ordered = append(ordered, stat)
		seen[provider] = struct{}{}
	}
	for _, stat := range stats {
		if _, ok := seen[stat.Provider]; ok {
			continue
		}
		ordered = append(ordered, byProvider[stat.Provider])
		seen[stat.Provider] = struct{}{}
	}
	return ordered
}

func (h *Handler) setAvatarBackfillState(state models.AvatarBackfillState) {
	h.avatarBackfillMu.Lock()
	h.avatarBackfillState = state
	h.avatarBackfillMu.Unlock()
	h.publishAvatarBackfillState(state)
}

func (h *Handler) publishAvatarBackfillState(state models.AvatarBackfillState) {
	if h.syncer != nil {
		status := "idle"
		if state.InProgress {
			status = state.Mode
			if state.CancelRequested {
				status = "canceling"
			}
		}
		h.syncer.Events().Publish(mail.Event{Type: mail.EventAvatarBackfill, Status: status, Current: state.Processed, Total: state.Total, Error: state.LastError})
	}
}

func (h *Handler) cancelAvatarBackfill() bool {
	h.avatarBackfillMu.Lock()
	if !h.avatarBackfillState.InProgress || h.avatarBackfillCancel == nil {
		h.avatarBackfillMu.Unlock()
		return false
	}
	h.avatarBackfillState.CancelRequested = true
	h.avatarBackfillCancel()
	state := h.avatarBackfillState
	h.avatarBackfillMu.Unlock()
	h.publishAvatarBackfillState(state)
	return true
}

func (h *Handler) clearAvatarBackfillCancel(runID int64) {
	h.avatarBackfillMu.Lock()
	if h.avatarBackfillRunID == runID {
		h.avatarBackfillCancel = nil
	}
	h.avatarBackfillMu.Unlock()
}

func (h *Handler) getAvatarBackfillState() models.AvatarBackfillState {
	h.avatarBackfillMu.RLock()
	defer h.avatarBackfillMu.RUnlock()
	return h.avatarBackfillState
}
