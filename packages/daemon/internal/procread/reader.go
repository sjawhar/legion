package procread

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// This file is the package's one stateful reader, beside the pure functions the rest of it is: a
// Reader keeps the descriptors of the files a sampler reads again and again and reads each of
// them through the descriptor it already holds, rather than opening the path every pass.
//
// Under this pod's gVisor, opening a path is the whole cost of a /proc read and reading an open
// one is free (INFRA-1208, measured 2026-11-05, 400 iterations of each against time.process_time):
//
//	/proc/meminfo     open+read+close 0.125 ms      kept fd, pread 0.000 ms
//	/proc/self/stat   open+read+close 0.050 ms      kept fd, pread 0.000 ms
//
// Every file is read with one pread from offset 0, which is one syscall and no seek, into a
// buffer the whole of it fits in. A smaps that does not fit is read on from where that pread
// stopped through a seek rather than a second pread, because a read of a /proc file past its
// beginning makes the kernel write the whole file out again: over the smaps of this pod's own
// sampler that is 0.195 ms through a kept descriptor against the 0.405 ms an open of the path
// costs (measured the same day, 2,000 iterations of each).

// keptProcesses is the most processes one Reader holds descriptors for: 512, which is at most
// 1,024 descriptors of the 1,048,576 this pod allows (INFRA-1208, measured 2026-11-05). It is a
// few hundred because that is the size of the process table a sampler reads again and again — the
// cost check's own pod is 200 processes and an issue pod runs a dozen — and a pod that forks
// thousands is read past the bound by path, which costs what it costs today and exhausts nothing.
const keptProcesses = 512

// fileBuffer is where a small file is read whole with one pread. Every file read this way is a
// line or forty of /proc or of the cgroup filesystem: a stat line is 400 bytes and a meminfo 1.5
// kilobytes. A file that fills it is read again into a buffer twice the size, so the number is
// what costs one syscall rather than what anything depends on.
const fileBuffer = 8 << 10

// smapsBuffer is the buffer one smaps is scanned through, which is also how much of it one read
// syscall takes: 64 KiB is about a hundred mappings of this pod's own smaps, where a mapping runs
// to 633 bytes (INFRA-1208, measured 2026-11-05).
const smapsBuffer = 64 << 10

// Reader is the files a sampler reads every pass, kept open: <root>/proc/meminfo, the two cgroup
// files a layout holds a pod's totals in, and the stat and the smaps of each process a caller is
// holding. Every reading it answers is the reading the pure function of the same name answers;
// what it saves is the open, which under gVisor is the whole cost of the read.
//
// A descriptor is opened on the first read of its file and kept until the caller forgets the
// process (Forget) or the reader is closed (Close). One reader serves one collector's passes,
// from one goroutine, as one Walk serves one /proc: nothing here takes a lock.
//
// A pid the kernel has handed to another process is never read through the descriptor of the
// process that wore it before. In this pod a descriptor held on a dead process's stat goes on
// answering, with the line that process exited on — the pid field reads 0 and the state X
// (INFRA-1208, measured 2026-11-05) — and its smaps answers no bytes at all; another kernel
// answers ESRCH. So a reading that is not the process asked for is no reading: the reader drops
// that descriptor, opens the path again, and answers what the path holds now, which is the new
// process where the pid was reused and fs.ErrNotExist where nothing wears it.
type Reader struct {
	root   string
	layout Layout

	// meminfo, memory and cpu are the files a sampler reads again and again: /proc/meminfo,
	// and the two the layout keeps a pod's totals in, each read on its own rate. Under
	// LayoutNone the two cgroup files have no path and are never opened.
	meminfo, memory, cpu kept

	// processes are the per-process descriptors, at most keptProcesses of them.
	processes map[int]*keptProcess

	// buf is the one buffer every small file is read into, mappings the one a process's
	// smaps is read into, and lines the reader a smaps too long for that buffer is scanned
	// through, so that a pass over a pod's processes allocates none of them.
	buf      []byte
	mappings []byte
	lines    *bufio.Reader
}

// kept is one file the reader holds open: the path it was opened from, which is what it is opened
// from again when the descriptor stops answering for the process it was opened on, the descriptor
// itself, nil until the first read of it, and that descriptor's number, which is what a pread is
// made on.
type kept struct {
	path string
	file *os.File
	fd   int
}

// keptProcess is the two files a sampler reads again and again of one process.
type keptProcess struct {
	stat, smaps kept
}

