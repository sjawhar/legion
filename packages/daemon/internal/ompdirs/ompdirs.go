// Package ompdirs is where Oh My Pi, started under a given environment, keeps a profile's state:
// the root its plugins, their lock and its agent directory live under, ported from Oh My Pi's own
// resolution (@oh-my-pi/pi-utils 18.1.21, src/dirs.ts) over that environment alone. The daemon's
// plugin gate reads the installed pi-legion manifest under it (internal/daemon), and the image's
// capability check the plugin lock (internal/capabilities), so both name the directory the pod's
// or the pane's Oh My Pi actually uses.
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
// state — its plugins under `plugins/` there — and the profile that decided it ("" for the default
// profile). It ports Oh My Pi's resolution (@oh-my-pi/pi-utils 18.1.21, src/dirs.ts) over env
// alone:
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
	requested, set := env["OMP_PROFILE"]
	if !set {
		requested = env["PI_PROFILE"]
	}
	profile, valid := NormalizeProfile(requested)
	if !valid {
		return "", "", fmt.Errorf(`Invalid OMP profile %q in the environment Oh My Pi starts under. Profile names must match %s, cannot be "." or "..", cannot end with ".", and cannot be a Windows reserved device name (CON, PRN, AUX, NUL, COM0-9, LPT0-9, or any of those with an extension).`,
			requested, profileName)
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
	if xdg := env["XDG_DATA_HOME"]; xdgOn && xdg != "" && (goruntime.GOOS == "linux" || goruntime.GOOS == "darwin") {
		candidate := filepath.Join(xdg, "omp")
		if profile != "" {
			candidate = filepath.Join(candidate, "profiles", profile)
		}
		if _, err := os.Stat(candidate); err == nil {
			root = candidate
		}
	}
	return root, profile, nil
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
