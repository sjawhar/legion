//go:build e2e

// The Stage 4a harness's upgrade check: an issue pod a daemon of the release before one container
// made, six role containers each running a role, is replaced by this release's runtime at its first
// launch, every resident role resumed in the one-container pod and charged one generation. The rig
// is live_test.go.

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// previousLaunch is one claim the previous release's fixture launched
// (testdata/previouslayout/fixture_test.go.txt), as it writes it.
type previousLaunch struct {
	Name    string          `json:"name"`
	Tree    string          `json:"tree"`
	Issue   string          `json:"issue"`
	Role    claim.Role      `json:"role"`
	Token   claim.Token     `json:"token"`
	Gen     uint64          `json:"generation"`
	Boot    string          `json:"bootToken"`
	Marker  string          `json:"marker"`
	Locator runtime.Locator `json:"locator"`
	Pod     string          `json:"pod"`
	Names   []string        `json:"containers"`
}

// previousLayoutBlocked reports why previous-layout cannot run: the script builds the previous
// release's fixture and names it with the previous release's worker image, both or neither.
func previousLayoutBlocked(r *liveRig) string {
	if r.env.previousHarness != "" && r.env.previousImage != "" {
		return ""
	}
	return "LEGION_E2E_PREVIOUS_HARNESS and LEGION_E2E_PREVIOUS_IMAGE are unset: scripts/e2e/stage4a-sandbox-runtime.sh builds the previous release's fixture only when LEGION_E2E_PREVIOUS_IMAGE names a worker image of main from before one container"
}

// adopt makes gen, with its boot token, claim c's current generation, as a daemon's store carries a
// claim's generation and token across an upgrade: its shim's hello is accepted, and the next mint
// is gen+1.
func (g *registry) adopt(c claim.Token, gen uint64, token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current[c] = gen
	g.byToken[token] = minted{claim: c, gen: gen, hash: tokenHash(token), armed: true}
}

