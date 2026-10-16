package capabilities

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities/capabilitiestest"
)

// fakeOmp answers the two Oh My Pi commands the checks run as this pod's omp answers them, by
// the variables a test sets in the Image's Env: Python unavailable (exit 1, as omp exits) under
// LEGION_TEST_NO_PYTHON, model fallback on under LEGION_TEST_FALLBACK_ON.
const fakeOmp = `#!/bin/sh
case "$*" in
"setup python --check --json")
  if [ -n "${LEGION_TEST_NO_PYTHON:-}" ]; then echo '{"available": false, "usingManagedEnv": false, "managedEnvPath": ""}'; exit 1; fi
  echo '{"available": true}' ;;
"config get retry.modelFallback --json")
  if [ -n "${LEGION_TEST_FALLBACK_ON:-}" ]; then v=true; else v=false; fi
  printf '{"key":"retry.modelFallback","value":%s,"type":"boolean"}\n' "$v" ;;
*) echo "fake omp: unexpected argv: $*" >&2; exit 64 ;;
esac
`

// stubs is a worker image in miniature: a bin directory first on PATH with a stub for every
// binary the image rows look for, a HOME whose legion profile's plugin lock enables the CodeGraph
// plugin, and an omp that answers the checks' commands.
type stubs struct {
	t              *testing.T
	bin, home, omp string
}

func newStubs(t *testing.T) stubs {
	t.Helper()
	s := stubs{t: t, bin: t.TempDir(), home: t.TempDir()}
	capabilitiestest.InstallBinaries(t, s.bin)
	s.omp = filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(s.omp, []byte(fakeOmp), 0o755); err != nil {
		t.Fatal(err)
	}
	s.writeLock(capabilitiestest.CodeGraphLock)
	return s
}

// install writes an executable script into bin.
func (s stubs) install(name, script string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte(script), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s stubs) remove(name string) {
	s.t.Helper()
	if err := os.Remove(filepath.Join(s.bin, name)); err != nil {
		s.t.Fatal(err)
	}
}

// lock is the legion profile's plugin lock.
func (s stubs) lock() string {
	return capabilitiestest.LockPath(s.home)
}

func (s stubs) writeLock(content string) {
	capabilitiestest.WriteLock(s.t, s.home, content)
}

// image is the Image the checks read: the stubs' PATH alone, never this process's, and the
// variables extra adds.
func (s stubs) image(extra ...string) Image {
	return Image{
		Launch:  s.omp,
		Env:     append([]string{"PATH=" + s.bin, "HOME=" + s.home, "OMP_PROFILE=legion"}, extra...),
		WorkDir: s.t.TempDir(),
	}
}

// byName is the states by capability.
func byName(states []State) map[Name]State {
	m := map[Name]State{}
	for _, state := range states {
		m[state.Name] = state
	}
	return m
}

