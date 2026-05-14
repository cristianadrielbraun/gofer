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
	avatarBackfillWorkers       = 32
	avatarBackfillStartInterval = 75 * time.Millisecond
	avatarBackfillRetryAttempts = 1
	avatarBackfillRetryDelay    = 250 * time.Millisecond
	avatarBackfillWatchInterval = 10 * time.Second
	avatarSlowCheckLogAfter     = 10 * time.Second
	avatarDomainFirstThreshold  = 5
	avatarWarmupQueueSize       = 200
	avatarWarmupWorkers         = 2
	avatarWarmupRequestLimit    = 50
	avatarWarmupProviderDelay   = 250 * time.Millisecond
	avatarWarmupAttemptWindow   = 7 * 24 * time.Hour
	avatarWarmupAttemptLimit    = 12
	avatarMissingTTL            = 24 * time.Hour
	avatarErrorRetryAfter       = 6 * time.Hour
)

type avatarWarmupRequest struct {
	Emails []string `json:"emails"`
}

type avatarBackfillResult struct {
	found    bool
	outcomes []avatarProviderOutcome
	err      error
}

type avatarProviderOutcome struct {
	provider string
	status   string
}

type avatarActiveCheck struct {
	email    string
	domain   string
	provider string
	started  time.Time
	updated  time.Time
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

func (h *Handler) startAvatarWarmupWorkers() {
	for i := 0; i < avatarWarmupWorkers; i++ {
		go func() {
			throttle := time.NewTicker(avatarWarmupProviderDelay)
			defer throttle.Stop()
			for candidate := range h.avatarWarmupQueue {
				func() {
					defer h.clearAvatarWarmupQueued(candidate.EmailHash)
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					log.Printf("avatar: warmup started email=%s", candidate.Email)
					_, found, outcomes, err := h.fetchAndPersistAvatar(ctx, candidate.EmailHash, candidate.Email, throttle, 0, nil)
					if err != nil && !errors.Is(err, context.Canceled) {
						log.Printf("avatar: warmup failed email=%s outcomes=%s err=%v", candidate.Email, avatarOutcomeSummary(outcomes), err)
						return
					}
					log.Printf("avatar: warmup completed email=%s found=%v outcomes=%s", candidate.Email, found, avatarOutcomeSummary(outcomes))
				}()
			}
		}()
	}
}

func (h *Handler) clearAvatarWarmupQueued(hash string) {
	h.avatarWarmupMu.Lock()
	delete(h.avatarWarmupQueued, strings.ToLower(strings.TrimSpace(hash)))
	h.avatarWarmupMu.Unlock()
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
	h.avatarBackfillState = models.AvatarBackfillState{InProgress: true, Mode: mode, StartedAt: startedAt, ProviderStats: emptyAvatarProviderStats()}
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
	domainCounts, err := h.db.GetSenderAvatarDomainCounts(ctx)
	if err != nil {
		log.Printf("avatar: domain count load failed: %v", err)
		state := finishAvatarBackfillCanceled(models.AvatarBackfillState{InProgress: true, Mode: mode, StartedAt: startedAt}, err)
		h.setAvatarBackfillState(state)
		return
	}

	total := stats.Due
	if force {
		total = stats.Total
	}
	state := models.AvatarBackfillState{InProgress: true, Mode: mode, Total: total, StartedAt: startedAt, ProviderStats: emptyAvatarProviderStats()}
	h.setAvatarBackfillState(state)

	handleResult := func(found bool, outcomes []avatarProviderOutcome, err error) {
		state.Processed++
		if state.Total < state.Processed {
			state.Total = state.Processed
		}
		state.ProviderStats = addAvatarProviderOutcomes(state.ProviderStats, outcomes)
		if err != nil {
			state.Errors++
			state.LastError = err.Error()
		} else if found {
			state.Found++
		} else {
			state.Missing++
		}
		h.setAvatarBackfillState(state)
	}

	if force {
		if err := h.runAvatarBackfillFull(ctx, domainCounts, handleResult); err != nil {
			state = finishAvatarBackfillCanceled(state, err)
			h.setAvatarBackfillState(state)
			return
		}
		state.InProgress = false
		state.FinishedAt = time.Now()
		h.setAvatarBackfillState(state)
		log.Printf("avatar: %s backfill worker finished processed=%d found=%d missing=%d errors=%d", mode, state.Processed, state.Found, state.Missing, state.Errors)
		return
	}

	for {
		if err := ctx.Err(); err != nil {
			state = finishAvatarBackfillCanceled(state, err)
			h.setAvatarBackfillState(state)
			return
		}

		candidates, err := h.db.GetDueSenderAvatarCandidates(ctx, avatarBackfillBatchSize)
		if err != nil {
			state = finishAvatarBackfillCanceled(state, err)
			h.setAvatarBackfillState(state)
			log.Printf("avatar: candidate load failed: %v", err)
			return
		}
		if len(candidates) == 0 {
			break
		}
		if state.Total < state.Processed+len(candidates) {
			state.Total = state.Processed + len(candidates)
			h.setAvatarBackfillState(state)
		}

		batchErr := h.runAvatarBackfillBatch(ctx, candidates, domainCounts, handleResult)
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

func (h *Handler) runAvatarBackfillBatch(ctx context.Context, candidates []storage.SenderAvatarCandidate, domainCounts map[string]int, handle func(found bool, outcomes []avatarProviderOutcome, err error)) error {
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
	return h.runAvatarBackfillWorkers(ctx, workerCount, domainCounts, func(ctx context.Context, jobs chan<- storage.SenderAvatarCandidate) error {
		for _, candidate := range candidates {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case jobs <- candidate:
			}
		}
		return ctx.Err()
	}, handle)
}

func (h *Handler) runAvatarBackfillFull(ctx context.Context, domainCounts map[string]int, handle func(found bool, outcomes []avatarProviderOutcome, err error)) error {
	return h.runAvatarBackfillWorkers(ctx, avatarBackfillWorkers, domainCounts, func(ctx context.Context, jobs chan<- storage.SenderAvatarCandidate) error {
		offset := 0
		for {
			candidates, err := h.db.GetAllSenderAvatarCandidates(ctx, avatarBackfillBatchSize, offset)
			if err != nil {
				return err
			}
			if len(candidates) == 0 {
				return ctx.Err()
			}
			offset += len(candidates)
			for _, candidate := range candidates {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case jobs <- candidate:
				}
			}
		}
	}, handle)
}

func (h *Handler) runAvatarBackfillWorkers(ctx context.Context, workerCount int, domainCounts map[string]int, produce func(context.Context, chan<- storage.SenderAvatarCandidate) error, handle func(found bool, outcomes []avatarProviderOutcome, err error)) error {
	if workerCount < 1 {
		workerCount = 1
	}

	jobs := make(chan storage.SenderAvatarCandidate)
	results := make(chan avatarBackfillResult)
	producerErr := make(chan error, 1)
	throttle := time.NewTicker(avatarBackfillStartInterval)
	defer throttle.Stop()
	active := map[string]avatarActiveCheck{}
	var activeMu sync.Mutex
	watchDone := make(chan struct{})
	defer close(watchDone)

	setActiveProvider := func(candidate storage.SenderAvatarCandidate, provider string) {
		activeMu.Lock()
		check := active[candidate.EmailHash]
		if check.started.IsZero() {
			check.started = time.Now()
		}
		check.email = candidate.Email
		check.domain = avatarresolver.EmailDomain(candidate.Email)
		check.provider = provider
		check.updated = time.Now()
		active[candidate.EmailHash] = check
		activeMu.Unlock()
	}
	clearActive := func(candidate storage.SenderAvatarCandidate, found bool, outcomes []avatarProviderOutcome, err error) {
		activeMu.Lock()
		check, ok := active[candidate.EmailHash]
		delete(active, candidate.EmailHash)
		activeMu.Unlock()
		if !ok {
			return
		}
		elapsed := time.Since(check.started)
		if elapsed >= avatarSlowCheckLogAfter {
			log.Printf("avatar: slow check completed email=%s domain=%s provider=%s elapsed=%s found=%v outcomes=%s err=%v", check.email, check.domain, check.provider, elapsed.Round(time.Millisecond), found, avatarOutcomeSummary(outcomes), err)
		}
	}

	go func() {
		watch := time.NewTicker(avatarBackfillWatchInterval)
		defer watch.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-ctx.Done():
				return
			case <-watch.C:
				h.logSlowAvatarChecks(active, &activeMu)
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				setActiveProvider(candidate, "queued")
				_, found, outcomes, err := h.fetchAndPersistAvatar(ctx, candidate.EmailHash, candidate.Email, throttle, avatarDomainSenderCount(candidate.Email, domainCounts), func(provider string) {
					setActiveProvider(candidate, provider)
				})
				clearActive(candidate, found, outcomes, err)
				results <- avatarBackfillResult{found: found, outcomes: outcomes, err: err}
			}
		}()
	}

	go func() {
		defer close(jobs)
		producerErr <- produce(ctx, jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		if ctx.Err() != nil || errors.Is(result.err, context.Canceled) {
			continue
		}
		handle(result.found, result.outcomes, result.err)
	}
	if err := <-producerErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

func avatarDomainSenderCount(email string, domainCounts map[string]int) int {
	domain := avatarresolver.EmailDomain(email)
	if domain == "" {
		return 0
	}
	if count := domainCounts[domain]; count > 0 {
		return count
	}
	return 1
}

func (h *Handler) logSlowAvatarChecks(active map[string]avatarActiveCheck, activeMu *sync.Mutex) {
	now := time.Now()
	slow := []avatarActiveCheck{}
	activeMu.Lock()
	for _, check := range active {
		if now.Sub(check.started) >= avatarSlowCheckLogAfter {
			slow = append(slow, check)
		}
	}
	activeMu.Unlock()
	if len(slow) == 0 {
		return
	}
	limit := len(slow)
	if limit > 5 {
		limit = 5
	}
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		check := slow[i]
		parts = append(parts, fmt.Sprintf("%s provider=%s domain=%s elapsed=%s", check.email, check.provider, check.domain, now.Sub(check.started).Round(time.Millisecond)))
	}
	log.Printf("avatar: backfill waiting on %d slow active checks: %s", len(slow), strings.Join(parts, "; "))
}

