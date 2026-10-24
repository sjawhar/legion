// packages/envoy/internal/broker/helper/machinefile.go

package helper

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// MachineFile is the machine login the helper keeps across restarts: the machine key the login's
// tokens are bound to, the launcher credential the broker minted for it, when that credential
// lapses unless a refresh moves it, and the refresh token the next refresh spends. It lives at
// AGENT_SECRETS_MACHINE_FILE (default ~/.config/agent-secrets/machine.json), readable only by its
// user.
type MachineFile struct {
	Key          *ecdsa.PrivateKey
	CredentialID string
	ExpiresAt    time.Time
	RefreshToken string
}

// machineFileJSON is the file's shape: the key is its PKCS#8 DER, base64 (standard, padded).
type machineFileJSON struct {
	Key          string    `json:"key"`
	CredentialID string    `json:"credential_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshToken string    `json:"refresh_token"`
}

// LoadMachineFile reads the machine login at path. A file that is not there is an error matching
// os.ErrNotExist; one whose mode grants its group or anyone else any access, or that this user
// does not own, is refused, naming path and the fix, since it holds the machine's key and refresh
// token; one that is there but is not exactly one whole machine login (unreadable JSON, anything
// after the object, a missing value, a key that is not a P-256 PKCS#8 key) is an error naming path.
// A field this release does not know is ignored, so a rollback past a release that added one still
// starts on the file that release wrote.
func LoadMachineFile(path string) (*MachineFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("machine file %s is mode %04o, which lets others read the machine's key; run: chmod 600 %s", path, perm, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("machine file %s is owned by uid %d, not this user (uid %d); remove it, or run the helper as its owner, and chmod 600 it", path, st.Uid, os.Getuid())
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw machineFileJSON
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("machine file %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("machine file %s: data after its one JSON object", path)
	}
	der, err := base64.StdEncoding.DecodeString(raw.Key)
	if err != nil {
		return nil, fmt.Errorf("machine file %s: key: %w", path, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("machine file %s: key: %w", path, err)
	}
	key, _ := parsed.(*ecdsa.PrivateKey)
	f := &MachineFile{Key: key, CredentialID: raw.CredentialID, ExpiresAt: raw.ExpiresAt, RefreshToken: raw.RefreshToken}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("machine file %s: %w", path, err)
	}
	return f, nil
}

// validate checks f is a whole machine login: a P-256 ECDSA key, and a credential id, an expiry
// and a refresh token. LoadMachineFile and Save both check it, so the helper never writes a file
// its next start would refuse.
func (f *MachineFile) validate() error {
	if f.CredentialID == "" || f.RefreshToken == "" || f.ExpiresAt.IsZero() {
		return errors.New("credential_id, expires_at and refresh_token are all required")
	}
	if f.Key == nil || f.Key.Curve == nil || f.Key.Curve.Params().Name != "P-256" {
		return errors.New("key is not a P-256 ECDSA key")
	}
	return nil
}

// Save writes f to path, mode 0600, creating its directory 0700 if it is absent; a MachineFile
// that is not a whole login (validate) is refused, writing nothing. It writes a new temporary file
// in path's directory (os.CreateTemp: a fresh random name, created exclusively and 0600, so no
// file or link already there is followed or reused), syncs it, renames it over path and syncs the
// directory, so path is always either the old login or the new one, never part of either. A save
// that fails removes its temporary file.
func (f *MachineFile) Save(path string) error {
	return f.save(path, (*os.File).Write)
}

// save is Save with the write of the new file's bytes injectable, so a test can fail it partway.
func (f *MachineFile) save(path string, write func(*os.File, []byte) (int, error)) (err error) {
	if err := f.validate(); err != nil {
		return fmt.Errorf("machine file %s: %w", path, err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(f.Key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(machineFileJSON{
		Key: base64.StdEncoding.EncodeToString(der), CredentialID: f.CredentialID,
		ExpiresAt: f.ExpiresAt.UTC(), RefreshToken: f.RefreshToken,
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := os.CreateTemp(dir, ".machine-*.json.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()
	if _, err := write(out, data); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// RemoveMachineFile deletes the machine file at path; one already gone is not an error.
func RemoveMachineFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveLeftoverMachineTemps deletes the temporary files a save interrupted between creating and
// renaming them left in path's directory (.machine-*.json.tmp, each holding a key and a refresh
// token). Nothing else there is touched, and a directory that is not there is nothing to do.
func RemoveLeftoverMachineTemps(path string) error {
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".machine-*.json.tmp"))
	if err != nil {
		return err
	}
	var errs []error
	for _, temp := range temps {
		if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
