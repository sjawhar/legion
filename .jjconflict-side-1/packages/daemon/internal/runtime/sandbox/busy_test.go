package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// etcdThrottle is the message etcd's throttle reaches a Kubernetes client with.
const etcdThrottle = "rpc error: code = ResourceExhausted desc = etcdserver: throttle: too many requests"

// etcdThrottled is the answer etcd's throttle reaches a Kubernetes client as: a 500 naming no
// reason, carrying etcdThrottle.
func etcdThrottled() error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Message: etcdThrottle}}
}

// An API server's overloaded or failed answer is told from its refusal of a request, and from no
// answer at all, in one place: a status error by its reason, and by its code (429, or 500 and up)
// only when it names no reason; a webhook's denial is a refusal whatever its code; and an error
// that carries no status got no answer and is not busy, wherever each sits in the error's chain.
func TestServerBusyTellsAnOverloadedServerFromARefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		busy bool
	}{
		"429 from flow control":    {apierrors.NewTooManyRequests("the request has been limited", 1), true},
		"etcd's throttle as a 500": {etcdThrottled(), true},
		"an internal error":        {apierrors.NewInternalError(errors.New("etcdserver: request timed out")), true},
		"503":                      {apierrors.NewServiceUnavailable("the server is currently unable to handle the request"), true},
		"504":                      {apierrors.NewTimeoutError("the request did not complete", 0), true},
		"the server's own timeout": {apierrors.NewServerTimeout(sandboxGVR.GroupResource(), "patch", 1), true},
		"502 by its code alone":    {&apierrors.StatusError{ErrStatus: metav1.Status{Code: http.StatusBadGateway}}, true},
		"a wrapped 500":            {fmt.Errorf("record the addresses: %w", apierrors.NewInternalError(errors.New("etcd leader changed"))), true},
		"403":                      {apierrors.NewForbidden(sandboxGVR.GroupResource(), "s", errors.New("RBAC")), false},
		"404":                      {apierrors.NewNotFound(sandboxGVR.GroupResource(), "s"), false},
		"409":                      {apierrors.NewConflict(sandboxGVR.GroupResource(), "s", errors.New("modified")), false},
		"422":                      {apierrors.NewInvalid(sandboxGVR.GroupVersion().WithKind("Sandbox").GroupKind(), "s", nil), false},
		"the client's own timeout": {fmt.Errorf("the client gave up: %w", context.DeadlineExceeded), false},
		"a connection refused":     {errors.New("dial tcp 192.0.2.1:443: connect: connection refused"), false},
		"no error":                 {nil, false},
		"a 500 with no reason":     {&apierrors.StatusError{ErrStatus: metav1.Status{Code: http.StatusInternalServerError}}, true},
		"a 500 with reason Forbidden, a webhook's denial": {&apierrors.StatusError{ErrStatus: metav1.Status{Code: http.StatusInternalServerError,
			Reason: metav1.StatusReasonForbidden, Message: `admission webhook "policy.example" denied the request: not allowed`}}, false},
		"a webhook's denial as a 500 with no reason": {&apierrors.StatusError{ErrStatus: metav1.Status{Code: http.StatusInternalServerError,
			Message: `admission webhook "policy.example" denied the request: not allowed`}}, false},
		"a 409 carrying etcd's throttle text": {apierrors.NewConflict(sandboxGVR.GroupResource(), "s", errors.New(etcdThrottle)), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := serverBusy(tc.err); got != tc.busy {
				t.Fatalf("serverBusy(%v) = %t, want %t", tc.err, got, tc.busy)
			}
		})
	}
}

// Only a launch's requests wait an overloaded server out: the same read on a context no launch
// made is sent once and fails with the server's answer, for its own caller to try again, and on a
// launch's context it is sent again until the server answers it.
func TestOnlyALaunchsRequestsAreSentAgainWhileTheServerIsOverloaded(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx       func(context.Context) context.Context
		wantSent  int32
		wantError bool
	}{
		"a request no launch made": {ctx: func(ctx context.Context) context.Context { return ctx }, wantSent: 1, wantError: true},
		"a launch's request":       {ctx: launching, wantSent: 4},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			name := roleSecretName(loc.Sandbox.Name, claim.RoleTester)
			var sent atomic.Int32
			g.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				if a.(k8stesting.GetAction).GetName() != name || sent.Add(1) > 3 {
					return false, nil, nil
				}
				return true, nil, apierrors.NewInternalError(errors.New(etcdThrottle))
			})
			secret, err := g.r.kube.CoreV1().Secrets(testNamespace).Get(tc.ctx(g.ctx), name, metav1.GetOptions{})
			if (err != nil) != tc.wantError {
				t.Fatalf("get %s = %v, want an error: %t", name, err, tc.wantError)
			}
			if err == nil && (secret == nil || secret.Name != name) {
				t.Fatalf("get %s answered %v", name, secret)
			}
			if got := sent.Load(); got != tc.wantSent {
				t.Fatalf("the read was sent %d times, want %d", got, tc.wantSent)
			}
		})
	}
}

