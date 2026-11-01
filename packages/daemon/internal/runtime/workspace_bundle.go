package runtime

import "time"

// MaxWorkspaceBundleBytes is the largest bundle a release carries. A bundle above it stops the
// release, which is recorded with its reason and leaves the pod where it is: the work a role has
// not pushed is kept at the cost of a pod, never dropped to save one. 40 MiB fits the worker
// stream's reassembly bound with room, and a change that large and still unpushed is rare enough
// that holding the pod for it is the cheaper mistake.
const MaxWorkspaceBundleBytes = 40 << 20

// WorkspaceBundle is the unpushed work of one issue's workspace, saved when its pod is released
// and restored into the workspace of the pod that replaces it. The bundle itself is a git bundle
// holding every visible commit no remote branch holds, on any head, and the working copy's own
// change, with the commits a new pod's workspace fetch already holds as its bases.
//
// Head is the commit the bundle's working copy sat on, Bookmarks maps each local bookmark that
// points into the saved set to its commit, and Tips names the heads the bundle carries, so the
// restore can make each visible again: a bundle that only unbundles leaves every saved head out of
// jj's view, since jj's visible heads live in .jj/repo, which dies with the pod.
type WorkspaceBundle struct {
	// Bundle is the git bundle's bytes.
	Bundle []byte
	// Head is the working-copy commit id the bundle was taken at.
	Head string
	// Tips are the commit ids of the heads the bundle carries.
	Tips []string
	// Bookmarks maps a local bookmark name to the commit id it pointed at.
	Bookmarks map[string]string
	// At is when the shim took the bundle.
	At time.Time
}

// Size is the bundle's length in bytes, which the release records and the 40 MiB bound reads.
func (b WorkspaceBundle) Size() int { return len(b.Bundle) }
