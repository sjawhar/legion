package session

// InstanceMatches reports whether a session's stored client instance id and the one a claimant
// carries name the same client instance (one Oh My Pi Envoy extension instance, one Claude Code
// channel-server process), which is what a soft claim naming its predecessor asks before it takes
// the role from that predecessor: the same conversation continuing inside its own instance may
// take it, another instance's live session may not be displaced by one.
//
// An empty id on either side is a client that predates the field, and keeps the old rule: the
// claim goes through (the listener then logs the takeover), because a client that cannot name its
// instance must still be able to recover its own role, and refusing it would strand it instead.
// Two non-empty ids must be equal; nothing else makes two instances the same one.
//
// The ids are compared as stored, with no trimming of their own: the wire value is trimmed where
// it enters the entry, so a value that reached the registry is already the id itself.
func InstanceMatches(stored, requested string) bool {
	return stored == "" || requested == "" || stored == requested
}
