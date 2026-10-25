// packages/envoy/internal/broker/helper/machinefile_test.go

package helper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

func testMachineFile(t *testing.T, id, refreshToken string) *MachineFile {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &MachineFile{Key: key, CredentialID: id, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second), RefreshToken: refreshToken}
}

// TestTheMachineFileRoundTripsAndIsPrivate: a saved machine file loads back as the same login (the
// key's thumbprint, the credential id, its expiry and the refresh token), is readable only by its
// user, sits in a directory only its user can enter, and a save that fails partway leaves the
// file it was replacing intact and no partial file behind.
func TestTheMachineFileRoundTripsAndIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-secrets", "machine.json")
	if _, err := LoadMachineFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading a machine file that does not exist: %v; want os.ErrNotExist", err)
	}
	saved := testMachineFile(t, "cred-1", "refresh-1")
	if err := saved.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMachineFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantTP, _ := proof.Thumbprint(&saved.Key.PublicKey)
	gotTP, _ := proof.Thumbprint(&loaded.Key.PublicKey)
	if gotTP != wantTP || loaded.CredentialID != "cred-1" || !loaded.ExpiresAt.Equal(saved.ExpiresAt) || loaded.RefreshToken != "refresh-1" {
		t.Fatalf("loaded %+v (thumbprint %s); want %+v (thumbprint %s)", loaded, gotTP, saved, wantTP)
	}
	if loaded.Key.D.Cmp(saved.Key.D) != 0 {
		t.Fatal("the loaded key is not the saved private key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("machine file mode %o; want 600", mode)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Fatalf("machine file directory mode %o; want 700", mode)
	}

	// A save that fails after writing part of the new file: the old file is still the login.
	replacement := testMachineFile(t, "cred-2", "refresh-2")
	failing := func(f *os.File, data []byte) (int, error) {
		n, _ := f.Write(data[:len(data)/2])
		return n, errors.New("disk full")
	}
	if err := replacement.save(path, failing); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a save whose writer fails: %v; want its error", err)
	}
	again, err := LoadMachineFile(path)
	if err != nil || again.CredentialID != "cred-1" || again.RefreshToken != "refresh-1" {
		t.Fatalf("after a failed save the file reads %+v (%v); want the old login cred-1", again, err)
	}
	if left := tempFiles(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("a failed save left %v behind", left)
	}
}

// tempFiles lists dir's entries other than machine.json.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() != "machine.json" {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestASaveNeverUsesAPathAlreadyThere: a symlink or a world-readable file at the predictable
// machine.json.tmp path (another local user's, or an old crash's) is neither followed nor
// written: the save goes through a fresh, exclusively created temporary file, the link's target
// and the stale file are untouched, and the saved file is 0600.
func TestASaveNeverUsesAPathAlreadyThere(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "machine.json")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(string) error{
		"a symlink":             func(p string) error { return os.Symlink(target, p) },
		"a world-readable file": func(p string) error { return os.WriteFile(p, []byte("stale"), 0o644) },
	} {
		tmp := path + ".tmp"
		_ = os.Remove(tmp)
		if err := plant(tmp); err != nil {
			t.Fatal(err)
		}
		if err := testMachineFile(t, "cred-1", "refresh-1").Save(path); err != nil {
			t.Fatalf("%s at %s: Save: %v", name, tmp, err)
		}
		if data, _ := os.ReadFile(target); string(data) != "theirs" {
			t.Fatalf("%s at %s: the link's target now reads %q", name, tmp, data)
		}
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s at %s: machine.json %v %v; want a regular 0600 file", name, tmp, info, err)
		}
		if loaded, err := LoadMachineFile(path); err != nil || loaded.CredentialID != "cred-1" {
			t.Fatalf("%s at %s: loaded %+v %v", name, tmp, loaded, err)
		}
	}
}

// TestASaveRefusesWhatALoadWouldRefuse: Save checks the login as LoadMachineFile does, so the
// helper never writes a file its next start refuses, and writes nothing for one it refuses.
func TestASaveRefusesWhatALoadWouldRefuse(t *testing.T) {
	good := testMachineFile(t, "cred-1", "refresh-1")
	for name, f := range map[string]*MachineFile{
		"no credential id": {Key: good.Key, ExpiresAt: good.ExpiresAt, RefreshToken: "r"},
		"no refresh token": {Key: good.Key, CredentialID: "c", ExpiresAt: good.ExpiresAt},
		"no expiry":        {Key: good.Key, CredentialID: "c", RefreshToken: "r"},
		"no key":           {CredentialID: "c", ExpiresAt: good.ExpiresAt, RefreshToken: "r"},
	} {
		path := filepath.Join(t.TempDir(), "machine.json")
		if err := f.Save(path); err == nil {
			t.Errorf("%s: Save = nil, want an error", name)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: Save wrote %s (%v)", name, path, err)
		}
	}
}

