package sandbox

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// pluginsRoot is where a role's installed plugin generations live, under its own state home
// (roleStateHome): <role state home>/legion-plugins. Not the Oh My Pi profile's plugin directory
// (ompProfileDir+"/plugins") and nowhere extension discovery reads, so an installed pair never
// loads twice (the boot gate's pod lane refuses an image where it would).
func pluginsRoot(stateHome string) string {
	return stateHome + "/legion-plugins"
}

// pluginGenDir is one generation's own directory under pluginsRoot, named as a role launcher's
// private generation directory is (shimwire.GenerationDir: g<generation>): where the installed
// pair lands, and the directory agentArgv names the two --extension roots under for an installing
// launch.
func pluginGenDir(stateHome string, generation uint64) string {
	return pluginsRoot(stateHome) + "/" + shimwire.GenerationDir(generation)
}

// pluginRoots are the two --extension roots an installing launch's agentArgv names in place of
// the image's (envoyPlugin, legionPlugin): pi-envoy first, then pi-legion, both under genDir's
// node_modules, where `bun add` lands them.
func pluginRoots(genDir string) (envoy, legion string) {
	return genDir + "/node_modules/@sjawhar/pi-envoy", genDir + "/node_modules/@sjawhar/pi-legion"
}

// bunSpec is what `bun add` is handed for one plugin source: a tarball path (absolute) or an
// http(s) URL travels as written, since bun resolves those by their own shape; anything else is
// the exact version the loader admitted (config.readPluginSource, and the release's linked bare
// versions), which bun reads as a package name unless it is spelled pkg@version. The loader already
// canonicalises a URL's scheme to lower case before this is reached; the prefix test is
// case-insensitive anyway, so the renderer never disagrees with the loader over what counts as a
// URL.
func bunSpec(pkg, source string) string {
	if strings.HasPrefix(source, "/") || hasSchemeFold(source, "http://") || hasSchemeFold(source, "https://") {
		return source
	}
	return pkg + "@" + source
}

// hasSchemeFold reports whether source begins with scheme, ignoring case.
func hasSchemeFold(source, scheme string) bool {
	return len(source) >= len(scheme) && strings.EqualFold(source[:len(scheme)], scheme)
}

// pluginCheckScript is the node -e script the install wrapper runs over the installed pi-legion's
// package.json: a file it cannot read and one with no legion.daemonApiVersion field are the
// same kind of failure, naming what is missing; a declared contract that differs from the
// daemon's (argv[2]) is refused by name, naming the installed version and both contracts. argv[1]
// is the package.json path.
const pluginCheckScript = `
const fs = require('fs');
const path = process.argv[1];
const want = Number(process.argv[2]);
let pkg;
try {
  pkg = JSON.parse(fs.readFileSync(path, 'utf8'));
} catch (e) {
  console.error('legion plugins: reading ' + path + ': ' + e.message);
  process.exit(1);
}
const got = pkg.legion && pkg.legion.daemonApiVersion;
if (typeof got !== 'number') {
  console.error('legion plugins: ' + path + ' names no legion.daemonApiVersion');
  process.exit(1);
}
if (got !== want) {
  console.error('legion plugins: @sjawhar/pi-legion@' + pkg.version + ' declares daemon API contract ' + got + '; this daemon requires ' + want);
  process.exit(1);
}
`

// startName is the start wrapper's $0 (startArgv): what the shell names itself in its own
// messages, and what a role container's process list shows for the wrapper before its exec.
const startName = "legion-start"