func avatarOutcomeSummary(outcomes []avatarProviderOutcome) string {
	if len(outcomes) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		parts = append(parts, outcome.provider+":"+outcome.status)
	}
	return strings.Join(parts, ",")
}

func waitForAvatarProviderSlot(ctx context.Context, throttle *time.Ticker) error {
	if throttle == nil || avatarBackfillStartInterval <= 0 {
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
	throttled      bool
	missingMessage func(string, string) string
	errorMessage   func(string, string, error) string
}

func avatarProviderSpecs() []avatarProviderSpec {
	return []avatarProviderSpec{
		{
			name:      "gravatar",
			throttled: true,
			resolve: func(ctx context.Context, h *Handler, hash, _ string) (avatarresolver.Image, bool, error) {
				return h.avatar.ResolveGravatar(ctx, hash)
			},
			missingMessage: func(hash, _ string) string { return gravatarMissingAttemptMessage(hash) },
			errorMessage:   func(hash, _ string, err error) string { return gravatarErrorAttemptMessage(hash, err) },
		},
		{
			name:      "libravatar",
			throttled: true,
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
		{
			name: "domain_icon",
			skip: func(email string) (bool, string) {
				domain := avatarresolver.EmailDomain(email)
				if domain == "" || avatarresolver.IsPublicMailboxDomain(domain) {
					return true, domainIconSkippedAttemptMessage(domain)
				}
				return false, ""
			},
			resolve: func(ctx context.Context, h *Handler, _, email string) (avatarresolver.Image, bool, error) {
				return h.avatar.ResolveDomainIcon(ctx, email)
			},
			missingMessage: func(_, email string) string {
				return domainIconMissingAttemptMessage(avatarresolver.EmailDomain(email))
			},
			errorMessage: func(_, email string, err error) string {
				return domainIconErrorAttemptMessage(avatarresolver.EmailDomain(email), err)
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

type avatarProviderPlan struct {
	providers []string
	skipped   map[string]string
}

func avatarProviderPlanForEmail(email string, domainSenderCount int) avatarProviderPlan {
	domain := avatarresolver.EmailDomain(email)
	if domain == "" {
		return avatarProviderPlan{providers: []string{"gravatar", "libravatar", "bimi", "domain_icon"}}
	}
	if avatarresolver.IsPublicMailboxDomain(domain) {
		return avatarProviderPlan{
			providers: []string{"gravatar", "libravatar"},
			skipped: map[string]string{
				"bimi":        bimiSkippedAttemptMessage(domain),
				"domain_icon": domainIconSkippedAttemptMessage(domain),
			},
		}
	}
	if isRoleLikeSender(email) {
		return avatarProviderPlan{
			providers: []string{"bimi", "domain_icon"},
			skipped: map[string]string{
				"gravatar":   gravatarSkippedAttemptMessage(email, "role-like sender; domain avatar preferred"),
				"libravatar": libravatarSkippedAttemptMessage(email, "role-like sender; domain avatar preferred"),
			},
		}
	}
	if domainSenderCount >= avatarDomainFirstThreshold {
		return avatarProviderPlan{
			providers: []string{"bimi", "domain_icon"},
			skipped: map[string]string{
				"gravatar":   gravatarSkippedAttemptMessage(email, "high-volume domain; domain avatar preferred"),
				"libravatar": libravatarSkippedAttemptMessage(email, "high-volume domain; domain avatar preferred"),
			},
		}
	}
	return avatarProviderPlan{providers: []string{"gravatar", "libravatar", "bimi", "domain_icon"}}
}

func isRoleLikeSender(email string) bool {
	local := emailLocalPart(email)
	if local == "" {
		return false
	}
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	compact := strings.NewReplacer(".", "", "-", "", "_", "").Replace(local)
	if compact == "noreply" || compact == "donotreply" {
		return true
	}
	roleNames := map[string]struct{}{
		"abuse": {}, "admin": {}, "billing": {}, "careers": {}, "contact": {}, "customerservice": {}, "events": {},
		"hello": {}, "help": {}, "info": {}, "jobs": {}, "legal": {}, "marketing": {}, "media": {}, "news": {},
		"newsletter": {}, "notifications": {}, "office": {}, "orders": {}, "press": {}, "privacy": {}, "recruiting": {},
		"report": {}, "reporting": {}, "reports": {}, "sales": {}, "security": {}, "service": {}, "support": {}, "team": {}, "webmaster": {},
	}
	if _, ok := roleNames[local]; ok {
		return true
	}
	for _, token := range strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '-' || r == '_' }) {
		if _, ok := roleNames[token]; ok {
			return true
		}
	}
	return false
}

func emailLocalPart(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return ""
	}
	return strings.TrimSpace(email[:at])
}

func avatarProviderSpecByName() map[string]avatarProviderSpec {
	providers := avatarProviderSpecs()
	byName := make(map[string]avatarProviderSpec, len(providers))
	for _, provider := range providers {
		byName[provider.name] = provider
	}
	return byName
}

func (h *Handler) fetchAndPersistAvatar(ctx context.Context, hash, email string, throttle *time.Ticker, domainSenderCount int, observeProvider func(string)) (avatarresolver.Image, bool, []avatarProviderOutcome, error) {
	providerStatuses := map[string]string{
		"gravatar":    "unchecked",
		"libravatar":  "unchecked",
		"bimi":        "unchecked",
		"domain_icon": "unchecked",
	}
	outcomes := []avatarProviderOutcome{}
	lastProviderErr := error(nil)
	lastErrorProvider := ""
	plan := avatarProviderPlanForEmail(email, domainSenderCount)
	for provider, message := range plan.skipped {
		providerStatuses[provider] = "skipped"
		outcomes = append(outcomes, avatarProviderOutcome{provider: provider, status: "skipped"})
		_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider, "skipped", message)
	}
	providersByName := avatarProviderSpecByName()

	for _, providerName := range plan.providers {
		provider, ok := providersByName[providerName]
		if !ok {
			continue
		}
		if observeProvider != nil {
			observeProvider(provider.name)
		}
		if provider.skip != nil {
			skipped, message := provider.skip(email)
			if skipped {
				providerStatuses[provider.name] = "skipped"
				outcomes = append(outcomes, avatarProviderOutcome{provider: provider.name, status: "skipped"})
				_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "skipped", message)
				continue
			}
		}

		if provider.throttled {
			if err := waitForAvatarProviderSlot(ctx, throttle); err != nil {
				return avatarresolver.Image{}, false, outcomes, err
			}
		}

		if provider.name == "domain_icon" {
			image, message, reused, err := h.reuseStoredDomainIconAvatar(ctx, hash, email, avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi"))
			if err != nil {
				providerStatuses[provider.name] = "error"
				outcomes = append(outcomes, avatarProviderOutcome{provider: provider.name, status: "error"})
				_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "error", provider.errorMessage(hash, email, err))
				lastProviderErr = err
				lastErrorProvider = provider.name
			}
			if reused {
				providerStatuses[provider.name] = "found"
				outcomes = append(outcomes, avatarProviderOutcome{provider: provider.name, status: "found"})
				_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "found", message)
				return image, true, outcomes, nil
			}
		}

		image, found, err := resolveAvatarWithRetry(ctx, func(ctx context.Context) (avatarresolver.Image, bool, error) {
			return provider.resolve(ctx, h, hash, email)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return avatarresolver.Image{}, false, outcomes, err
			}
			providerStatuses[provider.name] = "error"
			outcomes = append(outcomes, avatarProviderOutcome{provider: provider.name, status: "error"})
			_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "error", provider.errorMessage(hash, email, err))
			lastProviderErr = err
			lastErrorProvider = provider.name
			continue
		}
		if found {
			providerStatuses[provider.name] = "found"
			outcomes = append(outcomes, avatarProviderOutcome{provider: provider.name, status: "found"})
			_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "found", avatarFoundAttemptMessage(image))
			image, found, err = h.persistFoundAvatar(ctx, hash, email, image, avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi"))
			return image, found, outcomes, err
		}

		providerStatuses[provider.name] = "missing"
		outcomes = append(outcomes, avatarProviderOutcome{provider: provider.name, status: "missing"})
		_ = h.db.RecordSenderAvatarAttempt(ctx, hash, email, provider.name, "missing", provider.missingMessage(hash, email))
	}

	if lastProviderErr != nil {
		if err := h.db.SaveSenderAvatarError(ctx, hash, email, lastErrorProvider, lastProviderErr.Error(), time.Now().Add(avatarErrorRetryAfter), avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi")); err != nil {
			return avatarresolver.Image{}, false, outcomes, err
		}
		return avatarresolver.Image{}, false, outcomes, lastProviderErr
	}
	if err := h.db.SaveSenderAvatarMissing(ctx, hash, email, "none", time.Now().Add(avatarMissingTTL), avatarStatus(providerStatuses, "gravatar"), avatarStatus(providerStatuses, "bimi")); err != nil {
		return avatarresolver.Image{}, false, outcomes, err
	}
	return avatarresolver.Image{}, false, outcomes, nil
}

