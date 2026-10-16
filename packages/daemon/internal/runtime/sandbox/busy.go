package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/sjawhar/legion/daemon/internal/backoff"
)

// serverBusy is whether err is the API server's answer that it is overloaded or failed, rather
// than its refusal of the request. It is the one place the runtime tells the two apart. client-go
// hands back a status for every answer that is not a success, so an error that carries none got no
// answer — the client's own timeout, a connection that never reached the server — and is not busy.
// A status is judged by its reason: TooManyRequests, which API Priority and Fairness answers once
// client-go's own Retry-After retries ran out, InternalError, such as the 500 etcd's throttle
// reaches a client as (`rpc error: code = ResourceExhausted desc = etcdserver: throttle: too many
// requests`), ServerTimeout, Timeout and ServiceUnavailable are busy, and every other reason refuses
// the request; only a status that names no reason is judged by its code, 429 or 500 and up. A
// status whose message says an admission webhook denied the request is a refusal whatever its
// reason or code.
func serverBusy(err error) bool {
	if err == nil {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	s := status.Status()
	if strings.Contains(s.Message, "denied the request") {
		return false
	}
	switch s.Reason {
	case metav1.StatusReasonTooManyRequests, metav1.StatusReasonInternalError, metav1.StatusReasonServerTimeout,
		metav1.StatusReasonTimeout, metav1.StatusReasonServiceUnavailable:
		return true
	case metav1.StatusReasonUnknown:
		return s.Code == http.StatusTooManyRequests || s.Code >= http.StatusInternalServerError
	}
	return false
}

// launchKey marks a context as a launch's (launching).
type launchKey struct{}

// launching is ctx marked as a launch's: each Kubernetes request the runtime makes on it, or on a
// context made from it, is sent again while the API server answers it overloaded (resend). relaunch
// marks its whole launch, a failed launch's suspension of its Sandbox included (suspendFailedLaunch,
// bounded by apiTimeout), so every request of a launch waits the server out and none of the
// runtime's other requests does: a sweep, a claim's suspension and a hello's read each fail on the
// answer and are tried again by their own callers.
func launching(ctx context.Context) context.Context {
	return context.WithValue(ctx, launchKey{}, true)
}

func isLaunch(ctx context.Context) bool {
	marked, _ := ctx.Value(launchKey{}).(bool)
	return marked
}

// resend sends one request and, when it is a launch's (launching), sends it again after a jittered
// backoff (backoff.Wait from Options.BusyBackoff) for as long as the API server answers it
// overloaded (serverBusy) and the launch's own deadline for it allows: the boot timeout from its
// first send, or ctx's deadline when that comes first — a request a launch's wait makes (await) has
// that wait's budget as ctx's deadline, and a start that of its start step. What reaches the launch
// is the server's answer to the request, or, once the deadline leaves no room for another wait or
// ctx ends, its last overloaded one, saying how long it was waited out, which the launch then fails
// on as on any other error. A launch the server keeps waiting is charged nothing while it waits;
// supervision charges a launch that failed. what names the request in the log.
func resend[T any](ctx context.Context, r *Runtime, what string, send func() (T, error)) (T, error) {
	answer, err := send()
	if !serverBusy(err) || !isLaunch(ctx) {
		return answer, err
	}
	start := time.Now()
	deadline := start.Add(r.bootTimeout)
	if bound, ok := ctx.Deadline(); ok && bound.Before(deadline) {
		deadline = bound
	}
	r.log.Warn("sandbox runtime: the API server answered a launch's request overloaded; sending it again after a backoff",
		"request", what, "error", err, "deadline", deadline)
	for overloaded := 1; ; overloaded++ {
		wait := backoff.Wait(r.busyBackoff, overloaded)
		stopped := ""
		if !time.Now().Add(wait).Before(deadline) {
			stopped = "until the launch's deadline for it"
		} else if ended := backoff.Sleep(ctx, wait); ended != nil {
			stopped = "until the launch's context ended: " + ended.Error()
		}
		if stopped != "" {
			return answer, fmt.Errorf("%w (the API server answered %s overloaded %d times in %s, %s)",
				err, what, overloaded, time.Since(start).Round(time.Millisecond), stopped)
		}
		if answer, err = send(); !serverBusy(err) {
			r.log.Info("sandbox runtime: the API server answered a launch's request it had answered overloaded",
				"request", what, "overloaded", overloaded, "after", time.Since(start).Round(time.Millisecond), "error", err)
			return answer, err
		}
	}
}

// patientDynamic is the runtime's dynamic client for its requests, each read and write of a
// namespaced object sent through resend; a watch, which the runtime's informers make on clients of
// their own, and every other call pass through.
type patientDynamic struct {
	dynamic.Interface
	r *Runtime
}

func (d patientDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return patientResource{d.Interface.Resource(gvr), d.r, gvr.Resource}
}

type patientResource struct {
	dynamic.NamespaceableResourceInterface
	r        *Runtime
	resource string
}

func (p patientResource) Namespace(namespace string) dynamic.ResourceInterface {
	return patientObjects{p.NamespaceableResourceInterface.Namespace(namespace), p.r, p.resource}
}

type patientObjects struct {
	dynamic.ResourceInterface
	r        *Runtime
	resource string
}

func (p patientObjects) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return resend(ctx, p.r, "get "+p.resource+" "+name, func() (*unstructured.Unstructured, error) {
		return p.ResourceInterface.Get(ctx, name, opts, subresources...)
	})
}

