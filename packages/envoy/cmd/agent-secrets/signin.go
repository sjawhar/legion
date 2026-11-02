// packages/envoy/cmd/agent-secrets/signin.go
//
// This machine's sign-ins: the directory they live in, and the names this CLI's refusals use.
// The records themselves, the rule that says which person a session acts for, and the refusals
// that rule gives are internal/brokerclient's (signinstore.go), because they are ordinary logic
// and this package's tests cannot run everywhere — a pod under gVisor has no SO_PEERPIDFD, which
// this package's rig needs — while that package's run anywhere.
package main

import (
	"path/filepath"

	"github.com/sjawhar/envoy/internal/brokerclient"
)

const (
	// signInsDir is the directory under the key dir that holds one record per person.
	signInsDir = "signins"
	// personVar names which person a session acts for where the box has more than one sign-in.
	personVar = brokerclient.PersonVar
	// signInCommand is the command every refusal a sign-in would cure names.
	signInCommand = brokerclient.SignInCommand
)

// signIn is one person's sign-in on this machine.
type signIn = brokerclient.SignInRecord

// signInStore is where this machine keeps them: under the key dir, so a box, a pod and a
// person's machine each keep their own where everything else of that session's identity is.
func signInStore() brokerclient.SignInStore {
	return brokerclient.SignInStore{Dir: filepath.Join(keyDir(), signInsDir)}
}