// installLockWait is how long installBlock's flock waits for the shared bun install cache before
// giving up, in whole seconds (flock -w takes seconds): a third of the registration deadline the
// daemon would retire the role at, floored at one second. claudeCodeBlock's flock on the issue's
// npm cache waits the same bound, once (its own comment says why that fits).
//
// It is a proportion rather than a number because the bound only does its work while it stays in
// proportion to that deadline. The script runs the install twice, so two timed-out waits have to
// fit inside the deadline with room left over for the install that then runs and for the agent to
// start: a third each leaves a third of remainder — 120 seconds at the defaults, 60 at a halved
// worker_boot_timeout_seconds — so the margin shrinks with everything else instead of being
// generous at one setting and impossible at another. A constant cannot do that: 120 is right only
// at the defaults, and at a halved boot timeout it would mean two waits of 240 seconds against a
// 180 second deadline, so the bound could never fire before the deadline retired the role, which
// is the failure the bound exists to prevent. The remainder holds at every configuration but the
// smallest, where the floor below takes over: at a deadline under three seconds two floored waits
// of a second consume it, which is deliberate, since being serialised and then retired is no
// worse than racing — an unserialised cold fill fails the launch anyway.
//
// The other half of the bound is the queue the lock itself creates, and it is why the wait may
// not shrink freely: the last of a pod's six roles waits for the five installs before it, which
// the defaults' 120 seconds allows 24 seconds each. That lower bound is held by
// TestTheInstallLockWaitIsABoundInProportionToTheRegistrationDeadline against the installs
// themselves rather than against this derivation, so a divisor that made the wait too small to
// cover the queue fails there rather than passing as a different proportion.
//
// The sum at the defaults is the sanity check: worker_boot_timeout_seconds (120) times
// worker_boot_registration_deadline_intervals (3) is a 360 second deadline, two waits take two
// thirds of it, and the remaining third is 120 seconds for the install and the agent's start — so
// a stalled lock fails the launch by the script's own named failure branch, loudly and while the
// daemon is still watching, rather than being retired by the deadline. What makes that check
// meaningful is the size of the install it has to cover: a cold install of the plugin pair into
// an empty cache measured 505 milliseconds in an issue pod of this cluster (32 warm). The claim
// is the order of magnitude, not the milliseconds — a pod on a cold node pulling from npm is
// slower — and at two orders of magnitude of headroom it says plainly that the install side is
// not what constrains this bound; the deadline side is.
//
// The floor is a fence, not tidiness. A derived wait that rounds down to zero renders `-w 0`,
// which is byte for byte `-n`: measured against a held lock it exits 1 after no wait at all, so
// the cold-fill race would be back by arithmetic with every assertion over the script still
// green.
//
// The wait is a parameter of the render rather than a package constant because an operator's
// configuration determines it, which is what a production shape is for — deliberately unlike a
// test's convenience, which earlier in this branch was refused as a reason to widen a signature
// (the serialisation test substitutes its stub into the rendered text instead). The third is
// taken through runtime.RegistrationDeadline, the one computation that owns the deadline, rather
// than by re-multiplying boot by intervals here, so the wait follows the deadline if it ever
// stops being a plain product. Its grace term is zero, which looks like a dropped argument and is
// not: the grace the supervisor passes is ProvisionBound, and only while the claim is still
// launching (armRegistration, internal/supervise), where this script rides in the LauncherStart
// argv sent after the hello, by which time the timer is re-armed at the base bound. Where the
// larger pre-hello bound does still stand, a wait derived from the base one is merely
// conservative.
func installLockWait(boot time.Duration, intervals int) int {
	return max(int(runtime.RegistrationDeadline(boot, intervals, 0)/3/time.Second), 1)
}

