// packages/envoy/cmd/agent-secrets/signin.go
//
// This machine's sign-ins: the directory they live in, and the names this CLI's refusals use.
// The records themselves, the rule that says which person a session acts for, and the refusals
// that rule gives are internal/brokerclient's (signinstore.go), because they are ordinary logic
// and this package's tests cannot run everywhere — a pod under gVisor has no SO_PEERPIDFD, which
// this package's rig needs — while that package's run anywhere.
package main

import (
	"os"

	"github.com/sjawhar/envoy/internal/brokerclient"
)

// signInStore is where this machine keeps them: the state directory, which outlives the reboot
// a key directory does not, since signing in is a human ceremony and the broker lists the
// sign-in until its person revokes it. The directory is brokerclient's own (SignIns), the one
// place that names it, since agent-secrets-helper reads the same records to decide whose
// authority a session enrols under.
func signInStore() brokerclient.SignInStore {
	return brokerclient.SignIns(os.Getenv)
}
