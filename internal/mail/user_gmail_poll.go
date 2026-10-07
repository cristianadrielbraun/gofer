package mail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type userActivePollSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	since  time.Time
	refs   int
}
type userActivePollJob struct {
	owner, account string
	session        *userActivePollSession
}
type userActivePollBatch struct {
	owner   storage.ActivePollOwner
	session *userActivePollSession
	after   string
	done    bool
}

// BeginActiveUserSession tracks connected browsers without spawning a worker
// for each browser or owner. The last release cancels queued and current checks.
func (s *UserIMAP) BeginActiveUserSession(ctx context.Context, owner string) (func(), error) {
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	if !gmailAPIPollEnabled() {
		return func() {}, nil
	}
	s.mu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, errors.New("mail worker is shutting down")
	}
	session := s.activePollUsers[owner]
	if session == nil {
		work, cancel := context.WithCancel(s.ctx)
		session = &userActivePollSession{ctx: work, cancel: cancel, since: time.Now().UTC()}
		s.activePollUsers[owner] = session
	}
	session.refs++
	s.mu.Unlock()
	s.wakeActivePoll()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			session.refs--
			if session.refs == 0 && s.activePollUsers[owner] == session {
				delete(s.activePollUsers, owner)
				session.cancel()
			}
			s.mu.Unlock()
			s.wakeActivePoll()
		})
	}, nil
}
func (s *UserIMAP) wakeActivePoll() {
	select {
	case s.activePollWake <- struct{}{}:
	default:
	}
}
func (s *UserIMAP) activePollBatches() []userActivePollBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	owners := make([]string, 0, len(s.activePollUsers))
	for owner := range s.activePollUsers {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	var batches []userActivePollBatch
	for _, owner := range owners {
		session := s.activePollUsers[owner]
		batches = append(batches, userActivePollBatch{owner: storage.ActivePollOwner{UserID: owner, Since: session.since}, session: session})
	}
	return batches
}

func (s *UserIMAP) startActiveGmailPollingLocked() {
	for i := 0; i < 4; i++ {
		s.workers.Add(1)
		go s.workActiveGmailPoll()
	}
	s.workers.Add(1)
	go s.runActiveGmailPoller()
}

func (s *UserIMAP) runActiveGmailPoller() {
	defer s.workers.Done()
	ticker := time.NewTicker(gmailAPIPollSweepInterval)
	defer ticker.Stop()
	for {
		s.scanActiveGmailAccounts()
		select {
		case <-s.ctx.Done():
			return
		case <-s.activePollWake:
		case <-ticker.C:
		}
	}
}

