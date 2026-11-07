//go:build !unix

// packages/envoy/cmd/agent-secrets/pod_lock_other.go
//
// `enroll --pod` enrolls a Kubernetes pod, which runs Linux, and serializes its runs on a flock.
// Where there is no flock there is no such pod either, so the form refuses rather than enroll
// without the lock.
package main

func lockEnrollDir(string) (func(), error) {
	return nil, errNoEnrollLock
}
