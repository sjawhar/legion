package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceProvisionUsage = "legion workspace-init provision --issue <KEY> --repo <owner>/<repo> [--root /legion] --credential-helper <git helper> --feed <dir>"

const (
	// workspaceLostExitCode reports an expected tree volume with neither its shared clone nor
	// any retained session. A single missing role transcript is the role launcher's refusal.
	workspaceLostExitCode = 3
	// lockWaitEnv bounds how long this init container waits for another pod's provisioning of the
	// same repository. The daemon sets it on every pod from its own registration deadline, so the
	// wait never gives up on a pod the daemon would still tolerate.
	lockWaitEnv = "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS"
	// provisionTokenFileEnv points `workspace-init fetch` at the mounted provisioning token.
	provisionTokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"
	// removableWorkspacesEnv names the daemon's own candidate list for this pod's workspace-init
	// to judge and, where safe, remove from the tree volume: one JSON object,
	// `{"notAfter": "<RFC 3339>", "workspaces": [...runtime.RemovableWorkspace]}`, so the list and
	// its own expiry can never arrive apart. internal/daemon/removable.go's removableWorkspaces
	// states the candidate rule the daemon computes from its own claim store; relaunch
	// (internal/runtime/sandbox) also drops any candidate that still has a live pod of the tree,
	// a second guarantee on different evidence. notAfter is the launch time plus the init-wait
	// window (initWaitSeconds), stamped under the tree's launch turn; `workspace-init` removes
	// nothing at all, not even one candidate, once this pod's own workspace-fetch started later
	// than that (fetchStartedFile below) — a pod the Sandbox controller recreates on its own
	// (eviction, node drain, a hand deletion) runs workspace-init from the same pod template, the
	// same list, without the daemon ever retaking the tree's launch turn to refresh it, and for a
	// long-lived root architect pod that list can by then be hours old (dispatch://LEGION-583).
	// Comparing the fetch's own start, not wall-clock time at removal, is what keeps this bound
	// independent of how long the clone itself then takes (LEGION-585 raised that to 30 minutes);
	// unset or empty removes none, and a JSON object missing either field, naming a field this
	// build does not know, or followed by anything removes none, logged as malformed input the
	// same as any other.
	removableWorkspacesEnv = "LEGION_REMOVABLE_WORKSPACES"
	// fetchStartedFile is the file `workspace-init fetch` (workspace_fetch.go) writes into the
	// pod's own feed directory, RFC 3339, before its clone starts: the evidence `workspace-init
	// provision` reads back to judge whether this pod's own copy of removableWorkspacesEnv is
	// still fresh enough to trust (see its own doc comment above).
	fetchStartedFile = "fetch-started"
	// defaultLockWaitSeconds is for an invocation no daemon sized: three slow-command budgets, a
	// live holder's clone and fetch at full budget plus its local commands
	// (DEFAULT_WORKSPACE_INIT_LOCK_WAIT_SECONDS, workspace-init.ts).
	defaultLockWaitSeconds = 3 * int64(workspace.CommandTimeout/time.Second)
)

var lockWaitPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// volumeLostError is the one failure the runtime tells apart, by workspaceLostExitCode.
type volumeLostError string

func (e volumeLostError) Error() string { return string(e) }

// workspaceInitCommands is `legion workspace-init`, the Kubernetes runtime's init containers, which
// prepare a pod's persistent volume before the main container's worker-shim starts, and the one
// list of them `legion workspace-init --help` names. A tree pod runs two, which prepare an issue's
// jj workspace on the tree's volume: `fetch` is the first, the one process of the pod that holds the
// provisioning token, in a container that mounts nothing a tree agent can write; `provision` is the
// second, all the tree volume's work, in a container the provisioning Secret is not mounted in. The
// controller's pod (`controller: daemon`) runs `controller` alone, on the volume its own Sandbox
// owns. Each one's log lines go to stdout and its refusals and failures to stderr — together the init
// log the runtime quotes — with exit 1, or 3 for a lost volume.
var workspaceInitCommands = map[string]command{
	"fetch":      runWorkspaceFetch,
	"provision":  runWorkspaceProvision,
	"controller": runWorkspaceController,
}

func runWorkspaceInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "workspace-init", workspaceInitCommands, args, stdout, stderr)
}

