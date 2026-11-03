//go:build e2e

// The Stage 4a harness's update-round check (INFRA-484): a running issue pod whose configuration
// has drifted is kept while the workers in it still hold unpushed work, and is replaced only once
// each of them has pushed that work and answered. The rig is live_test.go.
//
// The round itself is the daemon's (internal/daemon's reconcilePodUpdates, which that package's
// tests pin); what reaches a cluster is this package's two halves of it — PodDrift, which the
// round is opened on, and ReplacePod, which ends it — and this check drives the two as that sweep
// drives them. The harness plays the sweep here as it plays the daemon's machine for boot tokens,
// enrollments and sessions: it reads the drift, tasks every worker of the pod, holds the pod while
// one of them has not answered, and replaces it once both have.
//
// Every pod of the run runs a stub agent (live_test.go's header), which reads no task and answers
// nothing, so each worker's own side of the round is carried out as that role's agent in the pod
// (asAgent): it commits the work it holds in the issue's workspace, pushes it to the issue's
// branch through the clone's own credential helper — its own App's, never the harness's — and
// reports that branch's head, which is what the real op carries (`ready_for_update`,
// claim.ReadyForUpdateRequest's `head`, api's claims/ready-for-update). What the check then reads
// back from GitHub is that head, so a head no push ever landed fails it.

package sandbox

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// liveTurn is one worker's turn over the update round, as the observation record numbers its
// deliveries: the harness tasked the worker at from and recorded its answer at to. Nothing between
// those two marks may end that worker's process uncharged — that is a turn cut mid-work, the one
// thing the round exists to prevent (assertNoTurnCut). A turn still open has to 0.
type liveTurn struct {
	claim    *liveClaim
	from, to int
}

