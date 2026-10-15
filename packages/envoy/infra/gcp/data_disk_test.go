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

// These tests run the bootcmd entries that format and mount the data disk as the golden holds
// them, under /bin/sh, with the device's path replaced by a file's. blkid is this machine's
// (util-linux), as on Container-Optimized OS; mkfs.ext4, fsck.ext4 and mount are stubs first on
// PATH that record their arguments. Building the ext4 images needs this machine's mkfs.ext4
// (e2fsprogs): the tests fail, never skip, without it or blkid.

const dataDevice = "/dev/disk/by-id/google-data"

// diskRun is one run of a bootcmd entry: its stderr, and each stubbed command's arguments.
type diskRun struct {
	stderr string
	calls  []string
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

// runEntry runs entry with device in place of the data disk's path, and stubs on PATH: each name in
// stubs is a script that records its arguments and exits with the code given.
func runEntry(t *testing.T, entry, device string, stubs map[string]int) diskRun {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "calls")
	for name, code := range stubs {
		stub := "#!/bin/sh\nprintf '%s\\n' \"" + name + " $*\" >>\"$RECORD\"\nexit " + strconv.Itoa(code) + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o700))
	}
	require.Equal(t, 1, strings.Count(entry, dataDevice), "the entry names the device once")
	cmd := exec.Command("/bin/sh", "-c", strings.ReplaceAll(entry, dataDevice, device))
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + systemPath(), "RECORD=" + record}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	run := diskRun{stderr: stderr.String()}
	calls, err := os.ReadFile(record)
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
		run.calls = strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n")
	}
	return run
}

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

// ext4 returns an 8 MiB file holding an ext4 filesystem, made by this machine's mkfs.ext4.
func ext4(t *testing.T) string {
	t.Helper()
	path := image(t, 8<<20)
	out, err := exec.Command(tool(t, "mkfs.ext4"), "-q", "-F", path).CombinedOutput()
	require.NoError(t, err, string(out))
	return path
}

// The disk is formatted only when blkid finds no filesystem on it and it reads, whole, as zeros, as
// a new persistent disk does. Anything else is left as it is, with a line on stderr: a filesystem
// whose primary superblock is damaged reads to blkid as no filesystem, yet e2fsck can repair it
// from a backup superblock, and mkfs.ext4's discard would make that impossible.
func TestTheDataDiskIsFormattedOnlyWhenItIsBlank(t *testing.T) {
	blkid := tool(t, "blkid")
	format := bootcmdEntry(t, "mkfs.ext4 ")
	mkfs := "mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "

	t.Run("a blank disk", func(t *testing.T) {
		disk := image(t, 2<<20)
		run := runEntry(t, format, disk, map[string]int{"mkfs.ext4": 0})
		require.Equal(t, []string{mkfs + disk}, run.calls)
		require.Empty(t, run.stderr)
	})
	t.Run("a disk holding a filesystem", func(t *testing.T) {
		run := runEntry(t, format, ext4(t), map[string]int{"mkfs.ext4": 0})
		require.Empty(t, run.calls)
		require.Empty(t, run.stderr)
	})
	t.Run("a filesystem whose primary superblock is damaged", func(t *testing.T) {
		disk := ext4(t)
		f, err := os.OpenFile(disk, os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteAt(make([]byte, 1024), 1024)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		probe := exec.Command(blkid, "-p", disk)
		require.Error(t, probe.Run())
		require.Equal(t, 2, probe.ProcessState.ExitCode(), "blkid finds no filesystem on it")

		run := runEntry(t, format, disk, map[string]int{"mkfs.ext4": 0})
		require.Empty(t, run.calls)
		require.Equal(t, "envoy-infra: "+disk+" holds data but no filesystem blkid knows; not formatting it\n", run.stderr)
	})
	t.Run("a device that is not there", func(t *testing.T) {
		disk := filepath.Join(t.TempDir(), "missing")
		run := runEntry(t, format, disk, map[string]int{"mkfs.ext4": 0})
		require.Empty(t, run.calls)
		require.Equal(t, "envoy-infra: "+disk+" does not read; not formatting it\n", run.stderr)
	})
	t.Run("a probe that fails otherwise", func(t *testing.T) {
		disk := image(t, 2<<20)
		run := runEntry(t, format, disk, map[string]int{"mkfs.ext4": 0, "blkid": 8})
		require.Equal(t, []string{"blkid -p " + disk}, run.calls)
		require.Equal(t, "envoy-infra: blkid -p "+disk+" exited 8; not formatting it\n", run.stderr)
	})
}

// The disk is checked with fsck.ext4 -p before it is mounted, and mounted only when fsck left no
// error it could not fix: a disk left unmounted keeps NATS and Tailscale down, since both units
// refuse to start unless the data disk is mounted.
func TestTheDataDiskIsMountedOnlyWhenFsckLeavesNoErrors(t *testing.T) {
	entry := bootcmdEntry(t, "fsck.ext4 ")
	disk := image(t, 2<<20)
	mount := "mount -o discard,defaults " + disk + " /mnt/disks/data"
	for _, row := range []struct {
		fsck  int
		calls []string
		log   string
	}{
		{0, []string{"fsck.ext4 -p " + disk, mount}, ""},
		{1, []string{"fsck.ext4 -p " + disk, mount}, ""},
		{4, []string{"fsck.ext4 -p " + disk}, "envoy-infra: fsck.ext4 -p " + disk + " exited 4; not mounting it\n"},
		{8, []string{"fsck.ext4 -p " + disk}, "envoy-infra: fsck.ext4 -p " + disk + " exited 8; not mounting it\n"},
	} {
		run := runEntry(t, entry, disk, map[string]int{"fsck.ext4": row.fsck, "mount": 0})
		require.Equal(t, row.calls, run.calls, "fsck exits %d", row.fsck)
		require.Equal(t, row.log, run.stderr, "fsck exits %d", row.fsck)
	}
}
