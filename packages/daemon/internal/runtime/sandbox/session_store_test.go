package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
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

// A role's shim and the image probe are told the providers directory (--provider-env-dir) exactly
// where their pod mounts the providers Secret there, which it does when something needs it
// (mountsProviders): a provider key, a providers secret, or the postgres store's URL key. Each
// refuses a directory it is told and cannot read (shim.ReadProviderEnv), and the probe's refusal
// fails the daemon's boot. Under cloudsql-iam no URL key is held, so a deployment with no provider
// key and no providers secret mounts no providers volume, and neither is told the directory.
func TestTheProvidersDirectoryIsToldWhereThePodMountsIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		store   ompsessions.Store
		keys    map[string]string
		mounted bool
	}{
		{"cloudsql-iam with no provider key", cloudSQLStore, nil, false},
		{"cloudsql-iam with a provider key", cloudSQLStore, map[string]string{"ANTHROPIC_API_KEY": "anthropic"}, true},
		{"postgres with no provider key", postgresStore, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.SessionStore, opts.ProviderKeys = tc.store, tc.keys
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l := launchOf(t, r, workerSpec(t))
			argv := launcherCommand(l, r).Argv
			probe := r.probeManifest(probeSandboxName, ImageProbe{Contract: 5, RoleReferences: testRoleReferences}, time.Time{}).Spec.PodTemplate.Spec
			for _, site := range []struct {
				name    string
				pod     corev1.PodSpec
				command []string
			}{
				{"a role's shim", r.podTemplate(l).Spec, argv[:slices.Index(argv, "--")]},
				{"the image probe", probe, containerNamed(t, probe, probeContainer).Command},
			} {
				t.Run(site.name, func(t *testing.T) {
					if got := containsSequence(site.command, []string{"--provider-env-dir", ProvidersDir}); got != tc.mounted {
						t.Errorf("told --provider-env-dir %s: %t (%q), want %t", ProvidersDir, got, site.command, tc.mounted)
					}
					volume := slices.ContainsFunc(site.pod.Volumes, func(v corev1.Volume) bool { return v.Name == providersVolume })
					var mounting []string
					for _, c := range slices.Concat(site.pod.InitContainers, site.pod.Containers) {
						if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == providersVolume || m.MountPath == ProvidersDir }) {
							mounting = append(mounting, c.Name)
						}
					}
					if volume != tc.mounted || (len(mounting) > 0) != tc.mounted {
						t.Errorf("the pod holds the providers volume: %t, mounted by %q; want %t", volume, mounting, tc.mounted)
					}
				})
			}
		})
	}
}

// A Claude Code role is told the session store's two variables as an Oh My Pi role of the pod is:
// the runtime tells every role pod both (mainEnvironment), whichever harness it runs. The URL
// file's pointer is what keeps the database's credentials out of Claude Code: under postgres the
// providers volume mounts the database's URL for the whole pod, and the shim exports a providers
// file into the agent's environment unless its `_FILE` pointer is set, so without the pointer the
// URL, password and all, would reach Claude Code and every command it runs.
func TestAClaudeWorkerIsToldTheSessionStoreAsAnOhMyPiWorkerIs(t *testing.T) {
	for _, store := range []ompsessions.Store{postgresStore, cloudSQLStore} {
		t.Run(string(store.Kind), func(t *testing.T) {
			opts := testOptions()
			opts.SessionStore = store
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			claude := envOf(workerOf(t, r, claudeWorkerSpec(t)))
			omp := envOf(workerOf(t, r, workerSpec(t)))
			for _, name := range sessionStoreVariables {
				if claude[name] == "" || claude[name] != omp[name] {
					t.Errorf("a Claude worker is told %s=%q, want %q as the Oh My Pi worker is", name, claude[name], omp[name])
				}
			}
			if store.Kind != ompsessions.StorePostgres {
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

// A Claude Code generation's start command names the session its launcher keeps in the session
// table, LEGION_CLAUDE_SESSION_ID, beside both of Oh My Pi's session variables, which are what the
// launcher reaches the table through: the fresh session its agent command starts (`--session-id`),
// or the recorded transcript's id when it resumes. An Oh My Pi generation's start command names no
// such session, so its launcher keeps nothing beside Oh My Pi. The id is the generation's, never
// the pod's: no container of the pod carries it, so a pod the next generation reuses is unchanged.
func TestAClaudeStartCommandNamesTheSessionItsLauncherKeeps(t *testing.T) {
	for _, store := range []ompsessions.Store{postgresStore, cloudSQLStore} {
		t.Run(string(store.Kind), func(t *testing.T) {
			opts := testOptions()
			opts.SessionStore = store
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			startEnv := func(spec runtime.SpawnSpec) (map[string]string, launch) {
				l := launchOf(t, r, spec)
				env := map[string]string{}
				for _, entry := range launcherCommand(l, r).Env {
					name, value, _ := strings.Cut(entry, "=")
					env[name] = value
				}
				return env, l
			}
			fresh, l := startEnv(claudeWorkerSpec(t))
			argv := l.agentArgv(r.agent)
			at := slices.Index(argv, "--session-id")
			if at < 0 || at+1 == len(argv) || fresh[claudelaunch.SessionVariable] != argv[at+1] {
				t.Errorf("a fresh Claude start command names %s=%q, want the session its agent command starts in %q",
					claudelaunch.SessionVariable, fresh[claudelaunch.SessionVariable], argv)
			}
			resumeSpec := claudeResumeSpec(t)
			resumed, _ := startEnv(resumeSpec)
			if resumed[claudelaunch.SessionVariable] != claudeTranscriptID {
				t.Errorf("a resuming Claude start command names %s=%q, want the recorded transcript's %s",
					claudelaunch.SessionVariable, resumed[claudelaunch.SessionVariable], claudeTranscriptID)
			}
			// The Oh My Pi generation each is compared with is the same generation of the same role.
			ompOf := func(spec runtime.SpawnSpec) map[string]string {
				spec.Backend, spec.ResumeSessionFile = "", ""
				env, _ := startEnv(spec)
				return env
			}
			for name, tc := range map[string]struct{ claude, omp map[string]string }{
				"a fresh Claude":    {fresh, ompOf(claudeWorkerSpec(t))},
				"a resuming Claude": {resumed, ompOf(resumeSpec)},
			} {
				for _, variable := range sessionStoreVariables {
					if tc.claude[variable] == "" || tc.claude[variable] != tc.omp[variable] {
						t.Errorf("%s start command names %s=%q, want %q as the Oh My Pi one does", name, variable, tc.claude[variable], tc.omp[variable])
					}
				}
				if value, named := tc.omp[claudelaunch.SessionVariable]; named {
					t.Errorf("an Oh My Pi start command names %s=%q, want no Claude session", claudelaunch.SessionVariable, value)
				}
			}
			pod := r.podTemplate(l).Spec
			for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
				for _, v := range c.Env {
					if v.Name == claudelaunch.SessionVariable {
						t.Errorf("the pod's container %s carries %s=%q, want it in the start command alone", c.Name, v.Name, v.Value)
					}
				}
			}
		})
	}
}
