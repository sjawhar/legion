// packages/envoy/cmd/agent-secrets/signin.go
//
// This machine's sign-ins: the directory they live in, and the names this CLI's refusals use.
// The records themselves, the rule that says which person a session acts for, and the refusals
// that rule gives are internal/brokerclient's (signinstore.go), because they are ordinary logic
// and this package's tests cannot run everywhere — a pod under gVisor has no SO_PEERPIDFD, which
// this package's rig needs — while that package's run anywhere.
package main

import "github.com/sjawhar/envoy/internal/brokerclient"

const (
	// personVar names which person a session acts for where the box has more than one sign-in.
	personVar = brokerclient.PersonVar
	// signInCommand is the command every refusal a sign-in would cure names.
	signInCommand = brokerclient.SignInCommand
)

// signIn is one person's sign-in on this machine.
type signIn = brokerclient.SignInRecord

// signInStore is where this machine keeps them: under the key dir, so a box, a pod and a
// person's machine each keep their own where everything else of that session's identity is.
// The directory inside it is brokerclient's own (SignInsIn), the one place that names it, since
// agent-secrets-helper reads the same records to decide whose authority a session enrols under.
func signInStore() brokerclient.SignInStore {
	return brokerclient.SignInsIn(keyDir())
}