func runWorkspaceFetch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init fetch", "usage: "+workspaceFetchUsage, stderr)
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	feed := flags.String("feed", "", "the pod's feed directory, the container's own (required)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceFetch(ctx, *repo, *feed, stdout), stderr)
}

func runWorkspaceProvision(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init provision", "usage: "+workspaceProvisionUsage, stderr)
	issue := flags.String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	root := flags.String("root", "/legion", "tree volume root directory")
	credentialHelper := flags.String("credential-helper", "", "git credential helper written into the shared clone's config (required)")
	feed := flags.String("feed", "", "the pod's feed directory, which workspace-init fetch filled (required)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceInit(ctx, *issue, *repo, *root, *credentialHelper, *feed, stdout), stderr)
}

const workspaceControllerUsage = "legion workspace-init controller [--root /legion]"

func runWorkspaceController(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init controller", "usage: "+workspaceControllerUsage, stderr)
	root := flags.String("root", "/legion", "the controller's volume root directory")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceController(*root), stderr)
}

// workspaceController prepares the controller's volume: the sessions directory Oh My Pi's
// sessions are mounted from, and nothing else — the controller works no repository and holds no
// GitHub credential, so it gets no workspace and no gh shim. On a resume it holds the controller to
// the session it recorded, as workspace-init holds a tree agent: a session gone from the volume
// means the volume was lost (the controller's volume holds nothing else to tell a lost volume from
// a lost file), which the runtime reads from workspaceLostExitCode, so the daemon relaunches a
// fresh controller rather than resuming one no attempt can find.
func workspaceController(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if _, set := os.LookupEnv(provisionTokenFileEnv); set {
		return errors.New(provisionTokenFileEnv + " is set: the controller's pod holds no provisioning token")
	}
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sessions, err)
	}
	session, set := os.LookupEnv("LEGION_RESUME_SESSION_FILE")
	if !set {
		return nil
	}
	present, err := pathPresent(session)
	if err != nil {
		return fmt.Errorf("stat the recorded OMP session file %s: %w", session, err)
	}
	if !present {
		return volumeLostError(fmt.Sprintf("The controller's volume holds no recorded OMP session file (%s): the volume was lost", session))
	}
	return nil
}

// parseWorkspaceInitFlags parses one subcommand's flags, refusing a positional argument; a false
// ok carries the exit code.
func parseWorkspaceInitFlags(flags *flag.FlagSet, args []string, stderr io.Writer) (code int, ok bool) {
	if code, ok := parseFlags(flags, args); !ok {
		return code, false
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", flags.Name(), flags.Arg(0))
		flags.Usage()
		return 2, false
	}
	return 0, true
}

// workspaceInitExit is a subcommand's exit status once it has run: 0, or its failure on stderr
// with 1, or workspaceLostExitCode for a lost volume.
func workspaceInitExit(flags *flag.FlagSet, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "%s: %v\n", flags.Name(), err)
	if errors.As(err, new(volumeLostError)) {
		return workspaceLostExitCode
	}
	return 1
}

