package sandbox

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// imageEnv are the variables the worker image's final stage sets (packages/daemon/docker/
// worker.Dockerfile, ENV), which every container inherits, and which a pod's env therefore may not
// override: AWS_CA_BUNDLE holds the AWS CLI to the system root bundle the image probe verifies;
// CGO_ENABLED=0 keeps cgo off by default in every process that inherits the image's environment,
// the image's gcc on PATH notwithstanding, so a role opts in per command (`CGO_ENABLED=1 go test
// -race …`) rather than a deployment for every build; COREPACK_DEFAULT_TO_LATEST=0 keeps the
// corepack shims on the pnpm and yarn their corepack ships; HOME and OMP_PROFILE place Oh My Pi's
// profile, whose models and settings the image carries and whose sessions a pod never writes,
// every session being a row in the session database; LEGION_OMP_PATH is the Oh My Pi the image
// probe proves the workers run; DO_NOT_TRACK=1 is CodeGraph's telemetry and update-check opt-out;
// and PATH, which the runtime sets as well. TestImageEnvIsWhatTheWorkerImageSets holds the list to
// the Dockerfile.
var imageEnv = []string{"AWS_CA_BUNDLE", "CGO_ENABLED", "COREPACK_DEFAULT_TO_LATEST", "DO_NOT_TRACK", "HOME", "LEGION_OMP_PATH", "OMP_PROFILE", "PATH"}

// setter is who sets a variable a worker's Oh My Pi starts with, and whether the operator's pod
// env may set it too: the pod baseline yields to the operator's own value, and the operator's
// variables are its own.
type setter struct {
	who         string
	operatorMay bool
}

// podVariables maps every variable a worker's Oh My Pi finds set before the shim exports a
// provider key (shim.ReadProviderEnv) to who sets it: the runtime (runtimeOwned, the session
// store's two variables named apart, since they are Legion's under either store, the controller's
// Slack channel list, which the runtime sets in the controller's pod alone, and the name the
// providers volume mounts the session database's URL under, ompsessions.DSNFileName), the worker
// image (imageEnv), every launch's spec (the role App's git identity, and each launch secret's
// `<NAME>_FILE` pointer), the pod baseline, and the operator's own pod env. The first setter of a
// name is the one named.
func podVariables(pod Pod, launchSecrets []string) map[string]setter {
	set := map[string]setter{}
	add := func(name, who string, operatorMay bool) {
		if _, taken := set[name]; !taken {
			set[name] = setter{who, operatorMay}
		}
	}
	for _, name := range sessionStoreVariables {
		add(name, "Legion sets to place the sessions a resume reads (runtime.kubernetes.session_store)", false)
	}
	add(ompsessions.DSNFileName, "is the name the providers volume mounts the session database's URL under (runtime.kubernetes.session_store)", false)
	add(slackChannelsVariable, "Legion's runtime sets for the controller it launches, from the project's slack_channels alone", false)
	for _, name := range slices.Sorted(maps.Keys(runtimeOwned)) {
		add(name, "Legion's runtime sets in every pod", false)
	}
	for _, name := range imageEnv {
		add(name, "the worker image sets (packages/daemon/docker/worker.Dockerfile ENV)", false)
	}
	for name := range (runtime.GitIdentity{}).Env() {
		add(name, "every launch sets (the git identity the role's GitHub App commits as)", false)
	}
	for _, name := range launchSecrets {
		add(name+"_FILE", "every launch sets (the pointer to the launch secret "+name+")", false)
	}
	for _, v := range podsafety.Variables() {
		if v.Reserved != "" {
			add(v.Name, "the pod baseline sets, and "+v.Reserved, false)
		} else {
			add(v.Name, "the pod baseline sets (internal/podsafety)", true)
		}
	}
	for name := range pod.Env {
		add(name, "runtime.kubernetes.pod.env sets", true)
	}
	return set
}

