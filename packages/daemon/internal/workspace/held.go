package workspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// heldFile is where provisioning records the commits a shared clone held the moment it was made: in
// the clone's own .jj directory, which jj never snapshots and which goes with the clone. It is
// written once, before the clone is moved into place (ensureRepoClone), and never again, so what it
// names was on the clone before any role of its pod wrote a commit.
const heldFile = "legion-held"

// heldRecordLimit bounds how much of the record Held reads: a 64-hex commit id and a newline for
// each of a million visible heads.
const heldRecordLimit = 65 << 20

// heldRevset is the commits the record names: every head jj sees in the clone, which is every
// commit any ref it imported names, the branches, remote-tracking refs and tags the clone came with.
// Every commit the clone held is an ancestor of one of them.
const heldRevset = "visible_heads()"

// RecordHeld records, in the clone at cloneDir, every visible head jj sees there (heldRevset), as
// the commits the clone held when it was made. Provisioning runs it once, on the new clone before
// it is moved into place; a check of the pod's work (internal/rescue) takes a commit any of them
// contains as one the pod did not make, whatever its date says.
func RecordHeld(ctx context.Context, run Runner, cloneDir string) error {
	listed, err := runCheckedOnClone(ctx, run, cloneDir, "log", "--no-graph", "-r", heldRevset, "-T", `commit_id ++ "\n"`)
	if err != nil {
		return fmt.Errorf("record the commits the clone %s holds: %w", cloneDir, err)
	}
	var body strings.Builder
	for _, commit := range strings.Fields(listed.Stdout) {
		if !commitID.MatchString(commit) {
			return fmt.Errorf("record the commits the clone %s holds: jj listed %q, not a commit id", cloneDir, commit)
		}
		body.WriteString(commit + "\n")
	}
	path := filepath.Join(cloneDir, ".jj", heldFile)
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		return fmt.Errorf("record the commits the clone %s holds: %w", cloneDir, err)
	}
	return nil
}

// Held is the commits RecordHeld recorded for the clone at cloneDir. ok is false when it holds no
// record: provisioning made the clone before it kept one. The record is on the pod's disk, which
// every role of the pod can write, so it is opened without following a link or blocking on a
// special file, read only up to heldRecordLimit, and anything but one commit id a line is refused,
// naming the file.
func Held(cloneDir string) (commits []string, ok bool, err error) {
	path := filepath.Join(cloneDir, ".jj", heldFile)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the clone's record of what it held %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("read the clone's record of what it held %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("the clone's record of what it held %s is not a regular file (%s)", path, info.Mode().Type())
	}
	if info.Size() > heldRecordLimit {
		return nil, false, fmt.Errorf("the clone's record of what it held %s is %d bytes, more than %d", path, info.Size(), heldRecordLimit)
	}
	scanner := bufio.NewScanner(io.LimitReader(file, heldRecordLimit))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !commitID.MatchString(line) {
			return nil, false, fmt.Errorf("the clone's record of what it held %s holds %q, not a commit id", path, line)
		}
		commits = append(commits, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("read the clone's record of what it held %s: %w", path, err)
	}
	return commits, true, nil
}