// previous-layout: with this runtime stopped, the previous release's own runtime and rig
// (LEGION_E2E_PREVIOUS_HARNESS, built from LEGION_E2E_PREVIOUS_IMAGE's revision with the fixture) launch issue S4A-9's
// architect and tester in one issue pod of six role containers, on the previous release's worker
// image, and exit, leaving both running, as a deployment upgrading onto this release has them. Their
// locators are taken over as store migration 0051 rewrites them (container `roles`) and this
// runtime starts on them: its first observation of each is StaleAddress naming the six containers,
// and nothing else of theirs. The architect's Resume replaces the pod with one `roles` container;
// the tester's old process then reads Displaced, uncharged, and its Resume starts in that same new
// pod. Each claim is charged exactly one generation, its previous one plus one, registers exactly
// once after the upgrade, and its marker holds its previous and its resumed incarnation.
func (r *liveRig) checkPreviousLayout() error {
	r.stopRuntime()
	note("runtime", "listener and runtime closed for the previous release's fixture")
	out := filepath.Join(r.env.work, "previous-layout.json")
	work := filepath.Join(r.env.work, "previous-layout")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	name := ""
	{
		token, err := claim.NewToken(r.env.claimProject, "S4A-9", claim.RoleArchitect)
		if err != nil {
			return err
		}
		name = SandboxName(token)
	}
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	fixture := exec.CommandContext(r.ctx, r.env.previousHarness, "-test.run", "^TestStage4aPreviousLayoutFixture$", "-test.v", "-test.timeout", "30m")
	// A previous release's harness built before LEGION_E2E_IMPLEMENT_APP_KEY_NAME reads the key's
	// name from LEGION_E2E_IMPLEMENT_APP_KEY and stops at identity without it, so the fixture is
	// told both names.
	fixture.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "LEGION_E2E_IMAGE=") || strings.HasPrefix(kv, "LEGION_E2E_WORK=") || strings.HasPrefix(kv, "LEGION_E2E_FROM=") ||
			strings.HasPrefix(kv, "LEGION_E2E_IMPLEMENT_APP_KEY=")
	}), "LEGION_E2E_IMAGE="+r.env.previousImage, "LEGION_E2E_WORK="+work, "LEGION_E2E_PREVIOUS_LAYOUT_OUT="+out,
		"LEGION_E2E_IMPLEMENT_APP_KEY="+r.env.appKeyName)
	transcript, err := fixture.CombinedOutput()
	if werr := os.WriteFile(filepath.Join(work, "fixture.log"), transcript, 0o600); werr != nil {
		return werr
	}
	if err != nil {
		return fmt.Errorf("the previous release's fixture: %v; its log is %s: %s", err, filepath.Join(work, "fixture.log"), lastLines(string(transcript), 15))
	}
	for _, line := range strings.Split(string(transcript), "\n") {
		if strings.Contains(line, "[previous]") {
			note("previous", "%s", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "[previous]")))
		}
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	if err := os.Remove(out); err != nil {
		return err
	}
	var launched []previousLaunch
	if err := json.Unmarshal(raw, &launched); err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	if len(launched) != 2 {
		return fmt.Errorf("the previous release's fixture launched %d claims, want the architect and the tester", len(launched))
	}
	var claims []*liveClaim
	oldUID := launched[0].Pod
	for _, l := range launched {
		want := make([]string, len(claim.Roles))
		for i, role := range claim.Roles {
			want[i] = string(role)
		}
		if l.Pod != oldUID || !slices.Equal(l.Names, want) || l.Locator.Sandbox == nil || l.Locator.Sandbox.Container != string(l.Role) {
			return fmt.Errorf("the previous release launched %s in pod %s with containers %v and locator %+v, want pod %s of containers %v, its own container named %s", l.Name, l.Pod, l.Names, l.Locator.Sandbox, oldUID, want, l.Role)
		}
		loc := l.Locator
		sandboxLoc := *loc.Sandbox
		sandboxLoc.Container = runtime.IssuePodContainer
		loc.Sandbox = &sandboxLoc
		c := &liveClaim{token: l.Token, tree: l.Tree, issue: l.Issue, role: l.Role, name: l.Name, marker: l.Marker,
			state: stateRunning, gen: l.Gen, loc: &loc, last: &loc}
		r.reg.adopt(c.token, c.gen, l.Boot)
		r.claims = append(r.claims, c)
		claims = append(claims, c)
	}
	architect, tester := claims[0], claims[1]
	note("operator", "pod %s (uid %s) runs containers %v; locators taken over with container %s, as store migration 0051 rewrites them", name, short(oldUID), launched[0].Names, runtime.IssuePodContainer)

	restarted := time.Now()
	mark := r.obs.mark()
	if err := r.startRuntime(); err != nil {
		return err
	}
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(nil), time.Hour); err != nil {
		return err
	}
	note("runtime", "fresh listener and sandbox.New; ReconcileOrphans with the two taken-over claims among the known")
	for _, c := range claims {
		loc := *c.loc
		first, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
			return o.Locator.Claim == c.token && o.Kind != runtime.Uncertain
		})
		if !ok {
			return fmt.Errorf("no first observation of %s within %s", c.name, liveGoneLimit)
		}
		if first.Kind != runtime.StaleAddress || !sameLocator(first.Locator, loc) || !strings.Contains(first.Detail, "the pod's containers") || !strings.Contains(first.Detail, "now "+runtime.IssuePodContainer) {
			return fmt.Errorf("%s's first observation is %s at %s: %s; want StaleAddress at its recorded %s naming the previous layout", c.name, first.Kind, first.Locator.Incarnation, first.Detail, loc.Incarnation)
		}
		note("runtime", "%s: first observation StaleAddress at its recorded %s — %s", c.name, short(loc.Incarnation), oneLine(firstLine(first.Detail)))
	}

	since := time.Now()
	moved, err := r.resume(architect, architect.marker)
	if err != nil {
		return fmt.Errorf("the architect's Resume out of the previous layout: %w", err)
	}
	if _, err := r.awaitRunning(architect, since); err != nil {
		return err
	}
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if string(pod.UID) == oldUID || moved.Sandbox.PodUID != string(pod.UID) || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != runtime.IssuePodContainer {
		return fmt.Errorf("after the architect's Resume the pod is uid %s with %d containers, want a new pod (not %s) of the one container %s", pod.UID, len(pod.Spec.Containers), oldUID, runtime.IssuePodContainer)
	}
	note("runtime", "Resume(architect) replaced pod %s with %s, one container %s", short(oldUID), short(string(pod.UID)), runtime.IssuePodContainer)

	ended, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == tester.token && (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess)
	})
	if !ok || !ended.Displaced || !sameLocator(ended.Locator, *tester.loc) {
		return fmt.Errorf("the tester's end in the replaced pod is %+v, want Displaced at its recorded %s", ended, tester.loc.Incarnation)
	}
	note("runtime", "tester: %s Displaced at its recorded %s, charged nothing", ended.Kind, short(tester.loc.Incarnation))
	tester.loc, tester.state = nil, stateDead
	since = time.Now()
	resumed, err := r.resume(tester, tester.marker)
	if err != nil {
		return fmt.Errorf("the tester's Resume into the new pod: %w", err)
	}
	if _, err := r.awaitRunning(tester, since); err != nil {
		return err
	}
	if resumed.Sandbox.PodUID != moved.Sandbox.PodUID {
		return fmt.Errorf("the tester resumed into pod %s, want the architect's new pod %s", resumed.Sandbox.PodUID, moved.Sandbox.PodUID)
	}

	for i, c := range claims {
		previous := launched[i]
		// A previous generation's shim may say hello again to this listener before its pod goes,
		// at its own generation; past that, the claim is charged exactly one new generation.
		regs := slices.DeleteFunc(r.reg.registrations(c.token), func(reg registration) bool {
			return reg.at.Before(restarted) || reg.gen <= previous.Gen
		})
		if c.gen != previous.Gen+1 || len(regs) != 1 || regs[0].gen != c.gen {
			return fmt.Errorf("%s runs generation %d after previous generation %d, with registrations %v of a later generation since the upgrade; want exactly one, at generation %d", c.name, c.gen, previous.Gen, regs, previous.Gen+1)
		}
		lines, err := r.markerLines(c)
		if err != nil {
			return err
		}
		own := roleIncarnations(lines, c.role)
		if !slices.Equal(own, []string{previous.Locator.Incarnation, c.loc.Incarnation}) {
			return fmt.Errorf("%s's marker lines are %v, want its previous %s and its resumed %s", c.name, own, previous.Locator.Incarnation, c.loc.Incarnation)
		}
		for _, o := range r.obs.since(mark) {
			if o.Locator.Claim == c.token && o.Kind == runtime.Gone && !o.Displaced {
				return fmt.Errorf("%s was observed Gone, not Displaced, at %s: %s", c.name, o.Locator.Incarnation, o.Detail)
			}
		}
		note("harness", "%s: generation %d → %d, one registration of a new generation since the upgrade; marker %v", c.name, previous.Gen, c.gen, own)
	}
	for _, c := range claims {
		if err := r.suspend(c); err != nil {
			return fmt.Errorf("suspend %s after the check: %w", c.name, err)
		}
	}
	return nil
}

// lastLines is text's last n lines, one line.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return oneLine(strings.Join(lines, " | "))
}
