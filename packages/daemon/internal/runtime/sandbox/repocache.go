package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A repository cache is the third kind of Sandbox the runtime makes, beside the issue pods and the
// controller's (podKind) and the image probe: one per mapped repository, holding a bare clone on a
// volume of its own and serving it to the issue pods' fetch containers (internal/repocache). It is
// no claim: nothing registers on the worker stream for it, no claim.Token names it, and `legion
// state` lists it under the repositories. Every place that lists the project's Sandboxes tells a
// cache apart by its marker label (labelCache) before anything else, as each does the probe.

// RepositoryCache is runtime.kubernetes.repository_cache: the volume each cache Sandbox owns, how
// often its process fetches GitHub, how long the cache of a repository the mapping no longer names
// is kept before the sweep deletes it, and the one container's reservation.
type RepositoryCache struct {
	Volume    resource.Quantity
	Refresh   time.Duration
	Retain    time.Duration
	Resources corev1.ResourceRequirements
}

// check is why c is no configuration a runtime can make caches with, or nil.
func (c RepositoryCache) check() error {
	switch {
	case c.Volume.Sign() <= 0:
		return fmt.Errorf("repository cache volume %s is not a positive quantity", c.Volume.String())
	case c.Refresh <= 0:
		return fmt.Errorf("repository cache refresh %s is not positive", c.Refresh)
	case c.Retain <= 0:
		return fmt.Errorf("repository cache retention %s is not positive", c.Retain)
	}
	return checkRequirements("the repository cache", c.Resources)
}

// CacheStatus is a repository's cache as the runtime's stores show it: the repository, exactly as
// the cache's annotation names it; its Sandbox; its State (starting, cloning, ready, failing or
// refused), with the kubelet's Reason while it is failing and its Service's while it is refused;
// whether its pod has passed its readiness probe behind its own Service, and its address then,
// that Service's (cacheURL); once the repository has left the mapping, until when the sweep keeps
// it; and, from RepositoryCaches alone, the pod's last health read: what its /healthz answered
// (Health), or why it could not be read (HealthError), both empty when it was not Ready to be read.
type CacheStatus struct {
	Repo          ghrepo.Repository
	Sandbox       string
	State         string
	Reason        string
	Ready         bool
	Address       string
	RetainedUntil time.Time
	Health        *repocache.Health
	HealthError   string
}

// cacheRepository is the repository a cache Sandbox, pod or Secret is for: its annotationCacheRepo,
// parsed. The one way any reader learns a cache's repository; the name is never parsed back, and
// the marker label names none.
func cacheRepository(annotations map[string]string) (ghrepo.Repository, error) {
	return ghrepo.Parse(annotationCacheRepo, annotations[annotationCacheRepo])
}

// sameRepository is whether a and b are one GitHub repository, which GitHub names regardless of
// case, as CacheSandboxName does.
func sameRepository(a, b ghrepo.Repository) bool { return strings.EqualFold(a.String(), b.String()) }

// cacheLabels are every object of a cache's labels: the project and the marker, no tree, issue,
// role or probe.
func (r *Runtime) cacheLabels() map[string]string {
	return map[string]string{labelProject: r.project, labelCache: cacheMarker}
}

// cacheManifest is repo's Sandbox as it is created: Suspended, as sandboxManifest creates every
// Sandbox, so no pod starts before its Secret exists; with a headless Service of its name
// (cacheURL), which resolves to the pod only while it is Ready; owning one claim template,
// cacheVolume, of the configured size, on the issue volumes' class; carrying the cache labels and
// the repository's annotation, as its pod template does.
func (r *Runtime) cacheManifest(repo ghrepo.Repository) sandbox {
	name := CacheSandboxName(r.project, repo)
	storageClass := r.storageClass
	return sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: r.namespace, Labels: r.cacheLabels(),
			Annotations: map[string]string{annotationCacheRepo: repo.String()},
		},
		Spec: sandboxSpec{
			PodTemplate: r.cachePodTemplate(name, repo), OperatingMode: modeSuspended, Service: new(true),
			VolumeClaimTemplates: []volumeClaimTemplate{{
				Metadata: volumeClaimMetadata{Name: cacheVolume, Labels: r.cacheLabels()},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &storageClass,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: r.cacheConfig.Volume},
					},
				},
			}},
		},
	}
}

