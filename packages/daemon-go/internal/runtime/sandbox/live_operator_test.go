//go:build e2e

// The Stage 4a harness's operator-pod checks: the root pod carries the operator's ServiceAccount
// and projected token, starts its agent on Legion's pod baseline under the operator's overlay, and
// hands the agent the providers Secret's keys while its shim never holds them. The rig is
// live_test.go.

package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/podsafety"
)

// operator-token: the root pod runs as the operator's ServiceAccount with no API server token, and
// holds the one projected token the operator's pod asks for — its audience, its lifetime, at its
// mount. Every expected value is the fixture's. The token is read into the harness's memory and only
// its claims are printed.
func (r *liveRig) checkOperatorToken() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	want, err := projectedToken(r.pod)
	if err != nil {
		return err
	}
	name := SandboxName(root.token)
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if pod.Spec.ServiceAccountName != r.pod.ServiceAccount || pod.Spec.AutomountServiceAccountToken == nil ||
		*pod.Spec.AutomountServiceAccountToken {
		return fmt.Errorf("pod %s runs as %q with automountServiceAccountToken %v, want the operator's %s and false",
			name, pod.Spec.ServiceAccountName, pod.Spec.AutomountServiceAccountToken, r.pod.ServiceAccount)
	}
	note("runtime", "pod %s: serviceAccountName %s, automountServiceAccountToken false", name, pod.Spec.ServiceAccountName)
	token, err := r.exec(root, "cat", want.file)
	if err != nil {
		return err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%s is not a JWT (%d dot-separated parts)", want.file, len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("%s's payload: %w", want.file, err)
	}
	var claims struct {
		Aud      []string `json:"aud"`
		Sub      string   `json:"sub"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return fmt.Errorf("%s's claims: %w", want.file, err)
	}
	sub := "system:serviceaccount:" + r.env.namespace + ":" + r.pod.ServiceAccount
	lifetime := time.Duration(claims.Exp-claims.Iat) * time.Second
	note("operator", "exec cat %s (kept in the harness's memory): aud %v, sub %s, lifetime %s", want.file, claims.Aud, claims.Sub, lifetime)
	switch {
	case !slices.Equal(claims.Aud, []string{want.audience}):
		return fmt.Errorf("the token's audience is %v, want exactly [%s]", claims.Aud, want.audience)
	case claims.Sub != sub:
		return fmt.Errorf("the token's subject is %s, want %s", claims.Sub, sub)
	case lifetime != want.lifetime:
		return fmt.Errorf("the token lives %s, want %s", lifetime, want.lifetime)
	}
	apiToken, err := r.exec(root, "sh", "-c", "test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo present || echo absent")
	if err != nil {
		return err
	}
	note("operator", "exec test -e /var/run/secrets/kubernetes.io/serviceaccount/token: %s", apiToken)
	if apiToken != "absent" {
		return errors.New("the pod holds the API server's service account token")
	}
	return nil
}

// fixtureToken is the operator pod's one projected ServiceAccount token: where the agent's container
// reads it, and what the API server must have issued.
type fixtureToken struct {
	file, audience string
	lifetime       time.Duration
}

// projectedToken finds the fixture's token: the one projected serviceAccountToken source, and the
// mount of its volume.
func projectedToken(pod Pod) (fixtureToken, error) {
	var found []fixtureToken
	for _, volume := range pod.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			token := source.ServiceAccountToken
			if token == nil || token.ExpirationSeconds == nil {
				continue
			}
			for _, mount := range pod.VolumeMounts {
				if mount.Name == volume.Name && mount.SubPath == "" {
					found = append(found, fixtureToken{
						file: path.Join(mount.MountPath, token.Path), audience: token.Audience,
						lifetime: time.Duration(*token.ExpirationSeconds) * time.Second,
					})
				}
			}
		}
	}
	if len(found) != 1 {
		return fixtureToken{}, fmt.Errorf("the operator's pod mounts %d projected service account tokens with a lifetime, want one", len(found))
	}
	return found[0], nil
}

// pod-baseline: the agent the root's shim started runs on Legion's pod baseline (internal/
// podsafety): the baseline overlay on the pod's state volume is PI_CONFIG_FILES' first element,
// ahead of the operator's; each baseline variable the operator left unset is set, and the
// operator's own are kept; the shim itself runs on the operator's value alone. Then the image's Oh
// My Pi, under the agent's environment, reads remote compaction off in a repository whose
// .omp/config.yml turns it on.
func (r *liveRig) checkPodBaseline() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	operatorOverlays := r.pod.Env["PI_CONFIG_FILES"]
	if operatorOverlays == "" {
		return errors.New("the operator's pod sets no PI_CONFIG_FILES; the check compares the baseline with it")
	}
	pid, err := r.agentPid(root)
	if err != nil {
		return err
	}
	agent, err := r.procEnviron(root, pid)
	if err != nil {
		return err
	}
	shim, err := r.procEnviron(root, "1")
	if err != nil {
		return err
	}
	overlay := path.Join(StateDir, podsafety.OverlayFile)
	want := map[string]string{
		"PI_CONFIG_FILES":   overlay + ":" + operatorOverlays,
		"OTEL_SDK_DISABLED": "true", "PI_AUTO_QA": "0", "PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "file",
	}
	for name, value := range r.pod.Env {
		if name != "PI_CONFIG_FILES" {
			want[name] = value
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if agent[name] != want[name] {
			return fmt.Errorf("the agent (pid %s) has %s=%q, want %q", pid, name, agent[name], want[name])
		}
		note("operator", "/proc/%s/environ (the agent): %s=%s", pid, name, agent[name])
	}
	if shim["PI_CONFIG_FILES"] != operatorOverlays {
		return fmt.Errorf("the shim (pid 1) has PI_CONFIG_FILES=%q, want the operator's %q alone", shim["PI_CONFIG_FILES"], operatorOverlays)
	}
	note("operator", "/proc/1/environ (the shim): PI_CONFIG_FILES=%s, the operator's alone", shim["PI_CONFIG_FILES"])
	mode, err := r.exec(root, "stat", "-c", "%a", overlay)
	if err != nil {
		return err
	}
	if mode != "444" {
		return fmt.Errorf("%s has mode %s, want 444", overlay, mode)
	}
	note("operator", "stat %s: mode %s", overlay, mode)
	// The agent's environment, verbatim, for one `omp config get` in a scratch repository.
	const readCompaction = `set -e
repo=$(mktemp -d)
mkdir "$repo/.omp"
printf 'compaction:\n  remoteEndpoint: https://repository.example/compact\n' >"$repo/.omp/config.yml"
cd "$repo"
xargs -0 sh -c 'exec env -i "$@" omp config get compaction.remoteEndpoint --json' agent-env <"/proc/$1/environ"`
	out, err := r.exec(root, "sh", "-c", readCompaction, "read-compaction", pid)
	if err != nil {
		return err
	}
	var setting struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal([]byte(out), &setting); err != nil {
		return fmt.Errorf("omp config get printed %q: %w", out, err)
	}
	note("operator", "omp config get compaction.remoteEndpoint --json, the agent's environment, a repository enabling it: %s", out)
	if setting.Value != "" {
		return fmt.Errorf("compaction.remoteEndpoint reads %v under the agent's environment, want the baseline's \"\"", setting.Value)
	}
	return nil
}

// procEnviron is a pod process's environment, as the operator reads /proc/<pid>/environ.
func (r *liveRig) procEnviron(c *liveClaim, pid string) (map[string]string, error) {
	out, err := r.exec(c, "sh", "-c", `tr '\0' '\n' <"/proc/$1/environ"`, "environ", pid)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env, nil
}

// provider-key: the root's shim hands its agent the providers Secret's key (--provider-env-dir) and
// never holds it: the agent's environment carries the Secret's value under the variable
// provider_keys names, and the shim's (pid 1) carries no such variable.
func (r *liveRig) checkProviderKey() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	secret := ProvidersSecretName(r.env.project)
	encoded, err := r.kubectl("get", "secret", secret, "-o", "jsonpath={.data."+liveProvidersSecretKey+"}")
	if err != nil {
		return err
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(value) == 0 {
		return fmt.Errorf("secret %s's key %s reads %q (%v), want the value the script wrote", secret, liveProvidersSecretKey, encoded, err)
	}
	pid, err := r.agentPid(root)
	if err != nil {
		return err
	}
	agent, err := r.procEnviron(root, pid)
	if err != nil {
		return err
	}
	shim, err := r.procEnviron(root, "1")
	if err != nil {
		return err
	}
	if agent[liveProviderKey] != string(value) {
		return fmt.Errorf("the agent (pid %s) has %s of %d bytes, want secret %s's key %s (%d bytes)", pid, liveProviderKey, len(agent[liveProviderKey]), secret, liveProvidersSecretKey, len(value))
	}
	note("operator", "/proc/%s/environ (the agent): %s equals secret %s's key %s (%d bytes, not printed)", pid, liveProviderKey, secret, liveProvidersSecretKey, len(value))
	if held, ok := shim[liveProviderKey]; ok {
		return fmt.Errorf("the shim (pid 1) holds %s (%d bytes), want the agent's environment alone to", liveProviderKey, len(held))
	}
	note("operator", "/proc/1/environ (the shim): no %s", liveProviderKey)
	return nil
}

// agentPid is the pid of a claim's stub agent, `exec sleep infinity`, which the loop finds.
func (r *liveRig) agentPid(c *liveClaim) (string, error) {
	const findAgent = `for d in /proc/[0-9]*; do [ "$(tr '\0' ' ' 2>/dev/null <"$d/cmdline")" = "sleep infinity " ] && echo "${d#/proc/}"; done; true`
	pid, err := r.exec(c, "sh", "-c", findAgent)
	if err != nil {
		return "", err
	}
	if pid == "" || strings.ContainsAny(pid, " \n") {
		return "", fmt.Errorf("the pod runs %q stub agents, want one pid", pid)
	}
	return pid, nil
}
