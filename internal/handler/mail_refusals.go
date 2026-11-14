package handler

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// The allowed mail lists refuse a mailbox when it's saved and each time it
// connects. When the rules change, the mailboxes they now refuse stop, and
// the ones they let in again reconnect, without waiting for their next
// connection: a worker watches the policy and membership revisions, which
// move with every change to policies, their assignments and groups, and
// works the refusals out again when either does. Mail security works them
// out right after its own changes, and lists the last result.
const (
	mailRefusalPoll = 5 * time.Second
	// mailRefusalStale is how old a result Mail security lists before it
	// works the refusals out again: a mailbox deleted since stays on the
	// list until then.
	mailRefusalStale = 10 * time.Minute
)

// mailRefusalState is the last check: the revisions it was made at, when,
// and what it found.
type mailRefusalState struct {
	// run lets one check go at a time.
	run sync.Mutex
	mu  sync.Mutex
	// checked says a check has been made since Gofer started.
	checked                            bool
	policyRevision, membershipRevision int64
	at                                 time.Time
	refused                            []mailAllowlistRefusal
	// stop and restart, when set, replace stopping and reconnecting a
	// mailbox, for tests.
	stop    func(id string)
	restart func(ctx context.Context, id string) error
}

// StartMailRefusalWorker checks the refusals at startup and whenever the
// rules change. It runs only when people's mail is kept in their own stores,
// where the lists apply.
func (h *Handler) StartMailRefusalWorker(ctx context.Context) {
	if h.ownedMailbox == nil {
		return
	}
	h.mailRefusalWorkers.Add(1)
	go func() {
		defer h.mailRefusalWorkers.Done()
		h.checkMailRefusalsIfChanged(ctx)
		ticker := time.NewTicker(mailRefusalPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.checkMailRefusalsIfChanged(ctx)
			}
		}
	}()
}

// WaitMailRefusalWorker waits for the worker to stop.
func (h *Handler) WaitMailRefusalWorker() { h.mailRefusalWorkers.Wait() }

// checkMailRefusalsIfChanged checks the refusals again when the rules
// changed since the last check.
func (h *Handler) checkMailRefusalsIfChanged(ctx context.Context) {
	policyRevision, membershipRevision, err := policy.CurrentRevisions(ctx, h.db.Read())
	if err != nil {
		log.Printf("allowed mail lists: read the policy revisions: %v", err)
		return
	}
	state := &h.mailRefusals
	state.mu.Lock()
	current := state.checked && state.policyRevision == policyRevision && state.membershipRevision == membershipRevision
	state.mu.Unlock()
	if current {
		return
	}
	if _, _, err := h.checkMailRefusals(ctx); err != nil {
		log.Printf("allowed mail lists: check refused mailboxes: %v", err)
	}
}

// currentMailRefusals is what Mail security lists: the last check, or a
// new one when the rules changed since or it's old.
func (h *Handler) currentMailRefusals(ctx context.Context) ([]mailAllowlistRefusal, time.Time, error) {
	policyRevision, membershipRevision, err := policy.CurrentRevisions(ctx, h.db.Read())
	if err != nil {
		return nil, time.Time{}, err
	}
	state := &h.mailRefusals
	state.mu.Lock()
	current := state.checked && state.policyRevision == policyRevision && state.membershipRevision == membershipRevision &&
		time.Since(state.at) < mailRefusalStale
	refused, at := state.refused, state.at
	state.mu.Unlock()
	if current {
		return refused, at, nil
	}
	return h.checkMailRefusals(ctx)
}

// checkMailRefusals works out which mailboxes the rules refuse, stops the
// ones newly refused and reconnects the ones let in again. When it can't
// tell, it ends every mail session, so each connects again through the
// checks.
func (h *Handler) checkMailRefusals(ctx context.Context) ([]mailAllowlistRefusal, time.Time, error) {
	state := &h.mailRefusals
	state.run.Lock()
	defer state.run.Unlock()
	// Read before working them out, so a change made meanwhile is checked
	// again.
	policyRevision, membershipRevision, err := policy.CurrentRevisions(ctx, h.db.Read())
	if err != nil {
		return nil, time.Time{}, err
	}
	refused, err := h.mailRefusalsNow(ctx)
	if err != nil {
		if h.ownedMailbox != nil && h.ownedMailbox.userIMAP != nil {
			h.ownedMailbox.userIMAP.StopMailSecuritySessions()
		}
		return nil, time.Time{}, err
	}
	at := time.Now()
	state.mu.Lock()
	before := state.refused
	state.checked, state.policyRevision, state.membershipRevision = true, policyRevision, membershipRevision
	state.at, state.refused = at, refused
	state.mu.Unlock()
	h.applyMailAllowlistChange(ctx, before, refused)
	return refused, at, nil
}

// mailRefusalsNow reads every webmail user's mailboxes and checks each
// against the lists that person's policies give them, by address. Nobody's
// mailboxes are read while no policy limits anything, nor those of someone
// the lists don't limit.
func (h *Handler) mailRefusalsNow(ctx context.Context) ([]mailAllowlistRefusal, error) {
	limited, err := policy.MailListsLimitAnyone(ctx, h.db.Read())
	if err != nil || !limited {
		return nil, err
	}
	users, err := h.db.ListAdminWebmailUsers(ctx)
	if err != nil {
		return nil, err
	}
	var refused []mailAllowlistRefusal
	for _, owner := range users {
		list, err := policy.MailAllowlistForUser(ctx, h.db, owner.ID)
		if err != nil {
			return nil, err
		}
		if !list.Limited() {
			continue
		}
		transports, err := h.ownedMailbox.userStorage.ReadMailboxTransports(ctx, owner.ID)
		if err != nil {
			return nil, err
		}
		for _, account := range transports {
			if err := list.CheckMailboxTransports(account); err != nil {
				refused = append(refused, mailAllowlistRefusal{owner: owner.ID, MailAllowlistRefusedAccount: models.MailAllowlistRefusedAccount{
					ID: account.ID, Email: account.Email, Username: owner.Username, Reason: mailAllowlistRefusalReason(ctx, err)}})
			}
		}
	}
	sort.SliceStable(refused, func(i, j int) bool {
		return strings.ToLower(refused[i].Email) < strings.ToLower(refused[j].Email)
	})
	return refused, ctx.Err()
}

// applyMailAllowlistChange stops the mailboxes after refuses that before
// didn't, and reconnects the ones before refused that after doesn't.
func (h *Handler) applyMailAllowlistChange(ctx context.Context, before, after []mailAllowlistRefusal) {
	state := &h.mailRefusals
	stop, restart := state.stop, state.restart
	if stop == nil {
		stop = h.stopMailSecurityAccount
	}
	if restart == nil && h.ownedMailbox != nil && h.ownedMailbox.userIMAP != nil {
		restart = h.ownedMailbox.userIMAP.RestartAccount
	}
	wasRefused := make(map[string]bool, len(before))
	for _, account := range before {
		wasRefused[account.ID] = true
	}
	for _, account := range after {
		if !wasRefused[account.ID] {
			stop(account.ID)
		}
		delete(wasRefused, account.ID)
	}
	if restart == nil {
		return
	}
	for id := range wasRefused {
		if err := restart(ctx, id); err != nil {
			log.Printf("allowed mail lists: reconnect account %s: %v", id, err)
		}
	}
}
