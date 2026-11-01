package sandbox

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The reclaim paragraph of docs/kubernetes.md is an operator's whole remedy for an issue volume
// near full, and its claim is an accounting one: every directory this layout puts on the volume is
// on its delete list or in one of its exemptions. A path the renderer creates and neither list
// carries is the failure this guards — the operator deletes the five paths, the volume is still
// full, and the bytes are in a directory nobody wrote down. Six fixes to this paragraph in one
// review were each a rendered path missing from it, found by a reader rather than by a check.
//
// The two lists are read as lists: a delete-list path is a bullet's whole content, an exempt path
// is the one a bullet opens with, and a path named later inside a bullet's prose is on neither
// list. That is not fastidiousness. An earlier version of this test asked only whether the
// paragraph mentioned a path anywhere between its markers, and it passed with /legion/uv/cache
// struck from the delete list, because an exemption's reason happens to mention it — the check
// could not fail where it mattered, which is the defect it was written against.
//
// A delete-list path covers what sits under it, which is how `/legion/go` accounts for GOCACHE and
// GOMODCACHE, and that coverage is the hole a coarsened bullet used to slip through: `/legion/uv`
// in place of `/legion/uv/cache` accounted for every rendered path while telling the operator to
// delete `/legion/uv/python/<issue>`, the interpreters an exemption exists to keep.
// TestTheReclaimParagraphNeverDeletesWhatItExempts closes that one.
//
// What the checks cannot do: they read the lists' structure, not their grounds. They can say that
// /legion/scratch/data is accounted for; they cannot say the sentence giving the reason is true,
// and an earlier head passed a check like this one carrying an exemption whose stated reason the
// code refuted. They also do not care which of the two lists carries a path, so a path moved from
// the delete list to the exemptions, or the reverse, still passes. Both are a reader's job.
//
// The markers bound the search to that paragraph. A rendered path mentioned anywhere else in a
// 4000-line document would otherwise satisfy the check while the remedy itself stays silent about
// it, which is the same miss wearing a pass.
const (
	reclaimParagraphStart = "The remedy for a volume near full is manual"
	reclaimParagraphEnd   = "As for the rest:"
	reclaimDeleteHeading  = "Delete list"
	reclaimExemptHeading  = "Exempt, each for a reason"
)

// Every directory roleEnvironment and the pod's uv variables put on the issue's volume is on the
// delete list or in an exemption of docs/kubernetes.md's reclaim paragraph. The rendered set comes
// from the renderer, never from a list written beside it: a list would have to be edited by whoever
// adds a cache, which is exactly the edit that gets forgotten.
func TestTheReclaimParagraphNamesEveryDirectoryTheRendererPutsOnTheIssueVolume(t *testing.T) {
	const issue = "LEGION-1234"
	const role = claim.RoleImplementer

	deleted, exempt := reclaimParagraphLists(t)
	for _, dir := range issueVolumeDirectories(issue, role) {
		// A per-role or per-issue directory reaches the prose as a placeholder, since no document can
		// name every role and every issue. Rewrite the rendered value's last segment to the form the
		// paragraph uses rather than matching it loosely, so an unrelated path that happens to share a
		// prefix cannot stand in for it.
		documented := dir
		switch dir {
		case roleStateHome(role):
			documented = path.Dir(dir) + "/<role>"
		case uvPythonDir(issue):
			documented = path.Dir(dir) + "/<issue>"
		}
		onDeleteList := namesPathOrAncestor(deleted, documented)
		isExempt := namesPathOrAncestor(exempt, documented)
		switch {
		case onDeleteList && isExempt:
			t.Errorf("docs/kubernetes.md's reclaim paragraph both deletes and exempts %s: the operator is "+
				"told to remove it and to leave it, so one of the two bullets is wrong", documented)
		case !onDeleteList && !isExempt:
			t.Errorf("docs/kubernetes.md's reclaim paragraph accounts for %s in neither list, though the "+
				"renderer puts it on the issue's volume: it belongs on the delete list or in an exemption, "+
				"and a mention inside an exemption's reason is neither", documented)
		}
	}
}

