package session

// SessionLookup abstracts session port/machine resolution.
type SessionLookup interface {
	Get(sessionID string) (SessionEntry, error)
	Put(sessionID string, entry SessionEntry) error
	Delete(sessionID string) error
}
