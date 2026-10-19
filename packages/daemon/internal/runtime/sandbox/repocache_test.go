package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

var cacheRepo = ghrepo.MustParse("acme/widgets")

// longRepo is a repository whose owner/name is 80 characters: longer than a label value may be.
var longRepo = ghrepo.MustParse(strings.Repeat("o", 39) + "/" + strings.Repeat("r", 40))

// bareRuntime is a runtime configured from testOptions and started on nothing: enough to build a
// manifest a test then stores as the API would hold it.
func bareRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// cacheObject is repo's cache Sandbox as the API would hold it after the runtime made it, with uid,
// in mode, annotated with annotated's repository (repo's own unless the test names another).
func cacheObject(t *testing.T, repo ghrepo.Repository, uid, mode, annotated string) *unstructured.Unstructured {
	t.Helper()
	s := bareRuntime(t).cacheManifest(repo)
	s.UID, s.ResourceVersion, s.Generation = types.UID(uid), "1", 2
	s.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC))
	s.Spec.OperatingMode = mode
	if annotated != "" {
		s.Annotations[annotationCacheRepo] = annotated
	}
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// cachePod is the pod of the cache Sandbox with ownerUID, running image, in phase.
func cachePod(repo ghrepo.Repository, uid, ownerUID, image string, phase corev1.PodPhase) *corev1.Pod {
	name := CacheSandboxName(testProject, repo)
	pod := podObject(name, uid, ownerUID, map[string]string{labelProject: testProject, labelCache: cacheMarker}, corev1.PodStatus{Phase: phase})
	pod.Spec.Containers = []corev1.Container{{Name: cacheContainer, Image: image}}
	return pod
}

// logBuffer is a log the runtime writes while a test reads it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withLog has the runtime log at info and above to buffer.
func withLog(buffer *logBuffer) rigOption {
	return withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
}

// sandboxWrites are the writes to Sandboxes and Secrets, in order, as "verb resource name".
func (g *rig) sandboxWrites() []string {
	var out []string
	for _, w := range g.writes() {
		if w.resource == "sandboxes" || w.resource == "secrets" {
			out = append(out, w.String())
		}
	}
	return out
}

// templatePatch is the pod template a JSON patch body sets, nil when it sets none.
func templatePatch(t *testing.T, body string) *podTemplate {
	t.Helper()
	if isMergePatch(body) {
		return nil
	}
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("patch %q: %v", body, err)
	}
	for _, op := range ops {
		if op.Path == "/spec/podTemplate" {
			var template podTemplate
			if err := json.Unmarshal(op.Value, &template); err != nil {
				t.Fatal(err)
			}
			return &template
		}
	}
	return nil
}

// A cache's repository is its annotation, exactly as GitHub names it, on the Sandbox, its pod
// template and its Secret; its labels carry only a marker, valid label values all; and its name is
// derived from the repository, never parsed back: at most 63 characters, a DNS label ending in eight
// hex digits of the repository's hash, so two repositories whose slugs coincide get two names.
func TestCacheIdentityIsTheAnnotationAndTheNameIsNeverParsed(t *testing.T) {
	r := bareRuntime(t)
	dnsLabel := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, repo := range []ghrepo.Repository{cacheRepo, longRepo, ghrepo.MustParse("acme/widgets-x"), ghrepo.MustParse("acme-widgets/x")} {
		name := CacheSandboxName(testProject, repo)
		if len(name) > maxNameLength || !dnsLabel.MatchString(name) || !regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(name) {
			t.Errorf("CacheSandboxName(%s) = %q (%d characters), want a DNS label of at most 63 ending in -<8 hex>", repo, name, len(name))
		}
		manifest := r.cacheManifest(repo)
		if manifest.Name != name {
			t.Errorf("the manifest of %s is named %q, want %q", repo, manifest.Name, name)
		}
		secret := r.cacheSecretMeta(&manifest, repo)
		for what, meta := range map[string]struct{ labels, annotations map[string]string }{
			"Sandbox":      {manifest.Labels, manifest.Annotations},
			"pod template": {manifest.Spec.PodTemplate.Metadata.Labels, manifest.Spec.PodTemplate.Metadata.Annotations},
			"Secret":       {secret.Labels, secret.Annotations},
		} {
			if meta.labels[labelCache] != cacheMarker || meta.labels[labelProject] != testProject {
				t.Errorf("the %s of %s's cache is labelled %v, want the project and the cache marker", what, repo, meta.labels)
			}
			for _, key := range []string{labelIssue, labelTree, labelRole, labelProbe} {
				if _, ok := meta.labels[key]; ok {
					t.Errorf("the %s of %s's cache carries %s", what, repo, key)
				}
			}
			for key, value := range meta.labels {
				if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
					t.Errorf("the %s of %s's cache has label %s=%q, not a label value: %v", what, repo, key, value, errs)
				}
			}
			if got := meta.annotations[annotationCacheRepo]; got != repo.String() {
				t.Errorf("the %s of %s's cache is annotated %q, want %q", what, repo, got, repo.String())
			}
			if got, err := cacheRepository(meta.annotations); err != nil || got != repo {
				t.Errorf("cacheRepository of the %s of %s's cache = %s, %v", what, repo, got, err)
			}
		}
	}
	if a, b := CacheSandboxName(testProject, ghrepo.MustParse("acme/widgets-x")), CacheSandboxName(testProject, ghrepo.MustParse("acme-widgets/x")); a == b {
		t.Errorf("acme/widgets-x and acme-widgets/x share the cache name %s", a)
	}
	for _, annotations := range []map[string]string{nil, {annotationCacheRepo: "acme"}} {
		if _, err := cacheRepository(annotations); err == nil || !strings.Contains(err.Error(), annotationCacheRepo) {
			t.Errorf("cacheRepository(%v) = %v, want an error naming %s", annotations, err, annotationCacheRepo)
		}
	}
}