// CheckPod refuses a piece of the operator's pod (runtime.kubernetes.pod) or a provider key that
// collides with what Legion puts in a pod, naming both. A variable the runtime, the image, or every
// launch sets would reach the agent's container twice, and one that places Oh My Pi's sessions
// would move them off the store Legion resumes them from; a volume name would be in the pod twice;
// a persistent_volume_claim naming one of Legion's own claims (legionClaim) would hand every pod an
// issue's workspace or a repository cache;
// a mount at, under, or above a path Legion mounts, the image owns (imageOwnedPaths), or a tool
// runs from hides it or is hidden by it; and a mount at, under, or above xdgStateHome
// (/home/legion/.local/state), the path Legion reserves in the image's home directory: no role's
// agent keeps its Oh My Pi state there any more — each role has a separate state home, under the
// issue's scratch on its volume (roleStateHome), which legionMountPaths() already fences through
// TreeRoot — and the image makes nothing there either, but Legion names it to the issue pod's
// workspace-init (xdgEnvironment), so an operator's mount over it is refused here, at
// `--check-config`, rather than left to surprise someone in a pod. (An operator's
// pod.VolumeMounts reaches the launcher containers and the image probe's and no init container, so
// this row speaks for the name rather than for workspace-init.) A provider key must name a
// variable nothing else in the pod sets, since the shim refuses one its own environment names
// and would replace one Oh My
// Pi's environment gains after (the pod baseline), and skips one whose `<NAME>_FILE` pointer the
// pod sets (shim.ReadProviderEnv); and it must read neither a providers secret's key nor the
// providers Secret's key that holds the session database's URL (Store.DSNSecretKey, which the
// postgres store alone has: under cloudsql-iam each pod signs in as itself and mounts no session
// key at all), which the shim would export into Oh My Pi's environment under the provider key's
// name. launchSecrets are the secrets every launch's spec carries, by name, and providersSecrets
// those of them the providers Secret carries, none of which may be the session database's URL
// key. configure runs it, and the daemon before its boot and for `legion start --check-config`.
func CheckPod(pod Pod, providerKeys map[string]string, tools Tools, launchSecrets, providersSecrets []string, store ompsessions.Store) error {
	sessionDSNKey := store.DSNSecretKey()
	if slices.Contains(providersSecrets, sessionDSNKey) {
		return fmt.Errorf("runtime.kubernetes.session_dsn_secret %s is the providers Secret's key the pod mounts as the launch secret %s: name a key of its own for the session database's URL",
			sessionDSNKey, sessionDSNKey)
	}
	variables := podVariables(pod, launchSecrets)
	for _, name := range slices.Sorted(maps.Keys(pod.Env)) {
		if s := variables[name]; !s.operatorMay {
			return fmt.Errorf("runtime.kubernetes.pod.env sets %s, which %s", name, s.who)
		}
	}
	// DISPATCH_PUBLIC_URL is Dispatch's dashboard as people open it, which every agent's dispatch
	// command links to; it turns Dispatch off over a value the URL standard's parser refuses, so
	// boot accepts only a value that parser takes (dashboardURL), where one typo would otherwise
	// stop every agent's Dispatch writes. Both sides hold to
	// packages/contracts/fixtures/dispatch-public-url.json.
	if value, set := pod.Env["DISPATCH_PUBLIC_URL"]; set && !dashboardURL(value) {
		return fmt.Errorf("runtime.kubernetes.pod.env sets DISPATCH_PUBLIC_URL to %+q, which is not an http or https URL with no userinfo, a host that is a DNS name of ASCII letters, digits and hyphens, a dotted-quad IPv4 address or a bracketed IPv6 address, and any port from 1 to 65535: boot accepts only a dashboard address every agent's dispatch command reads", value)
	}
	volumes := legionVolumeNames()
	for i, volume := range pod.Volumes {
		if slices.Contains(volumes, volume.Name) {
			return fmt.Errorf("runtime.kubernetes.pod.volumes[%d].name %s is a volume Legion puts in every pod", i, volume.Name)
		}
		if claim := volume.PersistentVolumeClaim; claim != nil && legionClaim(claim.ClaimName) {
			return fmt.Errorf("runtime.kubernetes.pod.volumes[%d].persistent_volume_claim.name %s is a claim Legion makes for an issue's, a tree's or a repository cache's own volume: name a claim of your own", i, claim.ClaimName)
		}
	}
	imageOwned := append(imageOwnedPaths(), tools.GH, tools.Git, tools.JJ, tools.Legion)
	if tools.AgentSecrets != "" {
		imageOwned = append(imageOwned, tools.AgentSecrets)
	}
	// Each owner's forbidden is the tail of its refusal, after "nor above": the first two own a
	// set of paths, the last one directory.
	owners := []struct {
		paths           []string
		owns, forbidden string
	}{
		{legionMountPaths(), "Legion mounts in every pod", "one of Legion's"},
		{imageOwned, "the worker image owns", "one of the image's"},
		{[]string{xdgStateHome}, "Legion reserves in the image's home directory", "Legion's reserved state home"},
	}
	for i, mount := range pod.VolumeMounts {
		for _, owner := range owners {
			for _, owned := range owner.paths {
				if spelled, ok := throughImageLinks(mount.MountPath, owned, overlaps); ok {
					return fmt.Errorf("runtime.kubernetes.pod.volume_mounts[%d].mount_path %s overlaps %s%s, which %s: a mount may be neither at, under, nor above %s",
						i, mount.MountPath, owned, spelled, owner.owns, owner.forbidden)
				}
			}
		}
		for _, covered := range roleNamespacePaths {
			if spelled, ok := throughImageLinks(mount.MountPath, covered, atOrAbove); ok {
				return fmt.Errorf("runtime.kubernetes.pod.volume_mounts[%d].mount_path %s is at or above %s%s, which every role's namespace in an issue pod covers with its own: mount under it instead",
					i, mount.MountPath, covered, spelled)
			}
		}
		for _, device := range roleReplacedPaths {
			if spelled, ok := throughImageLinks(mount.MountPath, device, overlaps); ok {
				return fmt.Errorf("runtime.kubernetes.pod.volume_mounts[%d].mount_path %s overlaps %s%s, which every role's namespace in an issue pod replaces with a filesystem of its own and carries no mount back onto: a mount may be neither at, under, nor above %s",
					i, mount.MountPath, device, spelled, device)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(providerKeys)) {
		if name == ompsessions.DSNFileName {
			return fmt.Errorf("provider_keys names %s, the file the providers volume mounts the session database's URL as (runtime.kubernetes.session_store): two of the volume's items would share that path", name)
		}
		if s, set := variables[name]; set {
			return fmt.Errorf("provider_keys names %s, which %s: a provider key must name a variable nothing else in the pod sets", name, s.who)
		}
		if s, set := variables[name+"_FILE"]; set {
			return fmt.Errorf("provider_keys names %s, whose pointer %s_FILE %s: the shim skips a key whose pointer the pod sets", name, name, s.who)
		}
		if key := providerKeys[name]; slices.Contains(providersSecrets, key) {
			return fmt.Errorf("provider_keys names %s from the providers Secret's key %s, which the pod mounts as the launch secret %s: the shim would export that secret into Oh My Pi's environment as %s", name, key, key, name)
		}
		if key := providerKeys[name]; key == sessionDSNKey {
			return fmt.Errorf("provider_keys names %s from the providers Secret's key %s, which holds the session database's URL (runtime.kubernetes.session_dsn_secret): the shim would export that URL into Oh My Pi's environment as %s", name, key, name)
		}
	}
	return nil
}

// overlaps reports whether one of two clean absolute paths is the other or lies under it.
func overlaps(a, b string) bool {
	return a == b || atOrAbove(a, b) || atOrAbove(b, a)
}

// atOrAbove reports whether the clean absolute path above is below or lies under it.
func atOrAbove(above, below string) bool {
	return above == below || above == "/" || strings.HasPrefix(below, above+"/")
}

// throughImageLinks reports whether a mount at mount stands to guarded as related says, comparing
// every spelling the worker image's directory links give each (pathSpellings): the kubelet
// resolves a mount path in the image, so a mount at /run/legion/roles is one at
// /var/run/legion/roles. spelled is "" when the paths as written are related, and otherwise the
// note a refusal adds, naming the two spellings that are.
func throughImageLinks(mount, guarded string, related func(mount, guarded string) bool) (spelled string, ok bool) {
	if related(mount, guarded) {
		return "", true
	}
	for _, m := range pathSpellings(mount) {
		for _, g := range pathSpellings(guarded) {
			if related(m, g) {
				return fmt.Sprintf(" (spelled %s against %s, through the worker image's directory links)", m, g), true
			}
		}
	}
	return "", false
}

// legionClaim reports whether claim is named as the volume claims Legion's pods and Sandboxes make
// or made are: an issue pod's own disk, the generic ephemeral volume the kubelet names for its pod
// and its volume (`legion-…-issue`, issuePod.disk); the controller's, which its Sandbox still owns
// (IssueClaimName, `issue-legion-…`); a repository cache's (cacheClaimName, `cache-legion-…`); or a
// tree's from the earlier layout legion-v10.0.0 kept (`tree-legion-…`, every clone, workspace and
// session file of a tree). Every pod mounting such a claim would read and write that workspace or
// that cache, so an operator volume naming one is refused. The pod's own disk is named the other
// way round from the Sandbox-owned claims — the volume's name is the suffix, not the prefix — so it
// needs a rule of its own rather than a fourth prefix.
func legionClaim(claim string) bool {
	for _, prefix := range []string{issueVolume + "-legion-", cacheVolume + "-legion-", "tree-legion-"} {
		if strings.HasPrefix(claim, prefix) {
			return true
		}
	}
	return strings.HasPrefix(claim, "legion-") && strings.HasSuffix(claim, "-"+issueVolume)
}

// dashboardURL is whether value is a DISPATCH_PUBLIC_URL boot lets through: a rule narrower than
// the URL standard's parser the agents' dispatch command reads the value with (envoy-client's
// dispatchSettings), so boot accepts only values the agents accept, and refuses some they would
// take (userinfo, a trailing dot, IPv4 shorthand, an internationalized name). It is http or https
// with no userinfo; a plain host, which is a DNS name of ASCII letters, digits and hyphens (no
// label empty, longer than 63 or starting `xn--`, and a last label neither all digits nor `0x`,
// which the agents read as an IPv4 address), a four-octet dotted IPv4 address, or a bracketed
// IPv6 address with no zone; and, when it names one, a port from 1 to 65535.
func dashboardURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Opaque != "" || parsed.User != nil {
		return false
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	host := parsed.Hostname()
	if strings.HasPrefix(parsed.Host, "[") {
		address, err := netip.ParseAddr(host)
		return err == nil && address.Is6() && address.Zone() == ""
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Is4() && address.String() == host
	}
	return dashboardDNSName(host)
}

// dashboardDNSName is whether host is a DNS name dashboardURL takes: labels of ASCII letters,
// digits and hyphens, none empty, longer than 63 or starting `xn--`, at most 253 characters in all,
// and a last label that is neither all digits nor starts `0x`.
func dashboardDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	// Lower-case only an ASCII host: strings.ToLower folds `İ` (U+0130) to `i` and the Kelvin sign
	// (U+212A) to `k`, which would pass a non-ASCII name the agents may read as another host.
	for _, r := range host {
		if r > unicode.MaxASCII {
			return false
		}
	}
	labels := strings.Split(strings.ToLower(host), ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "xn--") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != "" && !strings.HasPrefix(last, "0x")
}
