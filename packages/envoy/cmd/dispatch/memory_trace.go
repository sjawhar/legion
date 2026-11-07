package main

// The memory trace DISPATCH_MEMORY_TRACE turns on: one line every memoryTraceInterval holding
// every class of memory the Go runtime has mapped, the collector's limit and goal, the process's
// resident memory as /proc reports it and the container's memory.current. It is off unless the
// setting names a file, and it is how a pod near its memory limit is told apart from one the
// runtime is holding inside it: the limit the boot sets (setGoMemoryLimit) bounds
// total − released, so a resident figure past the limit is either memory outside the runtime or,
// on a kernel that counts pages the runtime has handed back, the released class.
//
// Every real-process memory drive runs its server with the trace on and reads two figures of it
// (memory_harness_test.go, peakResident). The first is total − released, what the limit bounds,
// which no kernel reports and which a drive weighs against the limit the server logged. The
// second is for the reason above: this repository's sandboxes run under gVisor, which keeps a
// page the runtime released resident unless the madvise call that released it covered 2 MiB or
// more, and the collector's scavenger releases in smaller pieces, so VmRSS there counts memory
// the pod's own kernel would not, and a drive on such a kernel reads the charge as
// vmrss − heap_released instead.

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"
)

// memoryTraceInterval is how often the trace samples. A sample costs one read of
// /proc/self/status, one of the control group's memory.current and one metrics.Read, and the
// line it writes is about 500 bytes, so an hour of tracing is some 70,000 lines and 35 MB.
const memoryTraceInterval = 50 * time.Millisecond

// memoryTraceClasses are the runtime metrics each sample holds, in the order it writes them,
// under the field names the line uses. The memory classes are every class
// /memory/classes/total:bytes is the sum of, so a reader can account for the whole of it.
var memoryTraceClasses = []struct{ field, metric string }{
	{"total", "/memory/classes/total:bytes"},
	{"heap_released", "/memory/classes/heap/released:bytes"},
	{"heap_objects", "/memory/classes/heap/objects:bytes"},
	{"heap_unused", "/memory/classes/heap/unused:bytes"},
	{"heap_free", "/memory/classes/heap/free:bytes"},
	{"heap_stacks", "/memory/classes/heap/stacks:bytes"},
	{"os_stacks", "/memory/classes/os-stacks:bytes"},
	{"metadata_mspan_inuse", "/memory/classes/metadata/mspan/inuse:bytes"},
	{"metadata_mspan_free", "/memory/classes/metadata/mspan/free:bytes"},
	{"metadata_mcache_inuse", "/memory/classes/metadata/mcache/inuse:bytes"},
	{"metadata_mcache_free", "/memory/classes/metadata/mcache/free:bytes"},
	{"metadata_other", "/memory/classes/metadata/other:bytes"},
	{"profiling_buckets", "/memory/classes/profiling/buckets:bytes"},
	{"other", "/memory/classes/other:bytes"},
	{"go_memory_limit", "/gc/gomemlimit:bytes"},
	{"heap_goal", "/gc/heap/goal:bytes"},
	{"gc_cycles", "/gc/cycles/total:gc-cycles"},
	{"gc_limiter_last_enabled_cycle", "/gc/limiter/last-enabled:gc-cycle"},
	{"goroutines", "/sched/goroutines:goroutines"},
}

// memoryTraceStatus are the fields of /proc/self/status each sample holds. A kernel that keeps
// none of a field, as gVisor keeps no VmHWM, RssAnon or RssFile, writes -1 for it.
var memoryTraceStatus = []string{"VmRSS", "RssAnon", "RssFile", "VmHWM"}

// startMemoryTrace writes a sample of the process's memory to path every memoryTraceInterval
// until ctx ends, and writes nothing when path is empty. main's ctx never ends, so a server's
// trace runs to the end of the process, its shutdown included. A path it cannot create, or a
// write that fails, is one warning and no trace: a diagnostic never stops the server.
func startMemoryTrace(ctx context.Context, path string) {
	if path == "" {
		return
	}
	file, err := os.Create(path)
	if err != nil {
		slog.Warn("dispatch: no memory trace written", "path", path, "error", err)
		return
	}
	slog.Info("dispatch: memory trace", "path", path, "interval", memoryTraceInterval)
	samples := make([]metrics.Sample, len(memoryTraceClasses))
	for index, class := range memoryTraceClasses {
		samples[index].Name = class.metric
	}
	go func() {
		defer func() { _ = file.Close() }()
		writer := bufio.NewWriter(file)
		ticker := time.NewTicker(memoryTraceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = writer.Flush()
				return
			case <-ticker.C:
			}
			if _, err := writer.WriteString(memoryTraceLine(samples)); err != nil {
				slog.Warn("dispatch: the memory trace stopped", "path", path, "error", err)
				return
			}
			if err := writer.Flush(); err != nil {
				slog.Warn("dispatch: the memory trace stopped", "path", path, "error", err)
				return
			}
		}
	}()
}

// memoryTraceLine is one sample, in the key=value form the server's own log lines take, ending in
// a newline. samples holds memoryTraceClasses' metrics, which it reads.
func memoryTraceLine(samples []metrics.Sample) string {
	metrics.Read(samples)
	// The one read of /proc/self/status this sample costs (memoryTraceInterval), which every
	// field below is pulled from. A read that fails leaves no line any field matches, so each
	// field is -1, as a field this kernel keeps no line for is.
	raw, _ := os.ReadFile("/proc/self/status")
	status := string(raw)
	line := make([]string, 0, len(samples)+len(memoryTraceStatus)+2)
	line = append(line, "time="+time.Now().UTC().Format(time.RFC3339Nano))
	for index, class := range memoryTraceClasses {
		value := int64(-1)
		if samples[index].Value.Kind() == metrics.KindUint64 {
			value = int64(samples[index].Value.Uint64())
		}
		line = append(line, class.field+"="+strconv.FormatInt(value, 10))
	}
	for _, field := range memoryTraceStatus {
		line = append(line, strings.ToLower(field)+"="+strconv.FormatInt(procStatusBytes(status, field), 10))
	}
	return strings.Join(line, " ") + " cgroup_memory_current=" + strconv.FormatInt(cgroupMemoryCurrent(), 10) + "\n"
}

// procStatusBytes is the named field of status, one read of /proc/self/status, in bytes, or -1
// where the read failed, where this kernel keeps no such line - gVisor keeps no VmHWM, RssAnon or
// RssFile - or where the line does not parse.
func procStatusBytes(status, field string) int64 {
	for line := range strings.SplitSeq(status, "\n") {
		value, ok := strings.CutPrefix(line, field+":")
		if !ok {
			continue
		}
		kibibytes, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		if err != nil {
			return -1
		}
		return kibibytes << 10
	}
	return -1
}

// cgroupMemoryCurrent is what the container's control group is charged: memory.current under
// control group v2, memory.usage_in_bytes under v1, or -1 where neither is readable, as outside a
// container or under a sandbox that exposes no control group.
func cgroupMemoryCurrent() int64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory/memory.usage_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		return value
	}
	return -1
}
