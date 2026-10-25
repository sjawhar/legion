package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

// The two session stores a runtime takes as a test names them: the providers Secret key the
// postgres store's URL lives under, and the Cloud SQL instance, database and IAM login the
// cloudsql-iam store signs in with.
var (
	postgresStore = ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"}
	cloudSQLStore = ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM, Database: ompsessions.Database{
		Instance: "example-project:us-central1:example-instance",
		Name:     "legion_sessions",
		User:     "legion-sessions@example-project.iam",
	}}
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

// Every role's Oh My Pi keeps its session in a database (Options.SessionStore), which configure
// requires: each generation of a workflow role and of the controller starts with
// OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE naming the URL file its store writes. Under
// postgres that file is the providers Secret's key, mounted read-only at a file named for that
// pointer, and the shim reads the providers directory and, seeing the pointer, never exports the
// URL into Oh My Pi's environment; the image probe mounts the key too, so a Secret that lacks it
// refuses boot. Under cloudsql-iam no pod and no probe mounts a session key at all: the file is the
// generation's own, written beside the socket each role container's launcher proxies to the
// instance, and that launcher is told the instance, the database and the login. The image probe
// tells its Oh My Pi nothing of the store under either.
func TestASessionDatabaseIsWhereEveryRoleKeepsItsSession(t *testing.T) {
	projection := corev1.KeyToPath{Key: "SESSION_DSN", Path: "OMP_SESSION_SQL_DSN"}
	proxyFlags := []string{
		"--session-instance", cloudSQLStore.Database.Instance,
		"--session-database", cloudSQLStore.Database.Name,
		"--session-user", cloudSQLStore.Database.User,
	}
	for _, tc := range []struct {
		store ompsessions.Store
		// dsnFile is the URL file every generation of this store is pointed at; projected is
		// whether the providers volume carries the session key.
		dsnFile   func(generation uint64) string
		projected bool
	}{
		{store: postgresStore, dsnFile: func(uint64) string { return ProvidersDir + "/OMP_SESSION_SQL_DSN" }, projected: true},
		{store: cloudSQLStore, dsnFile: func(g uint64) string { return generationDir(g) + "/OMP_SESSION_SQL_DSN" }},
	} {
		t.Run(string(tc.store.Kind), func(t *testing.T) {
			opts := testOptions()
			opts.SessionStore = tc.store
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
				t.Run(name, func(t *testing.T) {
					worker := workerOf(t, r, spec)
					env := envOf(worker)
					pod := podOf(t, r, spec)
					items := providersItems(pod)
					if got := slices.Contains(items, projection); got != tc.projected {
						t.Errorf("the providers volume projects the session key: %t (%v), want %t", got, items, tc.projected)
					}
					role := containerNamed(t, pod, string(spec.Role))
					wantFlags := tc.store.Kind == ompsessions.StoreCloudSQLIAM
					if got := containsSequence(role.Command, proxyFlags); got != wantFlags {
						t.Errorf("the role container's launcher carries the session proxy's flags: %t (%q), want %t", got, role.Command, wantFlags)
					}
					dsnFile := tc.dsnFile(spec.Generation)
					if env["OMP_SESSION_STORAGE"] != "sql" || env["OMP_SESSION_SQL_DSN_FILE"] != dsnFile {
						t.Errorf("OMP_SESSION_STORAGE = %q, OMP_SESSION_SQL_DSN_FILE = %q; want sql and %s",
							env["OMP_SESSION_STORAGE"], env["OMP_SESSION_SQL_DSN_FILE"], dsnFile)
					}
					mount := mountHolding(worker, dsnFile)
					switch tc.store.Kind {
					case ompsessions.StorePostgres:
						if mount == nil || mount.Name != providersVolume || !mount.ReadOnly {
							t.Errorf("%s is held by %+v, want the providers volume read-only", dsnFile, mount)
						}
					case ompsessions.StoreCloudSQLIAM:
						if mount == nil || mount.Name != roleVolume("private", spec.Role) || mount.ReadOnly {
							t.Errorf("%s is held by %+v, want the role's writable private volume: its launcher writes the URL there", dsnFile, mount)
						}
					}
					if !slices.Contains(worker.Command, "--provider-env-dir") && tc.store.Kind == ompsessions.StorePostgres {
						t.Errorf("the shim is not told the providers directory: %q", worker.Command)
					}
				})
			}
			probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
			if got := slices.Contains(providersItems(probe), projection); got != tc.projected {
				t.Errorf("the probe pod mounts the session database's key: %t, want %t", got, tc.projected)
			}
			probeEnv := envOf(containerNamed(t, probe, probeContainer))
			if probeEnv["OMP_SESSION_STORAGE"] != "" {
				t.Errorf("the probe pod's Oh My Pi is told a session store: %v", probeEnv)
			}
			if _, set := probeEnv["OMP_SESSION_SQL_DSN_FILE"]; set != tc.projected {
				t.Errorf("the probe pod is told a session pointer: %t (%v), want %t: it is the mounted key's alone", set, probeEnv, tc.projected)
			}
			if tc.store.Kind == ompsessions.StorePostgres {
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
		})
	}
}

