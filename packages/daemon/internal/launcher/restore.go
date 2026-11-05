package launcher

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// restoreBudget bounds the pod's one restore: an unbundle of a pack of unpushed commits, one jj
// import, a bookmark per saved bookmark and one jj edit. A restore that cannot finish in it is
// one the roles start without, with a notice of what was lost, rather than one the pod waits
// longer for.
const restoreBudget = 5 * time.Minute

// restoreRefPrefix is where the restore's temporary branches live. A bundle that only unbundles
// leaves every saved head out of jj's view — jj's visible heads live in .jj/repo, which died with
// the released pod — so each saved head is given a branch under refs/heads, which `jj git import`
// makes a bookmark and a visible head, and the branches are dropped again through jj, which keeps
// the heads visible where deleting the git refs under jj would abandon the commits with them.
const restoreRefPrefix = "legion-restore-"

// bundleRestore is the one workspace-bundle restore of a pod, shared by every role manager of its
// container. The daemon sends the bundle in launcher_bundle slices before it starts any role;
// this holds them in memory — never in a role's private directory, which each start clears — and
// the first role's start applies the whole, digest-checked body once, before any role of the pod
// runs. A pod the daemon sends no bundle to restores nothing and waits for nothing.
type bundleRestore struct {
	mu sync.Mutex
	// sequence is the id of the slice sequence being assembled, with the count, the digest and
	// the release its slices agree on, and slices what has arrived of it. release is the id of
	// the release that saved the body, which the report the daemon acts on carries back (apply).
	sequence string
	count    int
	digest   string
	release  int64
	slices   [][]byte
	// results answers a repeated slice as a repeated launcher_start id is answered: the same
	// result for the same payload, a refusal for a different one under an id already used.
	results map[string]shimwire.LauncherBundleResult
	// body is the whole bundle, joined from the slices and held until a start applies it, and
	// released there: the slices hold the same bytes, and nothing reads the join again (apply).
	body    []byte
	applied bool
	// restored is whether that run put the saved work back, which the generation is told so that
	// the agent's own notice says what its workspace holds.
	restored bool
	notice   string
	// confirm is the report the daemon is owed for the restore that has run and is still unsent:
	// the daemon keeps its saved copy until one arrives, so this is held until a connection
	// carries it and sent again on every connection until one does (manager.reportRestore).
	confirm *shimwire.LauncherRestored
}

// restoreResult is what one start's call on the pod's restore answers: whether the workspace
// holds the released work, and the notice the role's log and the agent's own message carry.
type restoreResult struct {
	Restored bool
	Notice   string
}