func (p patientObjects) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return resend(ctx, p.r, "list "+p.resource, func() (*unstructured.UnstructuredList, error) {
		return p.ResourceInterface.List(ctx, opts)
	})
}

func (p patientObjects) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return resend(ctx, p.r, "create "+p.resource+" "+obj.GetName(), func() (*unstructured.Unstructured, error) {
		return p.ResourceInterface.Create(ctx, obj, opts, subresources...)
	})
}

func (p patientObjects) Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return resend(ctx, p.r, "update "+p.resource+" "+obj.GetName(), func() (*unstructured.Unstructured, error) {
		return p.ResourceInterface.Update(ctx, obj, opts, subresources...)
	})
}

func (p patientObjects) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return resend(ctx, p.r, "patch "+p.resource+" "+name, func() (*unstructured.Unstructured, error) {
		return p.ResourceInterface.Patch(ctx, name, pt, data, opts, subresources...)
	})
}

func (p patientObjects) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	_, err := resend(ctx, p.r, "delete "+p.resource+" "+name, func() (struct{}, error) {
		return struct{}{}, p.ResourceInterface.Delete(ctx, name, opts, subresources...)
	})
	return err
}

// patientKube is the runtime's typed client for its requests, each read and write of a Secret and
// each read of a pod sent through resend; every other call passes through.
type patientKube struct {
	kubernetes.Interface
	r *Runtime
}

func (k patientKube) CoreV1() corev1client.CoreV1Interface {
	return patientCore{k.Interface.CoreV1(), k.r}
}

type patientCore struct {
	corev1client.CoreV1Interface
	r *Runtime
}

func (c patientCore) Secrets(namespace string) corev1client.SecretInterface {
	return patientSecrets{c.CoreV1Interface.Secrets(namespace), c.r}
}

func (c patientCore) Pods(namespace string) corev1client.PodInterface {
	return patientPods{c.CoreV1Interface.Pods(namespace), c.r}
}

type patientSecrets struct {
	corev1client.SecretInterface
	r *Runtime
}

func (s patientSecrets) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error) {
	return resend(ctx, s.r, "get secrets "+name, func() (*corev1.Secret, error) {
		return s.SecretInterface.Get(ctx, name, opts)
	})
}

func (s patientSecrets) Create(ctx context.Context, secret *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error) {
	return resend(ctx, s.r, "create secrets "+secret.Name, func() (*corev1.Secret, error) {
		return s.SecretInterface.Create(ctx, secret, opts)
	})
}

func (s patientSecrets) Update(ctx context.Context, secret *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error) {
	return resend(ctx, s.r, "update secrets "+secret.Name, func() (*corev1.Secret, error) {
		return s.SecretInterface.Update(ctx, secret, opts)
	})
}

func (s patientSecrets) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	_, err := resend(ctx, s.r, "delete secrets "+name, func() (struct{}, error) {
		return struct{}{}, s.SecretInterface.Delete(ctx, name, opts)
	})
	return err
}

type patientPods struct {
	corev1client.PodInterface
	r *Runtime
}

func (p patientPods) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error) {
	return resend(ctx, p.r, "get pods "+name, func() (*corev1.Pod, error) {
		return p.PodInterface.Get(ctx, name, opts)
	})
}