// Under an image that carries everything, every row is rendered in Table's order: the image rows
// present with their evidence — codegraph among them, the CLI on PATH and the plugin enabled in the
// profile's lock, since a pod's launch now loads that plugin — the others as what they are, and no
// error.
func TestCheckImageRendersEveryRowWhenEveryImageRowIsPresent(t *testing.T) {
	s := newStubs(t)

	states, err := CheckImage(context.Background(), s.image())

	if err != nil {
		t.Fatalf("CheckImage = %v, want every image row present", err)
	}
	if len(states) != len(Table) {
		t.Fatalf("CheckImage rendered %d states, want one per row (%d)", len(states), len(Table))
	}
	for i, row := range Table {
		state := states[i]
		if state.Name != row.Name {
			t.Errorf("state %d is %s, want %s: the table's order", i, state.Name, row.Name)
		}
		want := map[Site]string{SiteImage: StatusPresent, SiteLive: StatusReported, SiteDeployment: StatusReported, SiteWithheld: StatusWithheld}[row.Site]
		if state.Status != want {
			t.Errorf("%s: status %q, want %q", state.Name, state.Status, want)
		}
		if row.Site == SiteWithheld && !strings.HasPrefix(state.Detail, row.Ruling+": ") {
			t.Errorf("%s: detail %q, want the ruling %s first", state.Name, state.Detail, row.Ruling)
		}
		if row.Site == SiteLive && state.Detail != "each session reports it at start; `legion state` renders it: "+row.Summary {
			t.Errorf("%s: detail %q, want it to say each session reports the row, then the summary", state.Name, state.Detail)
		}
	}
	got := byName(states)
	for name, want := range map[Name]string{
		EvalPython:           "omp setup python --check reports available=true",
		Browser:              filepath.Join(s.bin, "chromium") + " (chromium on PATH) --version: Chromium 141.0.0.0",
		LSP:                  "on PATH: " + filepath.Join(s.bin, "gopls") + ", " + filepath.Join(s.bin, "typescript-language-server") + ", " + filepath.Join(s.bin, "pyright-langserver"),
		CodeGraph:            filepath.Join(s.bin, "codegraph") + " on PATH; @bopstack/pi-codegraph enabled in " + s.lock(),
		Toolchain:            "on PATH: " + filepath.Join(s.bin, "go") + ", " + filepath.Join(s.bin, "curl") + ", " + filepath.Join(s.bin, "wget") + ", " + filepath.Join(s.bin, "python3") + ", " + filepath.Join(s.bin, "node") + ", " + filepath.Join(s.bin, "bun") + ", " + filepath.Join(s.bin, "uv") + ", " + filepath.Join(s.bin, "kubectl") + ", " + filepath.Join(s.bin, "jq") + ", " + filepath.Join(s.bin, "ssh-keygen") + "; go version go1.26.8 linux/amd64",
		RepositoryExtensions: "each session reports it at start; `legion state` renders it: loads the Oh My Pi extensions the repository it works carries",
	} {
		if got[name].Detail != want {
			t.Errorf("%s: detail %q, want %q", name, got[name].Detail, want)
		}
	}
	if want := "probe-image: capability eval-python: present (omp setup python --check reports available=true)"; got[EvalPython].String() != want {
		t.Errorf("State.String() = %q, want %q", got[EvalPython].String(), want)
	}
	if want := "probe-image: capability codegraph: present (" + got[CodeGraph].Detail + ")"; got[CodeGraph].Status != StatusPresent || got[CodeGraph].String() != want {
		t.Errorf("codegraph state = %q, want %q: present, with no sentence about a pod's launch", got[CodeGraph].String(), want)
	}
}

// Every missing image row is named, in the table's order, so one rebuild fixes them all; the
// rendered state says missing with the same detail; and the rows that are present still render
// present.
func TestCheckImageNamesEveryMissingRow(t *testing.T) {
	s := newStubs(t)
	s.remove("gopls")
	s.remove("pyright-langserver")

	states, err := CheckImage(context.Background(), s.image("LEGION_TEST_NO_PYTHON=1"))

	want := "capability eval-python is missing: omp setup python --check reports available=false; " +
		"capability lsp is missing: gopls, pyright-langserver are not on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("CheckImage = %v, want %q", err, want)
	}
	got := byName(states)
	if got[EvalPython].Status != StatusMissing || got[EvalPython].Detail != "omp setup python --check reports available=false" {
		t.Errorf("eval-python state = %+v, want missing with omp's answer", got[EvalPython])
	}
	if got[LSP].Status != StatusMissing || got[LSP].Detail != "gopls, pyright-langserver are not on PATH" {
		t.Errorf("lsp state = %+v, want missing naming both servers", got[LSP])
	}
	if got[Browser].Status != StatusPresent || got[Toolchain].Status != StatusPresent {
		t.Errorf("browser and toolchain = %+v, %+v, want both present still", got[Browser], got[Toolchain])
	}
	if len(states) != len(Table) {
		t.Errorf("rendered %d states, want the whole table (%d) however many rows are missing", len(states), len(Table))
	}
}

// One binary off PATH names that binary alone: onPath's single-absent branch (two absent is the lsp
// case in TestCheckImageNamesEveryMissingRow). Which names toolchain() checks, and in what order, is
// pinned by the Toolchain detail in TestCheckImageRendersEveryRowWhenEveryImageRowIsPresent, so one
// name is enough here.
func TestCheckImageNamesTheOneBinaryOffPATH(t *testing.T) {
	s := newStubs(t)
	s.remove("python3")

	states, err := CheckImage(context.Background(), s.image())

	want := "capability toolchain is missing: python3 is not on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("CheckImage = %v, want %q", err, want)
	}
	if got := byName(states)[Toolchain]; got.Status != StatusMissing || got.Detail != "python3 is not on PATH" {
		t.Errorf("toolchain state = %+v, want missing naming python3", got)
	}
}

