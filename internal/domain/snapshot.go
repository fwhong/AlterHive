package domain

import "encoding/json"

// SnapshotForAPI returns a detached, read-only view, never a live session or its
// world/auth state. Hold the hot-path lock through copying maps/slices so JSON
// serialization cannot race commands; mu also excludes lifecycle/log writers.
func (s *SessionContext) SnapshotForAPI() *SessionContext {
	if s == nil {
		return nil
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.Marshal(s)
	if err != nil {
		panic("session snapshot: " + err.Error())
	}
	var result SessionContext
	if err = json.Unmarshal(data, &result); err != nil {
		panic("session snapshot: " + err.Error())
	}
	result.Evidence = NewEvidenceState()
	if s.Evidence != nil {
		for _, token := range s.Evidence.Tokens() {
			result.Evidence.Hit(token)
		}
	}
	result.SuppressShellPrompt = s.SuppressShellPrompt
	return &result
}
