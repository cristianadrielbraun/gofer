package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// A rule run by hand is a job in the background: it counts every message in
// the folder that the rule would change, and once the owner confirms,
// changes them a batch at a time. Its progress reaches the owner's pages as
// events. Jobs live in memory; one cut short by a restart is run again, and
// what it already changed needs nothing the second time.
const (
	// At most this many runs work at once, across all users; others wait.
	mailRuleRunWorkers = 2
	// mailRuleRunEventEvery is how often a job tells about its progress.
	mailRuleRunEventEvery = 250 * time.Millisecond
)

type mailRuleRunJob struct {
	data    models.MailRuleRunData
	owner   string
	scope   storage.MailRuleRunScope
	matches []int64
	cancel  context.CancelFunc
	told    time.Time
}

// mailRuleRuns holds each owner's latest run of each rule.
type mailRuleRuns struct {
	mu    sync.Mutex
	jobs  map[string]*mailRuleRunJob
	slots chan struct{}
	// scanBatch is how many messages a step of counting looks at, and
	// pause the rest between two steps, which lets the owner's other work
	// and the queue of changes in.
	scanBatch int
	pause     time.Duration
}

func newMailRuleRuns() *mailRuleRuns {
	return &mailRuleRuns{jobs: map[string]*mailRuleRunJob{}, slots: make(chan struct{}, mailRuleRunWorkers),
		scanBatch: storage.MailRuleRunScanBatch, pause: 20 * time.Millisecond}
}

// job returns the owner's job by ID, or nil.
func (r *mailRuleRuns) job(owner, id string) *mailRuleRunJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	if job == nil || job.owner != owner {
		return nil
	}
	return job
}

// latest returns the owner's newest job for a rule, or nil.
func (r *mailRuleRuns) latest(owner, ruleID string) *mailRuleRunJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found *mailRuleRunJob
	for _, job := range r.jobs {
		if job.owner == owner && job.data.Rule.ID == ruleID && (found == nil || job.data.JobID > found.data.JobID) {
			found = job
		}
	}
	return found
}

func (r *mailRuleRuns) snapshot(job *mailRuleRunJob) models.MailRuleRunData {
	r.mu.Lock()
	defer r.mu.Unlock()
	return job.data
}

// add keeps a new job, in place of the owner's earlier jobs for the rule,
// which it stops.
func (r *mailRuleRuns) add(job *mailRuleRunJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, other := range r.jobs {
		if other.owner == job.owner && other.data.Rule.ID == job.data.Rule.ID {
			if other.cancel != nil {
				other.cancel()
			}
			delete(r.jobs, id)
		}
	}
	r.jobs[job.data.JobID] = job
}

// mailRuleRunContext is what jobs run under: until the owner's storage
// closes with per-user storage, or for as long as Gofer runs.
func (h *Handler) mailRuleRunContext() context.Context {
	if h.userStorageContext != nil {
		return h.userStorageContext
	}
	return context.Background()
}

func (h *Handler) mailRuleRunEvents() *mail.EventBus {
	if h.userIMAP != nil {
		return h.userIMAP.Events()
	}
	if h.syncer != nil {
		return h.syncer.Events()
	}
	return nil
}

// tell sends the job's progress to its owner's pages: right away when its
// state changed, otherwise at most every mailRuleRunEventEvery.
func (h *Handler) tellMailRuleRun(job *mailRuleRunJob, force bool) {
	runs := h.mailRuleRuns
	runs.mu.Lock()
	if !force && time.Since(job.told) < mailRuleRunEventEvery {
		runs.mu.Unlock()
		return
	}
	job.told = time.Now()
	d := job.data
	runs.mu.Unlock()
	events := h.mailRuleRunEvents()
	if events == nil {
		return
	}
	events.Publish(mail.Event{Type: mail.EventMailRuleRun, UserID: job.owner, Payload: map[string]any{
		"job": d.JobID, "seq": d.Seq, "rule": d.Rule.ID, "rule_name": d.Rule.Name, "state": d.State,
		"total": d.Total, "scanned": d.Scanned, "matches": d.Matches,
		"processed": d.Processed, "changed": d.Changed, "unchanged": d.Unchanged, "failed": d.Failed,
		"error": d.Error, "notice": views.MailRuleRunDoneNotice(d),
	}})
}

// update changes the job under the lock.
func (h *Handler) updateMailRuleRun(job *mailRuleRunJob, change func(*mailRuleRunJob)) {
	h.mailRuleRuns.mu.Lock()
	change(job)
	job.data.Seq++
	h.mailRuleRuns.mu.Unlock()
}