func (h *Handler) reuseStoredDomainIconAvatar(ctx context.Context, hash, email, gravatarStatus, bimiStatus string) (avatarresolver.Image, string, bool, error) {
	rec, err := h.db.GetReusableDomainIconAvatar(ctx, hash, email)
	if err != nil || rec == nil {
		return avatarresolver.Image{}, "", false, err
	}
	image := avatarresolver.Image{
		Data:        rec.ImageData,
		ContentType: rec.ContentType,
		ExpiresAt:   rec.ExpiresAt,
		Source:      "domain_icon",
		SourceURL:   storage.SenderAvatarURL(rec.EmailHash, rec.ExpiresAt),
	}
	if err := h.db.SaveSenderAvatarFound(ctx, hash, email, "domain_icon", rec.ContentType, rec.StoragePath, rec.ImageData, rec.ExpiresAt, gravatarStatus, bimiStatus); err != nil {
		return avatarresolver.Image{}, "", false, err
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventAvatarUpdated, AvatarHash: hash, AvatarURL: storage.SenderAvatarURL(hash, rec.ExpiresAt)})
	}
	return image, reusedDomainIconAttemptMessage(email, rec), true, nil
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

func gravatarSkippedAttemptMessage(email, reason string) string {
	return fmt.Sprintf("Skipped Gravatar lookup for %s: %s", strings.ToLower(strings.TrimSpace(email)), reason)
}