// The Toolchain row's Summary is the one copy of the toolchain list no surface renders (the probe
// prints the detail), so it is held to the list toolchain() checks: the names recovered from a
// passing detail, joined as the Summary spells them.
func TestToolchainSummaryNamesTheListToolchainChecks(t *testing.T) {
	s := newStubs(t)

	detail, ok := toolchain(context.Background(), s.image())

	if !ok {
		t.Fatalf("toolchain() = %q, not ok; the stubs must make it pass", detail)
	}
	const prefix = "on PATH: "
	listed, ok := strings.CutPrefix(detail, prefix)
	if !ok {
		t.Fatalf("toolchain() = %q, want it to begin %q", detail, prefix)
	}
	paths, _, _ := strings.Cut(listed, "; ")
	var names []string
	for _, path := range strings.Split(paths, ", ") {
		names = append(names, filepath.Base(path))
	}
	want := "builds and runs with the generic toolchain: " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	i := slices.IndexFunc(Table, func(c Capability) bool { return c.Name == Toolchain })
	if i < 0 {
		t.Fatal("Table has no Toolchain row")
	}
	if row := Table[i]; row.Summary != want {
		t.Errorf("Toolchain Summary %q, want %q (the list toolchain() checks)", row.Summary, want)
	}
}

// The browser is resolved as Oh My Pi resolves it: PUPPETEER_EXECUTABLE_PATH when the environment
// sets it, else chromium on PATH; and it is run, so a path that names nothing is missing even with
// a chromium on PATH, which Oh My Pi would never launch under that variable.
func TestCheckImageResolvesTheBrowserAsOhMyPiDoes(t *testing.T) {
	s := newStubs(t)
	s.install("my-chrome", capabilitiestest.BinaryScript)
	for name, testCase := range map[string]struct {
		env    []string
		status string
		detail string
	}{
		"PUPPETEER_EXECUTABLE_PATH naming nothing": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium"}, StatusMissing,
			"/nonexistent/chromium (PUPPETEER_EXECUTABLE_PATH) --version: fork/exec /nonexistent/chromium: no such file or directory",
		},
		"PUPPETEER_EXECUTABLE_PATH naming a browser": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=" + filepath.Join(s.bin, "my-chrome")}, StatusPresent,
			filepath.Join(s.bin, "my-chrome") + " (PUPPETEER_EXECUTABLE_PATH) --version: Chromium 141.0.0.0",
		},
		"a bare name, looked up on PATH as a spawn does": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=my-chrome"}, StatusPresent,
			filepath.Join(s.bin, "my-chrome") + " (PUPPETEER_EXECUTABLE_PATH) --version: Chromium 141.0.0.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			states, err := CheckImage(context.Background(), s.image(testCase.env...))
			got := byName(states)[Browser]
			if got.Status != testCase.status || got.Detail != testCase.detail {
				t.Errorf("browser state = %+v, want %s (%s)", got, testCase.status, testCase.detail)
			}
			if (err != nil) != (testCase.status == StatusMissing) {
				t.Errorf("CheckImage = %v, want an error exactly when the browser is missing", err)
			}
			if err != nil && !strings.Contains(err.Error(), "capability browser is missing") {
				t.Errorf("CheckImage = %v, want it to name the browser", err)
			}
		})
	}
	t.Run("no chromium anywhere", func(t *testing.T) {
		s := newStubs(t)
		s.remove("chromium")
		states, _ := CheckImage(context.Background(), s.image())
		if got := byName(states)[Browser]; got.Status != StatusMissing || got.Detail != "chromium is not on PATH, and PUPPETEER_EXECUTABLE_PATH names no executable" {
			t.Errorf("browser state = %+v, want missing naming both resolutions", got)
		}
	})
}