// workspaceInit validates everything before it touches the volume, --repo first, and refuses to
// run where the provisioning token is pointed at: this is the process that runs git and jj against what every
// agent of the tree can write. Then it installs the gh shim, creates the directories the main
// container mounts, holds a resume to the same agent, and provisions from the feed under the
// repository lock, which it holds until it returns.
func workspaceInit(ctx context.Context, issue, repo, root, credentialHelper, feed string, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return err
	}
	if !legionclaim.IsIssueKey(issue) {
		return fmt.Errorf("--issue must be a Dispatch issue key like LEGION-1 (got %q)", issue)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if credentialHelper == "" {
		return fmt.Errorf("--credential-helper is required: %s", workspaceProvisionUsage)
	}
	if !filepath.IsAbs(feed) {
		return fmt.Errorf("--feed must be an absolute path (got %q)", feed)
	}
	if _, set := os.LookupEnv(provisionTokenFileEnv); set {
		return errors.New(provisionTokenFileEnv + " is set: provisioning runs without the provisioning token, which `workspace-init fetch` alone holds")
	}
	lockWait, err := workspaceInitLockWait()
	if err != nil {
		return err
	}
	tools, err := provisioningTools("git", "jj")
	if err != nil {
		return err
	}
	located, err := workspace.Location(root, repository, issue)
	if err != nil {
		return err
	}
	cloneDir := located.Clone

	if value, set := os.LookupEnv("LEGION_EXPECT_TREE_VOLUME"); set {
		expected, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("LEGION_EXPECT_TREE_VOLUME must be a boolean: %w", err)
		}
		if expected {
			cloned, err := pathPresent(cloneDir)
			if err != nil {
				return fmt.Errorf("stat the shared clone %s: %w", cloneDir, err)
			}
			if !cloned {
				sessions, err := hasRetainedSession(filepath.Join(root, "sessions"))
				if err != nil {
					return err
				}
				if !sessions {
					return volumeLostError(fmt.Sprintf("The volume of %s holds neither the clone (%s) nor retained sessions: the issue's volume was lost", issue, cloneDir))
				}
			}
		}
	}
	if err := workerbin.InstallGh(root); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "gh"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Join(root, dir), err)
		}
	}

	release, err := lockRepository(ctx, cloneDir+".lock", repository, lockWait, stdout)
	if err != nil {
		return err
	}
	defer release()
	run := workspace.NewRunner(workspace.CommandTimeout, tools)
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: repository, Issue: issue, CredentialHelper: credentialHelper, Source: workspace.FromFeed(feed),
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) },
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init: %s on %s\n", provisioned.Dir, provisioned.Bookmark)
	// Removal runs after this pod's own provisioning, whose fetch just brought the shared clone's
	// remote bookmarks current: a candidate's push-safety check (workspace.RemoveFinished) reads
	// them, and a stale view would risk nothing worse than a workspace kept one launch too long,
	// never one removed too early (dispatch://LEGION-583). This order is also why the removal
	// pass's own jj commands need no exemption for a fresh pod's empty config home: provisioning
	// has already run jj against this shared clone by the time any candidate is snapshotted,
	// migrating this pod's own copy of the clone's per-repo config before that snapshot runs.
	if fetchStart, err := readFetchStarted(feed); err != nil {
		fmt.Fprintf(stdout, "workspace-init: %v, removing nothing\n", err)
	} else {
		removeFinishedWorkspaces(ctx, run, root, repository, issue, stdout, time.Now, removalBudget, fetchStart)
	}
	if fromRef, set := os.LookupEnv("LEGION_WORKSPACE_RECOVERED_FROM"); set {
		return writeRecoveryMarker(ctx, run, provisioned, issue, fromRef)
	}
	return nil
}

func hasRetainedSession(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == dir {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read retained sessions in %s: %w", dir, err)
	}
	return found, nil
}

// readFetchStarted reads fetchStartedFile from feed, written by workspace-init fetch before its
// clone began: the evidence removeFinishedWorkspaces compares against removableWorkspacesEnv's
// own notAfter (its doc comment, below, states why). A missing or malformed file is the same
// kind of refusal as a malformed payload: the caller logs it and skips removal rather than guess
// a value that could let a recreated pod's stale list through.
func readFetchStarted(feed string) (time.Time, error) {
	raw, err := os.ReadFile(filepath.Join(feed, fetchStartedFile))
	if err != nil {
		return time.Time{}, fmt.Errorf("read this pod's own %s: %w", fetchStartedFile, err)
	}
	started, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is not a valid timestamp: %w", fetchStartedFile, err)
	}
	return started, nil
}

// removalBudget is how long removeFinishedWorkspaces spends starting new candidates, measured
// from its own first candidate: once spent, it stops before starting the next one (never
// interrupts one already running) and logs the rest as deferred to the tree's next launch,
// rather than let a long candidate list run past the registration deadline this whole init
// container shares with the provisioning it still has to report done: the base
// worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals bound (360s default),
// plus the runtime's own ProvisionBound (workspace.FetchTimeout + initWaitSeconds, about 38
// minutes by default, LEGION-585) while the claim is still launching — or the 480s default
// LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS a sibling pod's own provisioning waits behind this
// pod's repository lock. A near-full tree volume measured 63-100s per snapshot
// (dispatch://LEGION-583), so 90s leaves room for one such candidate comfortably and a second
// partway, never the whole list a first launch after deploy can name at once.
const removalBudget = 90 * time.Second