// A repository's cache is made as an issue pod is: its Sandbox created Suspended, with a headless
// Service, then its Secret written owned by the created Sandbox's uid, holding the implement App's
// token for the repository's owner and a random bearer token of the cache's own, then one patch
// sets its pod template and Running. The pod runs the cache process alone, ready once /readyz
// answers, with both tokens read-only and the volume at CacheRoot, placed as every Legion pod is,
// and with nothing of an agent's: no providers, no Dispatch. A cache already as it should be is
// left alone, its bearer token unchanged.
func TestEnsureRepositoryCacheCreatesSuspendedWritesTheSecretThenSetsRunning(t *testing.T) {
	g := newRig(t, nil)
	name := CacheSandboxName(testProject, cacheRepo)
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache: %v", err)
	}
	var writes []action
	var order []string
	for _, w := range g.writes() {
		if w.resource == "sandboxes" || w.resource == "secrets" {
			writes = append(writes, w)
			order = append(order, w.String())
		}
	}
	if want := []string{"create sandboxes " + name, "create secrets " + cacheSecretName(name), "patch sandboxes " + name}; !slices.Equal(order, want) {
		t.Fatalf("EnsureRepositoryCache wrote %q, want %q", order, want)
	}
	created, err := decodeSandbox(writes[0].object.(*unstructured.Unstructured))
	if err != nil {
		t.Fatal(err)
	}
	if created.Spec.OperatingMode != modeSuspended || created.Spec.Service == nil || !*created.Spec.Service {
		t.Errorf("the cache Sandbox was created %q with service %v, want Suspended with a Service", created.Spec.OperatingMode, created.Spec.Service)
	}
	stored := g.sandbox(name)
	secret := writes[1].object.(*corev1.Secret)
	if refs := secret.OwnerReferences; len(refs) != 1 || refs[0].Kind != "Sandbox" || refs[0].Name != name || refs[0].UID != stored.UID {
		t.Errorf("the cache Secret is owned by %+v, want the Sandbox %s uid %s", refs, name, stored.UID)
	}
	bearer := string(secret.Data[cacheTokenKey])
	if got := string(secret.Data[provisionTokenKey]); got != "ghs_provision_acme" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(bearer) || len(secret.Data) != 2 {
		t.Errorf("the cache Secret holds %v, want the implement token for acme and a 32-byte bearer token", secret.Data)
	}
	template := templatePatch(t, writes[2].patch)
	if template == nil || !modePatched(t, writes[2].patch, modeRunning) {
		t.Fatalf("the last write %q sets no pod template and Running", writes[2].patch)
	}
	spec := template.Spec
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		t.Fatalf("the cache pod runs %d containers and %d init containers, want the cache alone", len(spec.Containers), len(spec.InitContainers))
	}
	c := spec.Containers[0]
	if want := []string{"/opt/legion/bin/legion", "workspace-init", "cache", "--repo", "acme/widgets", "--root", CacheRoot, "--listen", ":8418", "--refresh", "10m0s"}; c.Name != cacheContainer || !slices.Equal(c.Command, want) || c.Image != testImage {
		t.Errorf("the cache container is %s running %q on %s, want %s running %q on %s", c.Name, c.Command, c.Image, cacheContainer, want, testImage)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Path != "/readyz" || p.HTTPGet.Port.IntValue() != 8418 || len(p.HTTPGet.HTTPHeaders) != 0 {
		t.Errorf("the cache container's readiness probe is %+v, want GET /readyz on 8418 with no header", p)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if m := mounts[ProvisionDir]; m.Name != provisionVolume || !m.ReadOnly {
		t.Errorf("the token is mounted %+v, want %s read-only at %s", m, provisionVolume, ProvisionDir)
	}
	if m := mounts[CacheRoot]; m.Name != cacheVolume || m.ReadOnly {
		t.Errorf("the cache volume is mounted %+v, want %s at %s", m, cacheVolume, CacheRoot)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["LEGION_PROVISION_TOKEN_FILE"] != ProvisionDir+"/"+provisionTokenKey || env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] != ProvisionDir+"/"+cacheTokenKey {
		t.Errorf("LEGION_PROVISION_TOKEN_FILE = %q, LEGION_REPOSITORY_CACHE_TOKEN_FILE = %q", env["LEGION_PROVISION_TOKEN_FILE"], env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"])
	}
	for name, value := range env {
		if strings.HasPrefix(name, "DISPATCH") || strings.HasSuffix(name, "_FILE") && name != "LEGION_PROVISION_TOKEN_FILE" && name != "LEGION_REPOSITORY_CACHE_TOKEN_FILE" || value == bearer {
			t.Errorf("the cache container is told %s=%q", name, value)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == providersVolume {
			t.Errorf("the cache pod mounts the providers Secret")
		}
		if v.Name == provisionVolume && (v.Secret == nil || v.Secret.SecretName != cacheSecretName(name) || len(v.Secret.Items) != 2) {
			t.Errorf("the token volume is %+v, want both keys of the cache Secret %s", v, cacheSecretName(name))
		}
		if v.Name == cacheVolume && (v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != cacheVolume+"-"+name) {
			t.Errorf("the cache volume is %+v, want the claim %s", v, cacheVolume+"-"+name)
		}
	}
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != gvisor || spec.NodeSelector[poolKey] != poolValue || len(spec.Tolerations) == 0 || spec.Tolerations[0].Key != poolKey {
		t.Errorf("the cache pod is placed %v %v %v, want gVisor on the Legion pool", spec.RuntimeClassName, spec.NodeSelector, spec.Tolerations)
	}
	if spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("the cache pod restarts %s, want Always", spec.RestartPolicy)
	}
	if templates := stored.Spec.VolumeClaimTemplates; len(templates) != 1 || templates[0].Metadata.Name != cacheVolume || templates[0].Spec.Resources.Requests.Storage().String() != "20Gi" {
		t.Errorf("the cache Sandbox owns %+v, want one %s claim of 20Gi", templates, cacheVolume)
	}

	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache again: %v", err)
	}
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("ensuring a cache as it should be wrote %q", writes)
	}
	if again := string(g.secret(cacheSecretName(name)).Data[cacheTokenKey]); again != bearer {
		t.Errorf("the bearer token moved from %s to %s", bearer, again)
	}
}

// The name of one repository's cache held by another repository's cache is a refusal naming both,
// and nothing is written.
func TestEnsureRepositoryCacheRefusesAnotherRepositorysCacheAtTheName(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, "acme/gadgets")})
	g.clearActions()
	err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo)
	if err == nil || !strings.Contains(err.Error(), "acme/gadgets") || !strings.Contains(err.Error(), "acme/widgets") {
		t.Fatalf("EnsureRepositoryCache = %v, want a refusal naming both repositories", err)
	}
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the refusal wrote %q", writes)
	}
}