// cachePodTemplate is a cache's pod: one container, cacheContainer, running `legion workspace-init
// cache` on the runtime's image, on the Legion pool under gVisor with the workers' pod security and
// the probe's placement, mounting its volume at CacheRoot and its Secret's two tokens read-only —
// the provisioning token, as workspace-fetch mounts the issue pod's (LEGION_PROVISION_TOKEN_FILE),
// and the bearer token its clients present (LEGION_REPOSITORY_CACHE_TOKEN_FILE) — with a memory
// TMPDIR for the fetch's one-shot credential and a readinessProbe of GET /readyz on
// repocache.Port, which needs no token. Restarted Always. It carries no model-route volume,
// provider key, operator mount or Dispatch variable: the process reads GitHub and serves pods,
// nothing else.
func (r *Runtime) cachePodTemplate(name string, repo ghrepo.Repository) podTemplate {
	memory := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}
	container := corev1.Container{
		Name:  cacheContainer,
		Image: r.image,
		Command: []string{
			r.tools.Legion, "workspace-init", "cache", "--repo", repo.String(), "--root", CacheRoot,
			"--listen", fmt.Sprintf(":%d", repocache.Port), "--refresh", r.cacheConfig.Refresh.String(),
		},
		Env: []corev1.EnvVar{
			{Name: "PATH", Value: imagePath},
			{Name: "TMPDIR", Value: initTempDir},
			{Name: "LEGION_PROVISION_TOKEN_FILE", Value: ProvisionDir + "/" + provisionTokenKey},
			{Name: repocache.CacheTokenFileEnv, Value: ProvisionDir + "/" + cacheTokenKey},
		},
		WorkingDir: CacheRoot,
		Ports:      []corev1.ContainerPort{{Name: cacheContainer, ContainerPort: repocache.Port, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(repocache.Port)}},
			PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: cacheVolume, MountPath: CacheRoot},
			{Name: provisionVolume, MountPath: ProvisionDir, ReadOnly: true},
			{Name: tempVolume, MountPath: initTempDir},
		},
		Resources:       r.cacheConfig.Resources,
		SecurityContext: restrictedContainer(),
	}
	kubeletLiteral(&container)
	return podTemplate{
		Metadata: podMetadata{
			Labels:      r.cacheLabels(),
			Annotations: map[string]string{annotationCacheRepo: repo.String(), "karpenter.sh/do-not-disrupt": "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: new(false),
			ServiceAccountName:           r.pod.ServiceAccount,
			EnableServiceLinks:           new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true), RunAsUser: new(int64(podUser)), RunAsGroup: new(int64(podUser)), FSGroup: new(int64(podUser)),
			},
			RuntimeClassName:  new(gvisor),
			NodeSelector:      r.nodeSelector(),
			Tolerations:       r.tolerations(),
			PriorityClassName: r.scheduling.PriorityClass,
			Volumes: []corev1.Volume{
				{Name: cacheVolume, VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: cacheClaimName(name)},
				}},
				{Name: provisionVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: cacheSecretName(name),
					Items: []corev1.KeyToPath{
						{Key: provisionTokenKey, Path: provisionTokenKey}, {Key: cacheTokenKey, Path: cacheTokenKey},
					},
					DefaultMode: new(int32(0o440)),
				}}},
				{Name: tempVolume, VolumeSource: memory},
			},
			Containers: []corev1.Container{container},
		},
	}
}

// cacheSecretMeta is a cache's Secret's metadata: the cache labels and annotation, and the Sandbox,
// by s's uid, as its one owner, as secretMeta gives a pod's Secret.
func (r *Runtime) cacheSecretMeta(s *sandbox, repo ghrepo.Repository) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: cacheSecretName(s.Name), Namespace: r.namespace, Labels: r.cacheLabels(),
		Annotations: map[string]string{annotationCacheRepo: repo.String()},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: s.Name, UID: s.UID,
		}},
	}
}

// cacheSandboxShape is why object is not a cache Sandbox this runtime builds, following its name,
// or "" when it is one: its repository annotation parses, it is labelled for no tree or issue, it
// runs one container, cacheContainer, and it owns one claim template, cacheVolume.
func cacheSandboxShape(object unstructured.Unstructured) string {
	if _, err := cacheRepository(object.GetAnnotations()); err != nil {
		return ": " + err.Error()
	}
	labels := object.GetLabels()
	if labels[labelIssue] != "" || labels[labelTree] != "" {
		return fmt.Sprintf(" is labelled for issue %q of tree %q, which no cache is", labels[labelIssue], labels[labelTree])
	}
	if names := nestedNames(object, "spec", "podTemplate", "spec", "containers"); !slices.Equal(names, []string{cacheContainer}) {
		return fmt.Sprintf(" runs containers %v, not the one %s", names, cacheContainer)
	}
	if names := nestedNames(object, "spec", "volumeClaimTemplates"); !slices.Equal(names, []string{cacheVolume}) {
		return fmt.Sprintf(" owns volume claim templates %v, not the one %s", names, cacheVolume)
	}
	return ""
}

