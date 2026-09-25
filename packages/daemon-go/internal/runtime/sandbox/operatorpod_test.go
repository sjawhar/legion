package sandbox

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// CheckPod refuses, naming both, an operator variable the runtime, the worker image, or every
// launch sets, or one that places Oh My Pi's sessions; a volume Legion names; a mount at, under, or
// above a path Legion mounts, the image owns, or a tool runs from; and a provider key naming any
// variable already set in the agent's environment, or whose pointer is. It passes the Go live
// harnesses' operator pod, and an operator's own settings overlay and a baseline variable it may
// override.
func TestCheckPodRefusesWhatCollidesWithLegionsOwn(t *testing.T) {
	mountAt := func(path string) Pod {
		return Pod{
			Volumes:      []corev1.Volume{{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-creds"}}}},
			VolumeMounts: []corev1.VolumeMount{{Name: "creds", MountPath: path, ReadOnly: true}},
		}
	}
	env := func(name, value string) Pod { return Pod{Env: map[string]string{name: value}} }
	envSets := func(name, who string) string { return "runtime.kubernetes.pod.env sets " + name + ", which " + who }
	overlaps := func(path, owned string, image bool) string {
		if image {
			return "runtime.kubernetes.pod.volume_mounts[0].mount_path " + path + " overlaps " + owned + ", which the worker image owns: a mount may be neither at, under, nor above one of the image's"
		}
		return "runtime.kubernetes.pod.volume_mounts[0].mount_path " + path + " overlaps " + owned + ", which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's"
	}
	keyNames := func(name, who string) string {
		return "provider_keys names " + name + ", which " + who + ": a provider key must name a variable nothing else in the pod sets"
	}
	keyPointer := func(name, who string) string {
		return "provider_keys names " + name + ", whose pointer " + name + "_FILE " + who + ": the shim skips a key whose pointer the pod sets"
	}
	const (
		byRuntime  = "Legion's runtime sets in every pod"
		byImage    = "the worker image sets (packages/daemon/docker/worker.Dockerfile ENV)"
		byIdentity = "every launch sets (the git identity the role's GitHub App commits as)"
		byEnvoy    = "every launch sets (the pointer to the launch secret ENVOY_TOKEN)"
		bySessions = "the pod baseline sets, and it decides where Oh My Pi keeps the session a resume reads"
		byBaseline = "the pod baseline sets (internal/podsafety)"
		byOperator = "runtime.kubernetes.pod.env sets"
	)
	fixture, err := config.ReadPodFile(filepath.Join("..", "..", "..", "..", "..", "scripts", "e2e", "fixtures", "operator-route", "pod.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pod  Pod
		keys map[string]string
		want string
	}{
		{name: "a variable the runtime sets", pod: env("LEGION_TREE", "LEGSMOKE-1"), want: envSets("LEGION_TREE", byRuntime)},
		{name: "the pod's PATH", pod: env("PATH", "/usr/bin"), want: envSets("PATH", byRuntime)},
		{name: "the image's HOME", pod: env("HOME", "/tmp/elsewhere"), want: envSets("HOME", byImage)},
		{name: "the image's profile", pod: env("OMP_PROFILE", "other"), want: envSets("OMP_PROFILE", byImage)},
		{name: "the Oh My Pi the probe proves", pod: env("LEGION_OMP_PATH", "/usr/bin/true"), want: envSets("LEGION_OMP_PATH", byImage)},
		{name: "the role prompts the probe reads", pod: env("LEGION_ROLE_PROMPTS_DIR", "/tmp/roles"), want: envSets("LEGION_ROLE_PROMPTS_DIR", byImage)},
		{name: "Oh My Pi's config root", pod: env("PI_CONFIG_DIR", ".elsewhere"), want: envSets("PI_CONFIG_DIR", bySessions)},
		{name: "Oh My Pi's session store", pod: env("OMP_SESSION_STORAGE", "sql"), want: envSets("OMP_SESSION_STORAGE", bySessions)},
		{name: "a variable of the App's git identity", pod: env("JJ_USER", "operator"), want: envSets("JJ_USER", byIdentity)},
		{name: "a variable pointing at a launch secret", pod: env("ENVOY_TOKEN_FILE", "/etc/envoy"), want: envSets("ENVOY_TOKEN_FILE", byEnvoy)},
		{name: "a volume Legion names", pod: Pod{Volumes: []corev1.Volume{
			{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-creds"}}},
			{Name: "boot", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "a"}}},
		}}, want: "runtime.kubernetes.pod.volumes[1].name boot is a volume Legion puts in every pod"},
		{name: "a mount at a path Legion mounts", pod: mountAt("/var/run/legion/boot"), want: overlaps("/var/run/legion/boot", "/var/run/legion/boot", false)},
		{name: "a mount under a path Legion mounts", pod: mountAt("/legion/operator"), want: overlaps("/legion/operator", "/legion", false)},
		{name: "a mount above a path Legion mounts", pod: mountAt("/var/run/legion"), want: overlaps("/var/run/legion", "/var/run/legion/boot", false)},
		{name: "a mount above the sessions Legion mounts", pod: mountAt("/home/legion/.omp/profiles/legion/agent"),
			want: overlaps("/home/legion/.omp/profiles/legion/agent", "/home/legion/.omp/profiles/legion/agent/sessions", false)},
		{name: "a mount at the root", pod: mountAt("/"), want: overlaps("/", "/home/legion/.config", false)},
		{name: "a mount under a path the image owns", pod: mountAt("/opt/omp/bin"), want: overlaps("/opt/omp/bin", "/opt/omp", true)},
		{name: "a mount over a database the image owns", pod: mountAt("/home/legion/.omp/profiles/legion/agent/agent.db"),
			want: overlaps("/home/legion/.omp/profiles/legion/agent/agent.db", "/home/legion/.omp/profiles/legion/agent/agent.db", true)},
		{name: "a mount above the image's gh and jj", pod: mountAt("/usr/local/bin"), want: overlaps("/usr/local/bin", "/usr/local/bin/gh", true)},
		{name: "a mount at the image's jj", pod: mountAt("/usr/local/bin/jj"), want: overlaps("/usr/local/bin/jj", "/usr/local/bin/jj", true)},
		{name: "a mount at the image's git", pod: mountAt("/usr/bin/git"), want: overlaps("/usr/bin/git", "/usr/bin/git", true)},
		{name: "a provider key the runtime sets", keys: map[string]string{"PATH": "search_path"}, want: keyNames("PATH", byRuntime)},
		{name: "a provider key the image sets", keys: map[string]string{"HOME": "home"}, want: keyNames("HOME", byImage)},
		{name: "a provider key whose pointer the runtime sets", keys: map[string]string{"DISPATCH_TOKEN": "dispatch"},
			want: keyPointer("DISPATCH_TOKEN", byRuntime)},
		{name: "a provider key for the settings overlays", keys: map[string]string{"PI_CONFIG_FILES": "overlays"},
			want: keyNames("PI_CONFIG_FILES", byBaseline)},
		{name: "a provider key for a baseline variable", keys: map[string]string{"OTEL_SDK_DISABLED": "otel"},
			want: keyNames("OTEL_SDK_DISABLED", byBaseline)},
		{name: "a provider key for another baseline variable", keys: map[string]string{"PI_AUTO_QA": "qa"}, want: keyNames("PI_AUTO_QA", byBaseline)},
		{name: "a provider key for the config root", keys: map[string]string{"PI_CONFIG_DIR": "root"}, want: keyNames("PI_CONFIG_DIR", bySessions)},
		{name: "a provider key for the session store", keys: map[string]string{"OMP_SESSION_STORAGE": "store"},
			want: keyNames("OMP_SESSION_STORAGE", bySessions)},
		{name: "a provider key of the App's git identity", keys: map[string]string{"GIT_AUTHOR_NAME": "author"},
			want: keyNames("GIT_AUTHOR_NAME", byIdentity)},
		{name: "a provider key a launch secret's pointer names", keys: map[string]string{"ENVOY_TOKEN": "envoy"},
			want: keyPointer("ENVOY_TOKEN", byEnvoy)},
		{name: "a provider key the pod's env also sets", pod: env("OPENAI_BASE_URL", "https://gateway.internal.example"),
			keys: map[string]string{"OPENAI_BASE_URL": "openai_base_url"}, want: keyNames("OPENAI_BASE_URL", byOperator)},
		{name: "a provider key whose pointer the pod's env sets", pod: env("GEMINI_API_KEY_FILE", "/var/run/operator/gemini"),
			keys: map[string]string{"GEMINI_API_KEY": "gemini"}, want: keyPointer("GEMINI_API_KEY", byOperator)},
		{name: "the live harnesses' operator pod", pod: Pod(fixture), keys: map[string]string{"ANTHROPIC_API_KEY": "anthropic"}},
		{name: "an operator's own overlays and a baseline variable it overrides",
			pod: Pod{Env: map[string]string{"PI_CONFIG_FILES": "/etc/operator/overlay.yml", "OTEL_SDK_DISABLED": "false"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPod(tc.pod, tc.keys, testOptions().Tools, []string{"ENVOY_TOKEN"})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("CheckPod = %v, want no refusal", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("CheckPod = %v, want %q", err, tc.want)
			}
		})
	}
}

// The runtime refuses to start on an operator pod that collides with Legion's own, and on a
// provider key colliding with one of Options.LaunchSecrets' pointers, before anything touches the
// cluster.
func TestTheRuntimeRefusesAnOperatorPodCollidingWithLegionsOwn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Options)
		want   string
	}{
		{"an image variable", func(o *Options) { o.Pod.Env = map[string]string{"HOME": "/tmp"} },
			"sandbox runtime: runtime.kubernetes.pod.env sets HOME, which the worker image sets (packages/daemon/docker/worker.Dockerfile ENV)"},
		{"a launch secret's pointer", func(o *Options) {
			o.LaunchSecrets, o.ProviderKeys = []string{"ENVOY_TOKEN"}, map[string]string{"ENVOY_TOKEN": "envoy"}
		}, "sandbox runtime: provider_keys names ENVOY_TOKEN, whose pointer ENVOY_TOKEN_FILE every launch sets (the pointer to the launch secret ENVOY_TOKEN): the shim skips a key whose pointer the pod sets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			tc.change(&opts)
			if _, err := configure(opts); err == nil || err.Error() != tc.want {
				t.Fatalf("configure = %v, want %q", err, tc.want)
			}
		})
	}
}

