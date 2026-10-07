package mail

// All registration/removal shares mu with StopUser. Entries represent only
// running jobs, including owner work between account operations; no persistent
// per-registered-user worker or history is retained here.
func (s *UserIMAP) trackUserRunLocked(run *userIMAPOperation, owner string) {
	if s.userRuns == nil {
		s.userRuns = make(map[*userIMAPOperation]string)
	}
	s.userRuns[run] = owner
}

// StopUser cancels an owner's current provider/account and whole-user service
// work, manual receives, IDLE and browser-driven polling. It takes no database
// lease and preserves retained data/credentials and other users' sessions.
// Central status/deletion intent must be changed first to reject fresh work;
// queued operations and publication continue to validate central authority.
func (s *UserIMAP) StopUser(owner string) {
	s.mu.Lock()
	for run, user := range s.userRuns {
		if user == owner {
			run.cancel()
		}
	}
	if run := s.manualRuns[owner]; run != nil {
		run.cancel()
	}
	for key, w := range s.watches {
		if w.owner == owner {
			w.suppressResync = true
			delete(s.watches, key)
			w.cancel()
		}
	}
	if session := s.activePollUsers[owner]; session != nil {
		delete(s.activePollUsers, owner)
		session.cancel()
		for account, pending := range s.activePollPending {
			if pending == session {
				delete(s.activePollPending, account)
			}
		}
	}
	delete(s.fileCleanup, owner)
	s.mu.Unlock()
	s.wakeActivePoll()
	s.wakeBackground()
}