// installBlock is the part of the start wrapper an installing launch runs first (LEGION-164): it
// drops any earlier generation's install under the role's plugins root (one role's directory is
// never another's, and no earlier generation of it should ever run beside this one), installs the
// exact pair the daemon named at this generation's own directory, and checks that the installed
// pi-legion declares this daemon's contract (pluginCheckScript). It runs in a subshell, so its cd
// into the generation directory ends with it and what runs after it starts in the container's
// working directory, and with its stdout on stderr: the shim reads the child's stdout as the RPC
// frame stream (shim.go), so nothing of the install may write there. A failure exits the
// subshell non-zero, which `set -e` makes the wrapper's own exit. bun is called by its image path;
// a pod whose image lacks it fails the same way, with the shell's own "not found" on stderr. Each
// source is handed to `bun add` as bunSpec renders it — a tarball path or URL verbatim, an exact
// version as the scoped package at that version — so the install can never read a bare version as
// a package name of its own. The install is `--backend=copyfile` because the bun install cache
// the issue's roles share (BUN_INSTALL_CACHE_DIR) and this generation's directory now both sit
// on the issue's volume: on one filesystem bun hardlinks the installed files out of that cache,
// so the installed file and the cache file are one inode and an in-place edit of either corrupts
// the other for the life of the volume. The flag on the command is the only way to say it — bun
// ignores the setting in a bunfig.toml and has no environment variable for it (oven-sh/bun#25391,
// #32158) — and it is the same copy-don't-link that uv is made to do for the same reason
// (uvLinkMode, manifest.go). The directory, the two specs and the contract are each shell-quoted
// into the script (shellprefix.Literal): they travel as literal text the daemon renders, never as
// something the shell expands.
//
// The install is serialised on the cache directory and retried once, each for its own reason.
// Filling a cold entry of a bun install cache is not concurrency-safe: six concurrent
// `bun add --backend=copyfile --exact` of the rendered pair into six separate directories against
// one empty BUN_INSTALL_CACHE_DIR leave several of the six exiting 1 with
// `ENOENT: failed copying files from cache to destination`, where the same six against the cache
// they just filled all exit 0 — the race is the cold entry fill, not the install out of it, and
// `--backend=copyfile` governs only how files leave the cache. How many of the six fail is not a
// property to rely on: four runs of the rendered pair left 3, 4, 2 and 3 of six failed, so the
// failure is reliable and its rate is not (the one probe whose rate was measured, over chalk and
// lodash, is scoped where it belongs, under "Sharing BUN_INSTALL_CACHE_DIR" in docs/kubernetes.md).
// This script is exposed to that race because the issue's roles share one install cache
// (roleEnvironment, manifest.go) and an installing launch renders it for every role of a kept pod
// whose recorded contract differs from the daemon's, so a plugin-version bump makes the pair cold
// for all six at once; a failed `bun add` here takes the failure branch and the role's agent
// never execs.
//   - The lock, /usr/bin/flock on the cache directory, covers the installs Legion itself renders,
//     which is the case that reaches production. It waits at most lockWait seconds, which the
//     runtime derives from the registration deadline (installLockWait), because
//     a wait with no bound charges one stalled install to every role of the pod and to every
//     relaunch, which renders the same script and queues again: a timed-out wait exits 1, falls
//     into the retry below and then into the named failure branch, so the launch fails loudly,
//     and says why, while the daemon is still watching it. Its scope is exactly that and no
//     wider: flock is advisory and per-inode, which holds for the containers of one pod, sharing
//     a kernel and an inode on the issue's volume, and not across pods, where two pods of two
//     issues have two volumes and two inodes anyway. The directory is made (mkdir -p) inside
//     legion_install, before each attempt's lock, so the lock's inode exists before anything
//     fills the cache and so an attempt that found the directory gone remakes it — EnsureScratch
//     makes the cache root, but this script cannot assume the generation's turn order, and an
//     operator reclaiming the volume between turns may have deleted the cache under a running
//     install. Made once before the retry, that shape is a one-way failure: flock creates a
//     missing path as a *regular file* and exits 0, and every later installing launch of the
//     issue then dies at `mkdir: File exists` under set -eu, before Oh My Pi starts, until a
//     person removes it. Hence the mkdir inside, and the rm of a non-directory before it, which
//     clears exactly that file and nothing a cache can legitimately be. The cache
//     directory is $BUN_INSTALL_CACHE_DIR itself, with no default behind it: the runtime sets that
//     variable for every role container (roleEnvironment, manifest.go; runtimeOwned refuses anyone
//     else setting it, and TestTheRuntimeTellsTheShimEveryScratchDirectoryItMakes pins that the
//     runtime names it), and a default would silence, for the one variable this script is about,
//     the `set -u` abort that is how a broken invariant should announce itself. flock is named by
//     the absolute path for the reason bun is, and the image is held to that exact path rather
//     than to flock being on PATH: the apt line, the build's toolchain check and the toolchain
//     capability row all ask only for the name, so a base that kept flock and moved it would pass
//     every one of them and fail every installing launch. The build's final step resolves `flock`
//     and refuses anything but /usr/bin/flock, which is where the declaration and this use are
//     held to agree (packages/daemon/docker/worker.Dockerfile).
//   - The retry covers what no lock of ours can reach: an agent's own `bun install` filling the
//     same cold entry at the same moment. One retry, then the failure branch.
func installBlock(stateHome string, generation uint64, plugins *runtime.PluginInstall, lockWait int) string {
	root, genDir := pluginsRoot(stateHome), pluginGenDir(stateHome, generation)
	legionSpec := bunSpec("@sjawhar/pi-legion", plugins.PiLegion)
	envoySpec := bunSpec("@sjawhar/pi-envoy", plugins.PiEnvoy)
	failMsg := fmt.Sprintf("legion plugins: installing %s and %s failed", legionSpec, envoySpec)
	return fmt.Sprintf(`(
  rm -rf -- %s/g*
  mkdir -p -- %s && cd -- %s
  cache="$BUN_INSTALL_CACHE_DIR"
  legion_install() {
    [ -d "$cache" ] || rm -f -- "$cache"
    mkdir -p -- "$cache"
    /usr/bin/flock -w %d -- "$cache" /usr/local/bin/bun add --backend=copyfile --exact %s %s
  }
  legion_install || legion_install \
    || { echo %s; exit 1; }
  node -e %s %s/node_modules/@sjawhar/pi-legion/package.json %d
) 1>&2
`,
		shellprefix.Literal(root),
		shellprefix.Literal(genDir), shellprefix.Literal(genDir),
		lockWait,
		shellprefix.Literal(legionSpec), shellprefix.Literal(envoySpec),
		shellprefix.Literal(failMsg),
		shellprefix.Literal(pluginCheckScript),
		shellprefix.Literal(genDir),
		plugins.Contract,
	)
}