// pod-update: an update round over a live issue pod, from the drift that opens it to the
// replacement that ends it. Two workers of one issue run in one pod. The pod their launch just
// made reads no drift at all; the operator then moves the configuration that pod's Sandbox records
// it was built from (podShapeAnnotation), which no running pod can be changed to carry, and the
// runtime reads exactly that as the round's reason. Both workers are tasked. While one of them has
// not answered, the pod is not replaced: it keeps its uid and stays Running, and both workers'
// processes stay Alive. Each worker answers by pushing its work to the issue's branch and
// reporting that branch's head, and once both have, ReplacePod ends the round: each worker's
// process is observed ended at its recorded locator with Displaced set — the verdict the
// supervisor resumes charging nothing, never a turn cut — and each resumes on its own session into
// the one new pod, which reads no drift. What each worker reported is on GitHub, read with `git
// ls-remote` as the role's own App, and the last head is still the branch's after the pod that
// held the work is gone.
func (r *liveRig) checkPodUpdate() error {
	first, second := r.claim("update-a"), r.claim("update-b")
	workers := []*liveClaim{first, second}
	for _, c := range workers {
		if err := r.ensureRunning(c); err != nil {
			return err
		}
	}
	issue, name := first.issue, SandboxName(first.token)
	old := *first.loc
	if other := *second.loc; other.Sandbox.PodUID != old.Sandbox.PodUID {
		return fmt.Errorf("the two workers of issue %s run in pods %s and %s, want the issue's one pod", issue, old.Sandbox.PodUID, other.Sandbox.PodUID)
	}
	for _, c := range workers {
		drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: c.token, Locator: c.loc})
		if err != nil {
			return fmt.Errorf("PodDrift(%s): %w", c.name, err)
		}
		if len(drifted) > 0 {
			return fmt.Errorf("the pod %s a launch of this run just made reads drifted for %s: %s", short(old.Sandbox.PodUID), c.name, strings.Join(drifted, "; "))
		}
	}
	note("runtime", "PodDrift of the pod %s a launch just made: nothing, for %s and %s alike", short(old.Sandbox.PodUID), first.name, second.name)

	before, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	held := before.Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
	if held == "" {
		return fmt.Errorf("the Sandbox %s's pod template records no %s: every launch of this release records the configuration its pod was built from", name, podShapeAnnotation)
	}
	moved, reason, err := movedPodShape(held)
	if err != nil {
		return err
	}
	if err := r.patchPodShape(name, moved); err != nil {
		return err
	}
	note("operator", "patched Sandbox %s: its pod template now records %s=%s, a configuration its running pod %s was never built from",
		name, podShapeAnnotation, moved, short(old.Sandbox.PodUID))

	var drifted []string
	if err := r.poll(liveAdoptTimeout, "the runtime to read the moved shape of "+name, func() (bool, error) {
		var err error
		drifted, err = r.rt.PodDrift(r.ctx, runtime.Known{Claim: first.token, Locator: first.loc})
		return len(drifted) > 0, err
	}); err != nil {
		return err
	}
	for _, c := range workers {
		drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: c.token, Locator: c.loc})
		if err != nil {
			return fmt.Errorf("PodDrift(%s): %w", c.name, err)
		}
		if !slices.Equal(drifted, []string{reason}) {
			return fmt.Errorf("PodDrift(%s) names %v, want the moved disk alone (%s): nothing else of the pod moved", c.name, drifted, reason)
		}
	}
	note("runtime", "PodDrift of %s's pod, for both workers: %s — what the round is opened on", issue, reason)

	round := "pod-update-" + short(string(before.UID))
	dir, err := workspace.Location(TreeRoot, r.env.repo, issue)
	if err != nil {
		return err
	}
	mark := r.obs.mark()
	var turns []*liveTurn
	var tasked []string
	for _, c := range workers {
		turns = append(turns, &liveTurn{claim: c, from: mark})
		tasked = append(tasked, c.name)
		note("harness", "update round %s: %s is tasked to push its work and answer, as the sweep tasks every worker claim of issue %s", round, c.name, issue)
	}
	var inPod []string
	for _, c := range r.claims {
		if c.state == stateRunning && c.loc != nil && c.loc.Sandbox.PodUID == old.Sandbox.PodUID {
			inPod = append(inPod, c.name)
		}
	}
	slices.Sort(inPod)
	if want := slices.Sorted(slices.Values(tasked)); !slices.Equal(inPod, want) {
		return fmt.Errorf("the pod %s runs %v and the round tasked %v: a round tasks every live worker of its pod", short(old.Sandbox.PodUID), inPod, want)
	}

	headFirst, err := r.answerUpdate(first, dir)
	if err != nil {
		return err
	}
	turns[0].to = r.obs.mark()
	note("harness", "update round %s: %s answered, reporting %s as the head of %s", round, first.name, short(headFirst), dir.Bookmark)
	if err := r.holdDriftedPod(name, old.Sandbox.PodUID, workers, mark, second); err != nil {
		return err
	}

	headSecond, err := r.answerUpdate(second, dir)
	if err != nil {
		return err
	}
	turns[1].to = r.obs.mark()
	if headSecond == headFirst {
		return fmt.Errorf("%s reported the head %s %s had already pushed: each worker pushes the work it holds", second.name, short(headSecond), first.name)
	}
	note("harness", "update round %s: %s answered, reporting %s as the head of %s; the round waits on nobody", round, second.name, short(headSecond), dir.Bookmark)

	if err := r.rt.ReplacePod(r.ctx, runtime.Known{Claim: first.token, Locator: first.loc}, stillDue); err != nil {
		return fmt.Errorf("ReplacePod, which ends the round over pod %s: %w", short(old.Sandbox.PodUID), err)
	}
	note("runtime", "ReplacePod(%s): the round is over, so pod %s is ended", first.name, short(old.Sandbox.PodUID))
	for _, c := range workers {
		loc := *c.loc
		ended, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
			return o.Locator.Claim == c.token && (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess)
		})
		if !ok {
			return fmt.Errorf("no end of %s's %s within %s", c.name, loc.Incarnation, liveGoneLimit)
		}
		if !sameLocator(ended.Locator, loc) || !ended.Displaced {
			return fmt.Errorf("%s's end is %s at %s, Displaced %t: %s; want Displaced at its recorded %s",
				c.name, ended.Kind, ended.Locator.Incarnation, ended.Displaced, ended.Detail, loc.Incarnation)
		}
		note("runtime", "Observe: %s for %s's recorded %s, Displaced — %s", ended.Kind, c.name, short(loc.Incarnation), oneLine(firstLine(ended.Detail)))
		c.loc, c.state = nil, stateDead
	}

	var fresh runtime.Locator
	for _, c := range workers {
		since := time.Now()
		resumed, err := r.resume(c, c.session)
		if err != nil {
			return fmt.Errorf("the %s's Resume after the round: %w", c.name, err)
		}
		reg, err := r.awaitRunning(c, since)
		if err != nil {
			return err
		}
		if reg.hash != tokenHash(c.bootToken) {
			return fmt.Errorf("%s resumed registering %s, want its generation %d's own token", c.name, short(reg.hash), c.gen)
		}
		if resumed.Sandbox.PodUID == old.Sandbox.PodUID {
			return fmt.Errorf("%s resumed into the round's own pod %s, which ReplacePod ended", c.name, old.Sandbox.PodUID)
		}
		if fresh.Sandbox == nil {
			fresh = resumed
		} else if resumed.Sandbox.PodUID != fresh.Sandbox.PodUID {
			return fmt.Errorf("%s resumed into pod %s, want the one pod %s the round's first resume made", c.name, resumed.Sandbox.PodUID, fresh.Sandbox.PodUID)
		}
		note("runtime", "Resume(%s, %s) returned %s in the new pod %s, registered at generation %d",
			c.name, c.session, short(resumed.Incarnation), short(resumed.Sandbox.PodUID), c.gen)
	}
	if err := r.assertNoTurnCut(turns); err != nil {
		return err
	}
	note("harness", "no worker was observed Gone or NotRecordedProcess without Displaced between its task and its answer: the round cut no turn")

	for _, c := range workers {
		drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: c.token, Locator: c.loc})
		if err != nil {
			return fmt.Errorf("PodDrift(%s) after the round: %w", c.name, err)
		}
		if len(drifted) > 0 {
			return fmt.Errorf("the pod %s the round made reads drifted for %s: %s", short(fresh.Sandbox.PodUID), c.name, strings.Join(drifted, "; "))
		}
	}
	note("runtime", "PodDrift of the pod %s the round made: nothing — the replacement carries the configuration a pod made now is built from", short(fresh.Sandbox.PodUID))

	kept, err := r.branchHead(first, dir)
	if err != nil {
		return err
	}
	if kept != headSecond {
		return fmt.Errorf("%s of %s is at %s after the replacement, want the last head %s its workers reported", dir.Bookmark, r.env.repo, kept, headSecond)
	}
	note("operator", "as %s's own agent in the new pod: git ls-remote names %s at %s, the head %s reported — the work outlived the pod that held it",
		first.name, dir.Bookmark, short(kept), second.name)

	for _, c := range workers {
		if err := r.suspend(c); err != nil {
			return fmt.Errorf("suspend %s after the check: %w", c.name, err)
		}
	}
	note("runtime", "%s and %s suspended; pod %s stays", first.name, second.name, short(fresh.Sandbox.PodUID))
	return nil
}