// A cache whose pod runs another image, or failed, or a cache Sandbox found Suspended with no pod,
// is brought back as a relaunch brings an issue pod: suspended (when it is not), its old pod waited
// out, then its pod template set on the runtime's image and Running in one patch. The Sandbox and
// its volume are kept: nothing is deleted, its claim template is as it was, and its Secret is
// updated in place, owned by the same uid.
func TestEnsureRepositoryCacheReplacesADriftedPodKeepingTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		pod     *corev1.Pod
		suspend bool
	}{
		{"a pod on another image", modeRunning, cachePod(cacheRepo, "uid-pod-old", "uid-cache", "ghcr.io/sjawhar/legion-worker@sha256:0ld", corev1.PodRunning), true},
		{"a failed pod", modeRunning, cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodFailed), true},
		{"a Suspended Sandbox with no pod", modeSuspended, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := CacheSandboxName(testProject, cacheRepo)
			stale := &corev1.Secret{
				ObjectMeta: bareRuntime(t).cacheSecretMeta(&sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-cache"}}, cacheRepo),
				Data:       map[string][]byte{provisionTokenKey: []byte("ghs_expired")},
			}
			objects := []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", tc.mode, ""), stale}
			if tc.pod != nil {
				objects = append(objects, tc.pod)
			}
			g := newRig(t, objects)
			before := g.sandbox(name).Spec.VolumeClaimTemplates
			g.clearActions()
			if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
				t.Fatalf("EnsureRepositoryCache: %v", err)
			}
			var suspended, running bool
			for _, w := range g.writes() {
				switch {
				case w.verb == "delete":
					t.Errorf("the replacement deleted %s", w)
				case w.resource == "secrets" && w.verb != "update":
					t.Errorf("the Secret was written by %s, want an update in place", w)
				case w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeSuspended):
					suspended = true
					if running {
						t.Error("the Sandbox was suspended after it was set Running")
					}
				case w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeRunning):
					running = true
					if template := templatePatch(t, w.patch); template == nil || template.Spec.Containers[0].Image != testImage {
						t.Errorf("the Running patch sets template %+v, want the runtime's image", template)
					}
				}
			}
			if suspended != tc.suspend || !running {
				t.Errorf("suspended %t and set Running %t, want %t and true", suspended, running, tc.suspend)
			}
			if secret := g.secret(cacheSecretName(name)); secret == nil || string(secret.Data[provisionTokenKey]) != "ghs_provision_acme" || secret.OwnerReferences[0].UID != "uid-cache" {
				t.Errorf("the cache Secret is %+v, want the fresh token owned by uid-cache", secret)
			}
			after := g.sandbox(name)
			if after.UID != "uid-cache" || !jsonEqual(t, after.Spec.VolumeClaimTemplates, before) {
				t.Errorf("the cache Sandbox is %s with templates %+v, want uid-cache with %+v", after.UID, after.Spec.VolumeClaimTemplates, before)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}

// The token refresh brings a cache's Secret back when it is lost, owned by the Sandbox's uid with a
// new bearer token, and replaces one another uid owns, keeping the bearer token it held, without
// touching the Sandbox; a Secret holding the fresh lease already is not written.
func TestRefreshCacheTokensRewritesALostOrForeignSecret(t *testing.T) {
	g := newRig(t, nil)
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	s := g.sandbox(name)
	secretsGVR := corev1.SchemeGroupVersion.WithResource("secrets")

	if err := g.kube.Tracker().Delete(secretsGVR, testNamespace, cacheSecretName(name)); err != nil {
		t.Fatal(err)
	}
	g.r.refreshCacheTokens(g.ctx)
	if secret := g.secret(cacheSecretName(name)); secret == nil || secret.OwnerReferences[0].UID != s.UID || string(secret.Data[provisionTokenKey]) != "ghs_provision_acme" || len(secret.Data[cacheTokenKey]) != 64 {
		t.Fatalf("after a refresh the lost Secret is %+v, want it back owned by %s with a bearer token", secret, s.UID)
	}
	bearer := string(g.secret(cacheSecretName(name)).Data[cacheTokenKey])
	if after := g.sandbox(name); after.mode() != s.mode() || after.Generation != s.Generation {
		t.Errorf("the refresh moved the Sandbox from %s/%d to %s/%d", s.mode(), s.Generation, after.mode(), after.Generation)
	}

	foreign := g.secret(cacheSecretName(name))
	foreign.OwnerReferences[0].UID = "uid-other"
	foreign.Data[provisionTokenKey] = []byte("ghs_provision_acme")
	if err := g.kube.Tracker().Update(secretsGVR, foreign, testNamespace); err != nil {
		t.Fatal(err)
	}
	g.clearActions()
	g.r.refreshCacheTokens(g.ctx)
	if writes := g.sandboxWrites(); !slices.Equal(writes, []string{"delete secrets " + cacheSecretName(name), "create secrets " + cacheSecretName(name)}) {
		t.Errorf("the refresh of a foreign Secret wrote %q, want it replaced", writes)
	}
	if secret := g.secret(cacheSecretName(name)); secret.OwnerReferences[0].UID != s.UID || string(secret.Data[cacheTokenKey]) != bearer {
		t.Errorf("the replaced Secret is owned by %s holding %q, want %s holding %q", secret.OwnerReferences[0].UID, secret.Data[cacheTokenKey], s.UID, bearer)
	}

	g.clearActions()
	g.r.refreshCacheTokens(g.ctx)
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the refresh of a current Secret wrote %q", writes)
	}
}

// A cache is ready, at its Service's http://<sandbox>.<namespace>.svc:8418, when the pod its Sandbox
// owns has passed its readiness probe, and not before, whatever address the pod holds; a long
// repository is found by the name it derives, with its whole name; and a cache name another
// repository's cache holds answers not found, logged once.
func TestRepositoryCacheReadsReadiness(t *testing.T) {
	logs := &logBuffer{}
	g := newRig(t, []k8sruntime.Object{cacheObject(t, ghrepo.MustParse("acme/gadgets"), "uid-gadgets", modeRunning, "acme/sprockets")}, withLog(logs))
	for _, repo := range []ghrepo.Repository{cacheRepo, longRepo} {
		if err := g.r.EnsureRepositoryCache(g.ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	status, found := g.r.RepositoryCache(cacheRepo)
	if !found || status.Ready || status.Address != "" || status.Sandbox != name || status.Repo != cacheRepo {
		t.Fatalf("RepositoryCache before readiness = %+v, %t; want found, not ready, no address", status, found)
	}
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.PodIP = "10.0.0.5"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	g.eventually("the cache ready", func() bool { status, _ := g.r.RepositoryCache(cacheRepo); return status.Ready })
	want := "http://" + name + "." + testNamespace + ".svc:8418"
	if status, _ := g.r.RepositoryCache(cacheRepo); status.Address != want || !status.RetainedUntil.IsZero() {
		t.Errorf("RepositoryCache once ready = %+v, want %s and no retention", status, want)
	}
	if status, found := g.r.RepositoryCache(longRepo); !found || status.Repo != longRepo {
		t.Errorf("RepositoryCache(%s) = %+v, %t; want found with the whole repository", longRepo, status, found)
	}
	for range 2 {
		if status, found := g.r.RepositoryCache(ghrepo.MustParse("acme/gadgets")); found {
			t.Errorf("the name acme/sprockets's cache holds answered %+v for acme/gadgets", status)
		}
	}
	if n := strings.Count(logs.String(), "acme/sprockets"); n != 1 {
		t.Errorf("the occupied name was logged %d times, want once:\n%s", n, logs)
	}
}

// crashLooping is pod with its cache container waiting to restart after failing four times.
func crashLooping(pod *corev1.Pod) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: cacheContainer, RestartCount: 4,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
	}}
	return pod
}