// receive takes one slice of the bundle. The sequence is rebuilt in memory and nothing is written
// anywhere until its digest matches the one every slice declares, so a body that did not arrive
// as it was sent reaches no workspace. The last slice of a whole sequence is answered Held: the
// launcher holds the bundle and will restore it before the pod's first role starts. The daemon
// keeps its own copy past that answer and drops it only on the restore's own outcome, which the
// launcher reports in a launcher_restored of its own (apply, manager.reportRestore).
func (r *bundleRestore) receive(frame shimwire.LauncherBundle) shimwire.LauncherBundleResult {
	if err := frame.Validate(); err != nil {
		return shimwire.LauncherBundleResult{ID: frame.ID, Index: frame.Index, Error: err.Error()}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := frame.ID + "/" + strconv.Itoa(frame.Index)
	if prior, ok := r.results[key]; ok {
		if prior.OK && !bytes.Equal(r.sliceAt(frame.ID, frame.Index), frame.Payload) {
			return shimwire.LauncherBundleResult{ID: frame.ID, Index: frame.Index,
				Error: "launcher_bundle reused an id with a different payload"}
		}
		return prior
	}
	if frame.ID != r.sequence {
		r.sequence, r.count, r.digest, r.release = frame.ID, frame.Count, frame.SHA256, frame.Release
		r.slices = make([][]byte, frame.Count)
		clear(r.results)
	}
	if frame.Count != r.count || frame.SHA256 != r.digest || frame.Release != r.release {
		return shimwire.LauncherBundleResult{ID: frame.ID, Index: frame.Index,
			Error: fmt.Sprintf("launcher_bundle %d of %d of release %d does not match its sequence's %d slices of %s of release %d",
				frame.Index, frame.Count, frame.Release, r.count, r.digest, r.release)}
	}
	r.slices[frame.Index] = frame.Payload
	result := shimwire.LauncherBundleResult{ID: frame.ID, Index: frame.Index, OK: true}
	if whole := r.whole(); whole != nil {
		if got := shimwire.BundleDigest(whole); got != r.digest {
			r.sequence, r.count, r.digest, r.release, r.slices = "", 0, "", 0, nil
			clear(r.results)
			return shimwire.LauncherBundleResult{ID: frame.ID, Index: frame.Index,
				Error: fmt.Sprintf("launcher_bundle sequence rebuilt to %s, not the %s its slices declared", got, frame.SHA256)}
		}
		r.body, r.applied, r.notice = whole, false, ""
		result.Held = true
	}
	r.results[key] = result
	return result
}

// sliceAt is the payload already held for one slice of id, nil when that is not the sequence
// being assembled.
func (r *bundleRestore) sliceAt(id string, index int) []byte {
	if id != r.sequence || index >= len(r.slices) {
		return nil
	}
	return r.slices[index]
}

// whole joins the sequence once every slice has arrived, and nil while one has not.
func (r *bundleRestore) whole() []byte {
	size := 0
	for _, slice := range r.slices {
		if slice == nil {
			return nil
		}
		size += len(slice)
	}
	body := make([]byte, 0, size)
	for _, slice := range r.slices {
		body = append(body, slice...)
	}
	return body
}

// apply restores the held bundle into the workspace env names, once per pod, and answers what the
// start that called it has to say: nothing at all when no bundle was sent, when one was already
// restored with nothing to say, or when the generation works no workspace. A second role starting
// while the restore runs waits here for it and is given the same notice.
func (r *bundleRestore) apply(ctx context.Context, env []string) restoreResult {
	// A manager built without one — a test's, which serves no daemon that could send a bundle —
	// restores nothing and waits for nothing.
	if r == nil {
		return restoreResult{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The restore has run: it is answered from what it left, since the body it read went with
	// it. This is read before the body, which a restore that has run no longer holds.
	if r.applied {
		return restoreResult{Restored: r.restored, Notice: r.notice}
	}
	if r.body == nil {
		return restoreResult{}
	}
	dir := envValue(env, "LEGION_WORKSPACE")
	if dir == "" {
		return restoreResult{}
	}
	r.applied = true
	bundle, err := shimwire.ParseWorkspaceBundle(r.body)
	// The joined body is the bundle's bytes a second time: the slices hold the same bytes, and
	// they stay, since a repeated slice is answered by comparing it with the one held (sliceAt).
	// The join is let go here instead, so a 40 MiB bundle is not resident twice for the pod's
	// whole life; nothing reads it again, since every later apply is answered above.
	r.body = nil
	if err != nil {
		r.notice = fmt.Sprintf("the released workspace's saved work could not be read (%v); this workspace holds only what was pushed,"+
			" and the bundle of release %d stays in the daemon's database, which sends it to no pod again", err, r.release)
	} else {
		r.restored, r.notice = restoreWorkspace(ctx, dir, env, r.release, bundle)
	}
	r.confirm = &shimwire.LauncherRestored{ID: r.sequence, Release: r.release, OK: r.restored}
	if !r.restored {
		r.confirm.Error = r.notice
	}
	return restoreResult{Restored: r.restored, Notice: r.notice}
}

// owed is the report the daemon has not been given yet, and nil when none is: every connection
// of every role of the pod sends it until one of them gets it across, since a restore the daemon
// never heard of leaves the saved work in its database for a pod that no longer needs it.
func (r *bundleRestore) owed() *shimwire.LauncherRestored {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.confirm
}

// sent forgets the report once a connection has carried it. A report the daemon read but could
// not act on is the daemon's to retry, not the launcher's: it keeps its copy of the work, which
// it sends no pod again (internal/runtime/sandbox's restoreWorkspace).
func (r *bundleRestore) sent(report *shimwire.LauncherRestored) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.confirm == report {
		r.confirm = nil
	}
}

// restoreWorkspace puts one saved bundle back into the workspace at dir and answers the notice of
// what it restored, or of what was lost and why. release is the idle release whose save built the
// bundle, which the notice of a failure names: that is the bundle an operator reads out of the
// daemon's database, since the daemon records one restore attempt per bundle and sends it to no
// pod again. A restore that fails leaves the workspace as provisioning made it — what the issue's
// branch holds — and the roles start on that, with the saved work in the database alone for the
// life of the issue's pods.
func restoreWorkspace(ctx context.Context, dir string, env []string, release int64, b shimwire.WorkspaceBundle) (bool, string) {
	// Every reason is written as one line (oneLine), however many lines the tool that failed
	// wrote: the notice goes to the container's log, where one line is one notice.
	lost := func(reason string) (bool, string) {
		return false, fmt.Sprintf("the released workspace's saved work was not restored (%s); this workspace holds only what was pushed,"+
			" and the work of %d head(s) and %d bookmark(s) saved at %s stays in the daemon's database as the bundle of release %d, which it sends to no pod again",
			oneLine(reason), len(b.Tips), len(b.Bookmarks), b.At.UTC().Format(time.RFC3339), release)
	}
	// A bundle is applied to the workspace the pod that replaces the released one provisioned,
	// and to no other. The daemon is what holds that rule: it records one restore attempt per
	// bundle, against the pod it sends it to, and sends that bundle to no pod and no launcher
	// again (store.TakeWorkspaceBundleForRestore), so a roles container that restarts on the
	// pod's own disk — coming back with a launcher process that holds no record of the restore
	// it already ran — is sent nothing to apply over the work the roles have done since.
	//
	// This check is the workspace's own half of it, and it catches the one thing the daemon
	// cannot see: a bundle that reached the very workspace it was saved from, whose creation
	// record (workspace.Created) is older than the save. Applying there would `jj edit` back to
	// the saved head and move bookmarks `--allow-backwards` over whatever that workspace holds
	// now.
	created, ok, err := workspace.Created(dir)
	switch {
	case err != nil:
		return lost(err.Error())
	case !ok:
		return lost(fmt.Sprintf("the workspace %s records no creation, so it cannot be told from the one the work was saved from", dir))
	case !created.After(b.At):
		return lost(fmt.Sprintf("the workspace %s was created at %s, before the work was saved, so it is the workspace the work came out of and not one provisioned since",
			dir, created.UTC().Format(time.RFC3339)))
	}
	gitDir, err := workspace.GitDir(dir)
	if err != nil {
		return lost(err.Error())
	}
	file, err := os.CreateTemp("", "legion-restore-*.bundle")
	if err != nil {
		return lost(fmt.Sprintf("make the bundle's file: %v", err))
	}
	path := file.Name()
	defer os.Remove(path)
	_, err = file.Write(b.Bundle)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return lost(fmt.Sprintf("write the bundle: %v", err))
	}
	ctx, cancel := context.WithTimeout(ctx, restoreBudget)
	defer cancel()
	// The restore runs its jj and git through the runner the save that built this bundle ran
	// through (internal/workspace): it is what names a stale git lock in a failure's report and
	// what stops a child holding the pipe past the budget.
	started := time.Now()
	run := func(stdin []byte, args ...string) (string, error) {
		return workspace.RunCommand(ctx, restoreBudget, started, env, dir, stdin, args)
	}
	// The bundle's own prerequisites are the commits the new pod's workspace fetch was expected
	// to hold; one that is gone — a branch force-pushed away since the release — is what git
	// refuses here, and it is the whole of what the notice has to name.
	if _, err := run(nil, "git", "--git-dir="+gitDir, "bundle", "unbundle", path); err != nil {
		return lost(err.Error())
	}
	if _, err := run(workspace.RefUpdates("create", restoreRefHeadPrefix, b.Tips), "git", "--git-dir="+gitDir, "update-ref", "--stdin"); err != nil {
		return lost(err.Error())
	}
	if _, err := run(nil, "jj", "git", "import", "-R", dir); err != nil {
		return lost(err.Error())
	}
	// A bookmark's name comes from the workspace the agent worked in, so it is an argument no
	// caller chose: `--` keeps jj from reading one that begins with a dash as an option of its
	// own. A name jj will not take costs that one bookmark, named in the notice, and never the
	// heads and the working copy the restore has already put back.
	var skipped []string
	for _, name := range slices.Sorted(maps.Keys(b.Bookmarks)) {
		if _, err := run(nil, "jj", "bookmark", "set", "-R", dir, "--ignore-working-copy", "--allow-backwards",
			"-r", b.Bookmarks[name], "--", name); err != nil {
			skipped = append(skipped, name)
		}
	}
	if _, err := run(nil, "jj", "edit", "-R", dir, b.Head); err != nil {
		return lost(err.Error())
	}
	// The temporary branches go through jj, which keeps each head visible; deleting the git refs
	// under it would have jj abandon the commits they made visible, taking the restored work with
	// them. Whatever jj did not export is deleted after, so none is left in the repository.
	for index := range b.Tips {
		if _, err := run(nil, "jj", "bookmark", "delete", "-R", dir, "--ignore-working-copy", "--", restoreRefPrefix+strconv.Itoa(index)); err != nil {
			return lost(err.Error())
		}
	}
	if _, err := run(workspace.RefUpdates("delete", restoreRefHeadPrefix, b.Tips), "git", "--git-dir="+gitDir, "update-ref", "--stdin"); err != nil {
		return lost(err.Error())
	}
	notice := fmt.Sprintf("the released workspace's saved work is restored: %d head(s), %d bookmark(s) and the working copy on %s, saved at %s",
		len(b.Tips), len(b.Bookmarks)-len(skipped), b.Head, b.At.UTC().Format(time.RFC3339))
	if len(skipped) > 0 {
		notice += fmt.Sprintf("; jj would not take %d bookmark name(s), which are not set: %s",
			len(skipped), strings.Join(skipped, ", "))
	}
	return true, notice
}

// restoreRefHeadPrefix is where the restore's temporary branches live while jj imports them: one
// branch per saved head, named by the head's place in the bundle (workspace.RefUpdates).
const restoreRefHeadPrefix = "refs/heads/" + restoreRefPrefix

// oneLine is a report as a notice carries it: its lines joined with "; ", so a notice is one line
// of the container's log however many lines git wrote.
func oneLine(report string) string {
	return strings.Join(workspace.OutputLines(report), "; ")
}