// imageEnv is exactly what the worker image's final stage sets with ENV: a variable the image adds
// and the list lacks is one an operator could override unrefused; one the list keeps that the
// image dropped is refused for nothing.
func TestImageEnvIsWhatTheWorkerImageSets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "daemon", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	// The final stage's instructions, each joined across its continuation lines.
	var instructions []string
	var current strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if current.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if continued, ok := strings.CutSuffix(line, "\\"); ok {
			current.WriteString(continued + " ")
			continue
		}
		current.WriteString(line)
		instruction := current.String()
		current.Reset()
		if strings.HasPrefix(instruction, "FROM ") {
			instructions = nil
		}
		instructions = append(instructions, instruction)
	}
	var set []string
	for _, instruction := range instructions {
		fields, ok := strings.CutPrefix(instruction, "ENV ")
		if !ok {
			continue
		}
		for _, pair := range strings.Fields(fields) {
			name, _, ok := strings.Cut(pair, "=")
			if !ok {
				t.Fatalf("ENV %q is not NAME=value pairs", fields)
			}
			set = append(set, name)
		}
	}
	slices.Sort(set)
	if len(set) == 0 || !slices.Equal(set, imageEnv) {
		t.Errorf("the worker image's final stage sets %v, and imageEnv is %v", set, imageEnv)
	}
}
