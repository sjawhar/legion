package session

// InstanceMatches reports whether a session's stored client instance id and the one a claimant
// carries name the same client instance (one Oh My Pi Envoy extension instance, one Claude Code
// channel-server process), which is what a soft claim naming its predecessor asks before it takes
// the role from that predecessor: the same conversation continuing inside its own instance may
// take it, another instance's live session may not be displaced by one.
//
// The predecessor rule is its one caller. Every other decision that weighs two instances - a
// claim of a role held under the claimant's own session id, and a release of that session's role
// claims (store.RoleReleaseAuthority) - reads an absent instance as naming no process at all, so
// it matches nothing and nothing stands in for it. This function's admission of an absent id is
// the predecessor rule's alone.
//
// An empty id on either side is a client that predates the field, and keeps the old rule: the
// claim goes through (the listener then logs the takeover), because a client that cannot name its
// instance must still be able to recover its own role, and refusing it would strand it instead.
// Two non-empty ids must be equal; nothing else makes two instances the same one.
//
// The ids are compared as stored, with no trimming of their own: both the value a registration
// stored and the one POST /v1/roles/set states are trimmed where they enter, so an id that
// reaches this test is already the id itself.
func InstanceMatches(stored, requested string) bool {
	return stored == "" || requested == "" || stored == requested
}