// setupBlock runs the deployment's setup command (runtime.kubernetes.setup), whose words are the
// wrapper's positional parameters after their count: $1 is the count n, the n words after it the
// command, and the rest the agent's argv. It shifts the count away, runs the first n words in a
// subshell that keeps only them as its "$@" and execs them, with stdin from /dev/null, so the
// command never reads the RPC frames the shim writes to the agent, and stdout on stderr, so it
// never writes into the frame stream the shim reads. A command that exits non-zero ends the
// wrapper with its status before Oh My Pi starts, after a line naming the status; otherwise its n
// words are shifted away and "$@" is the agent's argv alone. A count of 0 runs nothing.
const setupBlock = `n=$1
shift
if [ "$n" -gt 0 ]; then
  (
    i=0
    for word do
      if [ "$i" -eq 0 ]; then set --; fi
      [ "$i" -lt "$n" ] || break
      set -- "$@" "$word"
      i=$((i + 1))
    done
    exec "$@"
  ) </dev/null 1>&2 || {
    status=$?
    echo "legion setup: runtime.kubernetes.setup exited $status; not starting Oh My Pi" >&2
    exit "$status"
  }
  shift "$n"
fi
`

// claudeCodeRoot is where a Claude role's installed Claude Code generations live, beside its
// plugin generations (pluginsRoot) under its own state home: <role state home>/legion-claude-code,
// on the issue's volume, writable by the pod's uid and no other role's.
func claudeCodeRoot(stateHome string) string {
	return stateHome + "/legion-claude-code"
}

// claudeCodeGenDir is one generation's own directory under claudeCodeRoot, named as pluginGenDir
// names its own (shimwire.GenerationDir: g<generation>): where claudeCodeBlock installs the pinned
// Claude Code for that generation.
func claudeCodeGenDir(stateHome string, generation uint64) string {
	return claudeCodeRoot(stateHome) + "/" + shimwire.GenerationDir(generation)
}

// claudeCodeBinary is the `claude` a generation's install leaves, and the one that generation's
// agent command names (claudeArgv): the per-platform package's own executable, a self-contained
// ELF that needs no node. Never node_modules/.bin/claude: that is the npm package's placeholder
// stub, `bin/claude.exe`, which only the package's postinstall replaces, and the install runs no
// scripts. The platform is linux-x64 because the worker image builds for linux/amd64 alone.
func claudeCodeBinary(stateHome string, generation uint64) string {
	return claudeCodeGenDir(stateHome, generation) + "/node_modules/@anthropic-ai/claude-code-linux-x64/claude"
}