// nestedRepositoryWalkTimeout bounds workspace.RemoveFinished's own nested-repository walk for
// each candidate, fixed and never derived from how much of removalBudget happens to be left: a
// slow snapshot must never eat into the walk's own time, or a near-full volume's measured
// 63-100s snapshot would leave the walk nothing and keep every candidate regardless of whether it
// actually holds a nested repository. Walking a real, full-size checkout of this repository
// (every Go and TypeScript package, node_modules installed: 70,798 filesystem entries) took under
// 800ms on this host; 30s leaves well over an order of magnitude of headroom for a workspace
// larger or on a slower volume.
//
// Removal typically adds up to about 220s on top of whatever workspace-fetch's clone and this
// pod's own provisioning already spent inside the registration deadline every init container
// shares while its claim is still launching (the base 360s default, plus ProvisionBound —
// workspace.FetchTimeout plus initWaitSeconds, about 38 minutes by default, LEGION-585):
// removalBudget (90s) itself, plus up to ~100s for the one candidate whose snapshot is already
// running when the budget is spent (this comment's own 63-100s range), plus this 30s walk
// timeout for that same candidate. "Typically", not a bound: each jj command is itself bounded
// only by workspace.CommandTimeout (5 minutes), and the removed candidate's own recursive delete
// (workspace.Remove) has no bound here at all, so the figure excludes that delete, which a
// near-full volume can measure in the tens of seconds: the clone, this pod's own provisioning,
// that delete, and the agent's own boot after the init container exits all have to fit in
// whatever is left of the registration deadline.
const nestedRepositoryWalkTimeout = 30 * time.Second

