package capabilities

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stubBinaries are the binaries the image rows look for, each a stub answering what the checks
// run: `go version` and `chromium --version`.
var stubBinaries = []string{"gopls", "typescript-language-server", "pyright-langserver", "codegraph", "go", "curl", "wget", "python3", "node", "bun", "uv", "chromium"}

const stubScript = `#!/bin/sh
case "$1" in
version) echo "go version go1.26.8 linux/amd64" ;;
--version) echo "Chromium 141.0.0.0" ;;
esac
`

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

// codeGraphLock is the lock `omp plugin install` writes in the worker image.
const codeGraphLock = `{"plugins":{"@bopstack/pi-codegraph":{"version":"0.1.1","enabledFeatures":null,"enabled":true}}}`

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
	for _, name := range stubBinaries {
		s.install(name, stubScript)
	}
	s.omp = filepath.Join(t.TempDir(), "omp")
	if err := os.WriteFile(s.omp, []byte(fakeOmp), 0o755); err != nil {
		t.Fatal(err)
	}
	s.writeLock(codeGraphLock)
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
	return filepath.Join(s.home, ".omp", "profiles", "legion", "plugins", "omp-plugins.lock.json")
}

func (s stubs) writeLock(content string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.lock()), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(s.lock(), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
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

// byName is the lines by capability.
func byName(lines []Line) map[Name]Line {
	m := map[Name]Line{}
	for _, line := range lines {
		m[line.Name] = line
	}
	return m
}

// Under an image that carries everything, every row is rendered in Table's order: the image rows
// present with their evidence, the others as what they are, and no error.
func TestCheckImageRendersEveryRowWhenEveryImageRowIsPresent(t *testing.T) {
	s := newStubs(t)

	lines, err := CheckImage(context.Background(), s.image())

	if err != nil {
		t.Fatalf("CheckImage = %v, want every image row present", err)
	}
	if len(lines) != len(Table) {
		t.Fatalf("CheckImage rendered %d lines, want one per row (%d)", len(lines), len(Table))
	}
	for i, row := range Table {
		line := lines[i]
		if line.Name != row.Name {
			t.Errorf("line %d is %s, want %s: the table's order", i, line.Name, row.Name)
		}
		want := map[Site]string{SiteImage: present, SiteLive: live, SiteDeployment: reported, SiteWithheld: withheld}[row.Site]
		if line.Status != want {
			t.Errorf("%s: status %q, want %q", line.Name, line.Status, want)
		}
		if row.Site == SiteWithheld && !strings.HasPrefix(line.Detail, row.Ruling+": ") {
			t.Errorf("%s: detail %q, want the ruling %s first", line.Name, line.Detail, row.Ruling)
		}
		if row.Site == SiteLive && row.Ruling != "" && !strings.Contains(line.Detail, row.Ruling) {
			t.Errorf("%s: detail %q, want it to name the live check's issue %s", line.Name, line.Detail, row.Ruling)
		}
	}
	got := byName(lines)
	for name, want := range map[Name]string{
		EvalPython: "omp setup python --check reports available=true",
		Browser:    filepath.Join(s.bin, "chromium") + " (chromium on PATH) --version: Chromium 141.0.0.0",
		LSP:        "on PATH: " + filepath.Join(s.bin, "gopls") + ", " + filepath.Join(s.bin, "typescript-language-server") + ", " + filepath.Join(s.bin, "pyright-langserver"),
		CodeGraph:  filepath.Join(s.bin, "codegraph") + " on PATH; @bopstack/pi-codegraph enabled in " + s.lock(),
		Toolchain:  "on PATH: " + filepath.Join(s.bin, "go") + ", " + filepath.Join(s.bin, "curl") + ", " + filepath.Join(s.bin, "wget") + ", " + filepath.Join(s.bin, "python3") + ", " + filepath.Join(s.bin, "node") + ", " + filepath.Join(s.bin, "bun") + ", " + filepath.Join(s.bin, "uv") + "; go version go1.26.8 linux/amd64",
	} {
		if got[name].Detail != want {
			t.Errorf("%s: detail %q, want %q", name, got[name].Detail, want)
		}
	}
	if want := "probe-image: capability eval-python: present (omp setup python --check reports available=true)"; got[EvalPython].String() != want {
		t.Errorf("Line.String() = %q, want %q", got[EvalPython].String(), want)
	}
}

// Every missing image row is named, in the table's order, so one rebuild fixes them all; the
// rendered line says missing with the same detail; and the rows that are present still render
// present.
func TestCheckImageNamesEveryMissingRow(t *testing.T) {
	s := newStubs(t)
	s.remove("gopls")
	s.remove("pyright-langserver")

	lines, err := CheckImage(context.Background(), s.image("LEGION_TEST_NO_PYTHON=1"))

	want := "capability eval-python is missing: omp setup python --check reports available=false; " +
		"capability lsp is missing: gopls, pyright-langserver are not on PATH"
	if err == nil || err.Error() != want {
		t.Fatalf("CheckImage = %v, want %q", err, want)
	}
	got := byName(lines)
	if got[EvalPython].Status != missing || got[EvalPython].Detail != "omp setup python --check reports available=false" {
		t.Errorf("eval-python line = %+v, want missing with omp's answer", got[EvalPython])
	}
	if got[LSP].Status != missing || got[LSP].Detail != "gopls, pyright-langserver are not on PATH" {
		t.Errorf("lsp line = %+v, want missing naming both servers", got[LSP])
	}
	if got[Browser].Status != present || got[Toolchain].Status != present {
		t.Errorf("browser and toolchain = %+v, %+v, want both present still", got[Browser], got[Toolchain])
	}
	if len(lines) != len(Table) {
		t.Errorf("rendered %d lines, want the whole table (%d) however many rows are missing", len(lines), len(Table))
	}
}

// One binary off PATH names that binary alone.
func TestCheckImageNamesTheOneBinaryOffPATH(t *testing.T) {
	s := newStubs(t)
	s.remove("python3")

	lines, err := CheckImage(context.Background(), s.image())

	if want := "capability toolchain is missing: python3 is not on PATH"; err == nil || err.Error() != want {
		t.Fatalf("CheckImage = %v, want %q", err, want)
	}
	if got := byName(lines)[Toolchain]; got.Status != missing || got.Detail != "python3 is not on PATH" {
		t.Errorf("toolchain line = %+v, want missing naming python3", got)
	}
}

// The browser is resolved as Oh My Pi resolves it: PUPPETEER_EXECUTABLE_PATH when the environment
// sets it, else chromium on PATH; and it is run, so a path that names nothing is missing even with
// a chromium on PATH, which Oh My Pi would never launch under that variable.
func TestCheckImageResolvesTheBrowserAsOhMyPiDoes(t *testing.T) {
	s := newStubs(t)
	s.install("my-chrome", stubScript)
	for name, testCase := range map[string]struct {
		env    []string
		status string
		detail string
	}{
		"PUPPETEER_EXECUTABLE_PATH naming nothing": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium"}, missing,
			"/nonexistent/chromium (PUPPETEER_EXECUTABLE_PATH) --version: fork/exec /nonexistent/chromium: no such file or directory",
		},
		"PUPPETEER_EXECUTABLE_PATH naming a browser": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=" + filepath.Join(s.bin, "my-chrome")}, present,
			filepath.Join(s.bin, "my-chrome") + " (PUPPETEER_EXECUTABLE_PATH) --version: Chromium 141.0.0.0",
		},
		"a bare name, looked up on PATH as a spawn does": {
			[]string{"PUPPETEER_EXECUTABLE_PATH=my-chrome"}, present,
			filepath.Join(s.bin, "my-chrome") + " (PUPPETEER_EXECUTABLE_PATH) --version: Chromium 141.0.0.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			lines, err := CheckImage(context.Background(), s.image(testCase.env...))
			got := byName(lines)[Browser]
			if got.Status != testCase.status || got.Detail != testCase.detail {
				t.Errorf("browser line = %+v, want %s (%s)", got, testCase.status, testCase.detail)
			}
			if (err != nil) != (testCase.status == missing) {
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
		lines, _ := CheckImage(context.Background(), s.image())
		if got := byName(lines)[Browser]; got.Status != missing || got.Detail != "chromium is not on PATH, and PUPPETEER_EXECUTABLE_PATH names no executable" {
			t.Errorf("browser line = %+v, want missing naming both resolutions", got)
		}
	})
}