// No path the delete list names is an ancestor of, or equal to, a path the exemptions hold back.
// This is a property of the two lists rather than of a rendered directory, so it is its own test:
// the check above walks what the renderer puts on the volume and would never visit the pair that
// collides. An operator follows the remedy literally — `rm -rf` on each delete-list bullet — so a
// bullet written one segment too coarse removes the exemption underneath it while the same
// paragraph is still telling the reader to keep it, and the accounting check sees only that every
// rendered path is covered.
//
// The walk goes all the way up to TreeRoot, unlike namesPathOrAncestor, which stops short of it:
// TreeRoot is no coverage for a rendered path, but a delete list that names it would still have the
// operator remove every exemption in the paragraph, and that is the harm this test is about.
func TestTheReclaimParagraphNeverDeletesWhatItExempts(t *testing.T) {
	deleted, exempt := reclaimParagraphLists(t)
	for keep := range exempt {
		for p := keep; strings.HasPrefix(p, TreeRoot); p = path.Dir(p) {
			if !deleted[p] {
				continue
			}
			t.Errorf("docs/kubernetes.md's reclaim paragraph deletes %s, which holds %s that the same "+
				"paragraph exempts: an operator removing the delete-list bullet takes the exemption with "+
				"it, so the bullet is a segment too coarse", p, keep)
		}
	}
}

// issueVolumeDirectories is what a role and the pod are told to write under the issue's
// volume, taken from the two renderers that tell them: roleEnvironment, which carries the role's
// temporary directory, its XDG homes and the three tool caches, and the issue pod's agentEnv, which
// carries uv's cache and Pythons. A value under TreeRoot is on that volume, since TreeRoot is where
// the issue's claim is mounted (issueVolume); XDG_CONFIG_HOME, under the pod's in-memory home, is
// outside it and drops out here without being listed.
func issueVolumeDirectories(issue string, role claim.Role) []string {
	l := launch{
		spec:      runtime.SpawnSpec{Tree: "legion", Issue: issue},
		workspace: TreeRoot + "/workspaces/owner/repo/" + issue,
	}
	var dirs []string
	for _, env := range append(roleEnvironment(role), issuePod{}.agentEnv(l)...) {
		// LEGION_WORKSPACE is on the volume but is not a directory this layout creates to absorb what a
		// tool writes: it is the issue's checkout, the work itself, and the remedy deletes nothing in
		// it. Every other variable pointing under TreeRoot is scratch and belongs in the accounting.
		if env.Name == "LEGION_WORKSPACE" || !strings.HasPrefix(env.Value, TreeRoot+"/") {
			continue
		}
		dirs = append(dirs, env.Value)
	}
	return dirs
}

