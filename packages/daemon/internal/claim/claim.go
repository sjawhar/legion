// Package claim is the vocabulary of a role claim: the token that names one, the role it is on,
// and the wire the agent holding it speaks to the daemon.
//
// It is a leaf on purpose. The runtime, the worker stream, the supervisor, and the API all speak
// about claims, and each of them is reachable from the others; a `Token` declared in any of them
// would make one the root of the rest. Nothing here imports another Legion package.
package claim

// Token names one role claim — one agent on one issue at one generation — and is the key
// everything about that agent is filed under: its locator, its stream connection, its pending
// delivery, its row in the store.
type Token string

// Role names the agent that holds a claim on an issue.
type Role string

const (
	RoleArchitect   Role = "architect"
	RolePlanner     Role = "planner"
	RoleImplementer Role = "implementer"
	RoleTester      Role = "tester"
	RoleReviewer    Role = "reviewer"
	RoleMerger      Role = "merger"
)