// A cache's state, as legion state shows it and a failed fetch names it: starting while its pod
// is absent or not yet running, cloning while its container runs and it is not ready, failing, with
// the kubelet's reason, while its container waits to restart or its image cannot be pulled, still
// starting while its readiness probe passes before the controller has made its Service, and ready,
// at that Service, once both are true.
func TestRepositoryCacheReadsItsState(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, "")}, withoutController())
	name := CacheSandboxName(testProject, cacheRepo)
	state := func() (string, string) {
		status, _ := g.r.RepositoryCache(cacheRepo)
		return status.State, status.Reason
	}
	if got, _ := state(); got != cacheStarting {
		t.Fatalf("a cache with no pod is %q, want %q", got, cacheStarting)
	}
	pod := cachePod(cacheRepo, "uid-cache-pod", "uid-cache", testImage, corev1.PodPending)
	if err := g.kube.Tracker().Add(pod); err != nil {
		t.Fatal(err)
	}
	g.eventually("the pending pod", func() bool { return g.r.storedPod(name) != nil })
	if got, _ := state(); got != cacheStarting {
		t.Errorf("a cache whose pod is pending is %q, want %q", got, cacheStarting)
	}
	for _, tc := range []struct {
		edit   func(*corev1.Pod)
		state  string
		reason string
	}{
		{func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: cacheContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}, cacheCloning, ""},
		{func(p *corev1.Pod) { crashLooping(p) }, cacheFailing, "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)"},
		{func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: cacheContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}}}}
		}, cacheFailing, "container cache ImagePullBackOff: pull access denied (restarted 0 times)"},
		{func(p *corev1.Pod) {
			p.Status.PodIP = "10.0.0.5"
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: cacheContainer, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}, cacheStarting, ""},
	} {
		g.update(g.pod(name), tc.edit)
		// Two steps in a row are failing: the step is the store's once its reason is the step's too.
		g.eventually(fmt.Sprintf("the cache %s, saying %q", tc.state, tc.reason), func() bool {
			got, reason := state()
			return got == tc.state && reason == tc.reason
		})
	}
	if err := g.kube.Tracker().Add(cacheService(name, "uid-cache")); err != nil {
		t.Fatal(err)
	}
	g.eventually("the cache ready behind its Service", func() bool {
		status, _ := g.r.RepositoryCache(cacheRepo)
		return status.State == cacheReady && status.Ready && status.Address == g.r.cacheURL(name)
	})
}

// The boot census keeps a cache Sandbox the runtime builds and refuses, by name, a Sandbox marked
// as a cache that is not one: no repository annotation, an issue's claim template, or a container
// more. An issue Sandbox owning a cache's claim template is still refused as an issue Sandbox.
func TestCensusAcceptsACacheSandboxAndRefusesAMalformedOne(t *testing.T) {
	g := newRig(t, nil)
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	g.spawn(rootSpec(t))
	sandboxes := g.dyn.Resource(sandboxGVR).Namespace(testNamespace)
	if err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject); err != nil {
		t.Fatalf("census of a cache Sandbox and an issue pod = %v, want none", err)
	}
	malformed := map[string]func(u *unstructured.Unstructured){
		"no-annotation": func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "metadata", "annotations")
		},
		"issue-template": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"metadata": map[string]any{"name": issueVolume}}}, "spec", "volumeClaimTemplates")
		},
		"two-containers": func(u *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "containers")
			_ = unstructured.SetNestedSlice(u.Object, append(containers, map[string]any{"name": "sidecar"}), "spec", "podTemplate", "spec", "containers")
		},
	}
	for suffix, edit := range malformed {
		u := cacheObject(t, ghrepo.MustParse("acme/"+suffix), "uid-"+suffix, modeRunning, "")
		edit(u)
		if err := g.dyn.Tracker().Create(sandboxGVR, u, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	issue := sandboxObject(t, "legion-legion-legion-300", "uid-300", modeSuspended, map[string]string{labelProject: testProject, labelTree: "LEGION-300", labelIssue: "LEGION-300"})
	_ = unstructured.SetNestedSlice(issue.Object, []any{map[string]any{"metadata": map[string]any{"name": cacheVolume}}}, "spec", "volumeClaimTemplates")
	containers := []any{}
	for _, role := range []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"} {
		containers = append(containers, map[string]any{"name": role})
	}
	_ = unstructured.SetNestedSlice(issue.Object, containers, "spec", "podTemplate", "spec", "containers")
	if err := g.dyn.Tracker().Create(sandboxGVR, issue, testNamespace); err != nil {
		t.Fatal(err)
	}
	err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject)
	if err == nil {
		t.Fatal("census of malformed caches passed")
	}
	for suffix := range malformed {
		if !strings.Contains(err.Error(), "cache Sandbox "+CacheSandboxName(testProject, ghrepo.MustParse("acme/"+suffix))) {
			t.Errorf("census = %q, want it to refuse the cache Sandbox of acme/%s", err, suffix)
		}
	}
	if !strings.Contains(err.Error(), "Sandbox legion-legion-legion-300 owns a volume claim template named \"cache\"") {
		t.Errorf("census = %q, want the issue Sandbox with a cache template refused by its layout", err)
	}
	if strings.Contains(err.Error(), CacheSandboxName(testProject, cacheRepo)) {
		t.Errorf("census = %q, which refuses the cache this runtime built", err)
	}
}

