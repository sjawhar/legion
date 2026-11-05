// packages/envoy/internal/broker/helper/identitykey.go

package helper

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// identityKeyPEMType is the PEM block a P-256 key is kept in, the one `agent-secrets keygen`
// writes a session key as, so one reader of a PKCS#8 PEM reads either.
const identityKeyPEMType = "PRIVATE KEY"

// LoadOrMakeIdentityKey is the key a machine that proves an identity of its own trades with: the
// one its launcher credential is bound to, and the one the broker names that credential by, since
// a machine's subject carries the RFC 7638 thumbprint of the key its proof presented
// (exchange.MachineSubject). It answers the key kept at path, or makes one and keeps it there,
// and made says which.
//
// The key is kept because the subject is what makes a restart move the machine's one credential
// instead of adding another. A helper that made a new key every start would, on each restart,
// leave the credential its sessions were enrolled under behind until that credential's own
// expiry: the sessions it re-pins could not revoke their old enrollments under the new credential
// (enroll.Service.Revoke answers ErrNotEnrollingCredential), and a box enrolled through
// `enroll-session-key`, whose renewer cannot enrol again, would be refused ENROLLMENT_NOT_LIVE
// once the old credential lapsed. Keeping it, the restarted helper trades the same subject, the
// broker moves that one credential's expiry (enroll.Service.TokenLogin), and every enrollment
// under it keeps renewing.
//
// It is written 0600 in a directory made 0700, as the machine login beside it is (MachineFile),
// which is the storage a person already chose for the key a machine logs in with. A key file
// that others may read, or that another user owns, is refused naming path and the fix, and so is
// one that is not a P-256 PKCS#8 key: the cure is to remove it, after which this makes a new one.
//
// The file is made by linking a finished temporary file into place, which never overwrites, so
// two helpers starting at once settle on one key rather than each holding one the other wrote
// over.
func LoadOrMakeIdentityKey(path string) (key *ecdsa.PrivateKey, made bool, err error) {
	key, err = loadIdentityKey(path)
	switch {
	case err == nil:
		return key, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, false, err
	}
	key, err = proof.NewKey()
	if err != nil {
		return nil, false, fmt.Errorf("make this machine's key: %w", err)
	}
	switch kept, err := keepIdentityKey(path, key); {
	case err != nil:
		return nil, false, err
	case kept:
		return key, true, nil
	}
	// Another helper linked its key in between: that one is this machine's.
	key, err = loadIdentityKey(path)
	return key, false, err
}

// loadIdentityKey reads the key kept at path. A file that is not there answers an error matching
// os.ErrNotExist, which is a machine that has not traded here yet.
func loadIdentityKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("identity key %s is mode %04o, which lets others read the key this machine's launcher credential is bound to; run: chmod 600 %s", path, perm, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("identity key %s is owned by uid %d, not this user (uid %d); remove it, or run the helper as its owner, and chmod 600 it", path, st.Uid, os.Getuid())
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != identityKeyPEMType {
		return nil, fmt.Errorf("identity key %s is no %s PEM block; remove it and the helper makes a new key", path, identityKeyPEMType)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity key %s: %w; remove it and the helper makes a new key", path, err)
	}
	key, _ := parsed.(*ecdsa.PrivateKey)
	if key == nil || key.Curve == nil || key.Curve.Params().Name != "P-256" {
		return nil, fmt.Errorf("identity key %s is not a P-256 ECDSA key; remove it and the helper makes a new key", path)
	}
	return key, nil
}

// keepIdentityKey writes key at path and reports whether this call is the one that put it there:
// false, with no error, where a file is already at path, which is another helper's key.
func keepIdentityKey(path string, key *ecdsa.PrivateKey) (kept bool, err error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	out, err := os.CreateTemp(dir, ".identity-key-*.pem.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(out.Name())
	if _, err := out.Write(pem.EncodeToMemory(&pem.Block{Type: identityKeyPEMType, Bytes: der})); err != nil {
		out.Close()
		return false, err
	}
	if err := errors.Join(out.Sync(), out.Close()); err != nil {
		return false, err
	}
	if err := os.Link(out.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	return true, errors.Join(d.Sync(), d.Close())
}