// removeFinishedWorkspaces reads removableWorkspacesEnv's candidate list and calls
// workspace.RemoveFinished for each sibling that still has a workspace on the volume, other than
// issue — the one this pod provisions, never the daemon's to name but filtered out here too, in
// case it ever is — and the tree's own root, which Location names for no issue so it is never a
// candidate in the first place. fetchStart, this pod's own workspace-fetch start time
// (fetchStartedFile, read by the caller), is checked against the payload's NotAfter before
// anything else: past it, nothing is removed at all, logged distinctly from every other refusal
// below, since it means this pod's whole candidate list is untrustworthy rather than one
// candidate being malformed — comparing the fetch's own start, not wall-clock time at removal, is
// what keeps this bound independent of how long the clone itself then takes. Decoded strictly
// (runtime.RemovableWorkspacesPayload, an unknown field refused): invalid JSON, another shape, an
// unknown field, anything after the one JSON object, a zero NotAfter, or an empty Workspaces is
// the same malformed input, logged and removing nothing. Each candidate's shape is then checked
// before it ever reaches a revset or a path: Issue against the key form workspace.Location
// accepts (legionclaim.IsIssueKey), and MergedHead, when not empty, against workspace.IsCommitID;
// either refusal is logged and the candidate is skipped rather than acted on. One candidate's
// failure is logged and never stops the ones after it or the provisioning this pod already
// finished; a malformed env var removes nothing.
//
// The filtered candidates are rotated (rotateCandidates below) by this pod's own issue, role, and
// LEGION_GENERATION together before the loop: a candidate that always sorts first in the
// daemon's list, and so is always the one snapshotted first, would otherwise always spend the
// removal budget before any candidate after it in that same order is ever reached. Issue alone
// does not change between launches of the same issue (every phase worker of one child, every
// relaunch of a root), which left the same candidates starved on every launch of that issue; the
// generation does, since a relaunch always moves it. Generation alone does not distinguish one
// issue's own phase workers on their first launches (planner, implementer, tester, reviewer,
// merger, all at generation 1), which role closes. now and budget are the clock and
// removalBudget, exposed for a test; the one production call site passes time.Now and
// removalBudget.
func removeFinishedWorkspaces(ctx context.Context, run workspace.Runner, root string, repository ghrepo.Repository, issue string, stdout io.Writer, now func() time.Time, budget time.Duration, fetchStart time.Time) {
	raw := os.Getenv(removableWorkspacesEnv)
	if raw == "" {
		return
	}
	var payload runtime.RemovableWorkspacesPayload
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		fmt.Fprintf(stdout, "workspace-init: %s does not decode as runtime.RemovableWorkspacesPayload, removing nothing: %v\n", removableWorkspacesEnv, err)
		return
	}
	// Token, not More: More reports false for a stray `}` or `]`, letting it through.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		fmt.Fprintf(stdout, "workspace-init: %s holds data after its JSON object, removing nothing: a malformed payload\n", removableWorkspacesEnv)
		return
	}
	if payload.NotAfter.IsZero() {
		fmt.Fprintf(stdout, "workspace-init: %s has no notAfter, removing nothing: a malformed or truncated payload\n", removableWorkspacesEnv)
		return
	}
	if len(payload.Workspaces) == 0 {
		fmt.Fprintf(stdout, "workspace-init: %s has no workspaces, removing nothing: a malformed or truncated payload\n", removableWorkspacesEnv)
		return
	}
	if fetchStart.After(payload.NotAfter) {
		fmt.Fprintf(stdout, "workspace-init: %s's own fetch started at %s, past the removable-workspaces list's notAfter (%s), removing nothing: this pod may have been recreated by the Sandbox controller long after the daemon last computed it\n", issue, fetchStart.Format(time.RFC3339), payload.NotAfter.Format(time.RFC3339))
		return
	}
	all := payload.Workspaces

	var candidates []runtime.RemovableWorkspace
	for _, candidate := range all {
		if candidate.Issue == "" || candidate.Issue == issue {
			continue
		}
		if !legionclaim.IsIssueKey(candidate.Issue) {
			fmt.Fprintf(stdout, "workspace-init: %q is not a Dispatch issue key, keeping it off the removable list\n", candidate.Issue)
			continue
		}
		if candidate.MergedHead != "" && !workspace.IsCommitID(candidate.MergedHead) {
			fmt.Fprintf(stdout, "workspace-init: %s's mergedHead %q is not 40 hex characters, keeping its workspace\n", candidate.Issue, candidate.MergedHead)
			continue
		}
		candidates = append(candidates, candidate)
	}
	candidates = rotateCandidates(candidates, issue+"-"+os.Getenv("LEGION_ROLE")+"-"+os.Getenv("LEGION_GENERATION"))
	log := func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) }
	deadline := now().Add(budget)
	for i, candidate := range candidates {
		if now().After(deadline) {
			deferred := make([]string, len(candidates)-i)
			for j, remaining := range candidates[i:] {
				deferred[j] = remaining.Issue
			}
			log(fmt.Sprintf("removal budget (%s) spent; deferring %d candidate(s) to the tree's next launch: %s",
				budget, len(deferred), strings.Join(deferred, ", ")))
			return
		}
		located, err := workspace.Location(root, repository, candidate.Issue)
		if err != nil {
			fmt.Fprintf(stdout, "workspace-init: cannot locate %s's workspace, keeping it: %v\n", candidate.Issue, err)
			continue
		}
		if err := workspace.RemoveFinished(ctx, run, located, candidate.Issue, candidate.MergedHead, nestedRepositoryWalkTimeout, log); err != nil {
			fmt.Fprintf(stdout, "workspace-init: removing %s's workspace failed, keeping it: %v\n", candidate.Issue, err)
		}
	}
}

