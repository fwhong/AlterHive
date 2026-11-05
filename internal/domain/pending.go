package domain

import "time"

// QueueMutation defers cold-path application to the next serialized command.
// The queue is bounded: an absent client cannot accumulate unbounded callbacks.
func (s *SessionContext) QueueMutation(f func()) bool {
	if s == nil || f == nil {
		return false
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if len(s.pending) >= 64 {
		return false
	}
	s.pending = append(s.pending, f)
	return true
}

// ApplyPending must run on the hot path, never from a completion goroutine.
func (s *SessionContext) ApplyPending() {
	if s == nil {
		return
	}
	s.pendingMu.Lock()
	pending := s.pending
	s.pending = nil
	s.pendingMu.Unlock()
	for _, f := range pending {
		f()
	}
}
func (s *SessionContext) LockCommand()   { s.commandMu.Lock() }
func (s *SessionContext) UnlockCommand() { s.commandMu.Unlock() }
func (s *SessionContext) Events() []EventEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]EventEntry{}, s.EventLog...)
}
func (s *SessionContext) Commands() []CommandEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]CommandEntry{}, s.CommandLog...)
}

// DisconnectIfIdle checks and updates lifecycle state atomically with Touch.
func (s *SessionContext) DisconnectIfIdle(timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DisconnectedAt == nil && time.Since(s.LastActivity) > timeout {
		now := time.Now().UTC()
		s.DisconnectedAt = &now
	}
}
