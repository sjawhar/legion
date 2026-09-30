// packages/envoy/internal/buildversion/buildversion.go

// Package buildversion names the release a host binary (agent-secrets, agent-secrets-helper) was
// built as, for `--version` and the helper's startup line.
package buildversion

import "runtime/debug"

// tag is the release tag, stamped only by the release job of
// .github/workflows/release-envoy-listener.yaml:
//
//	-ldflags "-X github.com/sjawhar/envoy/internal/buildversion.tag=legion-envoy-vX.Y.Z"
//
// Every other build leaves it empty.
var tag string

// String is the stamped release tag, or "devel" for any other build, followed by the commit and
// dirty flag Go recorded when it built from a checkout (a release builds with -buildvcs=false, so
// it records none).
func String() string {
	if tag != "" {
		return tag
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "devel"
	}
	if modified == "true" {
		return "devel (" + revision + ", modified)"
	}
	return "devel (" + revision + ")"
}