// answerUpdate is one worker's answer to the update task, carried out as that role's own agent
// (asAgent), since the pod's stub agent reads no task: it writes the work it holds into the
// issue's workspace, commits it there, pushes it to the issue's branch through the clone's own
// `legion git-credential` helper — its own App's credentials, never the harness's — and reports
// that branch's head at origin, which is the `head` the op carries. The head it reports is read
// straight back from GitHub as the same App, so a head no push landed fails here. The branch is
// deleted when the run ends, and only while it still names the commit this push put on it
// (deleteBranchAt), so a run never deletes a branch it did not write.
func (r *liveRig) answerUpdate(c *liveClaim, dir workspace.Workspace) (string, error) {
	url := "https://github.com/" + r.env.repo.String()
	work := fmt.Sprintf("%s's work on %s, unpushed until its update task, at %s", c.role, c.issue, time.Now().UTC().Format(time.RFC3339Nano))
	script := strings.Join([]string{
		"set -e",
		"cd " + shellQuote(dir.Dir),
		"mkdir -p .legion-e2e",
		"printf '%s\\n' " + shellQuote(work) + " >" + shellQuote(".legion-e2e/"+string(c.role)+".txt"),
		// The author is the one the shim adopts a working copy with (AdoptWorkingCopy), which the
		// agent's own environment does not carry.
		"JJ_USER=" + shellQuote(r.identity.Name) + " JJ_EMAIL=" + shellQuote(r.identity.Email) +
			" jj commit -m " + shellQuote(runCommitMessage(dir.Bookmark)) + " >/dev/null",
		"head=$(jj log -r @- --no-graph -T commit_id --ignore-working-copy)",
		"git --git-dir=" + shellQuote(dir.Clone+"/.git") + " push --quiet " + shellQuote(url) + " \"$head:refs/heads/" + dir.Bookmark + "\" >/dev/null",
		`printf '%s\n' "$head"`,
	}, "\n")
	out, err := r.asAgent(c, script)
	if err != nil {
		return "", fmt.Errorf("%s's push of its work to %s: %v: %s", c.name, dir.Bookmark, err, oneLine(out))
	}
	head := strings.TrimSpace(out)
	if len(head) != 40 || strings.Trim(head, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%s's push reported %q as the head of %s, want the commit id it pushed", c.name, oneLine(out), dir.Bookmark)
	}
	r.t.Cleanup(func() {
		if err := r.deleteBranchAt(dir.Bookmark, head); err != nil {
			fmt.Printf("teardown: could not delete branch %s of %s: %v\n", dir.Bookmark, r.env.repo, err)
		}
	})
	note("operator", "as %s's own agent: jj commit in %s, then git push %s %s:refs/heads/%s through the clone's own credential helper",
		c.name, dir.Dir, url, short(head), dir.Bookmark)
	read, err := r.branchHead(c, dir)
	if err != nil {
		return "", err
	}
	if read != head {
		return "", fmt.Errorf("%s reported %s as the head of %s, and GitHub names %q", c.name, head, dir.Bookmark, read)
	}
	note("operator", "as %s's own agent: git ls-remote %s refs/heads/%s: %s — the head it reported is on GitHub", c.name, url, dir.Bookmark, short(read))
	return head, nil
}