// nestedNames are the names of the list at path in object — each entry's name, or its
// metadata.name — in order; an entry with neither is "".
func nestedNames(object unstructured.Unstructured, path ...string) []string {
	list, _, _ := unstructured.NestedSlice(object.Object, path...)
	names := make([]string, 0, len(list))
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		if name == "" {
			name, _, _ = unstructured.NestedString(entry, "metadata", "name")
		}
		names = append(names, name)
	}
	return names
}

// EnsureRepositoryCache makes repo's cache exist and run, under the pod's launch turn (lockPod on
// CacheSandboxName) so two mapping changes never race on one cache: the Sandbox is read and, absent,
// created Suspended (ensureCacheSandbox); its Secret brought to the current token
// (writeCacheSecret); and its pod made or replaced when it must be (reconcileCachePod). Readiness
// is not awaited: RepositoryCache reads it from the pod store, so a mapping change never blocks on
// a volume being bound or an image being pulled. A cache already as it should be costs one Sandbox
// read and one Secret read.
func (r *Runtime) EnsureRepositoryCache(ctx context.Context, repo ghrepo.Repository) error {
	_, err := r.ensureRepositoryCache(ctx, repo)
	return err
}

// ensureRepositoryCache is EnsureRepositoryCache answering the cache's Sandbox as the API last
// held it, which a launch checks the cache's Service against (awaitCacheService).
func (r *Runtime) ensureRepositoryCache(ctx context.Context, repo ghrepo.Repository) (*sandbox, error) {
	if repo.IsZero() {
		return nil, errors.New("ensure repository cache: no repository")
	}
	name := CacheSandboxName(r.project, repo)
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("ensure the repository cache of %s: take its pod's turn: %w", repo, err)
	}
	defer release()
	s, err := r.ensureCacheSandbox(ctx, name, repo)
	if err != nil {
		return nil, fmt.Errorf("ensure the repository cache of %s: %w", repo, err)
	}
	if err := r.writeCacheSecret(ctx, s, repo); err != nil {
		return nil, fmt.Errorf("ensure the repository cache of %s: write its token: %w", repo, err)
	}
	if err := r.reconcileCachePod(ctx, s, repo); err != nil {
		return nil, fmt.Errorf("ensure the repository cache of %s: %w", repo, err)
	}
	return s, nil
}

// ensureCacheSandbox is the Sandbox name, read from the API: created from cacheManifest when
// absent; refused when it is another repository's cache, is no cache Sandbox this runtime builds
// (cacheSandboxShape), or is another project's; waited out, within the boot timeout, while it is
// being deleted (the sweep retiring it), and then created afresh.
func (r *Runtime) ensureCacheSandbox(ctx context.Context, name string, repo ghrepo.Repository) (*sandbox, error) {
	deadline := time.Now().Add(r.bootTimeout)
	for {
		u, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			manifest, err := encodeSandbox(r.cacheManifest(repo))
			if err != nil {
				return nil, err
			}
			created, err := r.sandboxClient().Create(ctx, manifest, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) && time.Now().Before(deadline) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("create sandbox %s: %w", name, err)
			}
			return decodeSandbox(created)
		}
		if err != nil {
			return nil, fmt.Errorf("read sandbox %s: %w", name, err)
		}
		if project := u.GetLabels()[labelProject]; project != r.project {
			return nil, fmt.Errorf("sandbox %s exists but is not project %s's (%s=%q)", name, r.project, labelProject, project)
		}
		held, err := cacheRepository(u.GetAnnotations())
		if err != nil {
			return nil, fmt.Errorf("sandbox %s, the name of %s's cache, is no repository's cache: %w", name, repo, err)
		}
		if !sameRepository(held, repo) {
			return nil, fmt.Errorf("sandbox %s, the name of %s's cache, is the repository cache of %s", name, repo, held)
		}
		if reason := cacheSandboxShape(*u); reason != "" {
			return nil, fmt.Errorf("sandbox %s%s", name, reason)
		}
		if u.GetDeletionTimestamp() == nil {
			return decodeSandbox(u)
		}
		uid := u.GetUID()
		gone := func(ctx context.Context) (bool, error) {
			current, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return err == nil && current.GetUID() != uid, err
		}
		if err := r.await(ctx, time.Until(deadline), fmt.Sprintf("sandbox %s (uid %s) to be deleted", name, uid), gone); err != nil {
			return nil, err
		}
	}
}

