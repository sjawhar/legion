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
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed save left %s.tmp behind: %v", path, err)
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
		"an unknown field":         strings.Replace(string(data), `"credential_id"`, `"surprise":1,"credential_id"`, 1),
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