// claudeCodeCheckScript is the node -e script claudeCodeBlock runs once the install has
// succeeded: it reads the version the image's package.json pins for @anthropic-ai/claude-code
// (argv[1], claudeCodePin's), runs the installed binary (argv[2]) with --version, and refuses
// unless the first word it prints is that version — `2.1.310 (Claude Code)` for a pin of
// 2.1.310. `npm ci` already holds every tarball to the lockfile's sha512 integrity; this holds
// what actually runs to the pin as well, so a lockfile regenerated without its package.json, or a
// binary that is not the pinned release, fails the launch by name. Each failure names what was
// read and what was wanted.
const claudeCodeCheckScript = `
const fs = require('fs');
const { execFileSync } = require('child_process');
const pin = process.argv[1];
const bin = process.argv[2];
let want;
try {
  want = JSON.parse(fs.readFileSync(pin, 'utf8')).dependencies['@anthropic-ai/claude-code'];
} catch (e) {
  console.error('legion claude-code: reading the pin from ' + pin + ': ' + e.message);
  process.exit(1);
}
if (typeof want !== 'string' || want === '') {
  console.error('legion claude-code: ' + pin + ' pins no @anthropic-ai/claude-code');
  process.exit(1);
}
let got;
try {
  got = execFileSync(bin, ['--version'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] }).trim();
} catch (e) {
  console.error('legion claude-code: running ' + bin + ' --version: ' + e.message);
  process.exit(1);
}
if (got.split(/\s+/)[0] !== want) {
  console.error('legion claude-code: ' + bin + ' --version printed ' + JSON.stringify(got) + '; ' + pin + ' pins ' + want);
  process.exit(1);
}
`

// claudeCodeBlock is the part of the start wrapper a Claude role's launch runs before Claude Code
// starts: the worker image carries the pin and never the binary (Claude Code's license reserves
// every right and grants no redistribution, and the image is public), so every generation fetches
// the pinned release from npm here and verifies it, and a fetch that fails or does not verify
// fails the launch, loudly, before anything execs — there is no other Claude Code to fall back
// to, and a worker that came up without its harness is the failure this exists to prevent.
//
// It has installBlock's shape, for installBlock's reasons. It runs in a subshell with its stdout
// on stderr, since the shim reads the child's stdout as the RPC frame stream, and its cd into the
// generation's directory ends with it. It first drops every earlier generation's install under the
// role's root (claudeCodeRoot), so an old generation's copy — some hundreds of megabytes — never
// accumulates on the issue's volume. It copies the image's package.json and package-lock.json
// (claudeCodePin) into the generation's directory and runs `npm ci --ignore-scripts --omit=dev`
// there: the lockfile pins @anthropic-ai/claude-code exactly and carries the sha512 integrity of
// every tarball, which `npm ci` enforces, and with scripts off the per-platform package's binary
// is what runs (claudeCodeBinary). npm is called by its image path, as bun is. Then
// claudeCodeCheckScript holds the installed binary's --version to the pin. A failed copy or
// install prints the failure line and exits 1, and a failed check prints its own; either exits
// the subshell non-zero, which `set -e` makes the wrapper's own exit.
//
// npm's cache is the issue's, `$XDG_CACHE_HOME/legion-claude-code` on the issue's volume, so the
// pod's other Claude roles and its later pods reuse the tarballs one already fetched, and neither
// the cache nor the install is charged to the container's ephemeral storage. XDG_CACHE_HOME is
// read with no default, as installBlock reads BUN_INSTALL_CACHE_DIR: the runtime sets it for every
// role container (roleEnvironment) and owns it (runtimeOwned), and its absence is a broken
// invariant `set -u` should announce. The install takes /usr/bin/flock on that directory with
// installBlock's bound (lockWait, installLockWait), made (mkdir -p, after the same rm of a regular
// file an interrupted flock can leave) before the lock so the lock's inode exists, and so a
// stalled holder fails the launch by the failure branch while the daemon is still watching rather
// than being retired by the registration deadline. It is tried once: installBlock's retry covers
// an agent's own `bun install` filling the shared bun cache, and no agent's npm uses this cache.
// A Claude launch never also installs the plugin pair (specs.stalePluginInstall installs none for
// the claude backend), so its script spends at most one wait of the deadline, not installBlock's
// two. Every path is shell-quoted into the script (shellprefix.Literal).
func claudeCodeBlock(stateHome string, generation uint64, lockWait int) string {
	root, genDir := claudeCodeRoot(stateHome), claudeCodeGenDir(stateHome, generation)
	pin, lock := claudeCodePin+"/package.json", claudeCodePin+"/package-lock.json"
	failMsg := fmt.Sprintf("legion claude-code: installing the Claude Code %s pins into %s failed", lock, genDir)
	return fmt.Sprintf(`(
  rm -rf -- %s/g*
  mkdir -p -- %s
  cd -- %s
  cache="$XDG_CACHE_HOME/legion-claude-code"
  [ -d "$cache" ] || rm -f -- "$cache"
  mkdir -p -- "$cache"
  { cp -- %s %s . \
    && /usr/bin/flock -w %d -- "$cache" /usr/local/bin/npm ci --ignore-scripts --omit=dev --no-audit --no-fund --cache "$cache"; } \
    || { echo %s; exit 1; }
  node -e %s %s %s
) 1>&2
`,
		shellprefix.Literal(root),
		shellprefix.Literal(genDir), shellprefix.Literal(genDir),
		shellprefix.Literal(pin), shellprefix.Literal(lock),
		lockWait,
		shellprefix.Literal(failMsg),
		shellprefix.Literal(claudeCodeCheckScript),
		shellprefix.Literal(pin), shellprefix.Literal(claudeCodeBinary(stateHome, generation)),
	)
}

