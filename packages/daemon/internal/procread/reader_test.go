package procread

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests below read the same fixture roots the pure functions' own tests read, through a
// Reader that keeps the descriptors. What they are about is the three rules a kept descriptor
// adds to a read: it answers what the pure function answers, it is never read for a process it
// was not opened on, and it is given up when the caller forgets the process or closes the reader.

// sleepEnv names the test binary re-entered as a child that waits to be killed, which is the one
// real process these tests read: a kept descriptor's behaviour once its process has ended is the
// kernel's and not a fixture's.
const sleepEnv = "LEGION_PROCREAD_TEST_SLEEP"

// TestSleepHelper is that child. It is a test of this package only so that the binary can be
// re-entered as one; with the variable unset it is the empty test it looks like.
func TestSleepHelper(t *testing.T) {
	if os.Getenv(sleepEnv) == "" {
		return
	}
	time.Sleep(time.Minute)
}

// newReader is a reader of this root whose descriptors the test releases when it ends.
func newReader(t *testing.T, root string) *Reader {
	t.Helper()
	reader := NewReader(root, DetectLayout(root))
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("the reader could not be closed: %v", err)
		}
	})
	return reader
}

// descriptorsUnder are the files this process holds open under dir, by the path each descriptor
// was opened from, which /proc/self/fd names as the link target of each. It is how a test says
// what a reader is holding and what Close released. The directory is resolved first, since a
// link under /proc/self/fd names the file absolutely and a fixture root is a relative path.
func descriptorsUnder(t *testing.T, dir string) []string {
	t.Helper()
	resolved, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("%s could not be resolved: %v", dir, err)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("this process's own open descriptors could not be read: %v", err)
	}
	var held []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			// A descriptor closed between the listing and this read is one this
			// process does not hold.
			continue
		}
		if strings.HasPrefix(target, resolved+string(os.PathSeparator)) {
			held = append(held, target)
		}
	}
	return held
}

// A reader answers what the pure functions answer, over each of the three fixture profiles and
// through every file it keeps: the pod's meminfo, the totals of its layout, and each process's
// stat and smaps. Every reading is taken twice, because the second is the one that comes through
// the descriptor the first opened, and a kept descriptor that answered yesterday's bytes would
// be a sampler reporting a pod that has moved on.
func TestTheReaderAnswersWhatThePureFunctionsAnswer(t *testing.T) {
	for _, name := range []string{gvisor, microvm, runcnode} {
		t.Run(name, func(t *testing.T) {
			root := profile(name)
			reader := newReader(t, root)
			layout := DetectLayout(root)
			for round := range 2 {
				info, err := reader.MemInfo()
				want, wantErr := ReadMemInfo(root)
				if (err == nil) != (wantErr == nil) {
					t.Fatalf("round %d: Reader.MemInfo() = %v, ReadMemInfo() = %v", round, err, wantErr)
				}
				if err == nil && info != want {
					t.Errorf("round %d: Reader.MemInfo() = %+v, want %+v", round, info, want)
				}
				seconds, err := reader.PodCPUSeconds()
				if err != nil {
					t.Fatalf("round %d: Reader.PodCPUSeconds() = %v", round, err)
				}
				if want, err := PodCPUSeconds(root, layout); err != nil || !sameSeconds(seconds, want) {
					t.Errorf("round %d: Reader.PodCPUSeconds() = %g, want %g (%v)", round, seconds, want, err)
				}
				memory, err := reader.PodMemoryBytes()
				if err != nil {
					t.Fatalf("round %d: Reader.PodMemoryBytes() = %v", round, err)
				}
				if want, err := PodMemoryBytes(root, layout); err != nil || memory != want {
					t.Errorf("round %d: Reader.PodMemoryBytes() = %d, want %d (%v)", round, memory, want, err)
				}
				pids, err := PIDs(root)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("round %d: PIDs() = %v", round, err)
				}
				for _, pid := range pids {
					stat, err := reader.Stat(pid)
					wantStat, wantErr := ReadStat(root, pid)
					if (err == nil) != (wantErr == nil) {
						t.Fatalf("round %d: Reader.Stat(%d) = %v, ReadStat() = %v", round, pid, err, wantErr)
					}
					if err == nil && stat != wantStat {
						t.Errorf("round %d: Reader.Stat(%d) = %+v, want %+v", round, pid, stat, wantStat)
					}
					anonymous, err := reader.Anonymous(pid)
					wantAnonymous, wantErr := ReadAnonymous(root, pid)
					if (err == nil) != (wantErr == nil) {
						t.Fatalf("round %d: Reader.Anonymous(%d) = %v, ReadAnonymous() = %v",
							round, pid, err, wantErr)
					}
					if err == nil && anonymous != wantAnonymous {
						t.Errorf("round %d: Reader.Anonymous(%d) = %d, want %d",
							round, pid, anonymous, wantAnonymous)
					}
				}
			}
		})
	}
}

