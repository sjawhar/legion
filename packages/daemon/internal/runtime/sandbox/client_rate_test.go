package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeLimiter is a client-side rate limiter (flowcontrol.RateLimiter) with x/time/rate's Wait rule,
// which client-go's token bucket follows: a Wait takes the next free slot, qps of them a second
// with no burst, and is refused at once, taking none, when that slot lies past its context's
// deadline. Its clock runs speedup times faster than the wall clock, so a queue a deployment waits
// 30 s on costs the test 30 s / speedup; a deadline is compared with the wait the limiter models.
type fakeLimiter struct {
	qps, speedup float64
	epoch        time.Time
	mu           sync.Mutex
	// next is the next free slot, in modeled time since epoch.
	next time.Duration
}

func newFakeLimiter(qps, speedup float64) *fakeLimiter {
	return &fakeLimiter{qps: qps, speedup: speedup, epoch: time.Now()}
}

// now is the modeled time since epoch.
func (l *fakeLimiter) now() time.Duration {
	return time.Duration(float64(time.Since(l.epoch)) * l.speedup)
}

// backlog is how long, in modeled time, a Wait made now would wait.
func (l *fakeLimiter) backlog() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.next-l.now(), 0)
}

func (l *fakeLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	slot := max(now, l.next)
	wait := slot - now
	if deadline, ok := ctx.Deadline(); ok && wait > time.Until(deadline) {
		l.mu.Unlock()
		return errors.New("rate: Wait(n=1) would exceed context deadline")
	}
	l.next = slot + time.Duration(float64(time.Second)/l.qps)
	l.mu.Unlock()
	timer := time.NewTimer(time.Duration(float64(wait) / l.speedup))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *fakeLimiter) Accept()         { _ = l.Wait(context.Background()) }
func (l *fakeLimiter) TryAccept() bool { return false }
func (l *fakeLimiter) Stop()           {}
func (l *fakeLimiter) QPS() float32    { return float32(l.qps) }

// withClientLimiter puts limit in front of the requests a launch and a launcher's hello make
// through the runtime's clients, a watch excepted, as client-go's REST client puts its configured
// limiter in front of each request it sends (limited).
func withClientLimiter(limit flowcontrol.RateLimiter) rigOption {
	return func(g *rig, _ *Options) { g.limit = limit }
}

// limited is client-go's wait on a client's rate limiter before a request (rest/request.go,
// Request.tryThrottleWithInfo): on the request's own context, and before the client's own
// timeout starts; a refusal is wrapped as client-go wraps it, a context error is not.
func limited(ctx context.Context, limit flowcontrol.RateLimiter) error {
	err := limit.Wait(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("client rate limiter Wait returned an error: %w", err)
	}
	return err
}

// limitedKube is the typed fake behind limit. It embeds the fake itself, as hookedDynamic does, so
// the informers still see the fake's IsWatchListSemanticsUnSupported. Each Secret read is counted
// in g before limit sees it: what the runtime asked for, whether or not the limiter let it through.
type limitedKube struct {
	*kubefake.Clientset
	limit flowcontrol.RateLimiter
	g     *rig
}

func (k limitedKube) CoreV1() corev1client.CoreV1Interface {
	return limitedCore{k.Clientset.CoreV1(), k.limit, k.g}
}

type limitedCore struct {
	corev1client.CoreV1Interface
	limit flowcontrol.RateLimiter
	g     *rig
}

func (c limitedCore) Secrets(namespace string) corev1client.SecretInterface {
	return limitedSecrets{c.CoreV1Interface.Secrets(namespace), c.limit, c.g}
}

func (c limitedCore) Pods(namespace string) corev1client.PodInterface {
	return limitedPods{c.CoreV1Interface.Pods(namespace), c.limit}
}

type limitedSecrets struct {
	corev1client.SecretInterface
	limit flowcontrol.RateLimiter
	g     *rig
}

