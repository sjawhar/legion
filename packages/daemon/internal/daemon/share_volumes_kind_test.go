package daemon

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// The share volumes live proof (scripts/e2e/share-volumes-kind.sh, whose driver is
// share_volumes_live_test.go, built with -tags e2e) writes its legion.yaml files with the functions
// here, through writeBurstableLiveConfig, and builds its Sandbox runtime with burstableLiveRuntime.
// They are untagged so `go test` holds them to the loader's and the runtime's current contracts on
// every run, as the Burstable proof's are.

// shareLiveClaim is the PersistentVolumeClaim the proof's script makes in the legion namespace,
// bound to a PersistentVolume of its NFS server's `/team` export.
const shareLiveClaim = "team-share"

// shareLiveCases are the proof's four deployments, each one `runtime.kubernetes.pod` block over
// the script's NFS server at server: `shares`, whose issue pod the driver launches, mounting the
// operator's claim read-only at /mnt/team and the server's `/scratch` export writable at
// /mnt/scratch; and three the script runs `legion probe-boot` on, each with one share no pod can
// mount — a claim that does not exist, an export the server does not have, and an export of
// shareLiveSilentServer, an address no server answers.
func shareLiveCases(server string) []struct{ name, project, pod string } {
	block := func(claimName, server, exportPath string) string {
		return fmt.Sprintf(`    pod:
      volumes:
        - name: team
          persistent_volume_claim: {name: %s}
        - name: scratch
          nfs: {server: %s, path: %s}
      volume_mounts:
        - {volume: team, mount_path: /mnt/team}
        - {volume: scratch, mount_path: /mnt/scratch, read_only: false}
`, claimName, server, exportPath)
	}
	return []struct{ name, project, pod string }{
		{"shares", "SHARE", block(shareLiveClaim, server, "/scratch")},
		{"missing-claim", "NOCLAIM", block("absent-share", server, "/scratch")},
		{"missing-export", "NOEXPORT", block(shareLiveClaim, server, "/absent")},
		{"silent-server", "SILENT", block(shareLiveClaim, shareLiveSilentServer, "/scratch")},
	}
}

// shareLiveSilentServer is an address of RFC 5737's documentation range, which no host answers,
// so the node's mount.nfs times out on it as on a server whose packets are dropped.
const shareLiveSilentServer = "192.0.2.10"

// shareLivePod is the pod block each case's file must load to: the claim and the export as the
// case names them, the claim mounted read-only — its source read-only too, since no mount writes
// it — and the export writable.
func shareLivePod(claimName, server, exportPath string) config.PodConfig {
	return config.PodConfig{
		Volumes: []corev1.Volume{
			{Name: "team", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName, ReadOnly: true}}},
			{Name: "scratch", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: server, Path: exportPath}}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "team", MountPath: "/mnt/team", ReadOnly: true},
			{Name: "scratch", MountPath: "/mnt/scratch"},
		},
	}
}

// Each of the share proof's legion.yaml files passes the daemon's loader into the pod block the
// proof expects, and builds its runtime through sandboxOptions and sandbox.New against an API
// server standing in for the kind cluster; the launch passes the spawn-spec checks. Every step is
// the one the driver takes before it reaches the cluster.
func TestTheShareKindProofBuildsItsRuntimesFromItsConfigurations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(newIssueSandboxAPI())
	t.Cleanup(func() { cancel(); server.Close() })
	kubeconfig := writeKubeconfig(t, server.URL, "")
	want := map[string]config.PodConfig{
		"shares":         shareLivePod(shareLiveClaim, "10.96.0.20", "/scratch"),
		"missing-claim":  shareLivePod("absent-share", "10.96.0.20", "/scratch"),
		"missing-export": shareLivePod(shareLiveClaim, "10.96.0.20", "/absent"),
		"silent-server":  shareLivePod(shareLiveClaim, shareLiveSilentServer, "/scratch"),
	}
	for _, tc := range shareLiveCases("10.96.0.20") {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeBurstableLiveConfig(t, filepath.Join(dir, tc.name), tc.project, "192.0.2.10",
				"ghcr.io/sjawhar/legion-worker@sha256:"+strings.Repeat("a", 64), kubeconfig, "test", tc.pod)
			cfg, err := config.LoadForValidation(path, nil)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if got := cfg.Runtime.Kubernetes.Pod; !reflect.DeepEqual(got, want[tc.name]) {
				t.Fatalf("the file's pod block = %+v, want %+v", got, want[tc.name])
			}
			rc, err := kubeClient(*cfg.Runtime.Kubernetes)
			if err != nil {
				t.Fatal(err)
			}
			refuse := func(string) (claim.Token, uint64, bool, bool, error) { return "", 0, false, false, nil }
			listener, err := stream.Listen(ctx, "tcp://127.0.0.1:0", refuse, stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: quietLogger()})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := burstableLiveRuntime(ctx, cfg, rc, listener, quietLogger()); err != nil {
				t.Fatalf("the proof's runtime: %v", err)
			}
			project, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			issue := cfg.Project + "-1"
			token, err := claim.NewToken(project, issue, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.ValidateSpawnSpec(burstableLiveSpec(project, issue, token, "boot-token", filepath.Join(dir, "role.md")), nil); err != nil {
				t.Fatalf("the proof's root launch: %v", err)
			}
		})
	}
}
