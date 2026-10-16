package gcp_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/infra/gcp"
)

// These tests run the bootcmd entry that formats and mounts the data disk as the golden holds it,
// under the shell shellCommand names, with the device's path replaced by a file's. blkid and cmp
// are this machine's (util-linux and GNU diffutils), as on Container-Optimized OS; cmp runs
// through a stub that records its arguments. blockdev reads a block device's size and no file's,
// so a stub prints the file's size; mkfs.ext4, fsck.ext4 and mount are stubs that record their
// arguments and exit as each row says. The ext4 images are made by this machine's mkfs.ext4
// (e2fsprogs): the tests fail, never skip, without it, blkid or cmp.

const dataDevice = "/dev/disk/by-id/google-data"

// sentinel is data written on a disk that no line the entry prints may carry.
const sentinel = "PERSISTED-JETSTREAM-DATA"

// diskRun is one run of the entry: its stdout and stderr, and each stubbed command's arguments.
type diskRun struct {
	stdout, stderr string
	calls          []string
}

// bootcmdEntry returns the one golden bootcmd entry that holds text.
func bootcmdEntry(t *testing.T, text string) string {
	t.Helper()
	var doc gcp.CloudConfig
	require.NoError(t, yaml.Unmarshal([]byte(readGolden(t, "cloud-config.golden")), &doc))
	var found []string
	for _, entry := range doc.Bootcmd {
		if strings.Contains(entry, text) {
			found = append(found, entry)
		}
	}
	require.Len(t, found, 1, "bootcmd entries holding %q", text)
	return found[0]
}

// runEntry runs entry with device in place of the data disk's path. Each name in stubs is a script
// first on PATH that records its arguments and then runs the shell text given (exits, prints or
// runs).
func runEntry(t *testing.T, entry, device string, stubs map[string]string) diskRun {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "calls")
	for name, body := range stubs {
		stub := "#!/bin/sh\nprintf '%s\\n' \"" + name + " $*\" >>\"$RECORD\"\n" + body + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o700))
	}
	require.Equal(t, 1, strings.Count(entry, dataDevice), "the entry names the device once")
	cmd := shellCommand(t, "-c", strings.ReplaceAll(entry, dataDevice, device))
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + systemPath(), "RECORD=" + record}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	run := diskRun{stdout: stdout.String(), stderr: stderr.String()}
	calls, err := os.ReadFile(record)
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
		run.calls = strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n")
	}
	return run
}

// exits, prints and runs are stub bodies: exit with code, print out, or run the program at path
// with the stub's arguments.
func exits(code int) string    { return "exit " + strconv.Itoa(code) }
func prints(out string) string { return "printf '%s\\n' '" + out + "'" }
func runs(path string) string  { return "exec " + path + " \"$@\"" }

// systemPath is this machine's PATH with the directories util-linux and e2fsprogs install into
// added, which an unprivileged user's PATH can leave out.
func systemPath() string {
	return strings.Join([]string{os.Getenv("PATH"), "/usr/sbin", "/sbin"}, string(os.PathListSeparator))
}

// tool returns the path of name on systemPath, failing the test without it.
func tool(t *testing.T, name string) string {
	t.Helper()
	for _, dir := range filepath.SplitList(systemPath()) {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return filepath.Join(dir, name)
		}
	}
	t.Fatalf("these tests need %s on this machine", name)
	return ""
}

// image returns a file of size bytes, all zeros: what a new persistent disk reads as.
func image(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return path
}

// ext4 returns a 16 MiB file holding an ext4 filesystem of 4 KiB blocks, as a hub's disk has, made
// by this machine's mkfs.ext4.
func ext4(t *testing.T) string {
	t.Helper()
	path := image(t, 16<<20)
	out, err := exec.Command(tool(t, "mkfs.ext4"), "-q", "-F", "-b", "4096", path).CombinedOutput()
	require.NoError(t, err, string(out))
	return path
}

