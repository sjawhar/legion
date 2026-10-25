package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

// providersItems are the keys pod's providers volume projects; every pod Legion runs mounts that
// volume, since the session database's URL key is required.
func providersItems(pod corev1.PodSpec) []corev1.KeyToPath {
	for _, volume := range pod.Volumes {
		if volume.Name == providersVolume && volume.Secret != nil {
			return volume.Secret.Items
		}
	}
	return nil
}

// Every role's Oh My Pi keeps its session in the database Options.SessionDSNKey names
// (runtime.kubernetes.session_store postgres), the only store this runtime has: each generation of
// a workflow role and of the controller starts with OMP_SESSION_STORAGE=sql and
// OMP_SESSION_SQL_DSN_FILE naming the providers Secret's key, mounted read-only at a file named
// for that pointer, and the shim reads the providers directory and, seeing the pointer, never
// exports the URL into Oh My Pi's environment. The image probe mounts the key too, so a Secret
// that lacks it refuses boot, and tells its Oh My Pi nothing of the store.
func TestASessionDatabaseIsWhereEveryRoleKeepsItsSession(t *testing.T) {
	dsnFile := ProvidersDir + "/OMP_SESSION_SQL_DSN"
	projection := corev1.KeyToPath{Key: testSessionDSNKey, Path: "OMP_SESSION_SQL_DSN"}
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			worker := workerOf(t, r, spec)
			env := envOf(worker)
			items := providersItems(podOf(t, r, spec))
			if env["OMP_SESSION_STORAGE"] != "sql" || env["OMP_SESSION_SQL_DSN_FILE"] != dsnFile {
				t.Errorf("OMP_SESSION_STORAGE = %q, OMP_SESSION_SQL_DSN_FILE = %q; want sql and %s",
					env["OMP_SESSION_STORAGE"], env["OMP_SESSION_SQL_DSN_FILE"], dsnFile)
			}
			if !slices.Contains(items, projection) {
				t.Errorf("the providers volume projects %v, want %v among them", items, projection)
			}
			if mount := mountHolding(worker, dsnFile); mount == nil || mount.Name != providersVolume || !mount.ReadOnly {
				t.Errorf("%s is held by %+v, want the providers volume read-only", dsnFile, mount)
			}
			if !slices.Contains(worker.Command, "--provider-env-dir") {
				t.Errorf("the shim is not told the providers directory: %q", worker.Command)
			}
		})
	}
	probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
	if !slices.Contains(providersItems(probe), projection) {
		t.Errorf("the probe pod projects %v, want the session database's key %v among them", providersItems(probe), projection)
	}
	probeEnv := envOf(containerNamed(t, probe, probeContainer))
	if probeEnv["OMP_SESSION_STORAGE"] != "" {
		t.Errorf("the probe pod's Oh My Pi is told a session store: %v", probeEnv)
	}
	// The probe's and every worker's `legion` read the mounted directory as the shim does
	// (shim.ReadProviderEnv, through each container's own environment): the URL file is
	// skipped because its pointer is there.
	for container, env := range map[string]map[string]string{"the probe": probeEnv, "a worker": envOf(workerOf(t, r, workerSpec(t)))} {
		exported := providersExported(t, env)
		for _, pair := range exported {
			if strings.Contains(pair, "secret@") {
				t.Errorf("%s exports the session database's URL into Oh My Pi's environment: %q", container, exported)
			}
		}
	}
}

// providersExported is what shim.ReadProviderEnv exports from a providers directory holding the
// session database's URL file, in a container whose environment is env.
func providersExported(t *testing.T, env map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "OMP_SESSION_SQL_DSN"), []byte("postgres://legion:secret@db.internal.example/sessions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exported, err := shim.ReadProviderEnv(dir, func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return exported
}

// withSessionKey is pod with its providers volume projecting key as the session database's URL
// file, or, with key "", projecting none at all — the shape of a pod created before the database
// was configured, which no runtime builds any more now that the key is required.
func withSessionKey(pod *corev1.Pod, key string) *corev1.Pod {
	out := pod.DeepCopy()
	for i, volume := range out.Spec.Volumes {
		if volume.Name != providersVolume || volume.Secret == nil {
			continue
		}
		items := slices.DeleteFunc(slices.Clone(volume.Secret.Items), func(item corev1.KeyToPath) bool {
			return item.Path == sessionDSNFile
		})
		if key != "" {
			items = append(items, corev1.KeyToPath{Key: key, Path: sessionDSNFile})
		}
		out.Spec.Volumes[i].Secret.Items = items
	}
	return out
}

// A pod whose providers volume projects another session store than a pod created now would, the
// one created before the session database was configured above all, holds a move: its next
// generation would name a URL file the pod does not mount, so the relaunch replaces the pod.
func TestAPodWithoutTheSessionStoresProjectionHoldsAMove(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	now := &corev1.Pod{Spec: podOf(t, r, workerSpec(t))}
	for _, tc := range []struct {
		name  string
		pod   *corev1.Pod
		moved bool
	}{
		{"a pod created before the session database was configured", withSessionKey(now, ""), true},
		{"a pod projecting another key", withSessionKey(now, "OTHER_SESSION_DSN"), true},
		{"a pod projecting the key a pod created now does", now, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moved := r.movedInPod(tc.pod, workerSpec(t).Role)
			if got := slices.ContainsFunc(moved, func(m movedAddress) bool { return m.where == "the session database URL key" }); got != tc.moved {
				t.Errorf("movedInPod = %v, want the session store moved %t", moved, tc.moved)
			}
		})
	}
}
