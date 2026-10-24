package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dissociateClone copies objects from feed into an old shared clone before Fetch removes that feed.
// New clones have no alternates. A clone that borrows another store is left alone because removing
// this feed cannot remove that history.
func dissociateClone(ctx context.Context, run Runner, cloneDir, feed string, log func(string)) error {
	if cloneDir == "" {
		return nil
	}
	gitDir := filepath.Join(cloneDir, ".git")
	info, err := os.Lstat(gitDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read clone git directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("clone git directory %s is not a directory", gitDir)
	}
	alternates := filepath.Join(gitDir, "objects", "info", "alternates")
	info, err = os.Lstat(alternates)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read clone alternates: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("clone alternates %s is not a regular file", alternates)
	}
	contents, err := os.ReadFile(alternates)
	if err != nil {
		return fmt.Errorf("read clone alternates: %w", err)
	}
	if strings.TrimSpace(string(contents)) != filepath.Join(feed, "objects") {
		return nil
	}
	argv := []string{"git", "--git-dir=" + gitDir, "repack", "-a", "-d"}
	result, err := runCommandTimeout(ctx, run, argv, isolatedGitConfig, "", CloneTimeout)
	if _, err := checkedResult(argv, result, err); err != nil {
		return err
	}
	if err := os.Remove(alternates); err != nil {
		return fmt.Errorf("remove clone alternates: %w", err)
	}
	argv = []string{"git", "--git-dir=" + gitDir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"}
	result, err = runCommandTimeout(ctx, run, argv, isolatedGitConfig, "", CloneTimeout)
	if _, err := checkedResult(argv, result, err); err != nil {
		return err
	}
	log("dissociated the clone from the feed before replacing it")
	return nil
}