// The orphan sweep makes a mapped repository's cache that is missing, keeps it, replacing a pod of
// it that drifted, and retires an unmapped one: marked with when it left the mapping, kept while
// that is under Retain, then deleted in the foreground with its volume; one mapped again before
// then loses the mark and stays. The claim path's sweep never judges a cache.
func TestReconcileOrphansKeepsAMappedCacheAndRetiresAnUnmappedOne(t *testing.T) {
	var mapped atomic.Bool
	mapped.Store(true)
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) {
		o.Mapped = func() []ghrepo.Repository {
			if mapped.Load() {
				return []ghrepo.Repository{cacheRepo}
			}
			return nil
		}
	}))
	var propagation []metav1.DeletionPropagation
	g.dyn.PrependReactor("delete", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if policy := a.(k8stesting.DeleteActionImpl).DeleteOptions.PropagationPolicy; policy != nil {
			propagation = append(propagation, *policy)
		}
		return false, nil, nil
	})
	name := CacheSandboxName(testProject, cacheRepo)
	sweep := func(what string) {
		t.Helper()
		g.clearActions()
		if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans %s: %v", what, err)
		}
	}
	unmappedAt := func() string {
		g.t.Helper()
		if s := g.sandbox(name); s != nil {
			return s.Annotations[annotationCacheUnmappedAt]
		}
		return "gone"
	}

	sweep("with a mapped repository that has no cache")
	if writes := g.sandboxWrites(); !slices.Contains(writes, "create sandboxes "+name) {
		t.Fatalf("the sweep wrote %q, want the mapped repository's cache made", writes)
	}
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	g.eventually("the Running cache in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.mode() == modeRunning
	})

	sweep("of a mapped cache")
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the sweep of a mapped cache wrote %q", writes)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("the sweep of a mapped cache warned:\n%s", logs)
	}

	mapped.Store(false)
	sweep("of an unmapped cache")
	marked := unmappedAt()
	if at, err := time.Parse(time.RFC3339, marked); err != nil || !at.Equal(rigNow.Truncate(time.Second)) {
		t.Fatalf("the unmapped cache is marked %q, want the sweep's time %s", marked, rigNow)
	}
	g.eventually("the mark in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnmappedAt] == marked
	})
	g.advance(167 * time.Hour)
	sweep("within the retention")
	if unmappedAt() != marked || len(propagation) != 0 {
		t.Fatalf("the cache inside its retention is %q, deleted %v; want it kept as marked", unmappedAt(), propagation)
	}

	mapped.Store(true)
	sweep("of a cache mapped again")
	if got := unmappedAt(); got != "" {
		t.Fatalf("the cache mapped again is still marked %q", got)
	}
	g.eventually("the patch in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnmappedAt] == ""
	})

	g.update(g.pod(name), func(p *corev1.Pod) { p.Spec.Containers[0].Image = "ghcr.io/sjawhar/legion-worker@sha256:0ld" })
	g.eventually("the drifted pod in the store", func() bool {
		_, drifted := imageDrift(g.r.storedPod(name), testImage)
		return drifted
	})
	sweep("of a mapped cache whose pod drifted")
	var suspended, running bool
	for _, w := range g.writes() {
		if w.resource == "sandboxes" && w.verb == "patch" {
			suspended = suspended || modePatched(t, w.patch, modeSuspended)
			running = running || modePatched(t, w.patch, modeRunning)
		}
	}
	if !suspended || !running {
		t.Errorf("the sweep of a drifted cache suspended %t and set Running %t, want both", suspended, running)
	}

	mapped.Store(false)
	sweep("of an unmapped cache again")
	g.eventually("the mark in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnmappedAt] != ""
	})
	g.advance(168 * time.Hour)
	sweep("past the retention")
	if g.sandbox(name) != nil || !slices.Equal(propagation, []metav1.DeletionPropagation{metav1.DeletePropagationForeground}) {
		t.Fatalf("the cache past its retention is %v, deleted with %v; want it deleted in the foreground", g.sandbox(name), propagation)
	}
	if !strings.Contains(logs.String(), "acme/widgets") || !strings.Contains(logs.String(), "168h0m0s") {
		t.Errorf("the retirement was not logged with its repository and how long it was kept:\n%s", logs)
	}
}

// A mapped repository whose cache cannot be made — its owner's token cannot be minted — is logged
// as the sweep's warning naming the cache and tried at the next sweep; ReconcileOrphans answers
// nil, since its error holds the claims' launches back at boot and each launch ensures its own
// repository's cache.
func TestACacheThatCannotBeMadeNeverFailsTheReconciliation(t *testing.T) {
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) {
		o.Tokens = refusingTokens{}
		o.Mapped = func() []ghrepo.Repository { return []ghrepo.Repository{cacheRepo} }
	}))
	for range 2 {
		if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans with a cache that cannot be made = %v, want nil", err)
		}
	}
	name := CacheSandboxName(testProject, cacheRepo)
	warnings := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "repository cache sweep failed") && strings.Contains(line, name) && strings.Contains(line, "GitHub is down") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Errorf("%d sweep warnings name %s and the mint's failure, want one per sweep:\n%s", warnings, name, logs)
	}
}

// A tree's cleanup lists the tree's Sandboxes, which a cache never is: it survives.
func TestCleanupTreeNeverListsACache(t *testing.T) {
	g := newRig(t, nil)
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	g.spawn(rootSpec(t))
	g.store.close(testTree)
	if err := g.r.CleanupTree(g.ctx, testTree); err != nil {
		t.Fatalf("CleanupTree: %v", err)
	}
	if g.sandbox(SandboxName(rootToken)) != nil {
		t.Fatal("the tree's own Sandbox survived its cleanup")
	}
	if g.sandbox(CacheSandboxName(testProject, cacheRepo)) == nil {
		t.Error("the tree's cleanup deleted the repository cache")
	}
}

// The gh refresher walks the issue and controller pods; a cache pod, which has no role, is passed
// over with nothing logged and nothing written.
func TestGitHubRefreshSkipsCachePods(t *testing.T) {
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs))
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	before := logs.String()
	g.clearActions()
	g.r.refreshGitHubCredentials(g.ctx)
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the gh refresh wrote %q", writes)
	}
	if after := strings.TrimPrefix(logs.String(), before); after != "" {
		t.Errorf("the gh refresh logged over a cache pod:\n%s", after)
	}
}

// mountsOf is a container's mounts of the issue volume, as "<path> <subPath> <ro|rw>".
func mountsOf(c corev1.Container) []string {
	var mounts []string
	for _, m := range c.VolumeMounts {
		if m.Name != issueVolume {
			continue
		}
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		mounts = append(mounts, m.MountPath+" "+m.SubPath+" "+mode)
	}
	slices.Sort(mounts)
	return mounts
}

