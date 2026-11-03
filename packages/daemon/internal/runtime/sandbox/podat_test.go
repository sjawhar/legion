package sandbox

import (
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// PodAt is the project's running pod that holds an address, which is how the daemon's state route
// knows which pod's plugin asked (api's Options.CallerPod). Every address a pod holds counts, its
// IPv6 one and the IPv4-mapped form of its IPv4 one included; a pod that has run to its end is
// passed over, since its address may be a new pod's already; a pod of another project is not in
// this runtime's watch at all.
func TestPodAtIsTheRunningPodOfTheProjectThatHoldsTheAddress(t *testing.T) {
	project := map[string]string{labelProject: testProject}
	g := newRig(t, []k8sruntime.Object{
		podObject("legion-legion-legion-208", "uid-running", "", project, corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.1.24.6", PodIPs: []corev1.PodIP{{IP: "10.1.24.6"}, {IP: "fd00::6"}},
		}),
		podObject("legion-legion-legion-209", "uid-ended", "", project, corev1.PodStatus{Phase: corev1.PodFailed, PodIP: "10.1.24.7"}),
		podObject("legion-other-other-1", "uid-other", "", map[string]string{labelProject: "other"}, corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.1.24.8",
		}),
	})

	for _, tc := range []struct {
		addr string
		uid  string
	}{
		{addr: "10.1.24.6", uid: "uid-running"},
		{addr: "::ffff:10.1.24.6", uid: "uid-running"},
		{addr: "fd00::6", uid: "uid-running"},
		{addr: "10.1.24.7"},
		{addr: "10.1.24.8"},
		{addr: "192.0.2.1"},
	} {
		uid, ok := g.r.PodAt(netip.MustParseAddr(tc.addr))
		if uid != tc.uid || ok != (tc.uid != "") {
			t.Errorf("PodAt(%s) = %q, %v; want %q, %v", tc.addr, uid, ok, tc.uid, tc.uid != "")
		}
	}
}