// containsSequence reports whether argv carries want as a run of consecutive arguments.
func containsSequence(argv, want []string) bool {
	for i := range argv {
		if i+len(want) <= len(argv) && slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
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

// withoutSessionKey is pod with its providers volume projecting no session database URL file — the
// shape of a pod an earlier release created under the file store, which no runtime builds any more
// now that a store that keeps sessions in a database is required.
func withoutSessionKey(pod *corev1.Pod) *corev1.Pod {
	out := pod.DeepCopy()
	for i, volume := range out.Spec.Volumes {
		if volume.Name != providersVolume || volume.Secret == nil {
			continue
		}
		out.Spec.Volumes[i].Secret.Items = slices.DeleteFunc(slices.Clone(volume.Secret.Items), func(item corev1.KeyToPath) bool {
			return item.Path == ompsessions.DSNFileName
		})
	}
	return out
}

// A pod carries its whole session store for its life — the providers volume's projection under
// postgres, its launchers' proxy flags under cloudsql-iam — so a pod created under any other store
// than a pod created now would get holds a move, and the relaunch replaces it: its next generation
// would name a URL file the pod has no way to produce. A change of kind in either direction counts,
// and so does a changed key, or a changed instance, database or login under the same kind. A pod
// an earlier release created under the file store, which configure now refuses, holds one under
// either database store.
func TestAPodUnderAnotherSessionStoreHoldsAMove(t *testing.T) {
	runtimeFor := func(store ompsessions.Store) *Runtime {
		t.Helper()
		opts := testOptions()
		opts.SessionStore = store
		r, err := configure(opts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	postgres := runtimeFor(postgresStore)
	otherKey := runtimeFor(ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "OTHER_SESSION_DSN"})
	cloudSQL := runtimeFor(cloudSQLStore)
	moved := func(edit func(*ompsessions.Database)) *Runtime {
		database := cloudSQLStore.Database
		edit(&database)
		return runtimeFor(ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM, Database: database})
	}
	pod := func(r *Runtime) *corev1.Pod { return &corev1.Pod{Spec: podOf(t, r, workerSpec(t))} }
	fileStorePod := withoutSessionKey(pod(postgres))
	for name, tc := range map[string]struct {
		r     *Runtime
		pod   *corev1.Pod
		moved bool
	}{
		"a file-store pod under postgres":            {postgres, fileStorePod, true},
		"a postgres pod under another key":           {otherKey, pod(postgres), true},
		"a file-store pod under cloudsql-iam":        {cloudSQL, fileStorePod, true},
		"a postgres pod under cloudsql-iam":          {cloudSQL, pod(postgres), true},
		"a cloudsql-iam pod under postgres":          {postgres, pod(cloudSQL), true},
		"a cloudsql-iam pod under another instance":  {moved(func(d *ompsessions.Database) { d.Instance = "example-project:us-central1:other-instance" }), pod(cloudSQL), true},
		"a cloudsql-iam pod under another database":  {moved(func(d *ompsessions.Database) { d.Name = "legion_sessions_next" }), pod(cloudSQL), true},
		"a cloudsql-iam pod under another login":     {moved(func(d *ompsessions.Database) { d.User = "legion-sessions-next@example-project.iam" }), pod(cloudSQL), true},
		"a postgres pod under the same key":          {postgres, pod(postgres), false},
		"a cloudsql-iam pod under the same database": {cloudSQL, pod(cloudSQL), false},
	} {
		t.Run(name, func(t *testing.T) {
			moved := tc.r.movedInPod(tc.pod, workerSpec(t).Role)
			if got := slices.ContainsFunc(moved, func(m movedAddress) bool { return m.where == "the session store" }); got != tc.moved {
				t.Errorf("movedInPod = %v, want the session store moved %t", moved, tc.moved)
			}
		})
	}
}