// A reading the file does not hold is the pure function's own answer here too: a stat line no
// reader can parse is a refusal naming the file, which a collector counts, and never a Stat of
// zeroes it would publish as a process that spends nothing.
func TestTheReaderRefusesAStatLineItCannotRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proc", "7", "stat")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("nothing a reader of /proc can parse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stat, err := newReader(t, root).Stat(7)
	if err == nil {
		t.Fatalf("Reader.Stat() = %+v, want a refusal", stat)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal is %q, want the file %s named", err, path)
	}
	if stat != (Stat{}) {
		t.Errorf("Reader.Stat() = %+v beside its error, want the zero Stat", stat)
	}
}

// A pid the kernel has handed to another process is read as the process that wears it now. The
// descriptor the reader holds was opened on the process that wore it before, and in this pod
// that descriptor goes on answering: `0 (sleep) X …`, the line the process exited on, with its
// pid field zeroed and its state X (INFRA-1208, measured 2026-11-05). So the fixture models the
// kernel — the file the descriptor holds is written to that line, and the process that wears the
// pid now stands at the path — and the reader must answer the new process, never the old one and
// never the exited line.
func TestAPidTheKernelReusedIsReadAsTheNewProcess(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proc", "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stat")
	const before = "7 (go) S 1 7 7 0 -1 4194304 0 0 0 0 5000 0 0 0 20 0 1 0 4000 10000 900 0 0 0\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := newReader(t, root)
	first, err := reader.Stat(7)
	if err != nil {
		t.Fatalf("Reader.Stat(7) = %v", err)
	}
	if first.Comm != "go" || first.StartTicks != 4000 || first.UserTicks != 5000 {
		t.Fatalf("Reader.Stat(7) = %+v, want the build that wore the pid first", first)
	}

	// The kernel's side of a pid handed on: what the open descriptor answers is the exited
	// line of the process it was opened on, and the path is another process's file.
	const exited = "0 (go) X 1 7 7 0 -1 4194304 0 0 0 0 5000 0 0 0 20 0 0 0 4000 0 0 0 0 0\n"
	if err := os.WriteFile(path, []byte(exited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	const after = "7 (gopls) S 1 7 7 0 -1 4194304 0 0 0 0 3 0 0 0 20 0 1 0 9000 10000 120 0 0 0\n"
	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := reader.Stat(7)
	if err != nil {
		t.Fatalf("Reader.Stat(7) after the pid was handed on = %v", err)
	}
	if second.PID != 7 || second.Comm != "gopls" || second.StartTicks != 9000 || second.UserTicks != 3 {
		t.Errorf("Reader.Stat(7) = %+v, want the language server that wears the pid now", second)
	}
	// And the reading is the new process's own, which is what tells a caller the pid was
	// handed on at all: a walk reads the moment a process began and charges a lifetime where
	// it has moved.
	if second.StartTicks == first.StartTicks {
		t.Error("the reading carries the starting moment of the process that wore the pid before it")
	}
}

// A descriptor the reader holds on a process that has ended is never read as that process. This
// is the one test here that reads the real /proc, with a child of its own: what a kept descriptor
// answers once its process is gone is the kernel's business, and in this pod it answers the line
// the process exited on rather than failing, which is why the reader decides on the reading and
// not on an error. Where nothing wears the pid, opening the path again is fs.ErrNotExist, which
// is what a collector skips a process on.
func TestAProcessThatEndedIsNotExistThroughAKeptDescriptor(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("this test binary could not be found: %v", err)
	}
	child := exec.Command(self, "-test.run=^TestSleepHelper$")
	child.Env = append(os.Environ(), sleepEnv+"=1")
	if err := child.Start(); err != nil {
		t.Fatalf("the child this test reads could not be started: %v", err)
	}
	pid := child.Process.Pid
	ended := false
	t.Cleanup(func() {
		if ended {
			return
		}
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	reader := newReader(t, "/")
	alive, err := reader.Stat(pid)
	if err != nil {
		t.Fatalf("Reader.Stat(%d) of a living child = %v", pid, err)
	}
	if alive.PID != pid {
		t.Fatalf("Reader.Stat(%d) = %+v, want the child's own line", pid, alive)
	}
	if _, err := reader.Anonymous(pid); err != nil {
		t.Fatalf("Reader.Anonymous(%d) of a living child = %v", pid, err)
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("the child could not be killed: %v", err)
	}
	_, _ = child.Process.Wait()
	ended = true
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was still there 10 s after the child was reaped", path)
		}
		time.Sleep(5 * time.Millisecond)
	}

	stat, err := reader.Stat(pid)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Reader.Stat(%d) of a process that ended = %+v, %v; want fs.ErrNotExist", pid, stat, err)
	}
	if anonymous, err := reader.Anonymous(pid); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Reader.Anonymous(%d) of a process that ended = %d, %v; want fs.ErrNotExist", pid, anonymous, err)
	}
}