// writeCacheSecret brings s's Secret, cacheSecretName, to the implement App's token for repo's
// owner (Options.Tokens) and the cache's bearer token: the one the Secret holds already, else 32
// random bytes, hex. It is left alone when it already holds both under the cache's labels,
// annotation and owner, and otherwise written through upsertSecret: created when absent, updated
// in place when s owns it, replaced when another uid owns it (a Secret an earlier Sandbox of the
// name left). The bearer token is kept for the runtime's own reads (cacheBearer).
func (r *Runtime) writeCacheSecret(ctx context.Context, s *sandbox, repo ghrepo.Repository) error {
	minting, cancel := call(ctx)
	token, err := r.tokens.Token(minting, repo.Owner())
	cancel()
	if err != nil {
		return fmt.Errorf("mint the token for %s: %w", repo.Owner(), err)
	}
	existing, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, cacheSecretName(s.Name), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	bearer := ""
	if err == nil {
		bearer = string(existing.Data[cacheTokenKey])
	}
	if !cacheBearerShape.MatchString(bearer) {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return fmt.Errorf("make the repository cache's bearer token: %w", err)
		}
		bearer = hex.EncodeToString(random)
	}
	want := corev1.Secret{
		ObjectMeta: r.cacheSecretMeta(s, repo),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{provisionTokenKey: []byte(token), cacheTokenKey: []byte(bearer)},
	}
	if err == nil && ownedBySandbox(existing.OwnerReferences, s.UID) && len(existing.OwnerReferences) == 1 &&
		maps.EqualFunc(existing.Data, want.Data, func(a, b []byte) bool { return string(a) == string(b) }) &&
		maps.Equal(existing.Labels, want.Labels) && existing.Annotations[annotationCacheRepo] == repo.String() {
		r.keepCacheBearer(s.Name, bearer)
		return nil
	}
	if err := r.upsertSecret(ctx, want); err != nil {
		return err
	}
	r.keepCacheBearer(s.Name, bearer)
	return nil
}

// cacheBearerShape is a bearer token writeCacheSecret makes.
var cacheBearerShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// keepCacheBearer records name's bearer token as its Secret holds it.
func (r *Runtime) keepCacheBearer(name, bearer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheBearers[name] = bearer
}

// cacheBearer is the bearer token of the cache Sandbox name as its Secret last held it, "" when the
// runtime has not written or read that Secret.
func (r *Runtime) cacheBearer(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cacheBearers[name]
}

// cacheURL is the address an issue pod's fetch reaches the cache Sandbox name at: its headless
// Service's name in the runtime's namespace, which resolves to the pod only while it is Ready.
func (r *Runtime) cacheURL(name string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", name, r.namespace, repocache.Port)
}

// reconcileCachePod makes s's pod (again) when it must be: s is Suspended (a fresh create, or one a
// failed earlier run left Suspended), or the pod it owns runs an image other than the runtime's
// (imageDrift) or has ended (terminal: evicted, say). It then suspends s when it is not already,
// waits the old pod out (awaitPodGone), and sets the pod template, on the runtime's image, and
// Running in one patch, as relaunch does. The Sandbox and its volume are kept through every
// replacement: a drifted pod costs a new pod, not a new clone. A Running Sandbox whose pod is live
// on the right image, or that has no pod yet — the controller makes one — is left alone.
func (r *Runtime) reconcileCachePod(ctx context.Context, s *sandbox, repo ghrepo.Repository) error {
	pod := r.storedPod(s.Name)
	owned := ownedBy(pod, s.UID) && pod.DeletionTimestamp == nil
	_, drifted := imageDrift(pod, r.image)
	if s.mode() != modeSuspended && (!owned || (!drifted && !terminal(pod))) {
		return nil
	}
	if s.mode() != modeSuspended {
		if err := r.setMode(ctx, s, modeSuspended); err != nil {
			return fmt.Errorf("suspend sandbox %s: %w", s.Name, err)
		}
	}
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return fmt.Errorf("wait out sandbox %s's pod: %w", s.Name, err)
	}
	if _, err := r.patch(ctx, s,
		jsonPatchOp{Op: "add", Path: "/spec/podTemplate", Value: r.cachePodTemplate(s.Name, repo)},
		jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeRunning},
	); err != nil {
		return fmt.Errorf("set sandbox %s running: %w", s.Name, err)
	}
	r.log.Info("sandbox runtime: repository cache pod started", "sandbox", s.Name, "repo", repo.String(), "image", r.image)
	return nil
}

