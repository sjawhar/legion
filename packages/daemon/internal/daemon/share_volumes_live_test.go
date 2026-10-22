//go:build e2e

// The share volumes live proof's driver (scripts/e2e/share-volumes-kind.sh, which builds it with
// `-tags e2e`, owns the throwaway kind cluster and the NFS server it runs on, and reads, writes and
// probes the shares with kubectl and `legion probe-boot`). Each case's legion.yaml is read by the
// daemon's own loader, and the `shares` case's Agent Sandbox runtime, built by the daemon's own
// sandboxOptions, launches one issue's root on the cluster through Agent Sandbox's controller and
// the gvisor runtime class (share_volumes_kind_test.go and burstable_kind_test.go build both). What
// the proof stands in for lies outside the volume path, as the Burstable proof's does: the agent is
// a sleep stub, the store answers as the daemon's does for an issue's first launch, the repository
// is a public one, and the roles' gh files hold a placeholder token.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// TestShareVolumesMountInAnIssuePodLive writes every case's legion.yaml, holds each to the pod
// block the loader must read from it, launches the `shares` case's issue pod, and holds that pod,
// as the API server recorded it, to the block: the claim and the export as volumes, each mounted in
// every role container, the claim read-only and the export writable, under gVisor. It writes
// `<case> <namespace>/<pod> <legion.yaml>` per case to LEGION_E2E_RESULT (`-` for a case it does
// not launch) for the script.
func TestShareVolumesMountInAnIssuePodLive(t *testing.T) {
	env := map[string]string{}
	for _, name := range []string{"LEGION_E2E_KUBECONFIG", "LEGION_E2E_KUBE_CONTEXT", "LEGION_E2E_IMAGE", "LEGION_E2E_STREAM_HOST", "LEGION_E2E_WORK", "LEGION_E2E_RESULT", "LEGION_E2E_SHARE_SERVER"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			t.Fatalf("%s is unset; scripts/e2e/share-volumes-kind.sh sets it", name)
		}
	}
	if !strings.HasPrefix(env["LEGION_E2E_KUBE_CONTEXT"], "kind-") {
		t.Fatalf("LEGION_E2E_KUBE_CONTEXT %q is no kind cluster's context (kind-<name>); the proof runs only on the cluster its script creates", env["LEGION_E2E_KUBE_CONTEXT"])
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	server := env["LEGION_E2E_SHARE_SERVER"]
	want := map[string]config.PodConfig{
		"shares":         shareLivePod(shareLiveClaim, server, "/scratch"),
		"missing-claim":  shareLivePod("absent-share", server, "/scratch"),
		"missing-export": shareLivePod(shareLiveClaim, server, "/absent"),
		"silent-server":  shareLivePod(shareLiveClaim, shareLiveSilentServer, "/scratch"),
	}
	var results []string
	for _, tc := range shareLiveCases(server) {
		path := writeBurstableLiveConfig(t, filepath.Join(env["LEGION_E2E_WORK"], tc.name), tc.project, env["LEGION_E2E_STREAM_HOST"],
			env["LEGION_E2E_IMAGE"], env["LEGION_E2E_KUBECONFIG"], env["LEGION_E2E_KUBE_CONTEXT"], tc.pod)
		cfg, err := config.LoadForValidation(path, nil)
		if err != nil {
			t.Fatalf("%s: load %s: %v", tc.name, path, err)
		}
		if got := cfg.Runtime.Kubernetes.Pod; !reflect.DeepEqual(got, want[tc.name]) {
			t.Fatalf("%s: the loader read the pod block as %+v, want %+v", tc.name, got, want[tc.name])
		}
		fmt.Printf("== %s: %s loads %d volumes, %d mounts\n", tc.name, path, len(cfg.Runtime.Kubernetes.Pod.Volumes), len(cfg.Runtime.Kubernetes.Pod.VolumeMounts))
		if tc.name != "shares" {
			results = append(results, fmt.Sprintf("%s - %s", tc.name, path))
			continue
		}
		started := time.Now()
		pod := launchBurstableLiveRoot(ctx, t, cfg, log)
		fmt.Printf("   pod %s/%s: runtimeClassName %s, node %s, PodScheduled %s\n", pod.Namespace, pod.Name,
			liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName, liveCondition(pod, corev1.PodScheduled))
		if liveString(pod.Spec.RuntimeClassName) != "gvisor" {
			t.Errorf("pod %s runs under %q, want gvisor", pod.Name, liveString(pod.Spec.RuntimeClassName))
		}
		for _, volume := range want[tc.name].Volumes {
			var got []corev1.Volume
			for _, v := range pod.Spec.Volumes {
				if v.Name == volume.Name {
					got = append(got, v)
				}
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0].VolumeSource, volume.VolumeSource) {
				t.Errorf("pod %s carries volume %s as %+v, want exactly %+v", pod.Name, volume.Name, got, volume.VolumeSource)
			}
		}
		for _, c := range pod.Spec.Containers {
			for _, mount := range want[tc.name].VolumeMounts {
				found := false
				for _, m := range c.VolumeMounts {
					found = found || reflect.DeepEqual(m, mount)
				}
				if !found {
					t.Errorf("container %s mounts %+v, want %+v among them", c.Name, c.VolumeMounts, mount)
				}
			}
			fmt.Printf("   container %s mounts team at /mnt/team (read-only) and scratch at /mnt/scratch (writable)\n", c.Name)
		}
		for _, c := range pod.Spec.InitContainers {
			for _, m := range c.VolumeMounts {
				if m.Name == "team" || m.Name == "scratch" {
					t.Errorf("init container %s mounts the operator's %s", c.Name, m.Name)
				}
			}
		}
		fmt.Printf("ok %s launched in %s\n", tc.name, time.Since(started).Round(time.Second))
		results = append(results, fmt.Sprintf("%s %s/%s %s", tc.name, pod.Namespace, pod.Name, path))
	}
	if err := os.WriteFile(env["LEGION_E2E_RESULT"], []byte(strings.Join(results, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
