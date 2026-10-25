package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ensureCloneConnectivity refuses an old clone whose refs still name objects from a feed that was
// replaced. It runs before Jujutsu reads the clone, so provisioning names each broken ref instead
// of leaving Jujutsu to report an opaque bad object failure.
func ensureCloneConnectivity(ctx context.Context, run Runner, cloneDir string) error {
	borrows, err := cloneBorrowsObjects(cloneDir)
	if err != nil {
		return fmt.Errorf("read clone alternates: %w", err)
	}
	if !borrows {
		return nil
	}
	argv := []string{"git", "--git-dir=" + filepath.Join(cloneDir, ".git"), "fsck", "--connectivity-only", "--no-dangling", "--no-progress"}
	result, err := runCommand(ctx, run, argv, nil, "")
	if err != nil {
		return fmt.Errorf("check clone connectivity: %w", err)
	}
	if result.ExitCode == 0 && !result.TimedOut {
		return nil
	}
	if refs := missingObjectRefs(result.Stderr + "\n" + result.Stdout); len(refs) > 0 {
		return fmt.Errorf("the clone lost borrowed history at %s; rebuild the clone before provisioning again", strings.Join(refs, ", "))
	}
	return commandFailure(argv, result)
}

// ensureJujutsuConnectivity detects lost history that Git cannot see because it is named only by
// Jujutsu's operation view. The operation log is durable state, so no repair is attempted here.
func ensureJujutsuConnectivity(ctx context.Context, run Runner, cloneDir string) error {
	borrows, err := cloneBorrowsObjects(cloneDir)
	if err != nil {
		return fmt.Errorf("read clone alternates: %w", err)
	}
	if !borrows {
		return nil
	}
	argv, err := onClone(cloneDir, "log", "-r", "visible_heads()", "--no-graph", "-T", "commit_id")
	if err != nil {
		return err
	}
	result, err := runCommand(ctx, run, argv, nil, "")
	if err != nil {
		return fmt.Errorf("check Jujutsu's operation view: %w", err)
	}
	if result.ExitCode == 0 && !result.TimedOut {
		return nil
	}
	objects := objectIDs(result.Stderr + "\n" + result.Stdout)
	if len(objects) > 0 {
		return fmt.Errorf("the clone lost borrowed history in Jujutsu's operation view at %s; rebuild the clone before provisioning again", strings.Join(objects, ", "))
	}
	return commandFailure(argv, result)
}

var objectID = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

func objectIDs(output string) []string {
	ids := objectID.FindAllString(output, -1)
	sort.Strings(ids)
	return slices.Compact(ids)
}

func cloneBorrowsObjects(cloneDir string) (bool, error) {
	alternates := filepath.Join(cloneDir, ".git", "objects", "info", "alternates")
	info, err := os.Lstat(alternates)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("clone alternates %s is not a regular file", alternates)
	}
	return true, nil
}

// missingObjectRefs extracts the refs Git fsck names when their object is unavailable. Git can
// report a ref more than once for its pointer and reflog, so the result is sorted and de-duplicated.
func missingObjectRefs(output string) []string {
	var refs []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimPrefix(line, "error: ")
		ref, detail, found := strings.Cut(line, ": ")
		if !found || !strings.HasPrefix(ref, "refs/") {
			continue
		}
		if strings.Contains(detail, "invalid sha1 pointer") || strings.Contains(detail, "invalid reflog entry") {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	return slices.Compact(refs)
}