// The states of a repository cache, as legion state shows them and a failed fetch names them.
const (
	cacheStarting = "starting"
	cacheCloning  = "cloning"
	cacheReady    = "ready"
	cacheFailing  = "failing"
	cacheRefused  = "refused"
)

// sandboxNameHashLabel is the label the Agent Sandbox controller stamps on every pod and Service it
// makes for a Sandbox, and the one key a Service it makes selects by (agent-sandbox v1.0.3,
// controllers/sandbox_controller.go, sandboxLabel).
const sandboxNameHashLabel = "agents.x-k8s.io/sandbox-name-hash"

// sandboxNameHash is the controller's value of sandboxNameHashLabel for the Sandbox name: the
// name's 32-bit FNV-1a hash as eight lowercase hex digits (sandbox_controller.go, NameHash).
func sandboxNameHash(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

// cacheServiceRefusal is why the Service named name, as the store holds it, is not the cache
// Sandbox's with uid — "" when it is — and whether a Service of the name is made at all, or its
// store cannot be trusted. It is the Sandbox's only when the Sandbox controls it, by uid, it
// selects exactly the pods the controller stamps with the hash of the Sandbox's name
// (sandboxNameHashLabel), and it is headless with no external name: the shape the controller makes.
// The name alone proves nothing, and an issue pod's fetch presents the cache's token to whatever
// the name reaches. Anyone who may create a Service in the namespace could make one of the cache's
// name first. Without the controller's tracking label the controller refuses to adopt it and leaves
// it routing elsewhere; with that label the controller adopts it, writing its own controller
// reference and the exact selector, but keeps its type and external name, so an ExternalName
// Service whose name resolves to its maker's host passes on owner and selector alone. While the
// Services list or watch is failing (serviceFeed), the store may hold a Service that has since
// changed, so nothing in it is trusted.
func (r *Runtime) cacheServiceRefusal(name string, uid types.UID) (string, bool) {
	if err := r.serviceFeed.check(); err != nil {
		return "cannot be checked: " + err.Error(), true
	}
	obj, ok, err := r.services.GetStore().GetByKey(r.namespace + "/" + name)
	if err != nil || !ok {
		return "", false
	}
	service := obj.(*corev1.Service)
	controller := metav1.GetControllerOf(service)
	switch want := map[string]string{sandboxNameHashLabel: sandboxNameHash(name)}; {
	case controller == nil:
		return "has no controller", true
	case controller.Kind != "Sandbox" || controller.UID != uid:
		return fmt.Sprintf("is controlled by %s %s (%s), not the cache's Sandbox (%s)", controller.Kind, controller.Name, controller.UID, uid), true
	case !maps.Equal(service.Spec.Selector, want):
		return fmt.Sprintf("selects %v, not the cache's pod alone (%v)", service.Spec.Selector, want), true
	case service.Spec.ClusterIP != corev1.ClusterIPNone || service.Spec.ExternalName != "":
		kind := service.Spec.Type
		if kind == "" {
			kind = corev1.ServiceTypeClusterIP
		}
		return fmt.Sprintf("is a %s Service (clusterIP %q, externalName %q), not the headless Service the controller makes", kind, service.Spec.ClusterIP, service.Spec.ExternalName), true
	}
	return "", true
}

// awaitCacheService waits until the cache Sandbox cache, as the API last held it, has its own
// Service (cacheServiceRefusal), which the controller makes beside the cache's pod, before an
// issue pod is told the cache's address and token: its fetch presents that token to whatever the
// Service's name reaches. Each refusal is logged as it changes, and the wait fails at the boot
// timeout naming the Service and the last.
func (r *Runtime) awaitCacheService(ctx context.Context, repo ghrepo.Repository, cache *sandbox) error {
	name := cache.Name
	why := "is not made yet"
	err := r.await(ctx, r.bootTimeout, "the repository cache's Service "+name, func(context.Context) (bool, error) {
		refusal, made := r.cacheServiceRefusal(name, cache.UID)
		if made && refusal == "" {
			return true, nil
		}
		if made && refusal != why {
			why = refusal
			r.log.Warn("sandbox runtime: the repository cache's Service is not the cache's", "service", name, "repo", repo.String(), "why", refusal)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("the repository cache's Service %s %s: %w", name, why, err)
	}
	return nil
}

// cacheFailures are the reasons a container waits with that make a cache failing: it is restarting
// after exits, or cannot start at all.
var cacheFailures = []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError"}

// RepositoryCache is repo's cache as the stores show it: the Sandbox named CacheSandboxName(repo)
// whose cacheRepository is repo (a name another repository's cache occupies is logged once and
// answers not found); its State — refused, naming its Service and why, while a Service of its name
// is not its own (cacheServiceRefusal), since its issue pods would hand that Service the cache's
// token; otherwise starting while the Sandbox owns no live pod, the pod is not yet running, or the
// controller has made no Service for it yet, failing with the kubelet's reason while its container
// waits on one of cacheFailures or the pod has failed, ready once the pod's Ready condition is True
// (the readiness probe passed) and its Service is made, with its address, that Service's
// (cacheURL), and cloning otherwise — and RetainedUntil from annotationCacheUnmappedAt plus the
// retention when the annotation is set. Not found when no Sandbox of the name is stored.
func (r *Runtime) RepositoryCache(repo ghrepo.Repository) (CacheStatus, bool) {
	name := CacheSandboxName(r.project, repo)
	s, err := r.storedSandbox(name)
	if err != nil || s == nil || s.Labels[labelCache] == "" {
		return CacheStatus{}, false
	}
	held, err := cacheRepository(s.Annotations)
	if err != nil || !sameRepository(held, repo) {
		r.mu.Lock()
		first := !r.occupiedCaches[name]
		r.occupiedCaches[name] = true
		r.mu.Unlock()
		if first {
			r.log.Warn("sandbox runtime: the name of a repository's cache is held by another", "sandbox", name, "repo", repo.String(), "held", s.Annotations[annotationCacheRepo])
		}
		return CacheStatus{}, false
	}
	status := CacheStatus{Repo: held, Sandbox: name, State: cacheStarting}
	pod := r.storedPod(name)
	switch {
	case !ownedBy(pod, s.UID) || pod.DeletionTimestamp != nil:
	case pod.Status.Phase == corev1.PodFailed:
		status.State, status.Reason = cacheFailing, fmt.Sprintf("pod %s Failed: %s %s", pod.Name, pod.Status.Reason, pod.Status.Message)
	default:
		ready := podCondition(pod, corev1.PodReady)
		container := containerStatus(pod, cacheContainer)
		switch {
		case ready != nil && ready.Status == corev1.ConditionTrue:
			status.State, status.Ready, status.Address = cacheReady, true, r.cacheURL(name)
		case container != nil && container.State.Waiting != nil && slices.Contains(cacheFailures, container.State.Waiting.Reason):
			status.State = cacheFailing
			status.Reason = fmt.Sprintf("container %s %s: %s (restarted %d times)", cacheContainer, container.State.Waiting.Reason, container.State.Waiting.Message, container.RestartCount)
		case pod.Status.Phase == corev1.PodRunning:
			status.State = cacheCloning
		}
	}
	switch refusal, made := r.cacheServiceRefusal(name, s.UID); {
	case refusal != "":
		status.State, status.Reason, status.Ready, status.Address = cacheRefused, "its Service "+name+" "+refusal, false, ""
	case !made && status.State == cacheReady:
		status.State, status.Ready, status.Address = cacheStarting, false, ""
	}
	if at, err := time.Parse(time.RFC3339, s.Annotations[annotationCacheUnmappedAt]); err == nil {
		status.RetainedUntil = at.Add(r.cacheConfig.Retain)
	}
	return status, true
}

// RepositoryCaches is every cache the stores hold whose repository parses, sorted by repository,
// each as RepositoryCache answers it with the last health read (readCacheHealth) of the pod it
// owns now, none when that read was of another: what `legion state` shows of the caches.
func (r *Runtime) RepositoryCaches() []CacheStatus {
	var caches []CacheStatus
	for _, u := range r.storedCaches() {
		repo, err := cacheRepository(u.GetAnnotations())
		if err != nil {
			continue
		}
		status, found := r.RepositoryCache(repo)
		if !found {
			continue
		}
		r.mu.Lock()
		read, held := r.cacheHealth[status.Sandbox]
		r.mu.Unlock()
		if pod := r.storedPod(status.Sandbox); held && pod != nil && pod.UID == read.pod {
			status.Health, status.HealthError = read.health, read.err
		}
		caches = append(caches, status)
	}
	slices.SortFunc(caches, func(a, b CacheStatus) int {
		return strings.Compare(strings.ToLower(a.Repo.String()), strings.ToLower(b.Repo.String()))
	})
	return caches
}

// cacheHealthRead is one health read of the cache whose pod was pod when the read began: what its
// /healthz answered, or why it could not be read.
type cacheHealthRead struct {
	pod    types.UID
	health *repocache.Health
	err    string
}

// readCacheHealth runs on the gh refresher's tick, after refreshCacheTokens: it asks every Ready
// cache's /healthz, each bounded by a call's budget, and keeps the answer, or why there is none,
// as that cache's last health read, which RepositoryCaches serves without asking again. It asks the
// cache's pod at its address as the store holds it, http://<pod ip>:8418, so a daemon outside the
// cluster that reaches pod addresses but resolves no cluster name reads it too, and it carries no
// credential: /healthz holds nothing secret, so a pod that took the address once the cache's pod
// was gone learns nothing, and its answer, naming another repository, is that read's failure. A
// cache that is not Ready has no read: the store already says it is not serving.
func (r *Runtime) readCacheHealth(ctx context.Context) {
	reads := map[string]cacheHealthRead{}
	client := r.cacheHealthClient()
	for _, u := range r.storedCaches() {
		repo, err := cacheRepository(u.GetAnnotations())
		if err != nil {
			continue
		}
		status, found := r.RepositoryCache(repo)
		if !found || !status.Ready {
			continue
		}
		pod := r.storedPod(status.Sandbox)
		if pod == nil || pod.Status.PodIP == "" {
			continue
		}
		read := cacheHealthRead{pod: pod.UID}
		address := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(repocache.Port))
		health, err := cacheHealthOf(ctx, client, address, repo)
		if err != nil {
			read.err = err.Error()
		} else {
			read.health = &health
		}
		reads[status.Sandbox] = read
	}
	r.mu.Lock()
	r.cacheHealth = reads
	r.mu.Unlock()
}

// cacheHealthOf is GET <address>/healthz through client, carrying no credential, decoded, within a
// call's budget: an error when the answer names a repository other than repo
// (workspace.CacheRepositoryHeader).
func cacheHealthOf(ctx context.Context, client *http.Client, address string, repo ghrepo.Repository) (repocache.Health, error) {
	bounded, cancel := call(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, address+"/healthz", nil)
	if err != nil {
		return repocache.Health{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return repocache.Health{}, err
	}
	defer response.Body.Close()
	if named := response.Header.Get(workspace.CacheRepositoryHeader); !strings.EqualFold(named, repo.String()) {
		return repocache.Health{}, fmt.Errorf("GET %s/healthz answered %s naming the repository %q, not %s", address, response.Status, named, repo)
	}
	var health repocache.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return repocache.Health{}, fmt.Errorf("GET %s/healthz answered %s with a body that is not the cache's health: %w", address, response.Status, err)
	}
	return health, nil
}

// cacheHealthClient reaches a cache's pod directly through cacheDial: no proxy stands between the
// daemon and a pod of its namespace.
func (r *Runtime) cacheHealthClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: r.cacheDial, DisableKeepAlives: true}}
}

