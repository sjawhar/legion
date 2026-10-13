package sandbox

import (
	"fmt"
	"strings"

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

// installScript is the install wrapper an installing launch's agent argv runs before Oh My Pi
// starts (LEGION-164): it drops any earlier generation's install under the role's plugins
// root (one role's directory is never another's, and no earlier generation of it should ever run
// beside this one), installs the exact pair the daemon named at this generation's own directory,
// checks that the installed pi-legion declares this daemon's contract (pluginCheckScript), and
// execs its own argv — the agent's real command — only once all of that has succeeded. Every line
// but the final exec writes to stderr: the shim reads the child's stdout as the RPC frame stream
// (shim.go), so nothing of the install may write there. bun is called by its image path; a pod
// whose image lacks it fails the same way, with the shell's own "not found" on stderr. Each
// source is handed to `bun add` as bunSpec renders it — a tarball path or URL verbatim, an exact
// version as the scoped package at that version — so the install can never read a bare version as
// a package name of its own. The directory, the two specs and the contract are each shell-quoted
// into the script (shellprefix.Literal): they travel as literal text the daemon renders, never as
// something the shell expands.
func installScript(stateHome string, generation uint64, plugins *runtime.PluginInstall) string {
	root, genDir := pluginsRoot(stateHome), pluginGenDir(stateHome, generation)
	legionSpec := bunSpec("@sjawhar/pi-legion", plugins.PiLegion)
	envoySpec := bunSpec("@sjawhar/pi-envoy", plugins.PiEnvoy)
	failMsg := fmt.Sprintf("legion plugins: installing %s and %s failed", legionSpec, envoySpec)
	return fmt.Sprintf(`set -eu
{
  rm -rf -- %s/g*
  mkdir -p -- %s && cd -- %s
  /usr/local/bin/bun add --exact %s %s \
    || { echo %s; exit 1; }
  node -e %s %s/node_modules/@sjawhar/pi-legion/package.json %d
} 1>&2
exec "$@"
`,
		shellprefix.Literal(root),
		shellprefix.Literal(genDir), shellprefix.Literal(genDir),
		shellprefix.Literal(legionSpec), shellprefix.Literal(envoySpec),
		shellprefix.Literal(failMsg),
		shellprefix.Literal(pluginCheckScript),
		shellprefix.Literal(genDir),
		plugins.Contract,
	)
}

// installArgv wraps agent in the install wrapper (installScript) when plugins is set: the agent
// argv rides as positional parameters after $0 (legion-plugins), never quoted into the script
// itself, so none of it is reparsed by the shell. plugins is nil for a plain launch, which
// installArgv returns agent unchanged for — every tmux launch, and every Kubernetes launch that
// is not installing a stale-plugin relaunch's pair into a kept pod (relaunch.go).
func installArgv(stateHome string, generation uint64, plugins *runtime.PluginInstall, agent []string) []string {
	if plugins == nil {
		return agent
	}
	script := installScript(stateHome, generation, plugins)
	return append([]string{"/bin/sh", "-c", script, "legion-plugins"}, agent...)
}
