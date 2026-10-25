package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
					worker := workerOf(t, r, spec)
					env := envOf(worker)
					items := providersItems(podOf(t, r, spec))
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
	pod := func(r *Runtime) *corev1.Pod { return &corev1.Pod{Spec: podOf(t, r, workerSpec(t))} }
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
// so the issue pod's workspace-init does not expect the issue's volume to hold anything (a resume
// on a new volume provisions the workspace from the issue's pushed branch and continues the
// session), and the controller's workspace-init looks for no session file. The resume itself still
// names the session, which the role launcher finds in the table (internal/launcher) and Oh My Pi
// resumes. Sessions kept as files on the volume hold both as before.
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
// session there; under a session database an Oh My Pi session is kept in the table, so one
// recorded holds the volume to nothing.
func TestANewPodUnderASessionDatabaseIsNotHeldToItsIssuesOhMyPiSessions(t *testing.T) {
	for _, tc := range []struct {
		store  string
		key    string
		expect bool
	}{
		{"postgres", "SESSION_DSN", false},
		{"pvc", "", true},
	} {
		t.Run(tc.store, func(t *testing.T) {
			store := newFakeStore()
			store.sessions[rootSpec(t).Issue] = []string{resumeSession}
			g := newRig(t, nil, withOptions(func(o *Options) { o.Store, o.SessionDSNKey = store, tc.key }))
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

// A Claude Code role is never told OMP_SESSION_STORAGE, whatever the store: its session is a
// Claude Code transcript on the issue's volume, which that database holds no session of, and its
// role launcher stats the file it is asked to resume only while OMP_SESSION_STORAGE is not sql
// (internal/launcher's sessionWritten), so withholding it is what sends the launcher onto the
// transcript. Under a session database it is told OMP_SESSION_SQL_DSN_FILE all the same, as an Oh
// My Pi role of the pod is: the providers volume mounts the database's URL for the whole pod, and
// the shim exports a providers file into the agent's environment unless its `_FILE` pointer is
// set, so without the pointer the URL, password and all, would reach Claude Code and every
// command it runs. An Oh My Pi role of the same deployment is told both under a session database
// and neither without one.
func TestAClaudeWorkerIsToldTheSessionDatabasesURLFileAndNotItsStore(t *testing.T) {
	for _, tc := range []struct {
		store string
		key   string
	}{
		{"postgres", "SESSION_DSN"},
		{"pvc", ""},
	} {
		t.Run(tc.store, func(t *testing.T) {
			opts := testOptions()
			opts.SessionDSNKey = tc.key
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			claude := envOf(workerOf(t, r, claudeResumeSpec(t)))
			omp := envOf(workerOf(t, r, workerSpec(t)))
			if value, set := claude["OMP_SESSION_STORAGE"]; set {
				t.Errorf("a Claude worker is told OMP_SESSION_STORAGE=%q, want it never told", value)
			}
			if got, want := claude["OMP_SESSION_SQL_DSN_FILE"], map[bool]string{true: ProvidersDir + "/OMP_SESSION_SQL_DSN"}[tc.key != ""]; got != want {
				t.Errorf("a Claude worker is told OMP_SESSION_SQL_DSN_FILE=%q, want %q", got, want)
			}
			for _, name := range sessionStoreVariables {
				if _, set := omp[name]; set != (tc.key != "") {
					t.Errorf("an Oh My Pi worker is told %s: %t, want %t", name, set, tc.key != "")
				}
			}
			if tc.key == "" {
				return
			}
			for _, pair := range providersExported(t, claude) {
				if strings.Contains(pair, "secret@") {
					t.Errorf("a Claude worker's shim exports the session database's URL into its environment: %q", pair)
				}
			}
		})
	}
}

// Under a session database an issue pod's workspace-init expects the issue's volume by what the
// issue's claims recorded, whichever role's launch makes the pod: a Claude Code transcript is on
// that volume whatever session_store says, so a volume that was lost is workspace-init's loud
// refusal rather than a fresh clone that hides the loss from the Claude claim's resume, which
// would then fail to find its transcript until its budget ran out. An Oh My Pi session is in the
// table and holds the volume to nothing. A resuming Claude claim expects its own transcript's
// volume (issuePod.prepare), and a resuming Oh My Pi one nothing of its own.
func TestAnIssuesTranscriptHoldsItsPodToTheVolumeWhicheverRoleMakesIt(t *testing.T) {
	opts := testOptions()
	opts.SessionDSNKey = "SESSION_DSN"
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	claude := envOf(containerNamed(t, podOf(t, r, claudeResumeSpec(t)), initContainer))
	if claude["LEGION_EXPECT_ISSUE_VOLUME"] != "true" {
		t.Errorf("a resuming Claude claim's workspace-init is told LEGION_EXPECT_ISSUE_VOLUME=%q, want true", claude["LEGION_EXPECT_ISSUE_VOLUME"])
	}
	ompResume := workerSpec(t)
	ompResume.Generation, ompResume.BootToken, ompResume.ResumeSessionFile = 2, "boot-g2", resumeSession
	if init := envOf(containerNamed(t, podOf(t, r, ompResume), initContainer)); init["LEGION_EXPECT_ISSUE_VOLUME"] != "" {
		t.Errorf("a resuming Oh My Pi claim's workspace-init is told LEGION_EXPECT_ISSUE_VOLUME=%q, want nothing: its session is in the table", init["LEGION_EXPECT_ISSUE_VOLUME"])
	}
	launches := map[string]func(t *testing.T, g *rig) error{
		"a new Claude role": func(t *testing.T, g *rig) error {
			g.launcher(workerToken)
			_, err := g.r.Spawn(g.ctx, claudeWorkerSpec(t))
			return err
		},
		"a new Oh My Pi role": func(t *testing.T, g *rig) error {
			g.launcher(workerToken)
			_, err := g.r.Spawn(g.ctx, workerSpec(t))
			return err
		},
		"a resuming Oh My Pi role": func(_ *testing.T, g *rig) error {
			g.launcher(workerToken)
			_, err := g.r.Resume(g.ctx, nil, ompResume)
			return err
		},
	}
	for _, recorded := range []struct {
		name   string
		files  []string
		expect bool
	}{
		{"a Claude claim's transcript", []string{resumeSession, claudeTranscript(claim.RoleTester)}, true},
		{"Oh My Pi sessions alone", []string{resumeSession}, false},
	} {
		for name, launch := range launches {
			t.Run(recorded.name+"/"+name, func(t *testing.T) {
				store := newFakeStore()
				store.sessions[workerSpec(t).Issue] = recorded.files
				g := newRig(t, nil, withOptions(func(o *Options) { o.Store, o.SessionDSNKey = store, "SESSION_DSN" }))
				if err := launch(t, g); err != nil {
					t.Fatal(err)
				}
				pod := g.pod(SandboxName(workerToken))
				if pod == nil {
					t.Fatal("the worker's issue pod was not created")
				}
				init := envOf(containerNamed(t, pod.Spec, initContainer))
				if _, set := init["LEGION_EXPECT_ISSUE_VOLUME"]; set != recorded.expect {
					t.Errorf("with %q recorded, the new pod's workspace-init expects the issue's volume: %t, want %t", recorded.files, set, recorded.expect)
				}
			})
		}
	}
}