// Round-robin bounded owner/account pages prevent a large owner's directory
// from monopolizing discovery. Backpressure retains only IDs and session refs.
func (s *UserIMAP) scanActiveGmailAccounts() {
	if !gmailAPIPollEnabled() {
		return
	}
	batches := s.activePollBatches()
	remaining := len(batches)
	for remaining > 0 && s.ctx.Err() == nil {
		for i := range batches {
			batch := &batches[i]
			if batch.done {
				continue
			}
			if batch.session.ctx.Err() != nil {
				batch.done = true
				remaining--
				continue
			}
			page, err := s.Routing().ListActivePollAccounts(s.ctx, []storage.ActivePollOwner{batch.owner}, batch.after, time.Now(), 8)
			if err != nil {
				if s.ctx.Err() == nil {
					log.Printf("user Gmail poll discovery: %v", err)
				}
				batch.done = true
				remaining--
				continue
			}
			if len(page) == 0 {
				batch.done = true
				remaining--
				continue
			}
			for _, entry := range page {
				batch.after = entry.AccountID
				session := batch.session
				if session.ctx.Err() != nil {
					continue
				}
				job := userActivePollJob{owner: entry.UserID, account: entry.AccountID, session: session}
				s.mu.Lock()
				if s.closing || s.activePollUsers[job.owner] != session || s.activePollPending[job.account] != nil {
					s.mu.Unlock()
					continue
				}
				s.activePollPending[job.account] = session
				s.mu.Unlock()
				select {
				case s.activePollQueue <- job:
				case <-s.ctx.Done():
					s.removeActivePollJob(job)
					return
				case <-session.ctx.Done():
					s.removeActivePollJob(job)
				}
			}
		}
	}
}
func (s *UserIMAP) removeActivePollJob(job userActivePollJob) {
	s.mu.Lock()
	if s.activePollPending[job.account] == job.session {
		delete(s.activePollPending, job.account)
	}
	s.mu.Unlock()
}
func (s *UserIMAP) workActiveGmailPoll() {
	defer s.workers.Done()
	for {
		var job userActivePollJob
		select {
		case <-s.ctx.Done():
			return
		case job = <-s.activePollQueue:
		}
		if err := s.pollActiveGmailAccount(job); err != nil && s.ctx.Err() == nil && job.session.ctx.Err() == nil {
			log.Printf("user Gmail poll account=%s: %v", job.account, err)
		}
		s.removeActivePollJob(job)
		s.wakeActivePoll()
	}
}
func (s *UserIMAP) pollActiveGmailAccount(job userActivePollJob) error {
	if err := job.session.ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	interval := gmailAPIActivePollInterval()
	reservation, ok, err := s.Routing().ReserveActivePoll(job.session.ctx, job.owner, job.account, job.session.since, now, now.Add(interval))
	if err != nil || !ok {
		return err
	}
	// A full receive already checks the profile. Do not occupy a poll worker
	// waiting behind that receive; the successful reservation schedules a retry.
	s.mu.Lock()
	busy := s.queued[job.account] != nil
	if gate := s.gates[userIMAPKey{account: job.account}]; gate != nil && len(gate.token) > 0 {
		busy = true
	}
	s.mu.Unlock()
	changed := false
	if !busy {
		err = s.operation(job.session.ctx, job.owner, job.account, 0, gmailAPIPollRequestTimeout, func(ctx context.Context) error {
			scope, err := s.snapshot(ctx, job.owner, job.account)
			if err != nil {
				return err
			}
			if scope.config.Provider != providers.ProviderGmail || scope.config.AuthMethod != "oauth2" || scope.tokens == nil || !scope.account.EmailSyncEnabled {
				interval = gmailAPIPollMaxBackoff
				return nil
			}
			changed, err = s.checkUserGmailProfile(ctx, scope)
			return err
		})
	}
	failures := 0
	if busy {
		failures = reservation.Failures
	}
	if err != nil && job.session.ctx.Err() == nil {
		failures = min(reservation.Failures+1, 4)
		interval = min(interval*time.Duration(1<<failures), gmailAPIPollMaxBackoff)
	}
	if s.ctx.Err() == nil {
		_, completeErr := s.Routing().CompleteActivePoll(s.ctx, job.owner, job.account, reservation.Revision, time.Now().Add(interval), failures)
		err = errors.Join(err, completeErr)
	}
	if err == nil && changed && job.session.ctx.Err() == nil {
		// Leave the account gate before queueing normal reconciliation. QueueAccount
		// coalesces changes and retains a follow-up during a running receive.
		if err := s.Routing().ResetAccountPolling(job.session.ctx, job.owner, job.account); err != nil {
			return err
		}
		s.wakeBackground()
		return s.QueueAccount(job.session.ctx, job.account)
	}
	return err
}

func (s *UserIMAP) checkUserGmailProfile(ctx context.Context, scope *userIMAPScope) (bool, error) {
	o := NewSyncOrchestrator(nil, nil, nil, scope.tokens)
	o.imapScope = scope
	token, err := scope.tokens.GetOAuthTokenForAccount(ctx, scope.id)
	var profile string
	if err == nil {
		err = o.outlookRequest(ctx, token, func(access string) error {
			if err := scope.call(ctx, func(db *storage.DB) error {
				if !db.IsEmailSyncEnabled(ctx, scope.id) {
					return errors.New("mail receiving was disabled during Gmail polling")
				}
				return nil
			}); err != nil {
				return err
			}
			var err error
			profile, err = getGmailProfileHistoryID(ctx, access)
			if err == nil && strings.TrimSpace(profile) == "" {
				return fmt.Errorf("Gmail profile has no history ID")
			}
			return err
		})
	} else {
		err = o.recordOutlookRetry(ctx, err)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	changed := false
	pollErr := err
	publishErr := scope.call(ctx, func(db *storage.DB) error {
		if !db.IsEmailSyncEnabled(ctx, scope.id) {
			return errors.New("mail receiving was disabled during Gmail polling")
		}
		if pollErr == nil {
			state, err := db.GetLabelSyncState(ctx, scope.id, storage.LabelProviderGmail, "messages")
			if err != nil {
				return err
			}
			due, err := db.HasDueGmailMessageFetch(ctx, scope.id)
			if err != nil {
				return err
			}
			cursor := strings.TrimSpace(state.Cursor)
			changed = cursor == "" || !state.LastSuccessAt.Valid || newerGmailHistoryID(cursor, profile) != cursor || due
		}
		state, err := db.GetGmailPollState(ctx, scope.id)
		if err != nil {
			return err
		}
		state.ProfileHistoryID = profile
		state.LastCheckedAt = sqlNullTime(time.Now())
		if changed {
			state.LastChangedAt = state.LastCheckedAt
		}
		return db.MarkOwnedGmailPollCheck(ctx, scope.owner, scope.config.ProviderAccountID, state, changed, pollErr)
	})
	return changed, errors.Join(pollErr, publishErr)
}