// The feed lives on the issue's own volume, under the subPath feed: workspace-fetch mounts that
// alone, read-write, and no other part of the volume; workspace-init and every role container
// mount the volume whole and the feed again, read-only, over it, so no agent can write the feed the
// token-holding container reads. No emptyDir feed is left.
func TestTheFeedIsOnTheIssueVolumeAndOnlyTheFetchWritesIt(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(rootSpec(t))
	spec := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Spec
	feed := FeedDir + " " + feedSubPath
	for _, c := range spec.InitContainers {
		switch c.Name {
		case fetchContainer:
			if got := mountsOf(c); !slices.Equal(got, []string{feed + " rw"}) {
				t.Errorf("workspace-fetch mounts the issue volume %q, want the feed alone, read-write", got)
			}
		case initContainer:
			if got := mountsOf(c); !slices.Equal(got, []string{TreeRoot + "  rw", feed + " ro"}) {
				t.Errorf("workspace-init mounts the issue volume %q, want it whole and the feed read-only", got)
			}
		}
	}
	for _, c := range spec.Containers {
		if got := mountsOf(c); !slices.Contains(got, feed+" ro") || !slices.Contains(got, TreeRoot+"  rw") || slices.Contains(got, feed+" rw") {
			t.Errorf("role container %s mounts the issue volume %q, want it whole and the feed read-only", c.Name, got)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == "feed" {
			t.Errorf("the pod still has a volume named feed: %+v", v)
		}
	}
	if FeedDir != TreeRoot+"/"+feedSubPath {
		t.Errorf("FeedDir is %s, want the feed subPath under TreeRoot, where every container sees it", FeedDir)
	}
}

// fetchEnv is the workspace-fetch container's environment in the pod template of the Sandbox name.
func (g *rig) fetchEnv(name string) map[string]string {
	env := map[string]string{}
	for _, c := range g.sandbox(name).Spec.PodTemplate.Spec.InitContainers {
		if c.Name == fetchContainer {
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
		}
	}
	return env
}

// Every issue pod's fetch container is told its repository's cache — its Service's name, which
// resolves while the cache's pod is Ready and not before, so the fetch waits on it — and the cache's
// bearer token, as a file projected from the issue pod's own provisioning Secret, never an
// environment value. A launch makes the cache when the repository has none yet. The token reaches
// the fetch container alone, and is the cache's own.
func TestAnIssuePodsFetchIsToldItsRepositorysCacheAndItsToken(t *testing.T) {
	g := newRig(t, nil)
	repo := rootSpec(t).Repository
	loc := g.spawn(rootSpec(t))
	cache := CacheSandboxName(testProject, repo)
	if g.sandbox(cache) == nil {
		t.Fatalf("launching an issue of %s made no cache", repo)
	}
	bearer := string(g.secret(cacheSecretName(cache)).Data[cacheTokenKey])
	env := g.fetchEnv(loc.Sandbox.Name)
	if want := "http://" + cache + "." + testNamespace + ".svc:8418"; env["LEGION_REPOSITORY_CACHE_URL"] != want {
		t.Errorf("the fetch is told LEGION_REPOSITORY_CACHE_URL=%q, want %q", env["LEGION_REPOSITORY_CACHE_URL"], want)
	}
	if env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] != ProvisionDir+"/"+cacheTokenKey {
		t.Errorf("the fetch is told LEGION_REPOSITORY_CACHE_TOKEN_FILE=%q", env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"])
	}
	if issueSecret := g.secret(secretName(loc.Sandbox.Name)); issueSecret == nil || string(issueSecret.Data[cacheTokenKey]) != bearer || bearer == "" {
		t.Errorf("the issue pod's provisioning Secret holds %v, want the cache's bearer %q", issueSecret, bearer)
	}
	spec := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Spec
	for _, v := range spec.Volumes {
		if v.Name == provisionVolume && (v.Secret == nil || !slices.ContainsFunc(v.Secret.Items, func(k corev1.KeyToPath) bool { return k.Key == cacheTokenKey })) {
			t.Errorf("the provisioning volume %+v projects no cache token", v)
		}
	}
	for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, e := range c.Env {
			if e.Value == bearer {
				t.Errorf("container %s is told the cache's token in %s", c.Name, e.Name)
			}
		}
		if c.Name != fetchContainer && slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == provisionVolume }) {
			t.Errorf("container %s mounts the provisioning Secret", c.Name)
		}
	}
}