// What a reader holds is bounded: at most keptProcesses processes have descriptors, and a pod
// that forks more than that is read past the bound by path rather than taking a descriptor for
// each. Every process answers the same either way, which is what makes the bound a cost and
// never a correctness.
func TestTheReaderKeepsAtMostItsBoundAndAnswersPastIt(t *testing.T) {
	root := t.TempDir()
	const over = 64
	for pid := 1; pid <= keptProcesses+over; pid++ {
		dir := filepath.Join(root, "proc", strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("%d (sleep) S 1 %d %d 0 -1 4194304 0 0 0 0 %d 0 0 0 20 0 1 0 900 10000 %d 0 0 0\n",
			pid, pid, pid, pid, 100+pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "smaps"),
			[]byte(fmt.Sprintf("00400000-00401000 r-xp 0 fe:01 1 /bin/sleep\nAnonymous: %d kB\n", pid)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reader := newReader(t, root)
	for pid := 1; pid <= keptProcesses+over; pid++ {
		stat, err := reader.Stat(pid)
		if err != nil {
			t.Fatalf("Reader.Stat(%d) = %v", pid, err)
		}
		if want, err := ReadStat(root, pid); err != nil || stat != want {
			t.Fatalf("Reader.Stat(%d) = %+v, want %+v (%v)", pid, stat, want, err)
		}
		anonymous, err := reader.Anonymous(pid)
		if err != nil {
			t.Fatalf("Reader.Anonymous(%d) = %v", pid, err)
		}
		if want := int64(pid) * 1024; anonymous != want {
			t.Fatalf("Reader.Anonymous(%d) = %d, want %d", pid, anonymous, want)
		}
	}
	if held := len(reader.processes); held != keptProcesses {
		t.Errorf("the reader holds %d processes, want its bound of %d", held, keptProcesses)
	}
	// Two files a process, and nothing of the processes past the bound.
	if open, most := len(descriptorsUnder(t, root)), 2*keptProcesses; open > most {
		t.Errorf("the reader holds %d descriptors under %s, want at most %d", open, root, most)
	}

	// The processes the bound left out are read by path, so a pass over them answers the same
	// as one over the processes it kept.
	reader.Forget(1)
	if stat, err := reader.Stat(keptProcesses + over); err != nil || stat.PID != keptProcesses+over {
		t.Errorf("Reader.Stat(%d) past the bound = %+v, %v", keptProcesses+over, stat, err)
	}
}

// Forget gives up what the reader held of one process, which is what keeps a reader of a pod
// that forks and reaps all day holding the processes that exist and no others.
func TestForgetReleasesTheProcessesDescriptors(t *testing.T) {
	root := profile(gvisor)
	reader := newReader(t, root)
	if _, err := reader.Stat(1); err != nil {
		t.Fatalf("Reader.Stat(1) = %v", err)
	}
	if _, err := reader.Anonymous(1); err != nil {
		t.Fatalf("Reader.Anonymous(1) = %v", err)
	}
	if held := len(descriptorsUnder(t, filepath.Join(root, "proc", "1"))); held != 2 {
		t.Fatalf("the reader holds %d of process 1's files open, want its stat and its smaps", held)
	}
	reader.Forget(1)
	if held := descriptorsUnder(t, filepath.Join(root, "proc", "1")); len(held) != 0 {
		t.Errorf("a forgotten process left %v open", held)
	}
	if _, there := reader.processes[1]; there {
		t.Error("a forgotten process is still one the reader holds")
	}
	// A process forgotten and read again is read again from its path, which is what makes a
	// reused pid safe: forgetting costs the next read an open and nothing else.
	if _, err := reader.Stat(1); err != nil {
		t.Errorf("Reader.Stat(1) after it was forgotten = %v", err)
	}
}

// Close releases everything: the pod's own files and every process's, so a collector whose loop
// has ended holds no descriptor of the pod it was reading.
func TestCloseReleasesEveryDescriptorTheReaderHeld(t *testing.T) {
	root := profile(gvisor)
	reader := NewReader(root, DetectLayout(root))
	if _, err := reader.MemInfo(); err != nil {
		t.Fatalf("Reader.MemInfo() = %v", err)
	}
	if _, err := reader.PodCPUSeconds(); err != nil {
		t.Fatalf("Reader.PodCPUSeconds() = %v", err)
	}
	if _, err := reader.PodMemoryBytes(); err != nil {
		t.Fatalf("Reader.PodMemoryBytes() = %v", err)
	}
	for _, pid := range []int{1, 42} {
		if _, err := reader.Stat(pid); err != nil {
			t.Fatalf("Reader.Stat(%d) = %v", pid, err)
		}
		if _, err := reader.Anonymous(pid); err != nil {
			t.Fatalf("Reader.Anonymous(%d) = %v", pid, err)
		}
	}
	// meminfo, the two cgroup files of the v1 layout, and a stat and a smaps for each of the
	// two processes.
	if held := descriptorsUnder(t, root); len(held) != 7 {
		t.Fatalf("the reader holds %d files open, want seven: %v", len(held), held)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Reader.Close() = %v", err)
	}
	if held := descriptorsUnder(t, root); len(held) != 0 {
		t.Errorf("Close left %v open", held)
	}
	if len(reader.processes) != 0 {
		t.Errorf("Close left %d processes held", len(reader.processes))
	}
	if err := reader.Close(); err != nil {
		t.Errorf("a second Close = %v, want nothing left to release", err)
	}
}