// TestAMachineFileThatDoesNotParseIsAnErrorNamingIt: a file that is there but is not a machine
// login is an error that names its path, never a login the helper silently goes without.
func TestAMachineFileThatDoesNotParseIsAnErrorNamingIt(t *testing.T) {
	dir := t.TempDir()
	good := testMachineFile(t, "cred-1", "refresh-1")
	goodPath := filepath.Join(dir, "good.json")
	if err := good.Save(goodPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"not JSON":                 "{",
		"trailing garbage":         string(data) + "garbage",
		"a second object":          string(data) + "\n{}",
		"no refresh token":         strings.Replace(string(data), `"refresh-1"`, `""`, 1),
		"no credential id":         strings.Replace(string(data), `"cred-1"`, `""`, 1),
		"a key that is not PKCS#8": strings.Replace(string(data), `"key":"`, `"key":"AAAA`, 1),
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMachineFile(path); err == nil || errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: LoadMachineFile = %v; want an error naming %s", name, err, path)
		}
	}
}

// TestAMachineFileWithAFieldThisReleaseDoesNotKnowLoads: a field a later release added is ignored,
// so rolling back after that release still starts on the file it wrote, while the fields this
// release needs are still required.
func TestAMachineFileWithAFieldThisReleaseDoesNotKnowLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.json")
	if err := testMachineFile(t, "cred-1", "refresh-1").Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	later := strings.Replace(string(data), `"credential_id"`, `"broker_url":"https://broker.internal.example","credential_id"`, 1)
	if err := os.WriteFile(path, []byte(later), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := LoadMachineFile(path)
	if err != nil {
		t.Fatalf("LoadMachineFile of a file with a later release's field = %v; want it loaded", err)
	}
	if f.CredentialID != "cred-1" || f.RefreshToken != "refresh-1" {
		t.Fatalf("loaded %q/%q; want cred-1/refresh-1", f.CredentialID, f.RefreshToken)
	}
}

// TestAMachineFileOthersCanReadIsRefused: LoadMachineFile refuses a machine file whose mode grants
// its group or anyone else any access, naming the path and the fix, and loads it once it is 0600.
func TestAMachineFileOthersCanReadIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.json")
	if err := testMachineFile(t, "cred-1", "refresh-1").Save(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o610} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMachineFile(path); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %v: LoadMachineFile = %v; want a refusal naming %s and chmod 600", mode, err, path)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMachineFile(path); err != nil {
		t.Fatalf("mode 0600: %v", err)
	}
}

// TestASaveAfterTheBrokerClosesWritesNothing: once Close returns, no save of the machine login
// writes into its directory, so the caller that closed the broker can remove that directory
// while a refresh or an approved login is still finishing.
func TestASaveAfterTheBrokerClosesWritesNothing(t *testing.T) {
	dir := t.TempDir()
	b := &Broker{MachineFile: filepath.Join(dir, "machine.json")}
	b.Close()
	if err := b.saveFile(testMachineFile(t, "cred-1", "refresh-1")); err != nil {
		t.Fatalf("a save after Close: %v; want nil, since it writes nothing", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a save after Close wrote into the directory: %v %v", entries, err)
	}
}

// TestRemoveLeftoverMachineTempsRemovesOnlyTheSavesTemps: the temporary files an interrupted save
// leaves beside the machine file (.machine-*.json.tmp, holding a key and a refresh token) are
// removed; the machine file and anything else in the directory are left alone.
func TestRemoveLeftoverMachineTempsRemovesOnlyTheSavesTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "machine.json")
	if err := testMachineFile(t, "cred-1", "refresh-1").Save(path); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".machine-123.json.tmp", ".machine-abc.json.tmp", "operator", "machine.json.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveLeftoverMachineTemps(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "machine.json,machine.json.lock,operator" {
		t.Fatalf("directory after the cleanup: %v; want the machine file, its lock and the operator file", names)
	}
	if err := RemoveLeftoverMachineTemps(filepath.Join(t.TempDir(), "absent", "machine.json")); err != nil {
		t.Fatalf("a directory that is not there: %v; want nothing to do", err)
	}
}
