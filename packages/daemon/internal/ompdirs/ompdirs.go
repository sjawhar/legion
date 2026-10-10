// Package ompdirs is where Oh My Pi, started under a given environment, keeps a profile's
// directories: the data root its plugins, their lock and its agent directory live under
// (ProfileRoot), and the state root its logs and its browser broker's lock live under (StateRoot),
// each ported from Oh My Pi's own resolution (@oh-my-pi/pi-utils 18.1.21, src/dirs.ts; the 18.6.0
// that .omp-pin names keeps the rule, as internal/podsafety's test on that binary holds for the
// state root) over that environment alone. The daemon's plugin gate reads the installed pi-legion
// manifest under the data root (internal/daemon), the image's capability check the plugin lock
// (internal/capabilities), and the pod's shim makes the state root's directory before Oh My Pi
// starts (internal/podsafety), so each names the directory the pod's or the pane's Oh My Pi
// actually uses.
package ompdirs

import (
	"cmp"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
)

var (
	// profileName and windowsReservedProfile are the profile names Oh My Pi accepts and the device
	// names it refuses among them (PROFILE_NAME_RE and WINDOWS_RESERVED_BASENAME_RE,
	// @oh-my-pi/pi-utils src/dirs.ts).
	profileName            = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	windowsReservedProfile = regexp.MustCompile(`(?i)^(?:CON|PRN|AUX|NUL|COM[0-9]|LPT[0-9])(?:\..*)?$`)
)

// ProfileRoot is the root under which Oh My Pi, started under env in workDir, keeps the profile's
// data-class directories — its plugins under `plugins/` there; its state-class ones are
// StateRoot's — and the profile that decided it ("" for the default profile). It ports Oh My Pi's
// resolution (@oh-my-pi/pi-utils 18.1.21, src/dirs.ts) over env alone:
//
//   - the profile is OMP_PROFILE when it is set at all, even empty, else PI_PROFILE; trimmed, an
//     empty name or "default" is the default profile, and a name Oh My Pi would refuse is refused
//     here in its words (resolveProfileEnv, normalizeProfileName);
//   - the config root is PI_CONFIG_DIR, else `.omp`, under the home directory — HOME, else the
//     account's, as `os.homedir()` answers — with `profiles/<name>` under it for a named profile
//     (getConfigDirName, getBaseConfigRoot, getProfileConfigRoot);
//   - the agent directory is PI_CODING_AGENT_DIR, resolved against workDir as `path.resolve`
//     resolves it against Oh My Pi's own, under the default profile only, and
//     only when it is not the agent directory of the profile PI_PROFILE names, which an Oh My Pi
//     running under that profile hands its children (resolveActiveAgentDirOverride,
//     resolvePreProfileAgentDir, isProfileDerivedAgentDir); else it is the config root's own
//     `agent`. That is the one way the variable reaches the plugins: an agent directory other than
//     the config root's own turns the XDG data root off (DirResolver's constructor);
//   - the data root is `$XDG_DATA_HOME/omp` for the default profile, or
//     `$XDG_DATA_HOME/omp/profiles/<name>` for a named one, when that directory already exists
//     and the XDG data root is on, else the config root (DirResolver's constructor); that data
//     root is the profile root answered.
//
// env is all it reads. Before it resolves its directories, Oh My Pi fills XDG_DATA_HOME,
// PI_CONFIG_DIR and PI_CODING_AGENT_DIR from dotenv files it reads itself (~/.env, the config root's
// .env, the agent directory's .env, its working directory's .env; env.ts, then refreshDirsFromEnv),
// where parseEnvFile also mirrors OMP_CONFIG_DIR and OMP_CODING_AGENT_DIR onto the PI_ names, and a
// launch prefix can set any variable; neither reaches env. Where either moves the root, this names
// another than the one Oh My Pi uses, which is the caller's to notice.
func ProfileRoot(env map[string]string, workDir string) (root, profile string, err error) {
	return xdgRoot(env, workDir, "XDG_DATA_HOME")
}

// StateRoot is the root under which Oh My Pi, started under env in workDir, keeps the profile's
// state-class directories — `run/daemons`, the browser broker's lock and socket; `logs`;
// `browser-profiles`; `reports`; `terminal-sessions` (sessions are data-class, under ProfileRoot)
// — and the profile that decided it. It is ProfileRoot's resolution with XDG_STATE_HOME in
// XDG_DATA_HOME's place (DirResolver's constructor: the state root is the XDG_STATE_HOME candidate
// where it exists, else the config root, under the same existence test and the same agent-directory
// gate), so a state home whose candidate nobody made before Oh My Pi started changes nothing, which
// is why the pod's shim makes it (podsafety.EnsureStateHome, over StateRootCandidate).
func StateRoot(env map[string]string, workDir string) (root, profile string, err error) {
	return xdgRoot(env, workDir, "XDG_STATE_HOME")
}