// RepositoryCaches is every cache the stores hold, by repository, each with what its /healthz
// answered at the last health read (on the gh refresher's tick): a Ready cache's head, bytes and
// last fetch, a cache that could not be read with why, and one not Ready with nothing read. A cache
// of a repository no longer mapped carries when the sweep retires it.
func TestRepositoryCachesCarryTheirLastHealthRead(t *testing.T) {
	fetched := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	var presented atomic.Value
	presented.Store("")
	served := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		presented.Store(r.Header.Get("Authorization"))
		w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
		_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: "acme/widgets", Head: "0123abcd", LastFetchAt: fetched, Bytes: 6612345678})
	}))
	t.Cleanup(served.Close)
	network := &podNetwork{routes: map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}}
	gadgets := ghrepo.MustParse("acme/gadgets")
	var mapped atomic.Bool
	mapped.Store(true)
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Mapped = func() []ghrepo.Repository {
			if mapped.Load() {
				return []ghrepo.Repository{cacheRepo, gadgets}
			}
			return []ghrepo.Repository{cacheRepo}
		}
	}))
	g.r.cacheDial = network.dial
	for _, repo := range []ghrepo.Repository{cacheRepo, gadgets} {
		if err := g.r.EnsureRepositoryCache(g.ctx, repo); err != nil {
			t.Fatal(err)
		}
		name := CacheSandboxName(testProject, repo)
		g.eventually("the cache pod of "+repo.String(), func() bool { return g.r.storedPod(name) != nil })
	}
	g.update(g.pod(CacheSandboxName(testProject, cacheRepo)), func(p *corev1.Pod) {
		p.Status.PodIP = "10.0.0.5"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	g.eventually("the cache ready", func() bool { status, _ := g.r.RepositoryCache(cacheRepo); return status.Ready })
	mapped.Store(false)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	g.eventually("acme/gadgets marked", func() bool {
		s, _ := g.r.storedSandbox(CacheSandboxName(testProject, gadgets))
		return s != nil && s.Annotations[annotationCacheUnmappedAt] != ""
	})

	before := g.r.RepositoryCaches()
	if len(before) != 2 || before[0].Health != nil || before[0].HealthError != "" {
		t.Fatalf("RepositoryCaches before a health read = %+v, want two caches and nothing read", before)
	}
	g.r.readCacheHealth(g.ctx)
	if presented.Load() != "" {
		t.Errorf("the health read presented %q, want no credential", presented.Load())
	}
	caches := g.r.RepositoryCaches()
	if len(caches) != 2 || caches[0].Repo != gadgets || caches[1].Repo != cacheRepo {
		t.Fatalf("RepositoryCaches = %+v, want acme/gadgets then acme/widgets", caches)
	}
	if gadgetsCache := caches[0]; gadgetsCache.Ready || gadgetsCache.Health != nil || gadgetsCache.HealthError != "" || gadgetsCache.RetainedUntil.IsZero() {
		t.Errorf("the unmapped, not-Ready cache is %+v, want nothing read and its retention", gadgetsCache)
	}
	widgets := caches[1]
	if !widgets.Ready || widgets.Health == nil || widgets.Health.Head != "0123abcd" || widgets.Health.Bytes != 6612345678 || !widgets.Health.LastFetchAt.Equal(fetched) || widgets.HealthError != "" {
		t.Errorf("the Ready cache is %+v (health %+v), want its /healthz", widgets, widgets.Health)
	}

	served.Close()
	g.r.readCacheHealth(g.ctx)
	if widgets := g.r.RepositoryCaches()[1]; widgets.Health != nil || !strings.Contains(widgets.HealthError, "connection refused") {
		t.Errorf("a cache whose /healthz cannot be read is %+v, want the read's failure and no health", widgets)
	}
}

// podNetwork is a cacheDial for a daemon outside the cluster: it reaches pod addresses, each
// served by the listener routes names, and resolves no name at all, as a host with no cluster DNS
// does. It records every address dialed.
type podNetwork struct {
	routes map[string]string
	mu     sync.Mutex
	dialed []string
}

func (n *podNetwork) dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	n.dialed = append(n.dialed, address)
	n.mu.Unlock()
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) == nil {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	to, routed := n.routes[address]
	if !routed {
		return nil, fmt.Errorf("dial %s: no route to host", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, to)
}

func (n *podNetwork) addresses() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.dialed)
}

// healthServer is a cache's /healthz at an httptest listener, naming repo and recording the
// Authorization header of every request it answers.
func healthServer(t *testing.T, repo string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var credentials []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set(workspace.CacheRepositoryHeader, repo)
		_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: repo, Head: "0123abcd"})
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(credentials)
	}
}

// readyCache makes cacheRepo's cache and marks its pod Ready at podIP, returning the Sandbox's name.
func readyCache(t *testing.T, g *rig, podIP string) string {
	t.Helper()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.PodIP = podIP
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	g.eventually("the cache ready", func() bool { status, _ := g.r.RepositoryCache(cacheRepo); return status.Ready })
	return name
}

// A health read asks the cache pod at its address, as the store holds it, and carries no
// credential: /healthz holds nothing secret, so a daemon outside the cluster, which reaches pod
// addresses but resolves no cluster name, reads every cache's health. On the wire the request has
// no Authorization header. A read is the pod's it was made of: once that pod is replaced, its
// successor shows none until it is read.
func TestTheHealthReadAsksThePodAddressWithNoCredential(t *testing.T) {
	served, credentials := healthServer(t, cacheRepo.String())
	g := newRig(t, nil)
	network := &podNetwork{routes: map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}}
	g.r.cacheDial = network.dial
	name := readyCache(t, g, "10.0.0.5")

	g.r.readCacheHealth(g.ctx)

	if dialed := network.addresses(); !slices.Equal(dialed, []string{"10.0.0.5:8418"}) {
		t.Errorf("the health read dialed %q, want the pod's address alone", dialed)
	}
	if got := credentials(); !slices.Equal(got, []string{""}) {
		t.Errorf("the cache's /healthz was asked with Authorization %q, want one request carrying none", got)
	}
	caches := g.r.RepositoryCaches()
	if len(caches) != 1 || caches[0].Health == nil || caches[0].Health.Head != "0123abcd" || caches[0].HealthError != "" {
		t.Fatalf("RepositoryCaches = %+v, want the cache's own health", caches)
	}
	g.update(g.pod(name), func(p *corev1.Pod) { p.UID = "uid-cache-pod-successor" })
	g.eventually("the successor pod", func() bool { pod := g.r.storedPod(name); return pod != nil && pod.UID == "uid-cache-pod-successor" })
	if caches := g.r.RepositoryCaches(); len(caches) != 1 || caches[0].Health != nil || caches[0].HealthError != "" {
		t.Errorf("RepositoryCaches after the pod was replaced = %+v, want no read shown for the successor", caches)
	}
}

// A stranger holding the cache pod's address, as a pod that took it once the cache's pod was gone
// would, is asked the health with no credential, so it learns nothing: the cache's token goes to
// no health read. Its answer names another repository, which the read refuses: the cache shows that
// read's failure, and no health, until the next.
func TestAStrangerAtTheCachePodsAddressGetsNoCredential(t *testing.T) {
	stranger, credentials := healthServer(t, "acme/gadgets")
	g := newRig(t, nil)
	g.r.cacheDial = (&podNetwork{routes: map[string]string{"10.0.0.5:8418": stranger.Listener.Addr().String()}}).dial
	readyCache(t, g, "10.0.0.5")

	g.r.readCacheHealth(g.ctx)

	if got := credentials(); len(got) != 1 || got[0] != "" {
		t.Errorf("the stranger was asked with Authorization %q, want one request carrying none", got)
	}
	caches := g.r.RepositoryCaches()
	if len(caches) != 1 || caches[0].Health != nil || !strings.Contains(caches[0].HealthError, `naming the repository "acme/gadgets", not acme/widgets`) {
		t.Fatalf("RepositoryCaches = %+v, want the read refused for naming acme/gadgets and no health", caches)
	}
}

// cacheService is the headless Service the Agent Sandbox controller makes for the cache Sandbox
// name with uid: controlled by it, and selecting its pod by the controller's hash of its name.
func cacheService(name string, uid types.UID) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace, Labels: map[string]string{sandboxNameHashLabel: sandboxNameHash(name)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: uid, Controller: new(true)}},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{sandboxNameHashLabel: sandboxNameHash(name)}},
	}
}