// NewReader is a reader of the files under root — "/" in a pod, a fixture directory in a test —
// whose pod totals this layout holds. It opens nothing: every descriptor is opened on the first
// read of its own file, so a reader of a root with no cgroup filesystem costs nothing for the
// files it will never be asked for.
func NewReader(root string, layout Layout) *Reader {
	mount := filepath.Join(root, "sys", "fs", "cgroup")
	r := &Reader{
		root:      root,
		layout:    layout,
		meminfo:   kept{path: filepath.Join(root, "proc", "meminfo")},
		processes: make(map[int]*keptProcess),
		buf:       make([]byte, fileBuffer),
		mappings:  make([]byte, smapsBuffer),
	}
	switch layout {
	case LayoutV1:
		r.memory.path = filepath.Join(mount, "memory", "memory.usage_in_bytes")
		r.cpu.path = filepath.Join(mount, "cpuacct", "cpuacct.usage")
	case LayoutV2:
		r.memory.path = filepath.Join(mount, "memory.current")
		r.cpu.path = filepath.Join(mount, "cpu.stat")
	}
	return r
}

// MemInfo is <root>/proc/meminfo's AnonPages and Shmem in bytes, as ReadMemInfo answers them,
// read through the descriptor this reader keeps on the file.
func (r *Reader) MemInfo() (MemInfo, error) {
	raw, err := r.read(&r.meminfo)
	if err != nil {
		return MemInfo{}, err
	}
	var info MemInfo
	wanted := []struct {
		into   *int64
		prefix []byte
	}{
		{&info.AnonPages, []byte("AnonPages:")},
		{&info.Shmem, []byte("Shmem:")},
	}
	for line := range bytes.Lines(raw) {
		for _, want := range wanted {
			rest, found := bytes.CutPrefix(line, want.prefix)
			if !found {
				continue
			}
			size, err := kilobytes(rest)
			if err != nil {
				return MemInfo{}, fmt.Errorf("%s: malformed %s line: %s",
					r.meminfo.path, bytes.TrimSuffix(want.prefix, []byte(":")), bytes.TrimSpace(line))
			}
			*want.into = size
		}
	}
	return info, nil
}

// PodCPUSeconds is the CPU the pod has spent at the cgroup mount root, as PodCPUSeconds answers
// it for this reader's layout, read through the descriptor it keeps on the file that holds it.
func (r *Reader) PodCPUSeconds() (float64, error) {
	switch r.layout {
	case LayoutV1:
		nanoseconds, err := r.number(&r.cpu)
		if err != nil {
			return 0, err
		}
		return float64(nanoseconds) / 1e9, nil
	case LayoutV2:
		microseconds, err := r.key(&r.cpu, "usage_usec")
		if err != nil {
			return 0, err
		}
		return float64(microseconds) / 1e6, nil
	}
	return 0, nil
}

// PodMemoryBytes is the memory the pod holds at the cgroup mount root, as PodMemoryBytes answers
// it for this reader's layout, read through the descriptor it keeps on that file. It is read
// apart from the CPU beside it because it costs a gVisor pod some twenty times as much
// (PodCPUSeconds).
func (r *Reader) PodMemoryBytes() (int64, error) {
	switch r.layout {
	case LayoutV1, LayoutV2:
		return r.number(&r.memory)
	}
	return 0, nil
}

// Stat is <root>/proc/<pid>/stat parsed, as ReadStat answers it, read through the descriptor this
// reader keeps on that process's file. A process that ended is fs.ErrNotExist, which is what the
// caller tells it by and skips, and a line that cannot be parsed is a refusal naming the file.
//
// The descriptor is trusted only while the line it answers is the process asked for. A pid the
// kernel has handed on answers the line the process before it exited on, whose pid field is not
// this pid, and there the path is opened again and the process that wears the pid now is read.
func (r *Reader) Stat(pid int) (Stat, error) {
	held := r.process(pid)
	if held == nil {
		return ReadStat(r.root, pid)
	}
	if held.stat.file != nil {
		if stat, err := r.statOf(&held.stat); err == nil && stat.PID == pid {
			return stat, nil
		}
		held.stat.close()
	}
	if err := r.open(&held.stat); err != nil {
		r.forgetEmpty(pid, held)
		return Stat{}, err
	}
	return r.statOf(&held.stat)
}

// statOf is the stat line this descriptor holds, parsed as ReadStat parses it.
func (r *Reader) statOf(k *kept) (Stat, error) {
	raw, err := r.read(k)
	if err != nil {
		return Stat{}, err
	}
	stat, err := ParseStat(string(raw))
	if err != nil {
		return Stat{}, fmt.Errorf("%s: %w", k.path, err)
	}
	return stat, nil
}