// startScript is the start wrapper an agent argv runs in before the harness starts when the launch
// installs a stale-plugin relaunch's pair (plugins set), runs Claude Code (claudeDir set), the
// deployment names a setup command, or any of them: for a Claude launch the one `mkdir -p` of the
// Claude Code configuration directory and then the install of Claude Code itself
// (claudeCodeBlock), then the plugin install (installBlock) when plugins is set, then the setup
// command (setupBlock), then `exec "$@"` into the agent's real command, only once everything
// before it has succeeded, so the shim's child is the harness itself from then on. lockWait is
// the installs' flock bound in seconds (installLockWait), which only a Claude launch's block and
// an installing launch's render.
//
// claudeDir is the launch's CLAUDE_CONFIG_DIR (claudeConfigDir), "" for an Oh My Pi launch, which
// therefore never renders claudeCodeBlock. It is made here and nowhere else: it is on the issue's
// volume, which the image cannot prepare and an init container does not know the role's backend
// to make, and Claude Code is told it as a variable rather than asked to create it. The path is
// shell-quoted into the script (shellprefix.Literal), as the install blocks' paths are, so it
// travels as literal text.
func startScript(stateHome string, generation uint64, plugins *runtime.PluginInstall, claudeDir string, lockWait int) string {
	script := "set -eu\n"
	if claudeDir != "" {
		script += "mkdir -p -- " + shellprefix.Literal(claudeDir) + "\n"
		script += claudeCodeBlock(stateHome, generation, lockWait)
	}
	if plugins != nil {
		script += installBlock(stateHome, generation, plugins, lockWait)
	}
	return script + setupBlock + "exec \"$@\"\n"
}

// startArgv wraps agent in the start wrapper (startScript) when plugins, claudeDir or setup is
// set: `/bin/sh -c '<script>' legion-start <len(setup)> <setup…> <agent…>`. The setup command and
// the agent argv ride as positional parameters after $0 (startName), never quoted into the script
// itself, so none of either is reparsed by the shell. With none of the three — every tmux launch,
// and every Kubernetes launch of an Oh My Pi role whose deployment names no setup command and
// which is not installing a stale-plugin relaunch's pair into a kept pod (relaunch.go) — agent is
// returned unchanged. lockWait is the installs' flock bound in seconds (installLockWait), which
// only a Claude launch and an installing launch render.
func startArgv(stateHome string, generation uint64, plugins *runtime.PluginInstall, claudeDir string, setup, agent []string, lockWait int) []string {
	if plugins == nil && claudeDir == "" && len(setup) == 0 {
		return agent
	}
	argv := []string{"/bin/sh", "-c", startScript(stateHome, generation, plugins, claudeDir, lockWait), startName, strconv.Itoa(len(setup))}
	return append(append(argv, setup...), agent...)
}