func (s limitedSecrets) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error) {
	s.g.mu.Lock()
	if s.g.secretReads == nil {
		s.g.secretReads = map[string]int{}
	}
	s.g.secretReads[name]++
	s.g.mu.Unlock()
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.SecretInterface.Get(ctx, name, opts)
}

func (s limitedSecrets) Create(ctx context.Context, secret *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error) {
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.SecretInterface.Create(ctx, secret, opts)
}

func (s limitedSecrets) Update(ctx context.Context, secret *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error) {
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.SecretInterface.Update(ctx, secret, opts)
}

func (s limitedSecrets) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	if err := limited(ctx, s.limit); err != nil {
		return err
	}
	return s.SecretInterface.Delete(ctx, name, opts)
}

type limitedPods struct {
	corev1client.PodInterface
	limit flowcontrol.RateLimiter
}

func (p limitedPods) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error) {
	if err := limited(ctx, p.limit); err != nil {
		return nil, err
	}
	return p.PodInterface.Get(ctx, name, opts)
}

func (p limitedPods) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	if err := limited(ctx, p.limit); err != nil {
		return nil, err
	}
	return p.PodInterface.List(ctx, opts)
}

// limitedDynamic is the dynamic fake behind limit, for the Sandboxes the runtime reads and writes.
type limitedDynamic struct {
	*dynamicfake.FakeDynamicClient
	limit flowcontrol.RateLimiter
}

func (d limitedDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	if gvr != sandboxGVR {
		return d.FakeDynamicClient.Resource(gvr)
	}
	return limitedResource{d.FakeDynamicClient.Resource(gvr), d.limit}
}

type limitedResource struct {
	dynamic.NamespaceableResourceInterface
	limit flowcontrol.RateLimiter
}

func (r limitedResource) Namespace(namespace string) dynamic.ResourceInterface {
	return limitedSandboxes{r.NamespaceableResourceInterface.Namespace(namespace), r.limit}
}

type limitedSandboxes struct {
	dynamic.ResourceInterface
	limit flowcontrol.RateLimiter
}

func (s limitedSandboxes) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.ResourceInterface.Get(ctx, name, opts, subresources...)
}

func (s limitedSandboxes) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.ResourceInterface.List(ctx, opts)
}

func (s limitedSandboxes) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.ResourceInterface.Create(ctx, obj, opts, subresources...)
}

func (s limitedSandboxes) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if err := limited(ctx, s.limit); err != nil {
		return nil, err
	}
	return s.ResourceInterface.Patch(ctx, name, pt, data, opts, subresources...)
}

func (s limitedSandboxes) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	if err := limited(ctx, s.limit); err != nil {
		return err
	}
	return s.ResourceInterface.Delete(ctx, name, opts, subresources...)
}

// reconnectingPods are pods running issue pods of their own, one per issue from LEGION-300 on,
// each role's launcher Secret bound to its pod, as a daemon restart finds every live tree's pod;
// the hellos are each role launcher's, with its own token.
func reconnectingPods(t *testing.T, pods int) ([]k8sruntime.Object, []shimwire.LauncherHello) {
	t.Helper()
	var objects []k8sruntime.Object
	var hellos []shimwire.LauncherHello
	for i := range pods {
		issue := fmt.Sprintf("LEGION-%d", 300+i)
		labels := map[string]string{labelProject: testProject, labelTree: issue, labelIssue: issue}
		token, err := claim.NewToken(testProject, issue, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		name := SandboxName(token)
		sandboxUID, podUID := "uid-sandbox-"+issue, "uid-pod-"+issue
		objects = append(objects, sandboxObject(t, name, sandboxUID, modeRunning, labels),
			podObject(name, podUID, sandboxUID, labels, runningStatus()))
		for _, role := range claim.Roles {
			launcherToken := fmt.Sprintf("launcher-%s-%s", issue, role)
			objects = append(objects, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: roleSecretName(name, role), Namespace: testNamespace, Labels: labels,
					Annotations:     map[string]string{launcherPodUIDAnnotation: podUID, annotationIssue: issue},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: types.UID(sandboxUID)}},
				},
				Data: map[string][]byte{LauncherTokenFile: []byte(launcherToken)},
			})
			hellos = append(hellos, shimwire.LauncherHello{
				Token: launcherToken, Sandbox: name, Role: string(role), PodUID: podUID, LauncherID: "l-" + issue + "-" + string(role),
			})
		}
	}
	return objects, hellos
}

