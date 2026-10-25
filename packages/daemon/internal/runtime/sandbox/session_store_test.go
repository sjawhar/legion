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

// The three session stores as a test names them: the providers Secret key the postgres store's URL
// lives under, and the Cloud SQL instance, database and IAM login the cloudsql-iam store signs in
// with.
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

// Under a session database (Options.SessionStore) every role's Oh My Pi keeps its session in it:
// each generation of a workflow role and of the controller starts with OMP_SESSION_STORAGE=sql and
// OMP_SESSION_SQL_DSN_FILE naming the URL file its store writes. Under postgres that file is the
// providers Secret's key, mounted read-only at a file named for that pointer, and the shim reads
// the providers directory and, seeing the pointer, never exports the URL into Oh My Pi's
// environment; the image probe mounts the key too, so a Secret that lacks it refuses boot. Under
// cloudsql-iam no pod and no probe mounts a session key at all: the file is the generation's own,
// written beside the socket each role container's launcher proxies to the instance, and that
// launcher is told the instance, the database and the login. Under the volume store no pod carries
// either variable, the projection, or the launcher's flags.
func TestASessionDatabaseIsWhereEveryRoleKeepsItsSession(t *testing.T) {
	projection := corev1.KeyToPath{Key: "SESSION_DSN", Path: "OMP_SESSION_SQL_DSN"}
	proxyFlags := []string{
		"--session-instance", cloudSQLStore.Database.Instance,
		"--session-database", cloudSQLStore.Database.Name,
		"--session-user", cloudSQLStore.Database.User,
	}
	for _, tc := range []struct {
		store ompsessions.Store
		// dsnFile is the URL file every generation of this store is pointed at, "" when its
		// sessions are files; projected is whether the providers volume carries the session key.
		dsnFile   func(generation uint64) string
		projected bool
	}{
		{store: ompsessions.Store{Kind: ompsessions.StoreVolume}},
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
					// The launcher's flags are the pod's one container's, an issue pod's `roles`
					// serving every workflow role or the controller's own, which is the container
					// workerOf read.
					wantFlags := tc.store.Kind == ompsessions.StoreCloudSQLIAM
					podLauncher := containerNamed(t, pod, worker.Name)
					if got := containsSequence(podLauncher.Command, proxyFlags); got != wantFlags {
						t.Errorf("the pod's launcher carries the session proxy's flags: %t (%q), want %t", got, podLauncher.Command, wantFlags)
					}
					if tc.dsnFile == nil {
						if _, set := env["OMP_SESSION_STORAGE"]; set {
							t.Errorf("OMP_SESSION_STORAGE = %q under pvc, want it left to the pod baseline", env["OMP_SESSION_STORAGE"])
						}
						if _, set := env["OMP_SESSION_SQL_DSN_FILE"]; set {
							t.Errorf("a pvc pod points at a session database: %v", env)
						}
						return
					}
					dsnFile := tc.dsnFile(spec.Generation)
					if env["OMP_SESSION_STORAGE"] != "sql" || env["OMP_SESSION_SQL_DSN_FILE"] != dsnFile {
						t.Errorf("OMP_SESSION_STORAGE = %q, OMP_SESSION_SQL_DSN_FILE = %q; want sql and %s",
							env["OMP_SESSION_STORAGE"], env["OMP_SESSION_SQL_DSN_FILE"], dsnFile)
					}
					// The agent reads the URL at the path its own namespaces show it; the
					// container holds the volume behind it at the fixed path in the controller's
					// pod and under the role's own directory in an issue pod, which role-init
					// binds back to the fixed path.
					held := dsnFile
					if worker.Name == runtime.IssuePodContainer {
						held = strings.Replace(dsnFile, LauncherPrivateDir, roleDir(spec.Role)+"/private", 1)
					}
					switch tc.store.Kind {
					case ompsessions.StorePostgres:
						mount := mountHolding(worker, dsnFile)
						if mount == nil || mount.Name != providersVolume || !mount.ReadOnly {
							t.Errorf("%s is held by %+v, want the providers volume read-only", dsnFile, mount)
						}
					case ompsessions.StoreCloudSQLIAM:
						mount := mountHolding(podLauncher, held)
						if mount == nil || mount.Name != roleVolume("private", spec.Role) || mount.ReadOnly {
							t.Errorf("%s is held by %+v, want the role's writable private volume: its launcher writes the URL there", held, mount)
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

// A pod carries its whole session store for its life — the providers volume's projection under
// postgres, its launchers' proxy flags under cloudsql-iam — so a pod created under any other store
// than a pod created now would get holds a move, and the relaunch replaces it: its next generation
// would name a URL file the pod has no way to produce. A change of kind in either direction counts,
// and so does a changed instance, database or login under the same kind.
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
	volume := runtimeFor(ompsessions.Store{Kind: ompsessions.StoreVolume})
	postgres := runtimeFor(postgresStore)
	otherKey := runtimeFor(ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "OTHER_SESSION_DSN"})
	cloudSQL := runtimeFor(cloudSQLStore)
	moved := func(edit func(*ompsessions.Database)) *Runtime {
		database := cloudSQLStore.Database
		edit(&database)
		return runtimeFor(ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM, Database: database})
	}
	pod := func(r *Runtime) *corev1.Pod { return &corev1.Pod{Spec: podOf(t, r, workerSpec(t))} }
	for name, tc := range map[string]struct {
		r     *Runtime
		pod   *corev1.Pod
		moved bool
	}{
		"a file-store pod under postgres":            {postgres, pod(volume), true},
		"a postgres pod under the file store":        {volume, pod(postgres), true},
		"a postgres pod under another key":           {otherKey, pod(postgres), true},
		"a file-store pod under cloudsql-iam":        {cloudSQL, pod(volume), true},
		"a cloudsql-iam pod under the file store":    {volume, pod(cloudSQL), true},
		"a postgres pod under cloudsql-iam":          {cloudSQL, pod(postgres), true},
		"a cloudsql-iam pod under postgres":          {postgres, pod(cloudSQL), true},
		"a cloudsql-iam pod under another instance":  {moved(func(d *ompsessions.Database) { d.Instance = "example-project:us-central1:other-instance" }), pod(cloudSQL), true},
		"a cloudsql-iam pod under another database":  {moved(func(d *ompsessions.Database) { d.Name = "legion_sessions_next" }), pod(cloudSQL), true},
		"a cloudsql-iam pod under another login":     {moved(func(d *ompsessions.Database) { d.User = "legion-sessions-next@example-project.iam" }), pod(cloudSQL), true},
		"a postgres pod under the same key":          {postgres, pod(postgres), false},
		"a cloudsql-iam pod under the same database": {cloudSQL, pod(cloudSQL), false},
		"a file-store pod under the file store":      {volume, pod(volume), false},
	} {
		t.Run(name, func(t *testing.T) {
			moved := tc.r.movedInPod(tc.pod, workerSpec(t).Role)
			if got := slices.ContainsFunc(moved, func(m movedAddress) bool { return m.where == "the session store" }); got != tc.moved {
				t.Errorf("movedInPod = %v, want the session store moved %t", moved, tc.moved)
			}
		})
	}
}

// A resume under either session database holds the volume to nothing: its session is in the
// database, so the issue pod's workspace-init does not expect the issue's volume to hold anything
// (a resume on a new volume provisions the workspace from the issue's pushed branch and continues
// the session), and the controller's workspace-init looks for no session file. The resume itself
// still names the session, which the role launcher finds in the table (internal/launcher) and Oh My
// Pi resumes. Sessions kept as files on the volume hold both as before.
func TestAResumeUnderASessionDatabaseExpectsNothingOfTheVolume(t *testing.T) {
	for _, tc := range []struct {
		store  ompsessions.Store
		expect bool
	}{
		{postgresStore, false},
		{cloudSQLStore, false},
		{ompsessions.Store{Kind: ompsessions.StoreVolume}, true},
	} {
		t.Run(string(tc.store.Kind), func(t *testing.T) {
			opts := testOptions()
			opts.SessionStore = tc.store
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			worker := workerSpec(t)
			worker.Generation, worker.BootToken, worker.ResumeSessionFile = 2, "boot-g2", resumeSession
			init := envOf(containerNamed(t, podOf(t, r, worker), initContainer))
			if _, set := init["LEGION_EXPECT_ISSUE_VOLUME"]; set != tc.expect {
				t.Errorf("the issue pod's workspace-init expects the issue's volume: %t, want %t", set, tc.expect)
			}
			controller := controllerSpec(t)
			controller.Generation, controller.BootToken, controller.ResumeSessionFile = 2, "boot-g2", controllerSession
			controllerInit := envOf(containerNamed(t, podOf(t, r, controller), initContainer))
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

// A new issue pod's workspace-init expects the issue's volume when a claim of the issue kept a
// session there; under a session database no claim keeps one there, and the issue's sessions are
// not even read.
func TestANewPodUnderASessionDatabaseIsNotHeldToItsIssuesSessions(t *testing.T) {
	for _, tc := range []struct {
		sessions ompsessions.Store
		expect   bool
	}{
		{postgresStore, false},
		{cloudSQLStore, false},
		{ompsessions.Store{Kind: ompsessions.StoreVolume}, true},
	} {
		t.Run(string(tc.sessions.Kind), func(t *testing.T) {
			store := newFakeStore()
			store.sessions[rootSpec(t).Issue] = true
			g := newRig(t, nil, withOptions(func(o *Options) { o.Store, o.SessionStore = store, tc.sessions }))
			g.spawn(rootSpec(t))
			pod := g.pod(SandboxName(rootToken))
			if pod == nil {
				t.Fatal("the root's issue pod was not created")
			}
			init := envOf(containerNamed(t, pod.Spec, initContainer))
			if _, set := init["LEGION_EXPECT_ISSUE_VOLUME"]; set != tc.expect {
				t.Errorf("the new pod's workspace-init expects the issue's volume: %t, want %t", set, tc.expect)
			}
		})
	}
}
