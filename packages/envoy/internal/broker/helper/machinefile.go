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
// os.ErrNotExist; one that is there but is not exactly one whole machine login (unreadable JSON,
// anything after the object, an unknown field, a missing value, a key that is not a P-256 PKCS#8
// key) is an error naming path.
func LoadMachineFile(path string) (*MachineFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw machineFileJSON
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("machine file %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("machine file %s: data after its one JSON object", path)
	}
	if raw.CredentialID == "" || raw.RefreshToken == "" || raw.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("machine file %s: credential_id, expires_at and refresh_token are all required", path)
	}
	der, err := base64.StdEncoding.DecodeString(raw.Key)
	if err != nil {
		return nil, fmt.Errorf("machine file %s: key: %w", path, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("machine file %s: key: %w", path, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve.Params().Name != "P-256" {
		return nil, fmt.Errorf("machine file %s: key is not a P-256 ECDSA key", path)
	}
	return &MachineFile{Key: key, CredentialID: raw.CredentialID, ExpiresAt: raw.ExpiresAt, RefreshToken: raw.RefreshToken}, nil
}

// Save writes f to path, mode 0600, creating its directory 0700 if it is absent. It writes
// path.tmp, syncs it, renames it over path and syncs the directory, so path is always either the
// old login or the new one, never part of either.
func (f *MachineFile) Save(path string) error {
	return f.save(path, (*os.File).Write)
}

// save is Save with the write of the new file's bytes injectable, so a test can fail it partway.
func (f *MachineFile) save(path string, write func(*os.File, []byte) (int, error)) (err error) {
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
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	// A tmp file left by an earlier crash keeps its own mode through O_TRUNC.
	if err := out.Chmod(0o600); err != nil {
		return err
	}
	if _, err := write(out, data); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
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
