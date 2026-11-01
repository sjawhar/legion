//go:build unix

// packages/envoy/cmd/agent-secrets/vm_lock_unix.go
//
// The lock `enroll --vm` holds over one key directory. The lane's credential helper re-runs the
// command whenever it needs a secret, so two runs can overlap on one VM; flock makes the second
// wait for the first rather than log in again under the credential the first is enrolling with.
// The lock is released by the process ending too, so a run killed mid-enrollment leaves no stale
// lock behind.
package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockEnrollDir takes an exclusive flock on path, waiting for whoever holds it, and answers the
// release.
func lockEnrollDir(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the enrollment lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