// A launch's request the server keeps answering overloaded is sent again only while the launch's
// deadline for it allows a further wait, and then fails with the server's last answer, saying how
// often and how long it was waited out. The deadline is its context's when it has one, and
// otherwise the boot timeout from its first send: the bound each wait of a launch has, never a
// multiple of it.
func TestALaunchsRequestStopsWaitingAtItsDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		// bootTimeout is the rig's boot timeout, 0 for the rig's own; deadline is the request's
		// context's, 0 for none.
		bootTimeout, deadline time.Duration
		// least and most bound how long the request is waited out.
		least, most time.Duration
	}{
		"a deadline on its context":  {deadline: 50 * time.Millisecond, most: 50*time.Millisecond + recheckInterval},
		"no deadline on its context": {bootTimeout: 400 * time.Millisecond, least: 200 * time.Millisecond, most: 600 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			var options []rigOption
			if tc.bootTimeout != 0 {
				options = append(options, withOptions(func(o *Options) { o.BootTimeout = tc.bootTimeout }))
			}
			g := newRig(t, nil, options...)
			ctx := launching(g.ctx)
			if tc.deadline != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}
			var sent atomic.Int32
			g.kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				sent.Add(1)
				return true, nil, apierrors.NewInternalError(errors.New(etcdThrottle))
			})
			start := time.Now()
			_, err := g.r.kube.CoreV1().Secrets(testNamespace).Get(ctx, "a-secret", metav1.GetOptions{})
			took := time.Since(start)
			if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), "the API server answered get secrets a-secret overloaded") {
				t.Fatalf("get through a throttle that never clears = %v, want the server's last answer saying how long it was waited out", err)
			}
			if took < tc.least || took > tc.most {
				t.Fatalf("the read was waited out for %s, want between %s and %s", took, tc.least, tc.most)
			}
			if sent.Load() < 2 {
				t.Fatalf("the read was sent %d times, want it sent again inside its deadline", sent.Load())
			}
		})
	}
}

// burstImage is an image the rig's runtime no longer launches, which a pod launched by an earlier
// release of the daemon runs (imageDrift).
const burstImage = "ghcr.io/sjawhar/legion-worker@sha256:0ld0000000000000000000000000000000000000000000000000000000000000"

// A boot onto a new contract relaunches every live role at once (the daemon's stale-plugin pass,
// which relaunches through the same launch a stale address does), and an API server under that load
// answers a share of the launches' requests overloaded instead of acting on them: API Priority and
// Fairness's 429 and the 500 etcd's throttle reaches a client as.
// Here 24 roles of eight issue pods relaunch at once while every fifth request a launch sends is
// answered so. Half the pods run an image the runtime no longer launches, so the first relaunch into
// each replaces it and every kind of request a launch makes meets the throttle: the Sandbox's
// suspension and its Running patch, the wait for the old pod's end, the launcher Secrets' writes
// and binding, their reads, and the address record. The informers meet it too: their watches end as
// the burst begins, and the server refuses their next lists and watches for half a second, so every
// store's feed is failing while the launches wait for their launchers. The burst converges: every
// claim is launched, and none is charged a launch failure.
func TestABurstOfRelaunchesThroughAThrottlingAPIServerConvergesUncharged(t *testing.T) {
	for name, refusal := range map[string]error{
		"the feeds refused with 429":        apierrors.NewTooManyRequests("the request has been limited by the API server's flow control", 1),
		"the feeds refused with etcd's 500": apierrors.NewInternalError(errors.New(etcdThrottle)),
	} {
		t.Run(name, func(t *testing.T) { burstOfRelaunches(t, refusal) })
	}
}