// sandboxNameHash is the Agent Sandbox controller's NameHash (v1.0.3): the values below are what
// the controller's own function returns for these names, so the selector the daemon requires of a
// cache's Service is the one the controller writes.
func TestSandboxNameHashIsTheControllersNameHash(t *testing.T) {
	for name, want := range map[string]string{
		"":  "811c9dc5",
		"a": "e40c292c",
		"legion-legion-cache-acme-widgets-d782c874": "7e13b444",
	} {
		if got := sandboxNameHash(name); got != want {
			t.Errorf("sandboxNameHash(%q) = %s, want the controller's %s", name, got, want)
		}
	}
}

// The issue pods' fetch reaches the cache at its Service with the cache's token, so the daemon
// hands an issue pod the cache's address only once that Service is the cache Sandbox's: controlled
// by it, by uid, selecting exactly its pod by the controller's hash of its name, and headless with
// no external name. A Service of the cache's name that is not is refused: one no Sandbox controls
// and without the controller's tracking label, which anyone who may create a Service could have
// made first and which the controller refuses to adopt; one another Sandbox controls; one selecting
// other pods; and an ExternalName Service planted with the tracking label, as the controller
// leaves it once it has adopted it — its own controller reference and the exact selector written,
// its type and external name kept. The launch waits on it, logging why, and fails at the boot
// timeout naming the Service, with no issue pod made and no token written for one; and legion
// state shows the cache refused, naming the Service and why.
func TestAnIssuePodGetsNoCacheAddressFromAServiceThatIsNotTheCaches(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*corev1.Service)
		says string
	}{
		"a Service selecting other pods": {func(s *corev1.Service) { s.Spec.Selector = map[string]string{"app": "elsewhere"} }, "selects map[app:elsewhere]"},
		"a Service no Sandbox controls": {func(s *corev1.Service) {
			s.OwnerReferences, s.Labels = nil, map[string]string{"app": "elsewhere"}
		}, "has no controller"},
		"a Service another Sandbox controls": {func(s *corev1.Service) { s.OwnerReferences[0].UID = "uid-other" }, "(uid-other), not the cache's Sandbox (uid-cache)"},
		"an ExternalName Service the controller adopted": {func(s *corev1.Service) {
			s.Spec.Type, s.Spec.ClusterIP, s.Spec.ExternalName = corev1.ServiceTypeExternalName, "", "collector.attacker.example"
		}, "is a ExternalName Service"},
		"a ClusterIP Service whose maker wrote the controller reference": {func(s *corev1.Service) {
			s.Spec.Type, s.Spec.ClusterIP = corev1.ServiceTypeClusterIP, "10.0.0.196"
		}, "is a ClusterIP Service"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := rootSpec(t)
			cache := CacheSandboxName(testProject, spec.Repository)
			service := cacheService(cache, "uid-cache")
			tc.edit(service)
			logs := &logBuffer{}
			g := newRig(t, []k8sruntime.Object{cacheObject(t, spec.Repository, "uid-cache", modeRunning, ""), service}, withLog(logs))

			_, err := g.r.Spawn(g.ctx, spec)
			if err == nil || !strings.Contains(err.Error(), "the repository cache's Service "+cache) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Spawn = %v, want the launch refused naming the Service %s and %q", err, cache, tc.says)
			}
			issue := SandboxName(spec.Claim)
			if pod := g.pod(issue); pod != nil {
				t.Errorf("the refused launch made the issue pod %s", pod.Name)
			}
			if secret := g.secret(secretName(issue)); secret != nil {
				t.Errorf("the refused launch wrote the provisioning Secret %s: %v", secret.Name, slices.Sorted(maps.Keys(secret.Data)))
			}
			if !strings.Contains(logs.String(), "service="+cache) || !strings.Contains(logs.String(), tc.says) {
				t.Errorf("the launch logged %q, want its wait on the Service %s and why", logs.String(), cache)
			}
			status, found := g.r.RepositoryCache(spec.Repository)
			if !found || status.State != cacheRefused || status.Ready || status.Address != "" || !strings.Contains(status.Reason, "Service "+cache) || !strings.Contains(status.Reason, tc.says) {
				t.Errorf("RepositoryCache = %+v, %t; want it refused, naming the Service %s and %q", status, found, cache, tc.says)
			}
		})
	}
}

// While the Services list or watch is failing, the Service store may hold a Service that has since
// changed, so even the cache's own Service is not trusted: a launch waits, logging why, and fails
// at the boot timeout naming the Service and the stale store, with nothing written, and legion
// state shows the cache refused, saying its Service cannot be checked. Once a request succeeds the
// store is trusted again. The failure recorded here is what the Service informer's list or watch
// records when the API server refuses it.
func TestNoServiceIsTrustedWhileTheServiceWatchFails(t *testing.T) {
	spec := rootSpec(t)
	cache := CacheSandboxName(testProject, spec.Repository)
	logs := &logBuffer{}
	g := newRig(t, []k8sruntime.Object{cacheObject(t, spec.Repository, "uid-cache", modeRunning, ""), cacheService(cache, "uid-cache")}, withLog(logs))
	g.r.serviceFeed.record(errors.New(`services is forbidden: User "legion-daemon" cannot watch resource "services"`), time.Now())
	says := "cannot be checked: the Service store may be stale"

	_, err := g.r.Spawn(g.ctx, spec)
	if err == nil || !strings.Contains(err.Error(), "the repository cache's Service "+cache+" "+says) {
		t.Fatalf("Spawn = %v, want the launch refused naming the Service %s and %q", err, cache, says)
	}
	issue := SandboxName(spec.Claim)
	if pod, secret := g.pod(issue), g.secret(secretName(issue)); pod != nil || secret != nil {
		t.Errorf("the refused launch made pod %v and Secret %v, want neither", pod != nil, secret != nil)
	}
	if !strings.Contains(logs.String(), "service="+cache) || !strings.Contains(logs.String(), says) {
		t.Errorf("the launch logged %q, want its wait on the Service %s and why", logs.String(), cache)
	}
	if status, _ := g.r.RepositoryCache(spec.Repository); status.State != cacheRefused || status.Address != "" || !strings.Contains(status.Reason, "its Service "+cache+" "+says) {
		t.Errorf("RepositoryCache = %+v, want it refused, its Service unchecked", status)
	}

	g.r.serviceFeed.record(nil, time.Now())
	if refusal, made := g.r.cacheServiceRefusal(cache, "uid-cache"); refusal != "" || !made {
		t.Errorf("cacheServiceRefusal once the watch is fed again = %q, %t; want the cache's own Service", refusal, made)
	}
}
