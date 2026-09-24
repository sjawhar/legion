package sandbox

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const (
	sandboxUID = "uid-sandbox-7"
	recorded   = "uid-pod-recorded"
)

var rigNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func condition(kind, status, reason string, generation int64) metav1.Condition {
	return metav1.Condition{Type: kind, Status: metav1.ConditionStatus(status), Reason: reason, ObservedGeneration: generation}
}

func terminated(name string, exit int32, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Reason: reason}}}
}

func unscheduledFor(age time.Duration) corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/3 nodes are available", LastTransitionTime: metav1.NewTime(rigNow.Add(-age)),
	}}}
}

// The mapping, row by row and in the order the rows apply, against the synced stores. Every
// observation carries the recorded locator whatever it saw; the uid it saw goes in the detail.
func TestTheMappingRowByRowInPrecedence(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	withPod := func(mode string, conditions []metav1.Condition, uid, owner string, status corev1.PodStatus, edit ...func(*corev1.Pod)) []k8sruntime.Object {
		pod := podObject(name, uid, owner, labels, status)
		for _, e := range edit {
			e(pod)
		}
		return []k8sruntime.Object{sandboxObject(t, name, sandboxUID, mode, labels, conditions...), pod}
	}
	running := corev1.PodStatus{Phase: corev1.PodRunning}
	failed := func(statuses ...corev1.ContainerStatus) corev1.PodStatus {
		status := corev1.PodStatus{Phase: corev1.PodFailed}
		for _, s := range statuses {
			if s.Name == initContainer {
				status.InitContainerStatuses = append(status.InitContainerStatuses, s)
			} else {
				status.ContainerStatuses = append(status.ContainerStatuses, s)
			}
		}
		return status
	}
	for _, tc := range []struct {
		row     string
		objects []k8sruntime.Object
		want    runtime.ObservationKind
		detail  []string
		absent  []string
	}{
		{row: "2 sandbox absent", want: runtime.Gone, detail: []string{"sandbox legion/" + name + " is absent"}},
		{
			row:     "3 no pod while Running",
			objects: []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels)},
			want:    runtime.Gone, detail: []string{"no pod of sandbox " + name + " (operatingMode Running)"},
		},
		{
			row: "3 no pod while Suspended, with the current Suspended condition",
			objects: []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeSuspended, labels,
				condition(conditionSuspended, "True", "PodTerminated", 2))},
			want: runtime.Gone, detail: []string{"operatingMode Suspended; Suspended=True PodTerminated"},
		},
		{
			row:     "3 a same-named pod the sandbox does not own is no pod of the claim's",
			objects: withPod(modeRunning, nil, recorded, "uid-sandbox-earlier", running),
			want:    runtime.Gone, detail: []string{"pod " + name + " (uid " + recorded + ") holds the name but is not controller-owned by the sandbox (uid " + sandboxUID + ")"},
		},
		{
			row:     "4 another uid, before its phase is read",
			objects: withPod(modeRunning, nil, "uid-pod-newer", sandboxUID, failed(terminated(mainContainer, 137, "Error"))),
			want:    runtime.NotRecordedProcess, detail: []string{"is uid uid-pod-newer, not the recorded " + recorded},
		},
		{
			row:     "5 the main container ended",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, failed(terminated(initContainer, 0, "Completed"), terminated(mainContainer, 137, "Error"))),
			want:    runtime.Gone,
			detail:  []string{"pod " + name + " (uid " + recorded + ") Failed: main container worker terminated (Error, exit code 137)", "last lines of worker:\nfake logs"},
			absent:  []string{"workspace-lost"},
		},
		{
			row:     "5 workspace-init lost the workspace",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, failed(terminated(initContainer, 3, "Error"))),
			want:    runtime.Gone,
			detail:  []string{"workspace-lost: pod " + name, "init container workspace-init terminated (Error, exit code 3)", "last lines of workspace-init:"},
		},
		{
			row:     "5 the agent exited cleanly",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{terminated(mainContainer, 0, "Completed")}}),
			want:    runtime.Gone, detail: []string{"Succeeded: main container worker terminated (Completed, exit code 0)"},
		},
		{
			row: "5 before 7, quoting Finished only when current",
			objects: withPod(modeRunning, []metav1.Condition{
				condition(conditionReady, "False", reasonReconcilerError, 2), condition(conditionFinished, "True", "PodFailed", 2),
			}, recorded, sandboxUID, failed(terminated(mainContainer, 1, "Error"))),
			want: runtime.Gone, detail: []string{"; sandbox Finished=True PodFailed;"},
		},
		{
			row: "5 a Finished left by the previous generation is not quoted",
			objects: withPod(modeRunning, []metav1.Condition{condition(conditionFinished, "True", "PodSucceeded", 1)},
				recorded, sandboxUID, failed(terminated(mainContainer, 1, "Error"))),
			want: runtime.Gone, absent: []string{"Finished"},
		},
		{
			row: "6 unscheduled past the boot timeout",
			objects: append(withPod(modeRunning, nil, recorded, sandboxUID, unscheduledFor(3*time.Second)), &corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: name + ".1", Namespace: testNamespace},
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: name, UID: recorded},
				Type:           "Warning", Reason: "FailedScheduling", Message: "0/3 nodes are available: 3 node(s) had untolerated taint",
			}),
			want:   runtime.Gone,
			detail: []string{"unscheduled for 3s, past the boot timeout 2s (PodScheduled=False Unschedulable", "events: Warning FailedScheduling: 0/3 nodes are available: 3 node(s) had untolerated taint"},
		},
		{
			row:     "6 unscheduled within the boot timeout is alive",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, unscheduledFor(time.Second)),
			want:    runtime.Alive,
		},
		{
			row:     "7 the controller sees several pods",
			objects: withPod(modeRunning, []metav1.Condition{condition(conditionReady, "False", reasonMultiplePods, 2)}, recorded, sandboxUID, running),
			want:    runtime.Uncertain, detail: []string{"Ready=False MultiplePods"},
		},
		{
			row:     "7 a reconciler error left by the previous generation",
			objects: withPod(modeRunning, []metav1.Condition{condition(conditionReady, "False", reasonReconcilerError, 1)}, recorded, sandboxUID, running),
			want:    runtime.Alive,
		},
		{
			row: "8 pending before its init container starts (B4)",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{{
				Name: initContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			}}}),
			want: runtime.Alive, detail: []string{"(uid " + recorded + ") Pending"},
		},
		{
			row:     "8 a pod with no status yet",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, corev1.PodStatus{}),
			want:    runtime.Alive,
		},
		{
			row:     "8 running",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, running),
			want:    runtime.Alive, detail: []string{"(uid " + recorded + ") Running"},
		},
		{
			row: "8 a pod being deleted is alive until it is gone",
			objects: withPod(modeSuspended, nil, recorded, sandboxUID, running, func(p *corev1.Pod) {
				now := metav1.NewTime(rigNow)
				p.DeletionTimestamp, p.Finalizers = &now, []string{"test/hold"}
			}),
			want: runtime.Alive,
		},
		{
			row: "8 a new pod's ADD while the sandbox still holds the old Finished (B5)",
			objects: withPod(modeRunning, []metav1.Condition{
				condition(conditionFinished, "True", "PodFailed", 1), condition(conditionReady, "False", "PodFailed", 1),
			}, recorded, sandboxUID, corev1.PodStatus{Phase: corev1.PodPending}),
			want: runtime.Alive,
		},
		{
			row:     "a phase the mapping has no row for is uncertain",
			objects: withPod(modeRunning, nil, recorded, sandboxUID, corev1.PodStatus{Phase: corev1.PodUnknown}),
			want:    runtime.Uncertain, detail: []string{"phase Unknown"},
		},
	} {
		t.Run(tc.row, func(t *testing.T) {
			g := newRig(t, tc.objects, withoutController())
			loc := sandboxLocator(workerToken, recorded)
			obs, err := g.r.Probe(g.ctx, loc)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if obs.Kind != tc.want {
				t.Fatalf("kind %s, want %s (detail: %s)", obs.Kind, tc.want, obs.Detail)
			}
			if obs.Locator != loc {
				t.Fatalf("observation carries %+v, want the recorded locator", obs.Locator)
			}
			for _, want := range tc.detail {
				if !strings.Contains(obs.Detail, want) {
					t.Errorf("detail %q lacks %q", obs.Detail, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(obs.Detail, absent) {
					t.Errorf("detail %q has %q", obs.Detail, absent)
				}
			}
		})
	}
}

// breakablePods serves the pod informer's list and watch from the tracker until a test breaks
// them: then every list and watch request fails, as they do while the API server is unreachable,
// and the watch in flight ends.
type breakablePods struct {
	g      *rig
	broken atomic.Bool
	mu     sync.Mutex
	end    func()
}

func withBreakablePods(b *breakablePods) rigOption {
	return func(g *rig, _ *Options) {
		b.g = g
		refused := errors.New("dial tcp 10.1.0.1:443: connect: connection refused")
		g.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			if b.broken.Load() {
				return true, nil, refused
			}
			return false, nil, nil
		})
		g.kube.PrependWatchReactor("pods", func(a k8stesting.Action) (bool, watch.Interface, error) {
			if b.broken.Load() {
				return true, nil, refused
			}
			upstream, err := g.kube.Tracker().Watch(podsGVR, a.GetNamespace())
			if err != nil {
				return true, nil, err
			}
			events, done := make(chan watch.Event), make(chan struct{})
			go func() {
				defer close(events)
				defer upstream.Stop()
				for {
					select {
					case <-done:
						return
					case ev, ok := <-upstream.ResultChan():
						if !ok {
							return
						}
						select {
						case events <- ev:
						case <-done:
							return
						}
					}
				}
			}()
			b.mu.Lock()
			b.end = sync.OnceFunc(func() { close(done) })
			b.mu.Unlock()
			return true, watch.NewProxyWatcher(events), nil
		})
	}
}

