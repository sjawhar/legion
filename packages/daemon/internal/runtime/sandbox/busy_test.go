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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// etcdThrottle is the message etcd's throttle reaches a Kubernetes client with.
const etcdThrottle = "rpc error: code = ResourceExhausted desc = etcdserver: throttle: too many requests"

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
		"etcd's throttle as a 500": {&apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Message: etcdThrottle}}, true},
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
// often and how long it was waited out.
func TestALaunchsRequestStopsWaitingAtTheLaunchsDeadline(t *testing.T) {
	g := newRig(t, nil)
	ctx, cancel := context.WithTimeout(launching(g.ctx), 50*time.Millisecond)
	defer cancel()
	var sent atomic.Int32
	g.kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		sent.Add(1)
		return true, nil, apierrors.NewInternalError(errors.New(etcdThrottle))
	})
	start := time.Now()
	_, err := g.r.kube.CoreV1().Secrets(testNamespace).Get(ctx, "a-secret", metav1.GetOptions{})
	if !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), "the API server answered get secrets a-secret overloaded") {
		t.Fatalf("get through a throttle that never clears = %v, want the server's last answer saying how long it was waited out", err)
	}
	if took := time.Since(start); took > 50*time.Millisecond+recheckInterval {
		t.Fatalf("the read waited %s, past its 50ms deadline", took)
	}
	if sent.Load() < 2 {
		t.Fatalf("the read was sent %d times, want it sent again inside its deadline", sent.Load())
	}
}

// A launch's request on a context with no deadline stops being sent again at the boot timeout from
// its first send: the bound each wait of a launch has, never a multiple of it.
func TestALaunchsRequestWithNoDeadlineStopsWaitingAtTheBootTimeout(t *testing.T) {
	const boot = 400 * time.Millisecond
	g := newRig(t, nil, withOptions(func(o *Options) { o.BootTimeout = boot }))
	g.kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New(etcdThrottle))
	})
	start := time.Now()
	_, err := g.r.kube.CoreV1().Secrets(testNamespace).Get(launching(g.ctx), "a-secret", metav1.GetOptions{})
	took := time.Since(start)
	if !apierrors.IsInternalError(err) {
		t.Fatalf("get through a throttle that never clears = %v, want the server's last answer", err)
	}
	if took < boot/2 || took > boot*3/2 {
		t.Fatalf("the read was waited out for %s, want about the boot timeout %s", took, boot)
	}
}