// CodeGraph is the CLI on PATH and its plugin enabled in the profile's lock, both; each failure is
// named, and the row is missing and in CheckImage's error — the gate refuses an image lacking
// either half.
func TestCheckImageNeedsTheCodeGraphCLIAndItsPluginEnabled(t *testing.T) {
	for name, testCase := range map[string]struct {
		setup func(stubs)
		want  string
	}{
		"the plugin disabled": {func(s stubs) {
			s.writeLock(`{"plugins":{"@bopstack/pi-codegraph":{"version":"0.1.1","enabled":false}}}`)
		}, "@bopstack/pi-codegraph is disabled in the plugin lock "},
		"the plugin not installed":  {func(s stubs) { s.writeLock(`{"plugins":{"@sjawhar/pi-legion":{"enabled":true}}}`) }, "@bopstack/pi-codegraph is not in the plugin lock "},
		"no lock":                   {func(s stubs) { os.Remove(s.lock()) }, "cannot read the plugin lock "},
		"a lock that is not JSON":   {func(s stubs) { s.writeLock("not json") }, "is not the lock's JSON"},
		"the CLI off PATH":          {func(s stubs) { s.remove("codegraph") }, "codegraph is not on PATH"},
		"both the CLI and the lock": {func(s stubs) { s.remove("codegraph"); os.Remove(s.lock()) }, "codegraph is not on PATH; cannot read the plugin lock "},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStubs(t)
			testCase.setup(s)

			states, err := CheckImage(context.Background(), s.image())

			got := byName(states)[CodeGraph]
			if got.Status != StatusMissing || !strings.Contains(got.Detail, testCase.want) {
				t.Errorf("codegraph state = %+v, want missing saying %q", got, testCase.want)
			}
			if err == nil || !strings.HasPrefix(err.Error(), "capability codegraph is missing: ") {
				t.Errorf("CheckImage = %v, want it to name codegraph", err)
			}
		})
	}
}

// The lock follows the profile root Oh My Pi resolves (ompdirs.ProfileRoot): the named profile's
// under .omp/profiles, the default profile's under .omp itself; and a profile name Oh My Pi
// refuses is the refusal, naming it.
func TestPluginLockFollowsTheProfile(t *testing.T) {
	if got, err := pluginLock([]string{"HOME=/home/legion", "OMP_PROFILE=legion"}, "/work"); err != nil || got != "/home/legion/.omp/profiles/legion/plugins/omp-plugins.lock.json" {
		t.Errorf("pluginLock(legion) = %q, %v, want the legion profile's lock", got, err)
	}
	if got, err := pluginLock([]string{"HOME=/home/legion"}, "/work"); err != nil || got != "/home/legion/.omp/plugins/omp-plugins.lock.json" {
		t.Errorf("pluginLock(default) = %q, %v, want the default profile's lock", got, err)
	}
	if _, err := pluginLock([]string{"HOME=/home/legion", "OMP_PROFILE=Work"}, "/work"); err == nil || !strings.HasPrefix(err.Error(), `Invalid OMP profile "Work"`) {
		t.Errorf("pluginLock(Work) = %v, want Oh My Pi's refusal of the profile", err)
	}
}

// A profile name Oh My Pi refuses leaves no lock to read: the codegraph row is missing with the
// refusal as its detail, and the refusal names the row in CheckImage's error.
func TestCheckImageReportsAProfileOhMyPiRefusesOnTheCodeGraphRow(t *testing.T) {
	s := newStubs(t)

	states, err := CheckImage(context.Background(), s.image("OMP_PROFILE=Work"))

	got := byName(states)[CodeGraph]
	if got.Status != StatusMissing || !strings.HasPrefix(got.Detail, `Invalid OMP profile "Work" in the environment Oh My Pi starts under.`) {
		t.Errorf("codegraph state = %+v, want missing with Oh My Pi's refusal of the profile", got)
	}
	if err == nil || !strings.HasPrefix(err.Error(), `capability codegraph is missing: Invalid OMP profile "Work"`) {
		t.Errorf("CheckImage = %v, want it to name codegraph with the refusal", err)
	}
}