// branchHead is the commit GitHub names for the issue's branch, read from inside the pod as c's
// own App (asAgent, through the clone's own credential helper), and "" when the branch is not
// there. It is the read the `head` of an answer is held to, made by the identity that pushed it
// and not by the harness's.
func (r *liveRig) branchHead(c *liveClaim, dir workspace.Workspace) (string, error) {
	ref := "refs/heads/" + dir.Bookmark
	url := "https://github.com/" + r.env.repo.String()
	out, err := r.asAgent(c, "git --git-dir="+shellQuote(dir.Clone+"/.git")+" ls-remote "+shellQuote(url)+" "+shellQuote(ref))
	if err != nil {
		return "", fmt.Errorf("%s's git ls-remote of %s: %v: %s", c.name, dir.Bookmark, err, oneLine(out))
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || fields[1] != ref {
		return "", fmt.Errorf("%s's git ls-remote of %s answered %q, want one %s line", c.name, dir.Bookmark, oneLine(out), ref)
	}
	return fields[0], nil
}

// holdDriftedPod holds the drifted pod to what an open round promises: while one worker has not
// answered, nothing replaces the pod. Over liveSettle no process in it is observed ending or
// moving, the pod keeps its uid and stays Running with its Sandbox Running, and every worker in it
// probes Alive — the drift is read and the pod is kept.
func (r *liveRig) holdDriftedPod(name, uid string, workers []*liveClaim, mark int, waiting *liveClaim) error {
	time.Sleep(liveSettle)
	for _, o := range r.obs.since(mark) {
		for _, c := range workers {
			if o.Locator.Claim == c.token && o.Kind != runtime.Alive && o.Kind != runtime.Uncertain {
				return fmt.Errorf("%s was observed %s at %s while the round still waited on %s: %s",
					c.name, o.Kind, o.Locator.Incarnation, waiting.name, oneLine(firstLine(o.Detail)))
			}
		}
	}
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if string(pod.UID) != uid || pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("the drifted pod of %s is uid %s %s while the round waited on %s, want the running %s", name, pod.UID, pod.Status.Phase, waiting.name, uid)
	}
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if s.mode() != modeRunning {
		return fmt.Errorf("the Sandbox %s is %s while the round waited on %s, want Running", name, s.mode(), waiting.name)
	}
	for _, c := range workers {
		obs, err := r.rt.Probe(r.ctx, *c.loc)
		if err != nil || obs.Kind != runtime.Alive {
			return fmt.Errorf("%s probes %s while the round waited on %s: %v", c.name, obs.Kind, waiting.name, err)
		}
	}
	note("runtime", "%s has not answered: after %s the pod %s is the same uid and Running, its Sandbox Running, and every worker in it probes Alive",
		waiting.name, liveSettle, short(uid))
	return nil
}