// reclaimParagraphLists reads the paragraph's two markdown lists: the paths the delete list tells
// the operator to remove, and the paths the exemptions hold back. A delete-list path is a bullet's
// entire content and an exempt path is the one its bullet opens with, so the paths an exemption's
// reason goes on to mention — other halves, neighbouring caches — land in neither set and cannot
// stand in for a bullet that is missing.
func reclaimParagraphLists(t *testing.T) (deleted, exempt map[string]bool) {
	t.Helper()
	// The document is at the repository root, five directories above this package. Read it, and fail
	// when it cannot be read: a skip here would be an absence that reads exactly like a pass, and
	// this check exists because a silent absence is what the paragraph kept shipping.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	lines := strings.Split(string(doc), "\n")
	// The markers are prose and the document is hard-wrapped, so they straddle line ends: find them
	// in a flattened copy and carry the match back to the line it starts on. The lists themselves are
	// read from the lines, because a bullet is a shape of a line and flattening would erase it.
	flat, lineOf := flattenKeepingLines(lines)
	startOffset := strings.Index(flat, reclaimParagraphStart)
	if startOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q: the reclaim paragraph was reworded, and this check "+
			"stops checking the moment it cannot find it", reclaimParagraphStart)
	}
	endOffset := strings.Index(flat[startOffset:], reclaimParagraphEnd)
	if endOffset < 0 {
		t.Fatalf("docs/kubernetes.md has no %q after %q: the reclaim paragraph's end moved, and an "+
			"unbounded search would pass on a mention anywhere in the document",
			reclaimParagraphEnd, reclaimParagraphStart)
	}
	region := lines[lineOf[startOffset] : lineOf[startOffset+endOffset]+1]

	deleted, exempt = map[string]bool{}, map[string]bool{}
	var into map[string]bool
	var wholeBullet bool
	// The loop advances i itself over a wrapped bullet's continuation lines, so it stays explicit.
	for i := 0; i < len(region); i++ {
		line := strings.TrimRight(region[i], " ")
		switch {
		case strings.HasPrefix(line, reclaimDeleteHeading):
			into, wholeBullet = deleted, true
			continue
		case strings.HasPrefix(line, reclaimExemptHeading):
			into, wholeBullet = exempt, false
			continue
		}
		if into == nil || !strings.HasPrefix(line, "- ") {
			continue
		}
		// A wrapped bullet continues on the indented lines under it, and they are part of its content:
		// a delete-list bullet that carries prose is not a bare path, whatever its first line looks
		// like.
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		for i+1 < len(region) && strings.HasPrefix(region[i+1], " ") && strings.TrimSpace(region[i+1]) != "" {
			i++
			bullet += " " + strings.TrimSpace(region[i])
		}
		if named, ok := bulletPath(bullet, wholeBullet); ok {
			into[named] = true
		}
	}

	if len(deleted)+len(exempt) == 0 {
		t.Fatalf("the reclaim paragraph between %q and %q names no path at all, so the markers are no "+
			"longer bounding the paragraph they were written for", reclaimParagraphStart, reclaimParagraphEnd)
	}
	// An empty list is this check failing open: the heading is still there, the parse matched nothing,
	// and every rendered path would then be reported against a list that is not being read.
	if len(deleted) == 0 {
		t.Fatalf("the reclaim paragraph's %q list yields no path: its bullets are no longer one "+
			"backticked absolute path each, and nothing is being held to it", reclaimDeleteHeading)
	}
	if len(exempt) == 0 {
		t.Fatalf("the reclaim paragraph's %q list yields no path: its bullets no longer open with a "+
			"backticked absolute path, and nothing is being held to it", reclaimExemptHeading)
	}
	return deleted, exempt
}

// bulletPath is the path a bullet puts on its list. A delete-list bullet is the path and nothing
// else, since the operator removes what the bullet says; an exemption opens with the path it holds
// back and then argues for it, so only the opening code span counts. TreeRoot itself is a path a
// bullet can name: it covers no rendered directory (namesPathOrAncestor stops short of it), but a
// delete list naming it swallows every exemption, and that is a reading the lists must be held to.
func bulletPath(bullet string, whole bool) (string, bool) {
	if !strings.HasPrefix(bullet, "`") {
		return "", false
	}
	named, rest, closed := strings.Cut(bullet[1:], "`")
	if !closed || !strings.HasPrefix(named, TreeRoot+"/") && named != TreeRoot {
		return "", false
	}
	if whole && rest != "" {
		return "", false
	}
	return named, true
}

// flattenKeepingLines collapses the document's whitespace so prose that was hard-wrapped reads as
// one line, and records for every byte of the result the line it came from, which is what turns a
// marker found in the flat text back into a bound on the lines the lists live in.
func flattenKeepingLines(lines []string) (string, []int) {
	var flat strings.Builder
	lineOf := make([]int, 0, len(lines)*8)
	for i, line := range lines {
		for _, field := range strings.Fields(line) {
			if flat.Len() > 0 {
				flat.WriteByte(' ')
				lineOf = append(lineOf, i)
			}
			flat.WriteString(field)
			for range len(field) {
				lineOf = append(lineOf, i)
			}
		}
	}
	return flat.String(), lineOf
}

// namesPathOrAncestor accepts a directory a list covers by naming a parent of it, which is how the
// remedy handles the Go pair: it deletes /legion/go, and GOCACHE and GOMODCACHE are under it.
// TreeRoot itself is not an ancestor that counts, since deleting the tree root is not a remedy this
// document offers and every path in the prose starts with it.
func namesPathOrAncestor(named map[string]bool, dir string) bool {
	for p := dir; strings.HasPrefix(p, TreeRoot+"/"); p = path.Dir(p) {
		if named[p] {
			return true
		}
	}
	return false
}
