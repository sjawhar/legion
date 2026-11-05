package procread

import (
	"fmt"
	"syscall"
)

// UsedBytes is how much of the filesystem holding path is in use: its block count less its free
// blocks, each of the filesystem's own block size, as df reads the same numbers. It is one statfs
// of the mount rather than a walk of the tree, so an issue volume of a million files costs one
// syscall, and it answers for the whole filesystem the path sits on — a path under a mount of its
// own (the issue's volume) reports that mount, and a path that is not reports whatever carries it.
//
// A path that cannot be stat'd is an error naming it, since a volume that is not mounted where the
// sampler was told to look is something to report, not a zero to publish.
func UsedBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64((stat.Blocks - stat.Bfree) * uint64(stat.Bsize)), nil
}
