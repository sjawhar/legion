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
	// Release is the id of the release that saved the row this bundle was read from, which the
	// store fills on the read. The save takes that id as an argument of its own
	// (store.Store.PutWorkspaceBundle): the bundle itself is the shim's, built in a pod that
	// knows nothing of the release, and the id is the release's. The restore of the pod that
	// replaces the released one carries it to the launcher and back, so the report that confirms
	// a restore names the release whose work it put back and deletes no other
	// (internal/runtime/sandbox's restoreReported).
	Release int64
	// PodUID is the uid of the pod the work was taken off: the issue pod the save's own
	// BundleWorkspace asked, which the runtime fills because only it knows which pod the role
	// it asked was running in (sandbox's Runtime.BundleWorkspace reads it off the claim's
	// locator). The store keeps it on the row (store.Store.PutWorkspaceBundle) and refuses that
	// pod by it: a role relaunched in the pod being released asks for the bundle like any other
	// start, its workspace is the one the work came from, and it must leave the attempt for the
	// pod that replaces it. The read fills it back in from the row.
	PodUID string
}

// Size is the bundle's length in bytes, which the release records and the 40 MiB bound reads.
func (b WorkspaceBundle) Size() int { return len(b.Bundle) }

// WorkspaceBundleTake is what one pod's ask of an issue's saved bundle did to that bundle
// (store.Store.TakeWorkspaceBundleForRestore). A bundle is offered to one pod only — the first
// pod that is not the one its work came off to ask for it — and the ask that reaches that pod
// closes it to every later one, whether the body was sent or refused.
type WorkspaceBundleTake int

const (
	// WorkspaceBundleNone: there is nothing to send this pod and nothing was recorded. The issue
	// was never released, its restore is confirmed, a wake cancelled its release, the bundle has
	// had its one attempt already, or this pod is the one the work was taken off.
	WorkspaceBundleNone WorkspaceBundleTake = iota
	// WorkspaceBundleSend: this pod takes the bundle, and the row records the attempt as its
	// before a byte of it leaves the daemon.
	WorkspaceBundleSend
	// WorkspaceBundleRefusedOlder: this pod is not the one the work came off and was not created
	// after the save, so it is not the pod the release's teardown replaced — or it is, under a
	// node clock that runs ahead of the API server's — and nothing is sent to it. The refusal is
	// the bundle's one attempt: no later pod is sent it either, since the roles of this pod go
	// on working and pushing, and a bundle applied over that work would move the issue's
	// bookmarks back behind it. The work stays in the database for a person to recover and holds
	// the issue's pod (workflow.IdleHoldBundleUnrestored).
	WorkspaceBundleRefusedOlder
)
