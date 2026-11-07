package session

// InstanceMatches reports whether a session's stored client instance id and the one a caller
// carries name the same client instance (one Oh My Pi Envoy extension instance, one Claude Code
// channel-server process). Two decisions ask it, and both are a caller acting on a session it
// says is its own: a soft claim naming its predecessor, before it takes the role from that
// predecessor (POST /v1/roles/set), where the same conversation continuing inside its own
// instance may take it and another instance's live session may not be displaced by one; and the
// conditional DELETE /v1/sessions/<id> (DeleteConditions.keeps), where a caller that mints an
// instance id of its own deregisters the session it registered and must not delete the live
// entry another process wrote after it resumed the same session id.
//
// Every other decision that weighs two instances - a claim of a role held under the claimant's
// own session id, and a release of that session's role claims (store.RoleReleaseAuthority) -
// reads an absent instance as naming no process at all, so it matches nothing and nothing stands
// in for it. The admission of an absent id below is these two callers' alone.
//
// An empty id on either side is a client that predates the field, and keeps the old rule: the
// claim goes through (the listener then logs the takeover) and so does the delete, because a
// client that cannot name its instance must still be able to recover its own role and to
// deregister its own session, and refusing it would strand it instead. Two non-empty ids must be
// equal; nothing else makes two instances the same one.
//
// The ids are compared as stored, with no trimming of their own: the value a registration
// stored, the one POST /v1/roles/set states and the one DELETE /v1/sessions/<id> carries are
// each trimmed where they enter, so an id that reaches this test is already the id itself.
func InstanceMatches(stored, requested string) bool {
	return stored == "" || requested == "" || stored == requested
}