// assertNoTurnCut is the promise the round exists for, which no other check of the rig makes: no
// worker was ended uncharged while it was working on the update task. An end observed without
// Displaced — Gone, or the NotRecordedProcess the supervisor treats identically — between the mark
// where a worker was tasked and the mark where it answered is a turn the daemon cut, and with it
// whatever that worker had not pushed.
func (r *liveRig) assertNoTurnCut(turns []*liveTurn) error {
	for _, turn := range turns {
		until := turn.to
		if until == 0 {
			until = r.obs.mark()
		}
		for _, o := range r.obs.since(turn.from) {
			if o.seq > until || o.Locator.Claim != turn.claim.token || o.Displaced {
				continue
			}
			if o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess {
				return fmt.Errorf("%s was observed %s without Displaced at %s while it held the update task: %s — a turn the round cut",
					turn.claim.name, o.Kind, o.Locator.Incarnation, oneLine(firstLine(o.Detail)))
			}
		}
	}
	return nil
}

// patchPodShape replaces the configuration the Sandbox name's pod template records its pod was
// built from (podShapeAnnotation) with shape, as an operator's change to the deployment's pod
// configuration reaches every Sandbox written after it. The running pod is untouched: its own
// build is fixed, which is exactly what makes it drifted.
func (r *liveRig) patchPodShape(name, shape string) error {
	// The key holds no "~", so only its "/" is escaped for the pointer (RFC 6901).
	pointer := "/spec/podTemplate/metadata/annotations/" + strings.ReplaceAll(podShapeAnnotation, "/", "~1")
	body, err := json.Marshal([]map[string]any{{"op": "replace", "path": pointer, "value": shape}})
	if err != nil {
		return err
	}
	_, err = r.kubectl("patch", "sandbox", name, "--type=json", "-p", string(body))
	return err
}

// liveFleetRound is one issue's side of the fleet-wide round (pod-update-fleet): the workers of
// that issue's one pod, the Sandbox and the pod they run in, the workspace they push from, the
// turns the harness opened over them, and the head the last of them reported.
type liveFleetRound struct {
	issue, sandbox, uid string
	workers             []*liveClaim
	dir                 workspace.Workspace
	turns               []*liveTurn
	head                string
}

// names is the round's workers as a line an operator reads names them.
func (round *liveFleetRound) names() []string {
	var names []string
	for _, c := range round.workers {
		names = append(names, c.name)
	}
	return names
}