// StateRootCandidate is the directory StateRoot answers once it exists — `$XDG_STATE_HOME/omp` for
// the default profile, `$XDG_STATE_HOME/omp/profiles/<name>` for a named one — for a caller that
// makes it before Oh My Pi starts (podsafety.EnsureStateHome), so the directory it makes and the
// one StateRoot reads cannot drift. ok is false with XDG_STATE_HOME unset or empty, when Oh My Pi
// reads no state home, and a profile Oh My Pi would refuse is refused in ProfileRoot's words.
// Whether Oh My Pi then roots its state there is StateRoot's to answer over the whole environment:
// an honoured PI_CODING_AGENT_DIR elsewhere turns its XDG lookup off, and is not read here.
func StateRootCandidate(env map[string]string) (dir string, ok bool, err error) {
	profile, err := profileOf(env)
	if err != nil {
		return "", false, err
	}
	dir, ok = xdgCandidate(env["XDG_STATE_HOME"], profile)
	return dir, ok, nil
}

// xdgRoot is ProfileRoot's resolution with variable, XDG_DATA_HOME or XDG_STATE_HOME, as the XDG
// base directory whose candidate (xdgCandidate), where it already exists and the XDG lookup is on,
// is the root in the config root's place.
func xdgRoot(env map[string]string, workDir, variable string) (root, profile string, err error) {
	profile, err = profileOf(env)
	if err != nil {
		return "", "", err
	}
	home, err := ompHome(env)
	if err != nil {
		return "", "", err
	}
	base := filepath.Join(home, cmp.Or(env["PI_CONFIG_DIR"], ".omp"))
	root = base
	if profile != "" {
		root = filepath.Join(root, "profiles", profile)
	}
	xdgOn := true
	if agent := env["PI_CODING_AGENT_DIR"]; agent != "" && profile == "" {
		handedDown, valid := NormalizeProfile(env["PI_PROFILE"])
		if !valid || handedDown == "" || agent != filepath.Join(base, "profiles", handedDown, "agent") {
			if !filepath.IsAbs(agent) {
				agent = filepath.Join(workDir, agent)
			}
			xdgOn = filepath.Clean(agent) == filepath.Join(root, "agent")
		}
	}
	if candidate, ok := xdgCandidate(env[variable], profile); ok && xdgOn && (goruntime.GOOS == "linux" || goruntime.GOOS == "darwin") {
		if _, err := os.Stat(candidate); err == nil {
			root = candidate
		}
	}
	return root, profile, nil
}

// xdgCandidate is the directory Oh My Pi would root the profile under base, an XDG base directory:
// `omp` for the default profile, `omp/profiles/<name>` for a named one; ok is false with base
// empty, when Oh My Pi reads no such directory.
func xdgCandidate(base, profile string) (dir string, ok bool) {
	if base == "" {
		return "", false
	}
	dir = filepath.Join(base, "omp")
	if profile != "" {
		dir = filepath.Join(dir, "profiles", profile)
	}
	return dir, true
}

// profileOf is the profile env names, as Oh My Pi reads it: OMP_PROFILE when it is set at all, even
// empty, else PI_PROFILE, normalized (NormalizeProfile), with a name Oh My Pi would refuse refused
// in its words, naming the name as given.
func profileOf(env map[string]string) (string, error) {
	requested, set := env["OMP_PROFILE"]
	if !set {
		requested = env["PI_PROFILE"]
	}
	profile, valid := NormalizeProfile(requested)
	if !valid {
		return "", fmt.Errorf(`Invalid OMP profile %q in the environment Oh My Pi starts under. Profile names must match %s, cannot be "." or "..", cannot end with ".", and cannot be a Windows reserved device name (CON, PRN, AUX, NUL, COM0-9, LPT0-9, or any of those with an extension).`,
			requested, profileName)
	}
	return profile, nil
}

// NormalizeProfile is a profile name as Oh My Pi reads it (normalizeProfileName): trimmed, with an
// empty name or "default" the default profile, "", and a name Oh My Pi would refuse not valid.
func NormalizeProfile(requested string) (string, bool) {
	profile := strings.TrimSpace(requested)
	if profile == "default" {
		profile = ""
	}
	if profile != "" && (profile == "." || profile == ".." || strings.HasSuffix(profile, ".") ||
		!profileName.MatchString(profile) || windowsReservedProfile.MatchString(profile)) {
		return "", false
	}
	return profile, true
}

// ompHome is the home directory Oh My Pi started under env reads its roots under: HOME, else the
// account's, as `os.homedir()` answers.
func ompHome(env map[string]string) (string, error) {
	if home := env["HOME"]; home != "" {
		return home, nil
	}
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve the home directory Oh My Pi reads its plugins under: HOME is not set, and %w", err)
	}
	return account.HomeDir, nil
}