// burstOfRelaunches is the burst, the informers' lists and watches refused with feedRefusal.
func burstOfRelaunches(t *testing.T, feedRefusal error) {
	const issues = 8
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}
	g := newRig(t, nil, withBreakableFeeds(), withOptions(func(o *Options) { o.BootTimeout = 15 * time.Second }))
	type role struct {
		spec    runtime.SpawnSpec
		machine *supervise.Machine
		first   *runtime.Locator
	}
	var all []role
	for i := range issues {
		issue := fmt.Sprintf("LEGION-%d", 300+i)
		var name string
		for _, r := range roles {
			token, err := claim.NewToken(testProject, issue, r)
			if err != nil {
				t.Fatal(err)
			}
			spec := testSpec(t, token, r, issue)
			machine, _, _ := supervisedSandboxMachine(t, g, spec)
			all = append(all, role{spec: spec, machine: machine, first: startSupervisedSandboxRole(t, g, machine, spec, resumeSession)})
			name = SandboxName(token)
		}
		if i%2 == 0 {
			g.update(g.pod(name), func(p *corev1.Pod) {
				for c := range p.Spec.Containers {
					p.Spec.Containers[c].Image = burstImage
				}
			})
			g.eventually("the pod store to see "+name+"'s old image", func() bool {
				_, drifted := imageDrift(g.r.storedPod(name), testImage)
				return drifted
			})
		}
	}

	var throttling atomic.Bool
	var sent, tooMany, serverError atomic.Int64
	throttle := func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		switch a.GetVerb() {
		case "get", "create", "update", "patch", "delete":
		default:
			return false, nil, nil
		}
		if !throttling.Load() {
			return false, nil, nil
		}
		n := sent.Add(1)
		if n%5 != 0 {
			return false, nil, nil
		}
		switch (n / 5) % 2 {
		case 0:
			tooMany.Add(1)
			return true, nil, apierrors.NewTooManyRequests("the request has been limited by the API server's flow control", 1)
		default:
			serverError.Add(1)
			return true, nil, etcdThrottled()
		}
	}
	g.dyn.PrependReactor("*", "sandboxes", throttle)
	g.kube.PrependReactor("*", "secrets", throttle)
	g.kube.PrependReactor("*", "pods", throttle)
	throttling.Store(true)
	g.refuseFeeds(feedRefusal)
	testwait.Eventually(t, "the stores' feeds to fail", func() bool { return g.r.sandboxFeed.check() != nil && g.r.podFeed.check() != nil })
	time.AfterFunc(500*time.Millisecond, func() { g.refuseFeeds(nil) })

	relaunched := make(chan error, len(all))
	for _, r := range all {
		go func() {
			relaunched <- r.machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.first, Kind: runtime.StaleAddress, At: rigNow}})
		}()
	}
	for range all {
		select {
		case err := <-relaunched:
			if err != nil {
				t.Errorf("a relaunch of the burst: %v", err)
			}
		case <-time.After(time.Minute):
			t.Fatal("the burst of relaunches did not end")
		}
	}
	throttling.Store(false)
	for _, r := range all {
		if got := r.machine.Claim(); got.State == supervise.StateFailed || got.Budgets.LaunchFailures != 0 || got.Locator == nil {
			t.Errorf("%s after the burst: %s with %d launch failures and locator %v, want launched with none",
				r.spec.Claim, got.State, got.Budgets.LaunchFailures, got.Locator)
		}
	}
	for i := 0; i < issues; i += 2 {
		name := SandboxName(all[i*len(roles)].spec.Claim)
		if _, drifted := imageDrift(g.pod(name), testImage); drifted {
			t.Errorf("%s still runs the old image after the burst, want its pod replaced", name)
		}
	}
	if tooMany.Load() == 0 || serverError.Load() == 0 {
		t.Fatalf("the throttle answered %d requests 429 and %d 500 of %d sent; want some of each", tooMany.Load(), serverError.Load(), sent.Load())
	}
	feedRefusals := g.refuseFeeds(nil)
	if feedRefusals == 0 {
		t.Fatal("the server refused none of the informers' lists and watches")
	}
	t.Logf("the throttle answered %d requests 429 and %d 500 of %d sent, and refused %d of the informers' lists and watches",
		tooMany.Load(), serverError.Load(), sent.Load(), feedRefusals)
}

// A write the server applied and answered timed out (a 504) is sent again and refused as already
// done: the launch reads what the write left and goes on from it, and is launched, charged nothing.
func TestAWriteTheServerAppliedButAnsweredTimedOutIsLaunchedUncharged(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	machine, _, _ := supervisedSandboxMachine(t, g, spec)
	provisioning := secretName(SandboxName(spec.Claim))
	apply := k8stesting.ObjectReaction(g.kube.Tracker())
	var answered atomic.Bool
	g.kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		object, err := metaOf(a.(k8stesting.CreateAction).GetObject())
		if err != nil || object.GetName() != provisioning || !answered.CompareAndSwap(false, true) {
			return false, nil, nil
		}
		g.record(a)
		if _, _, err := apply(a); err != nil {
			t.Errorf("apply the create of %s: %v", provisioning, err)
		}
		return true, nil, apierrors.NewTimeoutError("the request did not complete in time", 0)
	})
	g.launcher(spec.Claim)
	if err := machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}); err != nil {
		t.Fatalf("spawn through a create applied and answered 504: %v", err)
	}
	if !answered.Load() {
		t.Fatalf("the launch never created %s", provisioning)
	}
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Budgets.LaunchFailures != 0 || got.Locator == nil {
		t.Fatalf("claim after a create applied and answered 504 = %s with %d launch failures and locator %v, want launching with none",
			got.State, got.Budgets.LaunchFailures, got.Locator)
	}
}