// pod-update-fleet: one change to the pod configuration drifts every pod of the deployment at
// once (INFRA-419), and each issue's pod is still a round of its own. Two issues run here, each
// with an architect and a worker in its own pod, and each pod's Sandbox records the one
// configuration this runtime builds an issue pod from. The operator moves that configuration on
// both Sandboxes in one step — the same moved shape on each, as one deployment change reaches
// every pod written after it — and the runtime then reads the same one reason for all four
// workers.
//
// What the round then promises across the fleet is that each pod waits for its own workers: the
// second issue's two answer, its pod is replaced, and the first issue's pod is untouched by that
// — over a settle window nothing in it ends or moves, it keeps its uid and stays Running, and
// both its workers still probe Alive. Only when its own two answer is it replaced in turn. No
// architect — a root's own claim — is ended uncharged between its task and its answer
// (assertNoTurnCut), which is this layer's whole say on a round leaving a root where it was: what
// a completion, a park or a wait on a person does to an issue record is the daemon's, and
// internal/daemon's own round tests hold it. Each worker's pushed work is on GitHub after the pod
// that held it is gone, read as that role's own App.
func (r *liveRig) checkPodUpdateFleet() error {
	var rounds []*liveFleetRound
	for _, pair := range [][]string{{"update-a", "update-b"}, {"fleet-a", "fleet-b"}} {
		workers := []*liveClaim{r.claim(pair[0]), r.claim(pair[1])}
		for _, c := range workers {
			if err := r.ensureRunning(c); err != nil {
				return err
			}
		}
		first, second := workers[0], workers[1]
		if first.loc.Sandbox.PodUID != second.loc.Sandbox.PodUID {
			return fmt.Errorf("the two workers of issue %s run in pods %s and %s, want the issue's one pod",
				first.issue, first.loc.Sandbox.PodUID, second.loc.Sandbox.PodUID)
		}
		dir, err := workspace.Location(TreeRoot, r.env.repo, first.issue)
		if err != nil {
			return err
		}
		rounds = append(rounds, &liveFleetRound{
			issue: first.issue, sandbox: SandboxName(first.token), uid: first.loc.Sandbox.PodUID, workers: workers, dir: dir,
		})
	}
	if rounds[0].uid == rounds[1].uid {
		return fmt.Errorf("both issues run in pod %s, want one pod each: a round is an issue's pod", short(rounds[0].uid))
	}
	for _, round := range rounds {
		for _, c := range round.workers {
			drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: c.token, Locator: c.loc})
			if err != nil {
				return fmt.Errorf("PodDrift(%s): %w", c.name, err)
			}
			if len(drifted) > 0 {
				return fmt.Errorf("the pod %s of issue %s reads drifted for %s before any change: %s",
					short(round.uid), round.issue, c.name, strings.Join(drifted, "; "))
			}
		}
	}
	note("runtime", "PodDrift of the two issue pods %s (%s) and %s (%s): nothing, for every worker of each",
		short(rounds[0].uid), rounds[0].issue, short(rounds[1].uid), rounds[1].issue)

	// One change, every pod. Both Sandboxes record the one configuration this runtime builds an
	// issue pod from, so the one moved shape is what the deployment's change leaves on each.
	var held string
	for _, round := range rounds {
		s, err := r.getSandbox(round.sandbox)
		if err != nil {
			return err
		}
		shape := s.Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
		if shape == "" {
			return fmt.Errorf("the Sandbox %s's pod template records no %s: every launch of this release records the configuration its pod was built from",
				round.sandbox, podShapeAnnotation)
		}
		if held == "" {
			held = shape
		} else if shape != held {
			return fmt.Errorf("the two issue pods were built from two configurations (%s and %s); one deployment drifts every pod from one of them", held, shape)
		}
	}
	moved, reason, err := movedPodShape(held)
	if err != nil {
		return err
	}
	for _, round := range rounds {
		if err := r.patchPodShape(round.sandbox, moved); err != nil {
			return err
		}
	}
	note("operator", "patched the Sandboxes %s and %s with the one moved configuration %s=%s: one change, every pod of both issues",
		rounds[0].sandbox, rounds[1].sandbox, podShapeAnnotation, moved)

	for _, round := range rounds {
		first := round.workers[0]
		if err := r.poll(liveAdoptTimeout, "the runtime to read the moved shape of "+round.sandbox, func() (bool, error) {
			drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: first.token, Locator: first.loc})
			return len(drifted) > 0, err
		}); err != nil {
			return err
		}
		for _, c := range round.workers {
			drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: c.token, Locator: c.loc})
			if err != nil {
				return fmt.Errorf("PodDrift(%s): %w", c.name, err)
			}
			if !slices.Equal(drifted, []string{reason}) {
				return fmt.Errorf("PodDrift(%s) names %v, want the one reason the fleet's change left (%s)", c.name, drifted, reason)
			}
		}
	}
	note("runtime", "PodDrift of both issue pods, for all four workers: %s — the one reason both rounds are opened on", reason)

	// Every worker of every drifted pod is tasked, as the sweep tasks every worker claim of every
	// issue whose pod has drifted.
	mark := r.obs.mark()
	for _, round := range rounds {
		for _, c := range round.workers {
			round.turns = append(round.turns, &liveTurn{claim: c, from: mark})
		}
		note("harness", "update round over %s's pod %s: %s are tasked to push their work and answer",
			round.issue, short(round.uid), strings.Join(round.names(), " and "))
	}

	// The second issue answers and its pod is replaced. The first issue's pod is not: its own
	// workers have answered nothing, and nothing of another issue's replacement reaches it.
	second, first := rounds[1], rounds[0]
	for i, c := range second.workers {
		head, err := r.answerUpdate(c, second.dir)
		if err != nil {
			return err
		}
		second.turns[i].to = r.obs.mark()
		second.head = head
		note("harness", "update round over %s's pod %s: %s answered, reporting %s as the head of %s",
			second.issue, short(second.uid), c.name, short(head), second.dir.Bookmark)
	}
	fresh, err := r.replaceFleetPod(second, mark)
	if err != nil {
		return err
	}
	if err := r.holdDriftedPod(first.sandbox, first.uid, first.workers, mark, first.workers[0]); err != nil {
		return err
	}
	note("harness", "%s's pod %s was replaced and %s's pod %s was not: a round ends the pod of its own issue alone",
		second.issue, short(second.uid), first.issue, short(first.uid))

	// The first issue's own workers answer, and its pod is replaced in turn.
	for i, c := range first.workers {
		head, err := r.answerUpdate(c, first.dir)
		if err != nil {
			return err
		}
		first.turns[i].to = r.obs.mark()
		first.head = head
		note("harness", "update round over %s's pod %s: %s answered, reporting %s as the head of %s",
			first.issue, short(first.uid), c.name, short(head), first.dir.Bookmark)
	}
	firstFresh, err := r.replaceFleetPod(first, mark)
	if err != nil {
		return err
	}
	if firstFresh.Sandbox.PodUID == fresh.Sandbox.PodUID {
		return fmt.Errorf("both issues resumed into pod %s, want one pod each", short(fresh.Sandbox.PodUID))
	}

	var turns []*liveTurn
	for _, round := range rounds {
		turns = append(turns, round.turns...)
	}
	if err := r.assertNoTurnCut(turns); err != nil {
		return err
	}
	note("harness", "no worker of either issue — each issue's architect among them — was observed Gone or NotRecordedProcess without Displaced between its task and its answer: the fleet's round cut no turn")

	for _, round := range rounds {
		kept, err := r.branchHead(round.workers[0], round.dir)
		if err != nil {
			return err
		}
		if kept != round.head {
			return fmt.Errorf("%s of %s is at %s after the replacement, want the last head %s its workers reported", round.dir.Bookmark, r.env.repo, kept, round.head)
		}
		note("operator", "as %s's own agent in the new pod: git ls-remote names %s at %s, the head its last worker reported — the work outlived the pod that held it",
			round.workers[0].name, round.dir.Bookmark, short(kept))
	}

	for _, round := range rounds {
		for _, c := range round.workers {
			if err := r.suspend(c); err != nil {
				return fmt.Errorf("suspend %s after the check: %w", c.name, err)
			}
		}
	}
	note("runtime", "the four workers of %s and %s suspended; the two pods the rounds made stay", first.issue, second.issue)
	return nil
}