// withMailRuleStore runs fn on the owner's mail store, with allow saying
// which of their accounts may be changed, then sends the changes fn reports
// on their way and tells pages about the folders.
func (h *Handler) withMailRuleStore(ctx context.Context, owner string, fn func(db *storage.DB, allow func(string) bool) ([]storage.FolderRef, error)) error {
	if h.userStorage == nil {
		changed, err := fn(h.db, func(string) bool { return true })
		if len(changed) > 0 {
			h.signalMessageMutationWorker()
			for _, folder := range changed {
				h.publishMutation(folder.AccountID, folder.FolderID)
			}
		}
		return err
	}
	state := &userMessageMutationState{owner: owner, routing: h.userStorage, checked: map[string]bool{}, credentials: h.userCredentials != nil}
	var fnErr error
	err := h.userAccounts.WithUser(ctx, owner, func(accounts *config.AccountStore, db *storage.DB) error {
		state.accounts = accounts
		defer func() { state.accounts = nil }()
		local := &Handler{db: db, auth: h.auth, userMutationState: state}
		allow := func(accountID string) bool {
			return local.checkUserMessageMutation(ctx, &storage.MessageMutationInfo{AccountID: accountID}) == nil
		}
		var changed []storage.FolderRef
		changed, fnErr = fn(db, allow)
		for _, folder := range changed {
			local.publishMutation(folder.AccountID, folder.FolderID)
		}
		return nil
	})
	h.deliverUserMutations(ctx, state)
	if err != nil {
		return err
	}
	return fnErr
}

// waitMailRuleRunSlot waits for one of the workers, and returns its
// release.
func (h *Handler) waitMailRuleRunSlot(ctx context.Context) (func(), error) {
	select {
	case h.mailRuleRuns.slots <- struct{}{}:
		return func() { <-h.mailRuleRuns.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *Handler) restBetweenMailRuleBatches(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.mailRuleRuns.pause):
		return nil
	}
}

// startMailRuleCount starts counting what a rule would change in a folder.
func (h *Handler) startMailRuleCount(owner string, rule models.MailRule, scopeValue, scopeName string) *mailRuleRunJob {
	scope, scopeValue := mailRuleRunScope(scopeValue)
	ctx, cancel := context.WithCancel(h.mailRuleRunContext())
	job := &mailRuleRunJob{owner: owner, scope: scope, cancel: cancel, data: models.MailRuleRunData{
		Rule: rule, JobID: newMailJobID(), Scope: scopeValue, ScopeName: scopeName, State: models.MailRuleRunCounting,
	}}
	// IDs sort by when jobs started, so the newest is found.
	job.data.JobID = fmt.Sprintf("%016x", time.Now().UnixNano()) + job.data.JobID[:16]
	h.mailRuleRuns.add(job)
	go h.countMailRuleRun(ctx, job)
	return job
}

func (h *Handler) countMailRuleRun(ctx context.Context, job *mailRuleRunJob) {
	release, err := h.waitMailRuleRunSlot(ctx)
	if err != nil {
		return
	}
	defer release()
	var before int64
	for {
		var step storage.MailRuleScanStep
		err := h.withMailRuleStore(ctx, job.owner, func(db *storage.DB, allow func(string) bool) ([]storage.FolderRef, error) {
			var err error
			step, err = db.ScanMailRuleRun(ctx, job.owner, job.data.Rule.ID, job.scope, allow, before, h.mailRuleRuns.scanBatch)
			return nil, err
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.failMailRuleRun(job, "count", err)
			return
		}
		h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
			if before == 0 {
				job.data.Total = step.Total
			}
			job.data.Scanned += step.Scanned
			job.data.Total = max(job.data.Total, job.data.Scanned)
			job.matches = append(job.matches, step.Matches...)
			job.data.Matches = len(job.matches)
			if step.Done {
				job.data.State = models.MailRuleRunCounted
				job.data.Total = job.data.Scanned
				job.cancel = nil
			}
		})
		h.tellMailRuleRun(job, step.Done)
		if step.Done {
			return
		}
		before = step.Next
		if h.restBetweenMailRuleBatches(ctx) != nil {
			return
		}
	}
}

// startMailRuleRun changes what a counted job found.
func (h *Handler) startMailRuleRun(job *mailRuleRunJob) bool {
	ctx, cancel := context.WithCancel(h.mailRuleRunContext())
	started := false
	h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
		if job.data.State != models.MailRuleRunCounted || job.data.Matches == 0 {
			return
		}
		job.data.State = models.MailRuleRunRunning
		job.cancel = cancel
		started = true
	})
	if !started {
		cancel()
		return false
	}
	h.tellMailRuleRun(job, true)
	go h.applyMailRuleRun(ctx, job)
	return true
}