// write writes data at offset in the file at path.
func write(t *testing.T, path string, offset int64, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(data, offset)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// zeroedFirstMiB is an ext4 disk holding data at 2 MiB whose first MiB has since been zeroed: the
// filesystem's data outlives what blkid reads.
func zeroedFirstMiB(t *testing.T) string {
	t.Helper()
	disk := ext4(t)
	write(t, disk, 2<<20, []byte(sentinel))
	write(t, disk, 0, make([]byte, 1<<20))
	return disk
}

// blkidExit is what blkid -p exits with on disk: 0 when it finds a filesystem, 2 when it finds
// none.
func blkidExit(t *testing.T, disk string) int {
	t.Helper()
	err := exec.Command(tool(t, "blkid"), "-p", disk).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

// leftUnmounted is the one line the entry prints when it leaves the disk unmounted, for reason.
func leftUnmounted(disk, reason string) string {
	return "envoy-hub: data disk " + disk + " left unmounted: " + reason +
		"; see docs/nats-hub.md, \"Repairing the data disk\"\n"
}

// diskStubs are the stubs a row runs the entry with, for the disk at path: blockdev prints its
// size, cmp runs this machine's, and mkfs.ext4, fsck.ext4 and mount succeed. edits replace them.
func diskStubs(t *testing.T, path string, edits map[string]string) map[string]string {
	t.Helper()
	size := "8388608"
	if info, err := os.Stat(path); err == nil {
		size = strconv.FormatInt(info.Size(), 10)
	}
	stubs := map[string]string{
		"blockdev": prints(size), "cmp": runs(tool(t, "cmp")),
		"mkfs.ext4": exits(0), "fsck.ext4": exits(0), "mount": exits(0),
	}
	for name, body := range edits {
		stubs[name] = body
	}
	return stubs
}

// The disk is formatted only when blkid finds no filesystem on it and the whole device reads as
// zeros, as a new persistent disk does: cmp reads it against /dev/zero to its end, stopping at the
// first byte that is not zero. A disk blkid finds a filesystem on is checked and mounted without a
// read. Any other disk is left unmounted, with one line naming the device and the reason and
// nothing of what the disk holds: a filesystem whose first MiB or primary superblock was zeroed
// still holds its files' blocks, which mkfs.ext4's discard would destroy, and a damaged superblock
// alone e2fsck repairs from a backup.
func TestTheDataDiskIsFormattedOnlyWhenItReadsAsZerosToItsEnd(t *testing.T) {
	entry := bootcmdEntry(t, "blkid -p ")
	calls := func(disk string, steps ...string) []string {
		size := "8388608"
		if info, err := os.Stat(disk); err == nil {
			size = strconv.FormatInt(info.Size(), 10)
		}
		all := map[string]string{
			"blkid":    "blkid -p " + disk,
			"blockdev": "blockdev --getsize64 " + disk,
			"cmp":      "cmp -s -n " + size + " /dev/zero " + disk,
			"mkfs":     "mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard " + disk,
			"fsck":     "fsck.ext4 -p " + disk,
			"mount":    "mount -o discard,defaults " + disk + " /mnt/disks/data",
		}
		out := make([]string, 0, len(steps))
		for _, s := range steps {
			out = append(out, all[s])
		}
		return out
	}
	noZeros := "blkid finds no filesystem on it, and it does not read as zeros to its end"
	for _, row := range []struct {
		name  string
		disk  func(t *testing.T) string
		blkid int // what this machine's blkid -p exits with on the disk; -1 when stubbed
		edits map[string]string
		steps []string
		why   string // the reason the line gives; "" when the disk is mounted
	}{
		{name: "a new disk, all zeros, is formatted", disk: func(t *testing.T) string { return image(t, 8<<20) }, blkid: 2,
			steps: []string{"blockdev", "cmp", "mkfs", "fsck", "mount"}},
		{name: "an intact ext4 disk is mounted without a read", disk: ext4, blkid: 0,
			steps: []string{"fsck", "mount"}},
		{name: "a zeroed first MiB with data at 2 MiB is left unmounted", disk: zeroedFirstMiB, blkid: 2,
			steps: []string{"blockdev", "cmp"}, why: noZeros},
		{name: "data in the last MiB is left unmounted", disk: func(t *testing.T) string {
			disk := image(t, 8<<20)
			write(t, disk, 8<<20-512<<10, []byte(sentinel))
			return disk
		}, blkid: 2, steps: []string{"blockdev", "cmp"}, why: noZeros},
		{name: "a damaged primary superblock is left unmounted", disk: func(t *testing.T) string {
			disk := ext4(t)
			write(t, disk, 1024, make([]byte, 1024))
			return disk
		}, blkid: 2, steps: []string{"blockdev", "cmp"}, why: noZeros},
		{name: "a probe that fails otherwise is left unmounted", disk: func(t *testing.T) string { return image(t, 8<<20) },
			blkid: -1, edits: map[string]string{"blkid": exits(8)}, steps: []string{"blkid"}, why: "blkid -p exited 8"},
		{name: "a disk whose size does not read is left unmounted", disk: func(t *testing.T) string { return image(t, 8<<20) },
			blkid: 2, edits: map[string]string{"blockdev": exits(1)}, steps: []string{"blockdev"},
			why: "blkid finds no filesystem on it, and blockdev --getsize64 exited 1"},
		{name: "a disk that does not read is left unmounted", disk: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "missing")
		}, blkid: 2, steps: []string{"blockdev", "cmp"}, why: "blkid finds no filesystem on it, and cmp exited 2 reading it"},
		{name: "a format that fails leaves the disk unmounted", disk: func(t *testing.T) string { return image(t, 8<<20) }, blkid: 2,
			edits: map[string]string{"mkfs.ext4": exits(1)}, steps: []string{"blockdev", "cmp", "mkfs"}, why: "mkfs.ext4 exited 1"},
		{name: "errors fsck fixed leave the disk mounted", disk: ext4, blkid: 0,
			edits: map[string]string{"fsck.ext4": exits(1)}, steps: []string{"fsck", "mount"}},
		{name: "errors fsck left leave the disk unmounted", disk: ext4, blkid: 0,
			edits: map[string]string{"fsck.ext4": exits(4)}, steps: []string{"fsck"}, why: "fsck.ext4 -p exited 4"},
		{name: "a check fsck could not run leaves the disk unmounted", disk: ext4, blkid: 0,
			edits: map[string]string{"fsck.ext4": exits(8)}, steps: []string{"fsck"}, why: "fsck.ext4 -p exited 8"},
		{name: "a mount that fails leaves the disk unmounted", disk: ext4, blkid: 0,
			edits: map[string]string{"mount": exits(32)}, steps: []string{"fsck", "mount"}, why: "mount exited 32"},
	} {
		t.Run(row.name, func(t *testing.T) {
			disk := row.disk(t)
			if row.blkid >= 0 {
				require.Equal(t, row.blkid, blkidExit(t, disk), "blkid -p on the disk")
			}
			run := runEntry(t, entry, disk, diskStubs(t, disk, row.edits))
			require.Equal(t, calls(disk, row.steps...), run.calls)
			require.Empty(t, run.stdout)
			require.NotContains(t, run.stdout+run.stderr, sentinel, "no line carries what the disk holds")
			if row.why == "" {
				require.Empty(t, run.stderr)
				return
			}
			lines := strings.SplitAfter(run.stderr, "\n")
			require.Equal(t, leftUnmounted(disk, row.why), lines[len(lines)-2], "the last line names the disk and the reason")
			require.Equal(t, 1, strings.Count(run.stderr, "envoy-hub: "), "one line of the entry's own")
			if row.name != "a disk that does not read is left unmounted" {
				require.Equal(t, leftUnmounted(disk, row.why), run.stderr, "the only line")
			}
		})
	}
}

// A disk the entry refuses is not written, so every later boot, as the group recreates the VM on
// the same disk, finds it as the first did and refuses it again: no boot formats it.
func TestARefusedDataDiskIsNeverFormattedOnALaterBoot(t *testing.T) {
	entry := bootcmdEntry(t, "blkid -p ")
	disk := zeroedFirstMiB(t)
	before, err := os.ReadFile(disk)
	require.NoError(t, err)
	for boot := 1; boot <= 2; boot++ {
		run := runEntry(t, entry, disk, diskStubs(t, disk, nil))
		require.Equal(t, []string{
			"blockdev --getsize64 " + disk,
			"cmp -s -n " + strconv.Itoa(len(before)) + " /dev/zero " + disk,
		}, run.calls, "boot %d: no mkfs.ext4, fsck.ext4 or mount", boot)
		require.Equal(t, leftUnmounted(disk, "blkid finds no filesystem on it, and it does not read as zeros to its end"),
			run.stderr, "boot %d", boot)
		after, err := os.ReadFile(disk)
		require.NoError(t, err)
		require.Equal(t, before, after, "boot %d leaves the disk as it was", boot)
	}
}