// replaceFleetPod ends one issue's round: ReplacePod on the pod its workers have all answered for,
// each worker's process observed ended at its recorded locator with Displaced set — the verdict
// the supervisor resumes charging nothing, never a turn cut — and each resumed on its own session
// into the one new pod, which reads no drift. It answers that new pod's locator.
func (r *liveRig) replaceFleetPod(round *liveFleetRound, mark int) (runtime.Locator, error) {
	first := round.workers[0]
	if err := r.rt.ReplacePod(r.ctx, runtime.Known{Claim: first.token, Locator: first.loc}, stillDue); err != nil {
		return runtime.Locator{}, fmt.Errorf("ReplacePod, which ends the round over %s's pod %s: %w", round.issue, short(round.uid), err)
	}
	note("runtime", "ReplacePod(%s): %s's round is over, so pod %s is ended", first.name, round.issue, short(round.uid))
	for _, c := range round.workers {
		loc := *c.loc
		ended, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
			return o.Locator.Claim == c.token && (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess)
		})
		if !ok {
			return runtime.Locator{}, fmt.Errorf("no end of %s's %s within %s", c.name, loc.Incarnation, liveGoneLimit)
		}
		if !sameLocator(ended.Locator, loc) || !ended.Displaced {
			return runtime.Locator{}, fmt.Errorf("%s's end is %s at %s, Displaced %t: %s; want Displaced at its recorded %s",
				c.name, ended.Kind, ended.Locator.Incarnation, ended.Displaced, ended.Detail, loc.Incarnation)
		}
		note("runtime", "Observe: %s for %s's recorded %s, Displaced — %s", ended.Kind, c.name, short(loc.Incarnation), oneLine(firstLine(ended.Detail)))
		c.loc, c.state = nil, stateDead
	}
	var fresh runtime.Locator
	for _, c := range round.workers {
		since := time.Now()
		resumed, err := r.resume(c, c.session)
		if err != nil {
			return runtime.Locator{}, fmt.Errorf("the %s's Resume after %s's round: %w", c.name, round.issue, err)
		}
		if _, err := r.awaitRunning(c, since); err != nil {
			return runtime.Locator{}, err
		}
		if resumed.Sandbox.PodUID == round.uid {
			return runtime.Locator{}, fmt.Errorf("%s resumed into the round's own pod %s, which ReplacePod ended", c.name, round.uid)
		}
		if fresh.Sandbox == nil {
			fresh = resumed
		} else if resumed.Sandbox.PodUID != fresh.Sandbox.PodUID {
			return runtime.Locator{}, fmt.Errorf("%s resumed into pod %s, want the one pod %s the round's first resume made", c.name, resumed.Sandbox.PodUID, fresh.Sandbox.PodUID)
		}
		note("runtime", "Resume(%s, %s) returned %s in the new pod %s, registered at generation %d",
			c.name, c.session, short(resumed.Incarnation), short(resumed.Sandbox.PodUID), c.gen)
	}
	for _, c := range round.workers {
		drifted, err := r.rt.PodDrift(r.ctx, runtime.Known{Claim: c.token, Locator: c.loc})
		if err != nil {
			return runtime.Locator{}, fmt.Errorf("PodDrift(%s) after %s's round: %w", c.name, round.issue, err)
		}
		if len(drifted) > 0 {
			return runtime.Locator{}, fmt.Errorf("the pod %s %s's round made reads drifted for %s: %s",
				short(fresh.Sandbox.PodUID), round.issue, c.name, strings.Join(drifted, "; "))
		}
	}
	note("runtime", "PodDrift of the pod %s %s's round made: nothing — the replacement carries the configuration a pod made now is built from",
		short(fresh.Sandbox.PodUID), round.issue)
	return fresh, nil
}