// rotateCandidates rotates candidates by a deterministic offset derived from seed: a different
// seed produces a different rotation of candidates[offset:] followed by candidates[:offset], so a
// candidate that always sorts first in the daemon's list is not always the one every pass starts
// with, and does not always starve every candidate after it of the removal budget. The caller's
// seed is what makes this change every launch, not this function.
func rotateCandidates(candidates []runtime.RemovableWorkspace, seed string) []runtime.RemovableWorkspace {
	if len(candidates) < 2 {
		return candidates
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	offset := int(h.Sum32() % uint32(len(candidates)))
	rotated := make([]runtime.RemovableWorkspace, len(candidates))
	n := copy(rotated, candidates[offset:])
	copy(rotated[n:], candidates[:offset])
	return rotated
}

// workspaceInitLockWait is LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS: a positive whole number of
// seconds, 900 when unset.
func workspaceInitLockWait() (int64, error) {
	value, set := os.LookupEnv(lockWaitEnv)
	if !set {
		return defaultLockWaitSeconds, nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if !lockWaitPattern.MatchString(value) || err != nil || seconds > int64(math.MaxInt64/time.Second) {
		return 0, fmt.Errorf("%s must be a positive whole number of seconds (got %q)", lockWaitEnv, value)
	}
	return seconds, nil
}

// provisioningTools resolves the named tools a provisioning step runs from PATH — in an init
// container the image's, with no worker-bin shim or operator rc ahead of them — where the
// TypeScript runner found them (processEnvRunner, workspace-init.ts).
func provisioningTools(names ...string) (map[string]string, error) {
	tools := map[string]string{}
	for _, tool := range names {
		path, err := exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("%s is not on PATH: %w", tool, err)
		}
		tools[tool] = path
	}
	return tools, nil
}

// lockPollInterval is how often a waiting init container tries the repository lock again.
const lockPollInterval = 250 * time.Millisecond

// lockRepository serializes provisioning across a tree volume's init containers: every issue's
// pod provisions against one shared clone, and two pods admitted together would otherwise run the
// clone, the fetch, and the git config writes against it at once (git's config lock refuses the
// second writer). The lock is flock(2) on <clone>.lock, the only state two pods share, held on this
// process's own descriptor until release — or until the process dies, since the kernel drops the
// lock with its last descriptor and no child inherits it (Go opens every file close-on-exec). No
// lease, no mtime, no takeover: a live holder holds, however long it takes; a dead one holds
// nothing. Every attempt is non-blocking (flock(2) promises waiters no order, so polling gives up
// nothing); the first refused one logs one line, so a pod stuck behind another's provisioning says
// so in its init log, and the attempts continue every lockPollInterval, bounded by waitSeconds and
// by ctx (withWorkspaceInitLock and holdFlock, workspace-init.ts).
func lockRepository(ctx context.Context, lockPath string, repo ghrepo.Repository, waitSeconds int64, log io.Writer) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(lockPath), err)
	}
	// Read-write: where flock(2) is emulated with POSIX locks (NFS), an exclusive lock needs a
	// descriptor open for writing.
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace-init lock %s: %w", lockPath, err)
	}
	release = func() { _ = file.Close() }
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for attempt := 0; ; attempt++ {
		err := flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			release()
			return nil, fmt.Errorf("flock workspace-init lock %s: %w", lockPath, err)
		}
		if attempt == 0 {
			fmt.Fprintf(log, "workspace-init: waiting for %s (another pod is provisioning %s)\n", lockPath, repo)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			release()
			return nil, fmt.Errorf("Timed out after %d s waiting for workspace-init lock %s", waitSeconds, lockPath)
		}
		select {
		case <-ctx.Done():
			release()
			return nil, fmt.Errorf("stopped waiting for workspace-init lock %s: %w", lockPath, context.Cause(ctx))
		case <-time.After(min(lockPollInterval, remaining)):
		}
	}
}

func flock(fd, how int) error {
	for {
		if err := syscall.Flock(fd, how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// writeRecoveryMarker is the command side of workspace recovery: a relaunch after a lost volume
// names the ref it recovers from, and the recreated workspace records it with the commit it was
// recreated at in .legion/<issue>/workspace-recovered.json (cmdWorkspaceInit's
// LEGION_WORKSPACE_RECOVERED_FROM branch, workspace-init.ts), under issue's own directory like
// every other handoff (dispatch://LEGION-565), so two trees recovering at once never touch the
// same path either. recoveredAt is an ISO instant in milliseconds, UTC, as JavaScript's
// toISOString writes it.
func writeRecoveryMarker(ctx context.Context, run workspace.Runner, ws workspace.Workspace, issue, fromRef string) error {
	result, err := workspace.RunCheckedIn(ctx, run, ws, []string{"jj", "log", "-r", "@", "--no-graph", "-T", "commit_id", "--color=never"})
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		RecoveredAt string `json:"recoveredAt"`
		FromRef     string `json:"fromRef"`
		SHA         string `json:"sha"`
		Reason      string `json:"reason"`
	}{time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fromRef, strings.TrimSpace(result.Stdout), "volume-missing"})
	if err != nil {
		return err
	}
	marker := filepath.Join(ws.Dir, handoffFile(issue, "workspace-recovered.json"))
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(marker), err)
	}
	if err := os.WriteFile(marker, body, 0o644); err != nil {
		return fmt.Errorf("write the recovery marker: %w", err)
	}
	return nil
}

func pathPresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