// Anonymous is the bytes of anonymous memory <root>/proc/<pid>/smaps accounts to the process, as
// ReadAnonymous answers them, summed as the file arrives through the descriptor this reader keeps
// on it. It is the costliest read the sampler makes, and the one an open costs most.
//
// It is taken with one pread from the start, into a buffer a smaps of a hundred mappings fits
// in. A smaps longer than that is read on from where the pread ended, through a seek and not a
// pread at that offset: a read of this file past its beginning makes the kernel write the whole
// of it out again, which for a file of several buffers is the cost of reading it that many
// times (INFRA-1208, measured 2026-11-05).
//
// A descriptor whose process has ended answers no bytes at all, where a live process always has
// its own program mapped, so a read that finds nothing is taken as no reading: the path is opened
// again and read. A kernel thread, which holds no mapping at all, is read twice for its nothing.
func (r *Reader) Anonymous(pid int) (int64, error) {
	held := r.process(pid)
	if held == nil {
		return ReadAnonymous(r.root, pid)
	}
	if held.smaps.file != nil {
		if total, read, err := r.anonymousOf(&held.smaps); err == nil && read > 0 {
			return total, nil
		}
		held.smaps.close()
	}
	if err := r.open(&held.smaps); err != nil {
		r.forgetEmpty(pid, held)
		return 0, err
	}
	total, _, err := r.anonymousOf(&held.smaps)
	return total, err
}

// anonymousOf sums the Anonymous line of every mapping this descriptor holds, and answers how
// many bytes of file it read on the way: a descriptor whose process has gone reads none.
func (r *Reader) anonymousOf(k *kept) (int64, int64, error) {
	n, err := pread(k.fd, r.mappings, 0)
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for line := range bytes.Lines(r.mappings[:n]) {
		switch err := addAnonymous(&total, line, k.path); {
		case err == nil:
		case errors.Is(err, errTruncated):
			return total, int64(n), nil
		default:
			return 0, int64(n), err
		}
	}
	if n < len(r.mappings) {
		return total, int64(n), nil
	}
	// A smaps longer than the buffer: the rest of it is read on from where the pread
	// stopped, which is a seek and then the file as it comes.
	read := int64(n)
	if _, err := k.file.Seek(read, io.SeekStart); err != nil {
		return 0, read, err
	}
	if r.lines == nil {
		r.lines = bufio.NewReaderSize(k.file, smapsBuffer)
	} else {
		r.lines.Reset(k.file)
	}
	// Every line is the reader's own bytes and no string is made of it: the smaps of a large
	// process runs to thousands of lines, and a string for each would be an allocation for
	// each. A line longer than the buffer — which no smaps writes — is read in pieces, and
	// only the piece the line begins with can carry the prefix.
	for {
		line, err := r.lines.ReadSlice('\n')
		read += int64(len(line))
		switch bad := addAnonymous(&total, line, k.path); {
		case bad == nil:
		case errors.Is(bad, errTruncated):
			return total, read, nil
		default:
			return 0, read, bad
		}
		switch {
		case err == nil || errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return total, read, nil
		default:
			return 0, read, err
		}
	}
}

// addAnonymous adds what one smaps line accounts to the process, and nothing for a line that is
// no Anonymous line. A line that is one and holds no size at all is the end of a read the
// kernel truncated — a process unmaps and exits while its smaps is being written, and gVisor was
// measured writing a bare "Anonymous:" for such a mapping (INFRA-1208, 2026-11-05) — so it ends
// the sum with what the pass had read rather than refusing: that process's reading is a little
// low for one pass, where a refusal would raise legion_sampler_read_errors_total for a kernel
// that did nothing wrong. A line holding something that is no number is still a refusal naming
// the file, as ReadAnonymous answers it.
func addAnonymous(total *int64, line []byte, path string) error {
	rest, found := bytes.CutPrefix(line, []byte("Anonymous:"))
	if !found {
		return nil
	}
	if len(bytes.TrimSpace(rest)) == 0 {
		return errTruncated
	}
	size, err := kilobytes(rest)
	if err != nil {
		return fmt.Errorf("%s: malformed Anonymous line: %s", path, bytes.TrimSpace(line))
	}
	*total += size
	return nil
}

// errTruncated is how addAnonymous says that the smaps it is reading ended mid-line, which its
// callers answer with the total they have and no error.
var errTruncated = errors.New("procread: the smaps read ended mid-line")

// Forget drops what this reader holds of one process, which is what bounds it: the caller walks
// the processes that are there and tells the reader about every one it stops holding, so the
// descriptors a reader keeps are the descriptors of processes that exist.
func (r *Reader) Forget(pid int) {
	held, there := r.processes[pid]
	if !there {
		return
	}
	held.stat.close()
	held.smaps.close()
	delete(r.processes, pid)
}

// forgetEmpty drops a process this reader holds no descriptor of, which is every process whose
// files it could not open: a pid it will never read is a pid that must take no place at the
// bound. A process whose other file is open is kept, since that file is being read.
func (r *Reader) forgetEmpty(pid int, held *keptProcess) {
	if held.stat.file == nil && held.smaps.file == nil {
		delete(r.processes, pid)
	}
}