// After a daemon restart every live role's launcher says hello again while the launches the
// previous daemon left unfinished run again (LEGION-723), and both wait on the client's one rate
// limiter: each hello reads its role's launcher Secret, each launch makes its requests. Here 150
// launchers reconnect, each again a second after every refusal (launcher.reconnectDelay), behind a
// limiter whose 30 s hold 60 requests — client-go's default 5 a second with a burst of 10 held 160,
// which production's 154 hellos and its launches' own requests overran. Each launcher's Secret is
// read once, however often its launcher says hello, every launcher is accepted, and the three
// launches in flight, two roles of one issue and one of another, wait their turn on the limiter and
// start their roles, charged no launch failure.
func TestLaunchesInFlightAtARestartOutwaitTheReconnectingLaunchersReads(t *testing.T) {
	const pods, speedup = 25, 100
	limit := newFakeLimiter(2, speedup)
	objects, hellos := reconnectingPods(t, pods)
	g := newRig(t, objects, withClientLimiter(limit), withOptions(func(o *Options) { o.BootTimeout = 5 * time.Minute }))

	accept := g.r.LauncherResolver()
	var sent, accepted atomic.Int64
	for _, hello := range hellos {
		go func() {
			for {
				sent.Add(1)
				if handler, _ := accept(hello); handler != nil {
					accepted.Add(1)
					return
				}
				select {
				case <-g.ctx.Done():
					return
				case <-time.After(time.Second / speedup):
				}
			}
		}()
	}
	testwait.Eventually(t, "the hellos' reads to fill the limiter's 30 s", func() bool { return limit.backlog() >= 29*time.Second })

	specs := []runtime.SpawnSpec{rootSpec(t), workerSpec(t), childSpec(t)}
	machines := make([]*supervise.Machine, len(specs))
	launched := make(chan error, len(specs))
	for i, spec := range specs {
		g.launcher(spec.Claim)
		machines[i], _, _ = supervisedSandboxMachine(t, g, spec)
		go func() { launched <- machines[i].Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}) }()
	}
	for range specs {
		select {
		case err := <-launched:
			if err != nil {
				t.Errorf("a launch in flight failed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the launches in flight did not end")
		}
	}
	for i, spec := range specs {
		if got := machines[i].Claim(); got.Budgets.LaunchFailures != 0 || got.State != supervise.StateLaunching || got.Locator == nil {
			t.Errorf("%s after its launch: %s with %d launch failures and locator %v, want launching in its pod with none",
				spec.Claim, got.State, got.Budgets.LaunchFailures, got.Locator)
		}
	}

	testwait.Eventually(t, "every reconnecting launcher accepted", func() bool { return accepted.Load() == int64(len(hellos)) })
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, hello := range hellos {
		name := roleSecretName(hello.Sandbox, claim.Role(hello.Role))
		if reads := g.secretReads[name]; reads != 1 {
			t.Errorf("%s was read %d times over %d hellos of %d launchers, want once", name, reads, sent.Load(), len(hellos))
		}
	}
}