func (h *Handler) applyMailRuleRun(ctx context.Context, job *mailRuleRunJob) {
	changed := map[string]int{}
	defer func() {
		// What was changed is recorded however the run ended.
		if len(changed) > 0 {
			h.recordMailRuleRun(job, changed)
		}
	}()
	release, err := h.waitMailRuleRunSlot(ctx)
	if err != nil {
		h.stoppedMailRuleRun(job)
		return
	}
	defer release()
	matches := job.matches
	for start := 0; start < len(matches); start += storage.MailRuleRunApplyBatch {
		batch := matches[start:min(start+storage.MailRuleRunApplyBatch, len(matches))]
		var result storage.MailRuleRunResult
		err := h.withMailRuleStore(ctx, job.owner, func(db *storage.DB, allow func(string) bool) ([]storage.FolderRef, error) {
			var err error
			result, err = db.RunMailRuleNow(ctx, job.owner, job.data.Rule.ID, job.scope, batch, allow)
			return result.Folders, err
		})
		for accountID, n := range result.Changed {
			changed[accountID] += n
		}
		h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
			for _, n := range result.Changed {
				job.data.Changed += n
			}
			job.data.Unchanged += result.Unchanged
			job.data.Failed += result.Failed
			job.data.Processed = job.data.Changed + job.data.Unchanged
		})
		if ctx.Err() != nil {
			h.stoppedMailRuleRun(job)
			return
		}
		if err != nil {
			h.failMailRuleRun(job, "run", err)
			return
		}
		h.tellMailRuleRun(job, false)
		if h.restBetweenMailRuleBatches(ctx) != nil {
			h.stoppedMailRuleRun(job)
			return
		}
	}
	h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
		job.data.State = models.MailRuleRunDone
		job.data.Processed = job.data.Matches
		job.matches, job.cancel = nil, nil
	})
	h.tellMailRuleRun(job, true)
}

// recordMailRuleRun records in the rule's activity what the run changed in
// each account.
func (h *Handler) recordMailRuleRun(job *mailRuleRunJob, changed map[string]int) {
	d := h.mailRuleRuns.snapshot(job)
	// The job's own context may be canceled; the summary is still written.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.mailRuleRunContext()), time.Minute)
	defer cancel()
	err := h.withMailRuleStore(ctx, job.owner, func(db *storage.DB, _ func(string) bool) ([]storage.FolderRef, error) {
		for accountID, n := range changed {
			detail := fmt.Sprintf("Run by hand on %s: changed %s", d.ScopeName, views.MailRuleMessages(n))
			if err := db.RecordMailRuleRunSummary(ctx, job.owner, d.Rule.ID, accountID, detail, n); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		log.Printf("mail rule run %s: record: %v", d.JobID, err)
	}
}

func (h *Handler) stoppedMailRuleRun(job *mailRuleRunJob) {
	h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
		job.data.State = models.MailRuleRunStopped
		job.matches, job.cancel = nil, nil
	})
	h.tellMailRuleRun(job, true)
}

func (h *Handler) failMailRuleRun(job *mailRuleRunJob, what string, err error) {
	log.Printf("mail rule run %s: %s: %v", job.data.JobID, what, err)
	h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
		job.data.State = models.MailRuleRunError
		switch {
		case errors.Is(err, storage.ErrMailRuleRunFolder):
			job.data.Error = "No account this rule is for has that folder."
		case errors.Is(err, sql.ErrNoRows):
			job.data.Error = "The rule is gone."
		case what == "count":
			job.data.Error = "The folder could not be checked. Try again."
		default:
			job.data.Error = "The rule stopped before it got through every message. Run it again for the rest."
		}
		job.matches, job.cancel = nil, nil
	})
	h.tellMailRuleRun(job, true)
}

// mailRuleRunDialogData loads what the run dialog shows, from the owner's
// store.
func (h *Handler) mailRuleRunDialogData(ctx context.Context, owner string) (models.MailRulesData, error) {
	var data models.MailRulesData
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		data, err = mailRulesData(ctx, db, owner)
		return err
	})
	return data, err
}

func (h *Handler) renderMailRuleRun(w http.ResponseWriter, r *http.Request, data models.MailRulesData, job *mailRuleRunJob, dialog bool) {
	run := h.mailRuleRuns.snapshot(job)
	data.Run = &run
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	if dialog {
		views.MailRuleDialog(data).Render(r.Context(), w)
		return
	}
	views.MailRuleDialogPanel(data).Render(r.Context(), w)
}

