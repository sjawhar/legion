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

// legacyFeedPaths are the two quarantines ownFeedRepository creates: a bare feed moved aside, or
// a symlinked owner moved aside without following its target.
func legacyFeedPaths(feed string) []string {
	return []string{feed + ".legacy", filepath.Join(filepath.Dir(feed)+".legacy", filepath.Base(feed))}
}

// dissociateLegacyClone repacks a legacy alternate clone after workspace-fetch quarantines a
// foreign feed and before provisioning reads the clone. This process holds no provisioning token.
func dissociateLegacyClone(ctx context.Context, run Runner, cloneDir, feed string, log func(string)) error {
	for _, legacy := range legacyFeedPaths(feed) {
		if _, err := os.Lstat(legacy); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("read quarantined feed: %w", err)
		}
		if err := dissociateClone(ctx, run, cloneDir, feed, legacy, log); err != nil {
			return err
		}
		quarantine := legacy
		if legacy == filepath.Join(filepath.Dir(feed)+".legacy", filepath.Base(feed)) {
			quarantine = filepath.Dir(legacy)
		}
		if err := os.RemoveAll(quarantine); err != nil {
			return fmt.Errorf("remove quarantined feed: %w", err)
		}
	}
	return nil
}

// dissociateClone points an old clone at the quarantined feed just long enough to repack every
// borrowed object into the clone, then removes the alternate and verifies connectivity.
func dissociateClone(ctx context.Context, run Runner, cloneDir, feed, legacy string, log func(string)) error {
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
	if err := os.WriteFile(alternates, []byte(filepath.Join(legacy, "objects")+"\n"), 0o600); err != nil {
		return fmt.Errorf("point clone alternates at quarantined feed: %w", err)
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
	log("dissociated the clone from the quarantined feed")
	return nil
}