// storedCaches are the cache Sandboxes in the store that are not being deleted.
func (r *Runtime) storedCaches() []*unstructured.Unstructured {
	var caches []*unstructured.Unstructured
	for _, obj := range r.sandboxes.GetStore().List() {
		u := obj.(*unstructured.Unstructured)
		if u.GetLabels()[labelCache] != "" && u.GetDeletionTimestamp() == nil {
			caches = append(caches, u)
		}
	}
	slices.SortFunc(caches, func(a, b *unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	return caches
}

// refreshCacheTokens runs on the gh refresher's tick, after the role Secrets: for every stored cache
// Sandbox whose repository parses, it brings the Secret to a fresh lease through writeCacheSecret —
// which also recreates a Secret that is gone, owned by the Sandbox's uid, and replaces one another
// uid owns — so the cache's fetches never run on an expired token and a lost Secret comes back
// within a tick. The Sandbox is neither suspended nor replaced for a lost Secret: a pod stuck
// Pending on the missing mount starts once the kubelet retries it, and a running pod has the file
// re-projected within the kubelet's sync period. A write that fails is logged with the Sandbox's
// name and tried at the next tick.
func (r *Runtime) refreshCacheTokens(ctx context.Context) {
	for _, u := range r.storedCaches() {
		repo, err := cacheRepository(u.GetAnnotations())
		if err != nil {
			continue
		}
		s, err := decodeSandbox(u)
		if err != nil {
			r.log.Warn("sandbox runtime: repository cache token refresh failed", "sandbox", u.GetName(), "err", err)
			continue
		}
		if err := r.writeCacheSecret(ctx, s, repo); err != nil {
			r.log.Warn("sandbox runtime: repository cache token refresh failed", "sandbox", s.Name, "repo", repo.String(), "err", err)
		}
	}
}

// sweepCaches is ReconcileOrphans' pass over the repository caches (the claim path, sweep, never
// sees one), against the repositories the mapping names now (Options.Mapped). For each stored cache
// Sandbox: a repository that does not parse, or a shape this runtime does not build, is logged and
// kept, as sweep keeps a Sandbox it cannot judge (the next boot's census refuses it by name); a
// mapped repository's cache has any annotationCacheUnmappedAt removed; an unmapped one's is marked
// with now once, kept while now minus the mark is under the retention, and deleted after it
// (deleteFenced, in the foreground, so its volume and Secret go with it), logged with the repository
// and how long it was kept. Then every mapped repository's cache is ensured (EnsureRepositoryCache):
// one missing is made — at boot, whose reconciliation runs this, and after an ensure a mapping
// change asked for failed — and a drifted or dead cache pod is replaced, all on the sweep's
// schedule. Each runs under the cache's launch turn, fenced to the Sandbox as the store held it.
// Every failure is logged, as a warning naming the cache, and tried at the next sweep; none is
// ReconcileOrphans' error, which holds the claims' launches back at boot (reconcileBootOrphans):
// each launch ensures its own repository's cache and fails, naming why, when it cannot be made, so
// one repository's cache never stops another's launches.
func (r *Runtime) sweepCaches(ctx context.Context) {
	mapped := r.mapped()
	for _, u := range r.storedCaches() {
		if err := r.sweepCache(ctx, u, mapped); err != nil && ctx.Err() == nil {
			r.log.Warn("sandbox runtime: repository cache sweep failed; tried at the next sweep", "sandbox", u.GetName(), "err", err)
		}
	}
	for _, repo := range mapped {
		if err := r.EnsureRepositoryCache(ctx, repo); err != nil && ctx.Err() == nil {
			r.log.Warn("sandbox runtime: repository cache sweep failed; tried at the next sweep", "sandbox", CacheSandboxName(r.project, repo), "repo", repo.String(), "err", err)
		}
	}
}

// sweepCache is sweepCaches' judgement of one stored cache Sandbox, u, against mapped.
func (r *Runtime) sweepCache(ctx context.Context, u *unstructured.Unstructured, mapped []ghrepo.Repository) error {
	repo, err := cacheRepository(u.GetAnnotations())
	if err != nil {
		return fmt.Errorf("%w; kept", err)
	}
	if reason := cacheSandboxShape(*u); reason != "" {
		return fmt.Errorf("Sandbox %s%s; kept", u.GetName(), reason)
	}
	release, err := r.lockPod(ctx, u.GetName())
	if err != nil {
		return err
	}
	defer release()
	s, err := decodeSandbox(u)
	if err != nil {
		return err
	}
	marked := s.Annotations[annotationCacheUnmappedAt]
	if slices.ContainsFunc(mapped, func(m ghrepo.Repository) bool { return sameRepository(m, repo) }) {
		if marked != "" {
			if _, err := r.markUnmapped(ctx, s, nil); err != nil {
				return err
			}
			r.log.Info("sandbox runtime: repository cache mapped again; kept", "sandbox", s.Name, "repo", repo.String())
		}
		return nil
	}
	now := r.now()
	if marked == "" {
		stamp := now.UTC().Format(time.RFC3339)
		if _, err := r.markUnmapped(ctx, s, &stamp); err != nil {
			return err
		}
		r.log.Info("sandbox runtime: repository cache no longer mapped; kept for its retention", "sandbox", s.Name, "repo", repo.String(), "retain", r.cacheConfig.Retain.String())
		return nil
	}
	at, err := time.Parse(time.RFC3339, marked)
	if err != nil {
		return fmt.Errorf("%s is %q, not a time: %w; kept", annotationCacheUnmappedAt, marked, err)
	}
	if kept := now.Sub(at); kept < r.cacheConfig.Retain {
		return nil
	}
	uid, version := s.UID, s.ResourceVersion
	return r.deleteFenced(ctx, s.Name, metav1.Preconditions{UID: &uid, ResourceVersion: &version},
		"sandbox runtime: retired a repository cache no longer mapped", "repo", repo.String(), "kept", now.Sub(at).Truncate(time.Second).String())
}

// markUnmapped sets s's annotationCacheUnmappedAt to stamp, or removes it when stamp is nil, in one
// JSON merge patch carrying s's uid, so a Sandbox replaced in between is never written.
func (r *Runtime) markUnmapped(ctx context.Context, s *sandbox, stamp *string) (*sandbox, error) {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": string(s.UID), "annotations": map[string]*string{annotationCacheUnmappedAt: stamp}},
	})
	if err != nil {
		return nil, err
	}
	u, err := r.sandboxClient().Patch(ctx, s.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("annotate sandbox %s: %w", s.Name, err)
	}
	return decodeSandbox(u)
}
