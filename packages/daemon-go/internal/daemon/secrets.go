package daemon

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// secretsDir is where the runtime writes a pane's secret files, under the state directory: the
// boot token as `<claim token>`, every other secret as `<claim token>-<name>`.
const secretsDir = "secrets"

// pruning is store with a claim's secret files removed whenever the claim is written with no
// process — suspended, failed, retired, or between one launch and the next. The runtime writes a
// pane's files and never removes them; the daemon, which knows when a claim's process ends, does
// (as the shipped daemon does — packages/daemon/src/daemon/processes.ts:5258-5280). A launch
// writes its files after the claim is persisted without a locator, so a relaunch's own files are
// never the ones removed.
func pruning(store supervise.Store, dir string, log *slog.Logger) supervise.Store {
	return pruningStore{Store: store, dir: dir, log: log}
}

type pruningStore struct {
	supervise.Store
	dir string
	log *slog.Logger
}

func (s pruningStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	if err := s.Store.PutClaim(ctx, c); err != nil {
		return err
	}
	if c.Locator == nil {
		removeSecretFiles(s.dir, s.log, func(name string) bool { return ownedBy(name, c.Token) })
	}
	return nil
}

// pruneAllBut is boot's half: every file in the secrets directory that no claim with a process
// names goes — what a daemon killed between clearing a locator and removing its files left.
func pruneAllBut(dir string, claims []supervise.Claim, log *slog.Logger) {
	removeSecretFiles(dir, log, func(name string) bool {
		for _, c := range claims {
			if c.Locator != nil && ownedBy(name, c.Token) {
				return false
			}
		}
		return true
	})
}

// ownedBy reports whether a secret file is one of token's: a claim token ends in its role, so no
// other claim's token begins with `<token>-`.
func ownedBy(name string, token claim.Token) bool {
	return name == string(token) || strings.HasPrefix(name, string(token)+"-")
}

// removeSecretFiles removes every file of dir that remove selects. A directory not made yet holds
// nothing; a file that will not go is logged and left for the next prune, never a failed write. A
// subdirectory is never a pane's secret file: the daemon's own provider-env directory
// (config.ProviderEnvDir) lives here, and outlives every claim.
func removeSecretFiles(dir string, log *slog.Logger, remove func(name string) bool) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Error("secrets: list the pane secret files", "dir", dir, "error", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !remove(entry.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Error("secrets: remove a pane secret file", "file", entry.Name(), "error", err)
		}
	}
}