// handleMailRuleRunDialog opens the run dialog of a rule: with its run in
// progress, or counting what it would change in the Inbox.
func (h *Handler) handleMailRuleRunDialog(w http.ResponseWriter, r *http.Request) {
	ctx, owner := r.Context(), h.userID(r.Context())
	data, err := h.mailRuleRunDialogData(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	index := slices.IndexFunc(data.Rules, func(rule models.MailRule) bool { return rule.ID == r.PathValue("id") })
	if index < 0 {
		http.NotFound(w, r)
		return
	}
	job := h.mailRuleRuns.latest(owner, data.Rules[index].ID)
	if job == nil || h.mailRuleRuns.snapshot(job).State != models.MailRuleRunRunning {
		job = h.startMailRuleCount(owner, data.Rules[index], "role:inbox", views.MailRuleRunScopeName("role:inbox", data.Accounts))
	}
	h.renderMailRuleRun(w, r, data, job, true)
}

// handleMailRuleRunScope counts again, in the folder chosen.
func (h *Handler) handleMailRuleRunScope(w http.ResponseWriter, r *http.Request) {
	ctx, owner := r.Context(), h.userID(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}
	data, err := h.mailRuleRunDialogData(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	index := slices.IndexFunc(data.Rules, func(rule models.MailRule) bool { return rule.ID == r.PathValue("id") })
	if index < 0 {
		http.NotFound(w, r)
		return
	}
	if job := h.mailRuleRuns.latest(owner, data.Rules[index].ID); job != nil && h.mailRuleRuns.snapshot(job).State == models.MailRuleRunRunning {
		// A running job isn't stopped by a change of folder.
		h.renderMailRuleRun(w, r, data, job, false)
		return
	}
	_, scope := mailRuleRunScope(strings.TrimSpace(r.FormValue("scope")))
	job := h.startMailRuleCount(owner, data.Rules[index], scope, views.MailRuleRunScopeName(scope, data.Accounts))
	h.renderMailRuleRun(w, r, data, job, false)
}

// handleMailRuleRunJob shows how a run is going.
func (h *Handler) handleMailRuleRunJob(w http.ResponseWriter, r *http.Request) {
	h.mailRuleRunAction(w, r, nil)
}

// handleStartMailRuleRun changes what a run counted.
func (h *Handler) handleStartMailRuleRun(w http.ResponseWriter, r *http.Request) {
	h.mailRuleRunAction(w, r, func(job *mailRuleRunJob) { h.startMailRuleRun(job) })
}

// handleStopMailRuleRun stops counting or running. What was changed stays
// changed.
func (h *Handler) handleStopMailRuleRun(w http.ResponseWriter, r *http.Request) {
	h.mailRuleRunAction(w, r, func(job *mailRuleRunJob) {
		var cancel context.CancelFunc
		h.updateMailRuleRun(job, func(job *mailRuleRunJob) {
			cancel = job.cancel
			if job.data.State == models.MailRuleRunCounting {
				job.data.State = models.MailRuleRunStopped
				job.matches, job.cancel = nil, nil
			}
		})
		if cancel != nil {
			cancel()
		}
		// A running job says it stopped once its batch is done.
		h.tellMailRuleRun(job, true)
	})
}

func (h *Handler) mailRuleRunAction(w http.ResponseWriter, r *http.Request, act func(*mailRuleRunJob)) {
	ctx, owner := r.Context(), h.userID(r.Context())
	job := h.mailRuleRuns.job(owner, r.PathValue("job"))
	if job == nil {
		http.NotFound(w, r)
		return
	}
	if act != nil {
		act(job)
	}
	data, err := h.mailRuleRunDialogData(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	h.renderMailRuleRun(w, r, data, job, false)
}

// mailRuleRunRoutes start, show and stop rule runs. They lease the owner's
// store themselves, a step at a time.
var mailRuleRunRoutes = []mailRuleRoute{
	{"GET /api/settings/rules/{id}/run", func(h *Handler) http.HandlerFunc { return h.handleMailRuleRunDialog }},
	{"POST /api/settings/rules/{id}/run/scope", func(h *Handler) http.HandlerFunc { return h.handleMailRuleRunScope }},
	{"GET /api/settings/rule-runs/{job}", func(h *Handler) http.HandlerFunc { return h.handleMailRuleRunJob }},
	{"POST /api/settings/rule-runs/{job}/start", func(h *Handler) http.HandlerFunc { return h.handleStartMailRuleRun }},
	{"POST /api/settings/rule-runs/{job}/stop", func(h *Handler) http.HandlerFunc { return h.handleStopMailRuleRun }},
}
