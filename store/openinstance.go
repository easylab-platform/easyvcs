package store

// instanceIsOpen reports whether no users are registered, i.e. a fresh
// single-user fixture where there is nothing to hide and no auth to gate on.
// The result is cached and invalidated whenever a user is created, so
// per-request checks don't rescan the users table.
func (s *CentralStore) instanceIsOpen() bool {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.openKnown {
		return s.openValue
	}
	users, err := s.ListUsers()
	if err != nil {
		return false
	}
	s.openValue = len(users) == 0
	s.openKnown = true
	return s.openValue
}

// invalidateOpenCache drops the cached instanceIsOpen result (called whenever
// a user is created, the only transition from open to closed).
func (s *CentralStore) invalidateOpenCache() {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.openKnown = false
}

// IsOpenInstance reports whether the instance has no registered users, in which
// case anonymous READ access is allowed (CLI/dev default). WRITE access is
// never granted anonymously: the caller must still authenticate.
func (s *CentralStore) IsOpenInstance() bool { return s.instanceIsOpen() }