// CodeGraph is the CLI on PATH and its plugin enabled in the profile's lock, both; each failure is
// named.
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

			lines, err := CheckImage(context.Background(), s.image())

			got := byName(lines)[CodeGraph]
			if got.Status != missing || !strings.Contains(got.Detail, testCase.want) {
				t.Errorf("codegraph line = %+v, want missing saying %q", got, testCase.want)
			}
			if err == nil || !strings.HasPrefix(err.Error(), "capability codegraph is missing: ") {
				t.Errorf("CheckImage = %v, want it to name codegraph", err)
			}
		})
	}
}

// The default profile's lock is under .omp itself.
func TestPluginLockFollowsTheProfile(t *testing.T) {
	if got, want := pluginLock([]string{"HOME=/home/legion", "OMP_PROFILE=legion"}), "/home/legion/.omp/profiles/legion/plugins/omp-plugins.lock.json"; got != want {
		t.Errorf("pluginLock(legion) = %q, want %q", got, want)
	}
	if got, want := pluginLock([]string{"HOME=/home/legion"}), "/home/legion/.omp/plugins/omp-plugins.lock.json"; got != want {
		t.Errorf("pluginLock(default) = %q, want %q", got, want)
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

	lines, err := CheckImage(context.Background(), s.image())

	got := byName(lines)[EvalPython]
	want := "omp setup python --check --json exited 2 without a JSON available field: setup: no such command"
	if got.Status != missing || got.Detail != want {
		t.Errorf("eval-python line = %+v, want missing with %q", got, want)
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
		"off by default":                 {s.image(), "off"},
		"on":                             {s.image("LEGION_TEST_FALLBACK_ON=1"), "on"},
		"the launch is a shell fragment": {Image{Launch: "/usr/bin/env LEGION_TEST_FALLBACK_ON=1 " + s.omp, Env: s.image().Env, WorkDir: t.TempDir()}, "on"},
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

// lookPath walks the PATH it is given, not this process's, and finds only executables under
// absolute elements.
func TestLookPathReadsThePATHItIsGiven(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "notes"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=relative/bin:" + bin}
	if got, ok := lookPath(env, "tool"); !ok || got != filepath.Join(bin, "tool") {
		t.Errorf("lookPath(tool) = %q, %t, want %s", got, ok, filepath.Join(bin, "tool"))
	}
	for _, name := range []string{"notes", "dir", "sh"} {
		if got, ok := lookPath(env, name); ok {
			t.Errorf("lookPath(%s) = %q, want none: not an executable on the given PATH", name, got)
		}
	}
	if _, ok := lookPath([]string{"PATH="}, "tool"); ok {
		t.Error("lookPath found tool on an empty PATH")
	}
	if got := envValue([]string{"PATH=a", "PATH=b"}, "PATH"); got != "b" {
		t.Errorf("envValue = %q, want the last assignment", got)
	}
}

// The table's lines are one per row and print in order, so the probe pod's log reads as the list.
func TestLinesPrintInTableOrder(t *testing.T) {
	s := newStubs(t)
	lines, err := CheckImage(context.Background(), s.image())
	if err != nil {
		t.Fatal(err)
	}
	var names []Name
	for _, line := range lines {
		names = append(names, line.Name)
		if !strings.HasPrefix(line.String(), "probe-image: capability "+string(line.Name)+": "+line.Status+" (") || !strings.HasSuffix(line.String(), ")") {
			t.Errorf("Line.String() = %q, want the probe's row shape", line.String())
		}
	}
	var want []Name
	for _, row := range Table {
		want = append(want, row.Name)
	}
	if !slices.Equal(names, want) {
		t.Errorf("lines name %v, want Table's order %v", names, want)
	}
}
