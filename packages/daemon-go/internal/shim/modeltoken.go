package shim

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// writeModelAccessToken keeps the daemon-delivered token out of OMP's environment. A shim without
// a configured path is a protocol/configuration mismatch, never a reason to forward the token.
func (s *shim) writeModelAccessToken(frame shimwire.ModelAccessToken) error {
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("malformed model-access-token frame: %w", err)
	}
	if s.cfg.ModelTokenFile == "" {
		return fmt.Errorf("this shim has no model token file")
	}
	if err := writeTokenAtomically(s.cfg.ModelTokenFile, frame.AccessToken); err != nil {
		return fmt.Errorf("write model access token: %w", err)
	}
	return nil
}

// writeTokenAtomically replaces path with one complete, owner-only token file. The temporary file
// is created beside the target so rename is atomic on the pod's memory-backed state volume.
func writeTokenAtomically(path, token string) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(token); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("rename token file: %w", err)
	}
	return nil
}