// After a restart, a launcher Secret whose read failed is not read again for credentialRetry: the
// launcher's hellos meanwhile are refused without a request, and the first hello after it reads the
// Secret again and is accepted.
func TestAFailedLauncherCredentialReadHoldsOffTheNextForTheRetryInterval(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	name := roleSecretName(loc.Sandbox.Name, claim.RoleTester)
	hello := shimwire.LauncherHello{
		Token: string(g.secret(name).Data[LauncherTokenFile]), Sandbox: loc.Sandbox.Name, Role: string(claim.RoleTester),
		PodUID: loc.Sandbox.PodUID, LauncherID: "l-1",
	}
	var reads atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if a.(k8stesting.GetAction).GetName() != name {
			return false, nil, nil
		}
		reads.Add(1)
		if failing.Load() {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})
	accept := secondRuntime(t, g, func(*Options) {})().LauncherResolver()

	for range 5 {
		if handler, _ := accept(hello); handler != nil {
			t.Fatal("the launcher was accepted while its Secret could not be read")
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("five hellos read the Secret %d times, want once", got)
	}
	g.advance(credentialRetry - time.Second)
	if handler, _ := accept(hello); handler != nil || reads.Load() != 1 {
		t.Fatalf("a hello inside the retry interval: accepted %t after %d reads, want refused without a read", handler != nil, reads.Load())
	}
	failing.Store(false)
	g.advance(time.Second)
	if handler, reason := accept(hello); handler == nil {
		t.Fatalf("the first hello after the retry interval was refused: %s", reason)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("the Secret was read %d times, want once more after the retry interval", got)
	}
}

// The runtime's clients share one limiter at 50 requests a second with a burst of 100, whatever
// rate the caller's configuration names; the requests' client bounds each request by apiTimeout,
// and the informers' has no timeout, which would end a watch.
func TestTheRuntimesClientsShareOneLimiterAtTheDaemonsRate(t *testing.T) {
	rc := &rest.Config{Host: "https://192.0.2.1", QPS: 5, Burst: 10, Timeout: time.Minute}
	informers, requests := clientConfigs(rc)
	if informers.RateLimiter == nil || informers.RateLimiter != requests.RateLimiter {
		t.Fatalf("informers' limiter %v, requests' %v, want one limiter for both", informers.RateLimiter, requests.RateLimiter)
	}
	if qps := requests.RateLimiter.QPS(); qps != clientQPS {
		t.Errorf("the limiter's rate is %v a second, want %d", qps, clientQPS)
	}
	for i := range clientBurst {
		if !requests.RateLimiter.TryAccept() {
			t.Fatalf("the limiter let %d requests through at once, want a burst of %d", i, clientBurst)
		}
	}
	if informers.Timeout != 0 || requests.Timeout != apiTimeout {
		t.Errorf("timeouts: informers %s, requests %s, want none and %s", informers.Timeout, requests.Timeout, apiTimeout)
	}
	if rc.RateLimiter != nil || rc.Timeout != time.Minute {
		t.Errorf("the caller's configuration was changed: %+v", rc)
	}
}

// A request's own timeout starts once the client's limiter lets it go (client-go's
// Request.request), which is what lets a launch wait out a queue longer than apiTimeout: here the
// limiter holds a request of the requests' client four times its timeout, and it succeeds.
func TestARequestsTimeoutStartsOnceTheClientsLimiterLetsItGo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"held","namespace":"legion"}}`)
	}))
	t.Cleanup(server.Close)
	_, requests := clientConfigs(&rest.Config{Host: server.URL})
	limit := newFakeLimiter(5, 1)
	if err := limit.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests.RateLimiter, requests.Timeout = limit, 50*time.Millisecond
	kube, err := kubernetes.NewForConfig(requests)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := kube.CoreV1().Secrets("legion").Get(context.Background(), "held", metav1.GetOptions{}); err != nil {
		t.Fatalf("a request the limiter held past its client's timeout failed: %v", err)
	}
	if waited := time.Since(started); waited < 150*time.Millisecond {
		t.Fatalf("the request went after %s, want it held about 200ms by the limiter", waited)
	}
}