// Close releases every descriptor this reader holds and forgets every process it held one for.
// A collector closes its reader when its loop ends. It answers the first failure a close gave,
// having tried every one of them.
func (r *Reader) Close() error {
	var first error
	for _, k := range []*kept{&r.meminfo, &r.memory, &r.cpu} {
		if err := k.close(); err != nil && first == nil {
			first = err
		}
	}
	for pid, held := range r.processes {
		for _, k := range []*kept{&held.stat, &held.smaps} {
			if err := k.close(); err != nil && first == nil {
				first = err
			}
		}
		delete(r.processes, pid)
	}
	return first
}

// process is what this reader holds of one process, made where it holds nothing yet, and nil past
// the bound: a pod that forks more processes than keptProcesses is read past it by path, which
// costs an open a read and exhausts no descriptors.
func (r *Reader) process(pid int) *keptProcess {
	if held, there := r.processes[pid]; there {
		return held
	}
	if len(r.processes) >= keptProcesses {
		return nil
	}
	dir := filepath.Join(r.root, "proc", strconv.Itoa(pid))
	held := &keptProcess{
		stat:  kept{path: filepath.Join(dir, "stat")},
		smaps: kept{path: filepath.Join(dir, "smaps")},
	}
	r.processes[pid] = held
	return held
}

// number is the one number a kept cgroup file holds, and zero for a file that is not there: a
// cgroup file belongs to a controller, and a controller this kernel did not mount wrote nothing
// to read. It is readInt's answer, read through a descriptor.
func (r *Reader) number(k *kept) (int64, error) {
	raw, err := r.read(k)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	text := string(bytes.TrimSpace(raw))
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is no number", k.path, text)
	}
	return value, nil
}

// key is the value one key names in a kept cgroup file of "<key> <value>" lines, and zero for a
// file that is not there or a key it does not carry, as readKeys answers them.
func (r *Reader) key(k *kept, key string) (int64, error) {
	raw, err := r.read(k)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for line := range bytes.Lines(raw) {
		name, value, found := bytes.Cut(bytes.TrimSpace(line), []byte(" "))
		if !found || string(name) != key {
			continue
		}
		number, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s is %q, no number", k.path, key, bytes.TrimSpace(value))
		}
		return number, nil
	}
	return 0, nil
}

// read is the whole of one small file, taken with one pread from offset 0 through the descriptor
// this reader keeps on it, which it opens where it holds none yet. The answer is the reader's own
// buffer and holds only until the next read of any file.
func (r *Reader) read(k *kept) ([]byte, error) {
	if k.file == nil {
		if err := r.open(k); err != nil {
			return nil, err
		}
	}
	for {
		n, err := pread(k.fd, r.buf, 0)
		if err != nil {
			return nil, err
		}
		// A read that filled the buffer may have left the rest of the file behind, so
		// the file is read again, whole and from the start, into a buffer that holds
		// twice as much. Every file read this way is a line or forty, so this is the
		// buffer being too small for a kernel nobody has met and never a pass's cost.
		if n < len(r.buf) {
			return r.buf[:n], nil
		}
		r.buf = make([]byte, 2*len(r.buf))
	}
}

// pread is one read of a file at an offset, which for a file of /proc is the file as the kernel
// writes it at this moment. It is the syscall itself and not os.File.ReadAt, which reads on
// until the buffer is full and so costs a second syscall for every file shorter than it: a
// sampler reads a few files of a few hundred bytes every second, and under gVisor what it is
// charged for is the number of syscalls it makes.
func pread(fd int, buf []byte, off int64) (int, error) {
	for {
		n, err := syscall.Pread(fd, buf, off)
		if errors.Is(err, syscall.EINTR) {
			// A signal the Go runtime delivered, which is no end of the read: the
			// profiler's own SIGPROF arrives at any syscall.
			continue
		}
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}

// open takes the descriptor this reader keeps on one file. A path with no file is the
// filesystem's own error, which is a cgroup file no controller writes or a process that has
// ended, and the caller answers for the difference.
func (r *Reader) open(k *kept) error {
	if k.path == "" {
		return fs.ErrNotExist
	}
	file, err := os.Open(k.path)
	if err != nil {
		return err
	}
	// The descriptor's own number is taken once and read through from here on. Taking it
	// puts the file in blocking mode, which a file of /proc is anyway: the runtime's poller
	// holds no regular file.
	k.file, k.fd = file, int(file.Fd())
	return nil
}

// close releases this file's descriptor, where one is held.
func (k *kept) close() error {
	if k.file == nil {
		return nil
	}
	err := k.file.Close()
	k.file, k.fd = nil, 0
	return err
}
