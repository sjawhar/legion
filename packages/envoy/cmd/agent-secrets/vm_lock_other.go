//go:build !unix

// packages/envoy/cmd/agent-secrets/vm_lock_other.go
//
// `enroll --vm` enrolls a Google Compute Engine VM, which runs Linux, and serializes its runs on
// a flock. Where there is no flock there is no such VM either, so the form refuses rather than
// enroll without the lock.
package main

func lockEnrollDir(string) (func(), error) {
	return nil, errNoEnrollLock
}