func gravatarErrorAttemptMessage(hash string, err error) string {
	return fmt.Sprintf("GET %s failed: %v", gravatarSourceURL(hash), err)
}

func libravatarMissingAttemptMessage(hash string) string {
	return fmt.Sprintf("GET %s -> 404; default=404; no Libravatar image", libravatarSourceURL(hash))
}

func libravatarSkippedAttemptMessage(email, reason string) string {
	return fmt.Sprintf("Skipped Libravatar lookup for %s: %s", strings.ToLower(strings.TrimSpace(email)), reason)
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

func domainIconSkippedAttemptMessage(domain string) string {
	if domain == "" {
		return "Skipped domain icon lookup: sender email has no registrable domain"
	}
	return fmt.Sprintf("Skipped domain icon lookup for %s: public mailbox domain", domain)
}

func domainIconMissingAttemptMessage(domain string) string {
	return fmt.Sprintf("GET https://%s/favicon.ico -> no usable domain icon", domain)
}

func domainIconErrorAttemptMessage(domain string, err error) string {
	return fmt.Sprintf("Domain icon lookup/fetch for %s failed: %v", domain, err)
}

func reusedDomainIconAttemptMessage(email string, rec *storage.SenderAvatarRecord) string {
	domain := avatarresolver.EmailDomain(email)
	detail := fmt.Sprintf("Reused stored domain icon for %s from %s; content_type=%s", domain, rec.EmailHash, avatarContentType(rec.ContentType))
	if rec.StoragePath != "" {
		detail += "; storage_path=" + rec.StoragePath
	}
	if !rec.ExpiresAt.IsZero() {
		detail += "; expires_at=" + rec.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return detail
}

func avatarSourceURL(image avatarresolver.Image) string {
	if image.SourceURL != "" {
		return image.SourceURL
	}
	if image.Source == "gravatar" {
		return "https://www.gravatar.com/avatar/<hash>?s=96&d=404&r=pg"
	}
	if image.Source == "libravatar" {
		return "https://seccdn.libravatar.org/avatar/<hash>?s=96&d=404"
	}
	if image.Source == "domain_icon" {
		return "https://<domain>/favicon.ico"
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

func (h *Handler) handleAvatarWarmup(w http.ResponseWriter, r *http.Request) {
	var req avatarWarmupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid warmup request", http.StatusBadRequest)
		return
	}
	queued := 0
	skipped := 0
	invalid := 0
	duplicate := 0
	notDue := 0
	notQueued := 0
	forced := 0
	capped := 0
	seen := map[string]struct{}{}
	for _, email := range req.Emails {
		if queued+skipped >= avatarWarmupRequestLimit {
			break
		}
		email = strings.ToLower(strings.TrimSpace(email))
		hash := avatarresolver.GravatarHash(email)
		if hash == "" {
			skipped++
			invalid++
			continue
		}
		if _, ok := seen[hash]; ok {
			skipped++
			duplicate++
			continue
		}
		seen[hash] = struct{}{}
		candidate, ok, forceRetry, attemptCapped, err := h.avatarWarmupCandidate(r.Context(), email, hash)
		if err != nil {
			http.Error(w, "failed to inspect avatar warmup candidates", http.StatusInternalServerError)
			return
		}
		if attemptCapped {
			skipped++
			capped++
			continue
		}
		if !ok {
			skipped++
			notDue++
			continue
		}
		if !h.enqueueAvatarWarmup(candidate) {
			skipped++
			notQueued++
			continue
		}
		if forceRetry {
			forced++
		}
		queued++
	}
	log.Printf("avatar: warmup request emails=%d queued=%d skipped=%d invalid=%d duplicate=%d not_due=%d not_queued=%d forced=%d capped=%d", len(req.Emails), queued, skipped, invalid, duplicate, notDue, notQueued, forced, capped)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"queued": queued, "skipped": skipped, "forced": forced, "capped": capped})
}

func (h *Handler) avatarWarmupCandidate(ctx context.Context, email, hash string) (storage.SenderAvatarCandidate, bool, bool, bool, error) {
	if err := h.db.UpsertSenderAvatarCandidate(ctx, email); err != nil {
		return storage.SenderAvatarCandidate{}, false, false, false, err
	}
	attempts, err := h.db.CountSenderAvatarAttemptsSince(ctx, hash, time.Now().Add(-avatarWarmupAttemptWindow))
	if err != nil {
		return storage.SenderAvatarCandidate{}, false, false, false, err
	}
	if attempts >= avatarWarmupAttemptLimit {
		return storage.SenderAvatarCandidate{}, false, false, true, nil
	}
	rec, err := h.db.GetSenderAvatarByHash(ctx, hash)
	if err != nil {
		return storage.SenderAvatarCandidate{}, false, false, false, err
	}
	if rec != nil && !senderAvatarRecordDue(*rec) {
		if rec.Status == "error" && h.allowAvatarWarmupForced(hash) {
			return storage.SenderAvatarCandidate{EmailHash: hash, Email: email}, true, true, false, nil
		}
		return storage.SenderAvatarCandidate{}, false, false, false, nil
	}
	return storage.SenderAvatarCandidate{EmailHash: hash, Email: email}, true, false, false, nil
}

func (h *Handler) allowAvatarWarmupForced(hash string) bool {
	hash = strings.ToLower(strings.TrimSpace(hash))
	now := time.Now()
	h.avatarWarmupMu.Lock()
	defer h.avatarWarmupMu.Unlock()
	until, ok := h.avatarWarmupForced[hash]
	if ok && now.Before(until) {
		return false
	}
	h.avatarWarmupForced[hash] = now.Add(avatarErrorRetryAfter)
	return true
}

func senderAvatarRecordDue(rec storage.SenderAvatarRecord) bool {
	now := time.Now()
	switch rec.Status {
	case "pending":
		return true
	case "found", "missing":
		return !rec.ExpiresAtValid || !now.Before(rec.ExpiresAt)
	case "error":
		return !rec.NextRetryAtValid || !now.Before(rec.NextRetryAt)
	default:
		return true
	}
}

func (h *Handler) enqueueAvatarWarmup(candidate storage.SenderAvatarCandidate) bool {
	h.avatarWarmupMu.Lock()
	if _, ok := h.avatarWarmupQueued[candidate.EmailHash]; ok {
		h.avatarWarmupMu.Unlock()
		return false
	}
	h.avatarWarmupQueued[candidate.EmailHash] = struct{}{}
	h.avatarWarmupMu.Unlock()

	select {
	case h.avatarWarmupQueue <- candidate:
		log.Printf("avatar: warmup queued email=%s", candidate.Email)
		return true
	default:
		h.clearAvatarWarmupQueued(candidate.EmailHash)
		return false
	}
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
	providers = orderedAvatarProviderNames(providers)
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
		_, _, _, _ = h.fetchAndPersistAvatar(ctx, rec.EmailHash, rec.Email, nil, 0, nil)
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

func emptyAvatarProviderStats() []models.AvatarProviderStats {
	stats := make([]models.AvatarProviderStats, 0, len(avatarProviderNames()))
	for _, provider := range avatarProviderNames() {
		stats = append(stats, models.AvatarProviderStats{Provider: provider})
	}
	return stats
}

func addAvatarProviderOutcomes(stats []models.AvatarProviderStats, outcomes []avatarProviderOutcome) []models.AvatarProviderStats {
	byProvider := make(map[string]int, len(stats)+len(outcomes))
	ordered := make([]models.AvatarProviderStats, 0, len(stats)+len(outcomes))
	for _, stat := range stats {
		stat.Provider = strings.ToLower(strings.TrimSpace(stat.Provider))
		if stat.Provider == "" {
			stat.Provider = "unknown"
		}
		ordered = append(ordered, stat)
		byProvider[stat.Provider] = len(ordered) - 1
	}
	for _, outcome := range outcomes {
		provider := strings.ToLower(strings.TrimSpace(outcome.provider))
		if provider == "" {
			provider = "unknown"
		}
		index, ok := byProvider[provider]
		if !ok {
			ordered = append(ordered, models.AvatarProviderStats{Provider: provider})
			index = len(ordered) - 1
			byProvider[provider] = index
		}
		switch strings.ToLower(strings.TrimSpace(outcome.status)) {
		case "found":
			ordered[index].Checked++
			ordered[index].Found++
		case "missing":
			ordered[index].Checked++
			ordered[index].Missing++
		case "error":
			ordered[index].Checked++
			ordered[index].Error++
		case "skipped":
			ordered[index].Skipped++
		}
	}
	return ordered
}

func orderedAvatarProviderNames(extra []string) []string {
	ordered := []string{}
	seen := map[string]struct{}{}
	for _, provider := range avatarProviderNames() {
		ordered = append(ordered, provider)
		seen[provider] = struct{}{}
	}
	for _, provider := range extra {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		ordered = append(ordered, provider)
		seen[provider] = struct{}{}
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
		h.syncer.Events().Publish(mail.Event{
			Type:    mail.EventAvatarBackfill,
			Status:  status,
			Current: state.Processed,
			Total:   state.Total,
			Error:   state.LastError,
			Payload: map[string]any{"backfill": state},
		})
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
	state := h.avatarBackfillState
	if len(state.ProviderStats) == 0 {
		state.ProviderStats = emptyAvatarProviderStats()
	}
	return state
}
