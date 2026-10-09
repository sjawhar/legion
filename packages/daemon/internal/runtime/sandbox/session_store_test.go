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

// providersItems are the keys pod's providers volume projects, nil when it mounts none.
func providersItems(pod corev1.PodSpec) []corev1.KeyToPath {
	for _, volume := range pod.Volumes {
		if volume.Name == providersVolume && volume.Secret != nil {
			return volume.Secret.Items
		}
	}
	return nil
}

// Under a session database (Options.SessionDSNKey, runtime.kubernetes.session_store postgres) every
// role's Oh My Pi keeps its session in it: each generation of a workflow role and of the controller
// starts with OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE naming the providers Secret's
// key, mounted read-only at a file named for that pointer, and the shim reads the providers
// directory and, seeing the pointer, never exports the URL into Oh My Pi's environment. The image
// probe mounts the key too, so a Secret that lacks it refuses boot, and tells its Oh My Pi nothing
// of the store. Without a key no pod carries either variable or the projection.
func TestASessionDatabaseIsWhereEveryRoleKeepsItsSession(t *testing.T) {
	dsnFile := ProvidersDir + "/OMP_SESSION_SQL_DSN"
	projection := corev1.KeyToPath{Key: "SESSION_DSN", Path: "OMP_SESSION_SQL_DSN"}
	for _, store := range []string{"postgres", "pvc"} {
		t.Run(store, func(t *testing.T) {
			opts := testOptions()
			if store == "postgres" {
				opts.SessionDSNKey = "SESSION_DSN"
			}
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
				t.Run(name, func(t *testing.T) {
					worker := workerOf(t, r, spec, false)
					env := envOf(worker)
					items := providersItems(podOf(t, r, spec, false))
					if store == "pvc" {
						if _, set := env["OMP_SESSION_STORAGE"]; set {
							t.Errorf("OMP_SESSION_STORAGE = %q under pvc, want it left to the pod baseline", env["OMP_SESSION_STORAGE"])
						}
						if _, set := env["OMP_SESSION_SQL_DSN_FILE"]; set || slices.Contains(items, projection) {
							t.Errorf("a pvc pod points at or mounts a session database: %v, %v", env, items)
						}
						return
					}
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
			if got := slices.Contains(providersItems(probe), projection); got != (store == "postgres") {
				t.Errorf("the probe pod mounts the session database's key: %t, want %t", got, store == "postgres")
			}
			probeEnv := envOf(containerNamed(t, probe, probeContainer))
			if probeEnv["OMP_SESSION_STORAGE"] != "" {
				t.Errorf("the probe pod's Oh My Pi is told a session store: %v", probeEnv)
			}
			if store == "postgres" {
				// The probe's and every worker's `legion` read the mounted directory as the shim does
				// (shim.ReadProviderEnv, through each container's own environment): the URL file is
				// skipped because its pointer is there.
				for container, env := range map[string]map[string]string{"the probe": probeEnv, "a worker": envOf(workerOf(t, r, workerSpec(t), false))} {
					exported := providersExported(t, env)
					for _, pair := range exported {
						if strings.Contains(pair, "secret@") {
							t.Errorf("%s exports the session database's URL into Oh My Pi's environment: %q", container, exported)
						}
					}
				}
			}
		})
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

// A pod whose providers volume projects another session store than a pod created now would, the
// one created before the session database was configured above all, holds a move: its next
// generation would name a URL file the pod does not mount, so the relaunch replaces the pod.
func TestAPodWithoutTheSessionStoresProjectionHoldsAMove(t *testing.T) {
	before, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.SessionDSNKey = "SESSION_DSN"
	after, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	pod := func(r *Runtime) *corev1.Pod { return &corev1.Pod{Spec: podOf(t, r, workerSpec(t), false)} }
	for _, tc := range []struct {
		name  string
		r     *Runtime
		pod   *corev1.Pod
		moved bool
	}{
		{"a file-store pod under a session database", after, pod(before), true},
		{"a session-database pod under the file store", before, pod(after), true},
		{"a session-database pod under the same database", after, pod(after), false},
		{"a file-store pod under the file store", before, pod(before), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moved := tc.r.movedInPod(tc.pod, workerSpec(t).Role)
			if got := slices.ContainsFunc(moved, func(m movedAddress) bool { return m.where == "the session database URL key" }); got != tc.moved {
				t.Errorf("movedInPod = %v, want the session store moved %t", moved, tc.moved)
			}
		})
	}
}

// A resume under a session database holds the volume to nothing: its session is in the database,
// so the issue pod's workspace-init does not expect the tree volume to hold anything (a resume on a
// new volume provisions the workspace from the issue's pushed branch and continues the session),
// and the controller's workspace-init looks for no session file. The resume itself still names the
// session, which the role launcher finds in the table (internal/launcher) and Oh My Pi resumes.
// Sessions kept as files on the volume hold both as before.
func TestAResumeUnderASessionDatabaseExpectsNothingOfTheVolume(t *testing.T) {
	for _, tc := range []struct {
		store  string
		key    string
		expect bool
	}{
		{"postgres", "SESSION_DSN", false},
		{"pvc", "", true},
	} {
		t.Run(tc.store, func(t *testing.T) {
			opts := testOptions()
			opts.SessionDSNKey = tc.key
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			worker := workerSpec(t)
			worker.Generation, worker.BootToken, worker.ResumeSessionFile = 2, "boot-g2", resumeSession
			init := envOf(containerNamed(t, podOf(t, r, worker, false), initContainer))
			if _, set := init["LEGION_EXPECT_TREE_VOLUME"]; set != tc.expect {
				t.Errorf("the issue pod's workspace-init expects the tree volume: %t, want %t", set, tc.expect)
			}
			controller := controllerSpec(t)
			controller.Generation, controller.BootToken, controller.ResumeSessionFile = 2, "boot-g2", controllerSession
			controllerInit := envOf(containerNamed(t, podOf(t, r, controller, false), initContainer))
			if _, set := controllerInit["LEGION_RESUME_SESSION_FILE"]; set != tc.expect {
				t.Errorf("the controller's workspace-init looks for its session file: %t, want %t", set, tc.expect)
			}
			for _, spec := range []runtime.SpawnSpec{worker, controller} {
				l, err := r.prepare(spec)
				if err != nil {
					t.Fatal(err)
				}
				if start := launcherCommand(l, r); start.ResumeFile != spec.ResumeSessionFile || !slices.Contains(start.Argv, "--resume="+spec.ResumeSessionFile) {
					t.Errorf("%s starts resuming %q with %q, want its session %s", spec.Claim, start.ResumeFile, start.Argv, spec.ResumeSessionFile)
				}
			}
		})
	}
}

// A new issue pod's workspace-init expects the tree volume when another claim of its tree kept a
// session there; under a session database no claim keeps one there, and the tree's sessions are
// not even read.
func TestANewPodUnderASessionDatabaseIsNotHeldToItsTreesSessions(t *testing.T) {
	for _, tc := range []struct {
		store  string
		key    string
		expect bool
	}{
		{"postgres", "SESSION_DSN", false},
		{"pvc", "", true},
	} {
		t.Run(tc.store, func(t *testing.T) {
			store := newTreeStore()
			store.sessions[testTree] = true
			g := newRig(t, nil, withOptions(func(o *Options) { o.Store, o.SessionDSNKey = store, tc.key }))
			g.spawn(rootSpec(t))
			pod := g.pod(SandboxName(rootToken))
			if pod == nil {
				t.Fatal("the root's issue pod was not created")
			}
			init := envOf(containerNamed(t, pod.Spec, initContainer))
			if _, set := init["LEGION_EXPECT_TREE_VOLUME"]; set != tc.expect {
				t.Errorf("the new pod's workspace-init expects the tree volume: %t, want %t", set, tc.expect)
			}
		})
	}
}