// fail makes every pod list and watch request fail and ends the watch in flight.
func (b *breakablePods) fail() {
	b.broken.Store(true)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.end != nil {
		b.end()
	}
}

// A store whose watch keeps failing after it synced holds what it last heard: while the latest
// list or watch request failed, the runtime answers Uncertain from it (row 1), never the Alive it
// last saw, and answers from the store again once a request succeeds.
func TestAFailingWatchMakesTheStoreUncertainUntilItRecovers(t *testing.T) {
	pods := &breakablePods{}
	g := newRig(t, nil, withBreakablePods(pods))
	loc := g.spawn(workerSpec(t))
	probe := func() runtime.Observation {
		t.Helper()
		obs, err := g.r.Probe(g.ctx, loc)
		if err != nil {
			t.Fatal(err)
		}
		return obs
	}
	if obs := probe(); obs.Kind != runtime.Alive {
		t.Fatalf("before the watch fails: %s %q, want Alive", obs.Kind, obs.Detail)
	}
	pods.fail()
	waitFor := func(what string, kind runtime.ObservationKind) runtime.Observation {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			obs := probe()
			if obs.Kind == kind {
				return obs
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: still %s %q", what, obs.Kind, obs.Detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	obs := waitFor("the pod watch failing", runtime.Uncertain)
	if !strings.Contains(obs.Detail, "connection refused") || !strings.Contains(obs.Detail, "pod store") {
		t.Errorf("the Uncertain detail %q names neither the failing pods watch nor its error", obs.Detail)
	}
	pods.broken.Store(false)
	waitFor("the pod watch recovering", runtime.Alive)
}
