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

// feedQuarantine is the cross-container hand-off from workspace-fetch to workspace-init.
type feedQuarantine struct {
	feed   string
	remove string
}

// feedQuarantines are the two possible quarantines ownFeedRepository creates under root: a bare
// feed moved aside, or a symlinked owner moved aside without following its target.
func feedQuarantines(feed, root string) [2]feedQuarantine {
	return [2]feedQuarantine{
		{feed: filepath.Join(root, filepath.Base(filepath.Dir(feed)), filepath.Base(feed)), remove: filepath.Join(root, filepath.Base(filepath.Dir(feed)), filepath.Base(feed))},
		{feed: filepath.Join(root, filepath.Base(filepath.Dir(feed)), filepath.Base(feed)), remove: filepath.Join(root, filepath.Base(filepath.Dir(feed)))},
	}
}

// dissociateLegacyClone repacks a legacy alternate clone after workspace-fetch quarantines a
// foreign feed and before provisioning reads the clone. This process holds no provisioning token.
func dissociateLegacyClone(ctx context.Context, run Runner, cloneDir, feed, quarantineRoot string, log func(string)) error {
	if feed == "" {
		return nil
	}
	quarantines := feedQuarantines(feed, quarantineRoot)
	if info, err := os.Lstat(filepath.Dir(quarantines[0].feed)); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		quarantines[0], quarantines[1] = quarantines[1], quarantines[0]
	}
	for _, quarantine := range quarantines {
		if _, err := os.Lstat(quarantine.feed); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("read quarantined feed: %w", err)
		}
		if err := dissociateClone(ctx, run, cloneDir, feed, quarantine.feed, log); err != nil {
			return err
		}
		if err := os.RemoveAll(quarantine.remove); err != nil {
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
	entries := strings.Fields(string(contents))
	if len(entries) != 1 {
		return fmt.Errorf("clone alternates %s names %d object stores; refusing to remove the quarantine", alternates, len(entries))
	}
	current := entries[0]
	original, quarantined := filepath.Join(feed, "objects"), filepath.Join(legacy, "objects")
	if current != original && current != quarantined {
		return fmt.Errorf("clone alternates %s names %q, not the feed or its quarantine", alternates, current)
	}
	if current == original {
		if err := os.WriteFile(alternates, []byte(quarantined+"\n"), 0o600); err != nil {
			return fmt.Errorf("point clone alternates at quarantined feed: %w", err)
		}
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
