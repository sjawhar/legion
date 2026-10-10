package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// createdFile is where provisioning records the moment it created a workspace: in the workspace's
// own .jj directory, which jj never snapshots, so the record is never committed, and which goes with
// the workspace, so a workspace created again records its own creation.
const createdFile = "legion-created"

// createdRecordLimit bounds how much of the record Created reads: one RFC 3339 instant and a newline.
const createdRecordLimit = 128

// RecordCreated records at, in UTC, as the moment the workspace at dir was created.
func RecordCreated(dir string, at time.Time) error {
	path := filepath.Join(dir, ".jj", createdFile)
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		return fmt.Errorf("record the creation of workspace %s: %w", dir, err)
	}
	return nil
}

// Created is the moment RecordCreated recorded for the workspace at dir. ok is false when it holds
// none: no workspace is there, or provisioning created it before it kept the record. The record is
// on the tree volume, which every agent of the tree can write, so it is opened without following a
// link or blocking on a special file and read only up to createdRecordLimit, and anything but one
// RFC 3339 instant is refused, naming the file.
func Created(dir string) (at time.Time, ok bool, err error) {
	path := filepath.Join(dir, ".jj", createdFile)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, false, fmt.Errorf("the creation record %s is not a regular file (%s)", path, info.Mode().Type())
	}
	body, err := io.ReadAll(io.LimitReader(file, createdRecordLimit))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the creation record %s: %w", path, err)
	}
	at, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(body)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("the creation record %s holds %q, not an RFC 3339 instant", path, body)
	}
	return at, true, nil
}
