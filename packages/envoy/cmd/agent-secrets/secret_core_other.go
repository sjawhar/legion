//go:build !unix

package main

// These platforms have no Unix core limit. Crash-dump policy is OS-managed;
// only piped input is supported, with no terminal prompt.
func preventCoreDumps() error { return nil }