// relabelSandbox is a Sandbox an issue's old tree left, Suspended and labelled with that tree, which
// a launch of the issue in testTree relabels (ensureSandbox); answer answers each relabel patch sent,
// counted from 1.
func relabelSandbox(t *testing.T, answer func(sent int32, a k8stesting.Action) (bool, k8sruntime.Object, error)) (*rig, *atomic.Int32) {
	t.Helper()
	name := SandboxName(workerToken)
	old := map[string]string{labelProject: testProject, labelTree: "LEGION-100", labelIssue: testTree}
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-moved", modeSuspended, old)})
	var sent atomic.Int32
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if !strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), labelPatchPath(labelTree)) {
			return false, nil, nil
		}
		return answer(sent.Add(1), a)
	})
	return g, &sent
}

// fenceRefused is the API server's answer to a JSON patch whose test operation failed.
func fenceRefused(name string) error {
	return apierrors.NewInvalid(sandboxGVR.GroupVersion().WithKind("Sandbox").GroupKind(), name, nil)
}

// A relabel the server applied and answered timed out is sent again, and its own fence refuses the
// resend, since the resourceVersion it tests has moved: the launch reads the Sandbox again, finds it
// relabelled, and goes on.
func TestARelabelTheServerAppliedButAnsweredTimedOutGoesOn(t *testing.T) {
	var g *rig
	g, sent := relabelSandbox(t, func(sent int32, a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if sent > 1 {
			return true, nil, fenceRefused(SandboxName(workerToken))
		}
		if _, _, err := k8stesting.ObjectReaction(generationTracker{g.dyn.Tracker()})(a); err != nil {
			t.Errorf("apply the relabel: %v", err)
		}
		return true, nil, apierrors.NewTimeoutError("the request did not complete in time", 0)
	})
	g.spawn(workerSpec(t))
	if got := g.sandbox(SandboxName(workerToken)).Labels[labelTree]; got != testTree {
		t.Fatalf("the Sandbox is labelled for tree %q after the launch, want %q", got, testTree)
	}
	if got := sent.Load(); got != 2 {
		t.Fatalf("the relabel was sent %d times, want twice: once applied and answered 504, once refused by its fence", got)
	}
}

// A relabel the server refuses as invalid for a reason of its own, which another read does not
// change, fails the try after one patch: the Sandbox read again still holds the resourceVersion the
// refused patch tested.
func TestALastingRefusalOfTheRelabelFailsTheTryAfterOnePatch(t *testing.T) {
	g, sent := relabelSandbox(t, func(int32, k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fenceRefused(SandboxName(workerToken))
	})
	g.launcher(workerToken)
	if _, err := g.r.Spawn(g.ctx, workerSpec(t)); err == nil || !strings.Contains(err.Error(), "relabel sandbox") {
		t.Fatalf("Spawn through a lasting refusal of the relabel = %v, want the refusal", err)
	}
	if got := sent.Load(); got != 1 {
		t.Fatalf("the relabel was sent %d times, want once", got)
	}
}

// A request inside a launch's wait ends with the wait's budget: the old pod stays in the store for
// most of awaitPodGone's budget, then the confirming read of it is answered overloaded, and the wait
// still ends at its budget rather than a boot timeout after that read began.
func TestARequestInsideALaunchsWaitEndsWithTheWaitsBudget(t *testing.T) {
	const boot = time.Second
	name := SandboxName(workerToken)
	labels := map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: testTree}
	g := newRig(t, []k8sruntime.Object{
		sandboxObject(t, name, "uid-waited", modeSuspended, labels),
		podObject(name, "uid-old-pod", "uid-waited", labels, runningStatus()),
	}, withoutController(), withOptions(func(o *Options) { o.BootTimeout = boot }))
	g.kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New(etcdThrottle))
	})
	time.AfterFunc(boot*7/10, func() { _ = g.kube.Tracker().Delete(podsGVR, testNamespace, name) })
	start := time.Now()
	_, err := g.r.awaitPodGone(launching(g.ctx), g.sandbox(name))
	took := time.Since(start)
	if err == nil {
		t.Fatal("awaitPodGone through a read answered overloaded throughout = nil, want it to end with an error")
	}
	if took > boot*13/10 {
		t.Fatalf("awaitPodGone took %s, past its %s budget: %v", took, boot, err)
	}
}