// A check that asks Oh My Pi reads what Oh My Pi said, not its exit code: an omp that exits 1
// with `available: false` is a missing Python, and one that prints no JSON is named by what it
// printed.
func TestCheckImageJudgesPythonByOhMyPisAnswer(t *testing.T) {
	s := newStubs(t)
	if err := os.WriteFile(s.omp, []byte("#!/bin/sh\necho 'setup: no such command' >&2; exit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	states, err := CheckImage(context.Background(), s.image())

	got := byName(states)[EvalPython]
	want := "omp setup python --check --json exited 2 without a JSON available field: setup: no such command"
	if got.Status != StatusMissing || got.Detail != want {
		t.Errorf("eval-python state = %+v, want missing with %q", got, want)
	}
	if err == nil || !strings.Contains(err.Error(), "capability eval-python is missing") {
		t.Errorf("CheckImage = %v, want it to name eval-python", err)
	}
}

// Every image row has a check, and every check is an image row's.
func TestEveryImageRowHasACheck(t *testing.T) {
	for _, row := range Table {
		if _, ok := imageChecks[row.Name]; ok != (row.Site == SiteImage) {
			t.Errorf("row %s is checked at %s, and imageChecks has a check for it: %t", row.Name, row.Site, ok)
		}
	}
}

// A context that ends ends the check with its error rather than a table of missing rows.
func TestCheckImageStopsWithItsContext(t *testing.T) {
	s := newStubs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := CheckImage(ctx, s.image())

	if !errors.Is(err, context.Canceled) {
		t.Errorf("CheckImage under an ended context = %v, want it to wrap context.Canceled", err)
	}
}

// ReadModelFallback is the JSON's value, on or off, read through the launch as a shell fragment,
// so a prefix in it runs; a command that fails, or prints no boolean, is an error naming the
// command and what it printed.
func TestReadModelFallback(t *testing.T) {
	s := newStubs(t)
	for name, testCase := range map[string]struct {
		img  Image
		want string
	}{
		"off by default":                 {s.image(), bootprobe.ModelFallbackOff},
		"on":                             {s.image("LEGION_TEST_FALLBACK_ON=1"), bootprobe.ModelFallbackOn},
		"the launch is a shell fragment": {Image{Launch: "/usr/bin/env LEGION_TEST_FALLBACK_ON=1 " + s.omp, Env: s.image().Env, WorkDir: t.TempDir()}, bootprobe.ModelFallbackOn},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadModelFallback(context.Background(), testCase.img)
			if err != nil || got != testCase.want {
				t.Errorf("ReadModelFallback = %q, %v, want %q", got, err, testCase.want)
			}
		})
	}
	t.Run("a failing command", func(t *testing.T) {
		img := s.image()
		img.Launch = s.omp + " --bogus"
		_, err := ReadModelFallback(context.Background(), img)
		want := s.omp + " --bogus config get retry.modelFallback --json exited 64: fake omp: unexpected argv: --bogus config get retry.modelFallback --json"
		if err == nil || err.Error() != want {
			t.Errorf("ReadModelFallback = %v, want %q", err, want)
		}
	})
	t.Run("no boolean value", func(t *testing.T) {
		omp := filepath.Join(t.TempDir(), "omp")
		if err := os.WriteFile(omp, []byte("#!/bin/sh\necho '{\"key\":\"retry.modelFallback\",\"value\":null}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		img := s.image()
		img.Launch = omp
		_, err := ReadModelFallback(context.Background(), img)
		if err == nil || !strings.Contains(err.Error(), "printed no JSON object with a boolean value: {\"key\":\"retry.modelFallback\",\"value\":null}") {
			t.Errorf("ReadModelFallback = %v, want an error quoting the output", err)
		}
	})
	t.Run("a launch that cannot run", func(t *testing.T) {
		img := s.image()
		img.Launch = "/nonexistent/omp"
		_, err := ReadModelFallback(context.Background(), img)
		if err == nil || !strings.Contains(err.Error(), "/nonexistent/omp config get retry.modelFallback --json exited 127") {
			t.Errorf("ReadModelFallback = %v, want the shell's 127 for a launch it cannot exec", err)
		}
	})
}

// envValue is name's value in env, the last assignment winning as it does for the process env
// starts.
func TestEnvValueTakesTheLastAssignment(t *testing.T) {
	if got := envValue([]string{"PATH=a", "PATH=b"}, "PATH"); got != "b" {
		t.Errorf("envValue = %q, want the last assignment", got)
	}
}

// The table's states are one per row and print in order, so the probe pod's log reads as the list.
func TestLinesPrintInTableOrder(t *testing.T) {
	s := newStubs(t)
	states, err := CheckImage(context.Background(), s.image())
	if err != nil {
		t.Fatal(err)
	}
	var names []Name
	for _, state := range states {
		names = append(names, state.Name)
		if !strings.HasPrefix(state.String(), "probe-image: capability "+string(state.Name)+": "+state.Status+" (") || !strings.HasSuffix(state.String(), ")") {
			t.Errorf("State.String() = %q, want the probe's row shape", state.String())
		}
	}
	var want []Name
	for _, row := range Table {
		want = append(want, row.Name)
	}
	if !slices.Equal(names, want) {
		t.Errorf("states name %v, want Table's order %v", names, want)
	}
}

// State.String() is the probe's line, pinned whole, so a reworded format is caught here.
func TestStateStringIsTheProbeLine(t *testing.T) {
	state := State{Name: Browser, Status: StatusPresent, Detail: "x"}
	if want := "probe-image: capability browser: present (x)"; state.String() != want {
		t.Errorf("State.String() = %q, want %q", state.String(), want)
	}
}
