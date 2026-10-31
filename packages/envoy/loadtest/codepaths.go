package main

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/pprof/profile"
)

// codepaths is the code-path table of one replay step: Dispatch's CPU by entry point
// (exclusive, the production profile's §2a rule) and by subsystem (inclusive, §2b, with INFRA-512's
// write-path functions), its working set at the window's end split by the same entry points, and the
// replay's cores and memory beside the 2026-10-26 outage's (§5), with the residual.

// defaultOutageBucketsName is the committed outage buckets file that -outage-buckets defaults to:
// the outage pod's 10-minute buckets 23:00-23:30 from the production profile's §5 table (Cloud
// Monitoring, 2026-10-26), each at its offset in minutes from 22:55.
const defaultOutageBucketsName = "outage-2026-10-26-buckets.csv"

//go:embed outage-2026-10-26-buckets.csv
var defaultOutageBucketsText string

// outageBucketMinutes is the width of one outage bucket: Cloud Monitoring's 10-minute buckets, each
// the mean and maximum of its 1-minute points.
const outageBucketMinutes = 10

// The Go package paths the classifier matches function names against.
const (
	dispatchPkg     = "github.com/sjawhar/envoy/internal/dispatch/"
	apiPkg          = dispatchPkg + "api."
	docsPkg         = dispatchPkg + "docs."
	ygoWebsocketPkg = "github.com/reearth/ygo/provider/websocket."
)

// The entry-point rows that are not HTTP handlers, in precedence order after the handlers.
const (
	entryFold       = "docs cold-read fold (singleflight goroutine: coldRead/foldDocument)"
	entryGCMark     = "GC background mark workers"
	entryGCSweep    = "GC sweep / scavenge"
	entryObserver   = "room observer"
	entrySettlement = "room settlement"
	entryCompaction = "room compaction"
	entryRoomsOther = "document rooms, other (ygo room server)"
	entryOutbox     = "outbox"
	entrySSE        = "SSE `/api/v1/events` streams"
	entryProfiling  = "profiling (net/http/pprof, runtime/pprof: the harness's own reads)"
	entryHTTPServer = "net/http serving outside a handler"
	entryScheduler  = "scheduler / netpoll"
	entryOther      = "other"
)

// docWebsocketHandler is the handler name of the document websocket route, whose handler is the
// docs service's ServeHTTP rather than a function of the API package.
const docWebsocketHandler = "/ws/doc/{room} (document websocket)"

// entryRule is one non-handler entry point: a stack (leaf first) belongs to it when match holds and
// no handler and no earlier rule took it.
type entryRule struct {
	label string
	match func(stack []string) bool
}

// entryRules are the non-handler entry points in precedence order. "document rooms, other",
// "profiling" and "net/http serving outside a handler" are not rows of §2a: they keep the room
// server's own goroutines, the profiles and memory statistics the harness reads from Dispatch's pprof
// endpoint during the window, and the HTTP server's work around a handler out of `other`.
var entryRules = []entryRule{
	{entryFold, anyFrame(func(f string) bool {
		return f == docsPkg+"(*Service).foldDocument" || strings.HasPrefix(f, docsPkg+"(*Service).coldRead.func")
	})},
	{entryGCMark, anyFrame(isFunc("runtime.gcBgMarkWorker", "runtime._GC"))},
	{entryGCSweep, anyFrame(isFunc("runtime.bgsweep", "runtime.bgscavenge"))},
	{entryObserver, anyFrame(hasPrefix(docsPkg + "(*renderedReplica)."))},
	{entrySettlement, anyFrame(hasPrefix(docsPkg+"(*Service).settleRoom", docsPkg+"(*Service).settleLastPeer",
		docsPkg+"(*Service).scheduleSettle", docsPkg+"(*Service).RunSettlementResumption"))},
	{entryCompaction, anyFrame(hasPrefix(docsPkg+"(*PgVersioned).Compact", docsPkg+"(*servicePersistenceAdapter).Compact",
		docsPkg+"(*Service).CompactAll"))},
	{entryRoomsOther, anyFrame(hasPrefix(ygoWebsocketPkg, docsPkg+"(*servicePersistenceAdapter).",
		docsPkg+"(*Service).RunStaleRoomEviction", docsPkg+"(*staleRoomEvictor)."))},
	{entryOutbox, anyFrame(hasPrefix(dispatchPkg + "outbox."))},
	{entrySSE, anyFrame(hasPrefix(apiPkg+"(*server).streamEvents", apiPkg+"(*eventStreamRelay).", dispatchPkg+"events.(*Stream)."))},
	{entryProfiling, anyFrame(hasPrefix("net/http/pprof.", "runtime/pprof."))},
	{entryHTTPServer, anyFrame(isHTTPServerFrame)},
	{entryScheduler, isSchedulerStack},
}

// fixedEntries are the non-handler rows in table order, `other` last.
func fixedEntries() []string {
	out := make([]string, 0, len(entryRules)+1)
	for _, r := range entryRules {
		out = append(out, r.label)
	}
	return append(out, entryOther)
}

// entryPoint is the one row a stack (function names, leaf first) belongs to: the HTTP handler its
// handler frame names, else the first rule that matches, else `other`.
func entryPoint(stack []string) string {
	if h := handlerOf(stack); h != "" {
		return "HTTP " + h
	}
	for _, r := range entryRules {
		if r.match(stack) {
			return r.label
		}
	}
	return entryOther
}

// closureName matches the parts of a Go function name that make it a closure, a go statement's
// wrapper or a range-over-func body rather than a named function.
var closureName = regexp.MustCompile(`\.func\d+|\.gowrap\d+|-range\d+`)

// handlerOf is the handler a request goroutine's stack runs: walking from the root, the first named
// (not closure) function of the API package below an HTTP server frame, by its bare name as §2a
// writes it (getIssueReferences); the middleware between the server and the handler is closures
// only. The document websocket's handler is the docs service's ServeHTTP. The event stream's
// handler (streamEvents) is not a handler row: its samples are the SSE row's. "" when the stack runs
// no handler.
func handlerOf(stack []string) string {
	served := false
	for i := len(stack) - 1; i >= 0; i-- {
		f := stack[i]
		if isHTTPServerFrame(f) {
			served = true
			continue
		}
		if !served {
			continue
		}
		if f == docsPkg+"(*Service).ServeHTTP" {
			return docWebsocketHandler
		}
		name, ok := strings.CutPrefix(f, apiPkg)
		if !ok || closureName.MatchString(name) {
			continue
		}
		name = strings.TrimSuffix(name, "-fm")
		name = name[strings.LastIndexByte(name, '.')+1:]
		if name == "streamEvents" {
			return ""
		}
		return name
	}
	return ""
}

// httpServerPrefixes are the function name prefixes of Go's HTTP server (HTTP/1 and HTTP/2, in net/http,
// its bundled or internal http2 copy, and x/net/http2); an HTTP client's frames are not among them.
var httpServerPrefixes = []string{
	"net/http.(*conn).", "net/http.(*connReader).", "net/http.serverHandler.", "net/http.(*ServeMux).",
	"net/http.HandlerFunc.ServeHTTP", "net/http.(*response).", "net/http.(*chunkWriter).",
	"net/http.(*http2serverConn).", "net/http.(*http2Server).", "net/http.(*http2responseWriter).",
	"net/http/internal/http2.(*serverConn).", "net/http/internal/http2.(*Server).",
	"golang.org/x/net/http2.(*serverConn).", "golang.org/x/net/http2.(*Server).",
}

var isHTTPServerFrame = hasPrefix(httpServerPrefixes...)

// isSchedulerFrame holds for the Go runtime's scheduling and network-poller functions.
var isSchedulerFrame = isFunc(
	"runtime.schedule", "runtime.findRunnable", "runtime.findrunnable", "runtime.netpoll", "runtime.sysmon",
	"runtime.park_m", "runtime.stopm", "runtime.startm", "runtime.wakep", "runtime.goschedImpl",
	"runtime.gosched_m", "runtime.gopreempt_m", "runtime.mstart", "runtime.mstart0", "runtime.mstart1",
	"runtime.notesleep", "runtime.mPark", "runtime.handoffp", "runtime.injectglist",
)

// isSchedulerStack holds for a stack with a scheduling frame, or one made only of runtime frames
// (the runtime's own goroutines and the profiler's pseudo-frames runtime._System, _ExternalCode
// and _VDSO).
func isSchedulerStack(stack []string) bool {
	if slices.ContainsFunc(stack, isSchedulerFrame) {
		return true
	}
	return len(stack) > 0 && !slices.ContainsFunc(stack, func(f string) bool {
		return !strings.HasPrefix(f, "runtime.") && !strings.HasPrefix(f, "internal/runtime/")
	})
}

func anyFrame(pred func(string) bool) func([]string) bool {
	return func(stack []string) bool { return slices.ContainsFunc(stack, pred) }
}

func hasPrefix(prefixes ...string) func(string) bool {
	return func(f string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(f, p) {
				return true
			}
		}
		return false
	}
}

func isFunc(names ...string) func(string) bool {
	return func(f string) bool { return slices.Contains(names, f) }
}

func hasSuffix(suffixes ...string) func(string) bool {
	return func(f string) bool {
		for _, s := range suffixes {
			if strings.HasSuffix(f, s) {
				return true
			}
		}
		return false
	}
}

// subsystemRule is one row of the inclusive table: a sample counts toward it when any frame of its
// stack matches.
type subsystemRule struct {
	name      string
	writePath bool
	match     func(string) bool
}

// subsystemRules are §2b's subsystems, then INFRA-512's write-path functions (matched by function
// name suffix; len16 is pmdoc's, not the API package's own len16, and loadRoom is ygo's
// (*Server).loadRoom), which overlap the subsystems and each other.
var subsystemRules = []subsystemRule{
	{"dispatch/docs (fold, read cache, rooms)", false, hasPrefix(docsPkg)},
	{"pmdoc (document tree, render, block tokens)", false, hasPrefix(dispatchPkg + "pmdoc.")},
	{"API handlers (dispatch/api)", false, hasPrefix(apiPkg)},
	{"GC mark (background workers and assists)", false, isFunc("runtime.gcBgMarkWorker", "runtime.gcAssistAlloc", "runtime._GC")},
	{"encoding/json (v1 and v2)", false, hasPrefix("encoding/json")},
	{"ygo crdt (Yjs)", false, hasPrefix("github.com/reearth/ygo/crdt")},
	{"runtime.mallocgc (allocation)", false, hasPrefix("runtime.mallocgc")},
	{"pgx / pgconn (database driver)", false, hasPrefix("github.com/jackc/pgx/", "github.com/jackc/puddle/")},
	{"crypto (sha256 of block tokens; TLS)", false, hasPrefix("crypto/")},
	{"goldmark (markdown)", false, hasPrefix("github.com/yuin/goldmark")},
	{"dispatch/events (SSE stream)", false, hasPrefix(dispatchPkg + "events.")},
	// The symbol table escapes the dot of the last path element: nats%2ego.
	{"NATS client", false, hasPrefix("github.com/nats-io/nats.go", "github.com/nats-io/nats%2ego")},
	{"refreshAnchors", true, hasSuffix(".refreshAnchors")},
	{"pmdoc.walkNode", true, hasSuffix("/pmdoc.walkNode")},
	{"len16", true, hasSuffix("/pmdoc.len16")},
	{"MarkQuote/MarkSpans", true, hasSuffix(".MarkQuote", ".MarkSpans")},
	{"ProjectMark", true, hasSuffix(".ProjectMark")},
	{"publishLiveWrite", true, hasSuffix(".publishLiveWrite")},
	{"RenderWithBlockOffsets", true, hasSuffix(".RenderWithBlockOffsets")},
	{"loadRoom/fold", true, hasSuffix(".loadRoom", "/docs.fold")},
}

// stackFrames appends to buf the function names of a sample's stack, leaf first, each inlined
// function its own frame; a location with no symbol is its address.
func stackFrames(buf []string, s *profile.Sample) []string {
	for _, loc := range s.Location {
		if len(loc.Line) == 0 {
			buf = append(buf, fmt.Sprintf("0x%x", loc.Address))
			continue
		}
		for _, ln := range loc.Line {
			if ln.Function != nil {
				buf = append(buf, ln.Function.Name)
			}
		}
	}
	return buf
}

// valueIndex is the index of the profile's sample type typ.
func valueIndex(p *profile.Profile, typ string) (int, error) {
	for i, st := range p.SampleType {
		if st.Type == typ {
			return i, nil
		}
	}
	var have []string
	for _, st := range p.SampleType {
		have = append(have, st.Type)
	}
	return 0, fmt.Errorf("the profile has no %s sample type (it has %s)", typ, strings.Join(have, ", "))
}

// goroutineStart is the outermost frame of a stack outside the runtime, the function its goroutine
// started in, or its root frame when it has none.
func goroutineStart(stack []string) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if !strings.HasPrefix(stack[i], "runtime.") {
			return stack[i]
		}
	}
	if len(stack) == 0 {
		return "(empty stack)"
	}
	return stack[len(stack)-1]
}

// --- the report ------------------------------------------------------------------------------

// codepathsReport is codepaths.json: every number of CODEPATHS.md. A number an input did not give is
// null, never 0.
type codepathsReport struct {
	Step          string        `json:"step"`
	Inputs        []cpInput     `json:"inputs"`
	CPU           *cpCPU        `json:"cpu"`
	Subsystems    []cpSubsystem `json:"subsystems"`
	Memory        cpMemory      `json:"memory"`
	Outage        cpOutage      `json:"outage"`
	NotReproduced []string      `json:"not_reproduced"`
}

// cpInput is one input file: its path, or why it is missing.
type cpInput struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Missing string `json:"missing,omitempty"`
}

// cpCPU is the CPU profile's samples by entry point.
type cpCPU struct {
	Profile         string     `json:"profile"`
	DurationS       float64    `json:"profile_duration_s"`
	Samples         int64      `json:"samples"`
	CPUS            float64    `json:"cpu_s"`
	CgroupCoresMean *float64   `json:"cgroup_cores_mean"`
	CoresMissing    string     `json:"cores_missing,omitempty"`
	Rows            []cpCPURow `json:"rows"`
	// OtherByStart is the `other` row's samples by goroutineStart, the 10 largest.
	OtherByStart []cpCPURow `json:"other_by_goroutine_start"`
}

// cpCPURow is one row of the CPU tables: share is its samples over the profile's (null with no
// sample), cores its share of the cgroup's cores_mean (null without one).
type cpCPURow struct {
	Row     string   `json:"row"`
	Samples int64    `json:"samples"`
	CPUNs   int64    `json:"cpu_ns"`
	Share   *float64 `json:"share"`
	CPUS    float64  `json:"cpu_s"`
	Cores   *float64 `json:"cores"`
}

// cpSubsystem is one row of the inclusive table.
type cpSubsystem struct {
	cpCPURow
	WritePath bool `json:"write_path"`
	InProfile bool `json:"in_profile"`
}

// cpMemory is the Dispatch working set at the window's end, split.
type cpMemory struct {
	WorkingSetBytes  *float64      `json:"working_set_bytes"`
	WorkingSetSource string        `json:"working_set_source"`
	HeapInuse        *float64      `json:"heap_inuse_bytes"`
	HeapSys          *float64      `json:"heap_sys_bytes"`
	HeapReleased     *float64      `json:"heap_released_bytes"`
	StackSys         *float64      `json:"stack_sys_bytes"`
	Sys              *float64      `json:"sys_bytes"`
	Rows             []cpMemoryRow `json:"rows"`
	Notes            []string      `json:"notes"`
}

// cpMemoryRow is one part of the working set. WindowAllocBytes is the entry point's alloc_space
// rise over the window, for the Go heap rows only.
type cpMemoryRow struct {
	Row               string   `json:"row"`
	Bytes             *float64 `json:"bytes"`
	ShareOfWorkingSet *float64 `json:"share_of_working_set"`
	InuseShare        *float64 `json:"inuse_share,omitempty"`
	WindowAllocBytes  *float64 `json:"window_alloc_bytes"`
}

// cpOutage is the replay beside the outage.
type cpOutage struct {
	BucketsSource      string     `json:"buckets_source"`
	WindowOffsetMin    int        `json:"window_offset_min"`
	Buckets            []cpBucket `json:"buckets"`
	OutageCoresLow     float64    `json:"outage_cores_low"`
	OutageCoresHigh    float64    `json:"outage_cores_high"`
	OutageMemoryGiB    float64    `json:"outage_memory_gib"`
	ReplayCores        *float64   `json:"replay_cores_mean"`
	ReplayMemoryGiB    *float64   `json:"replay_working_set_gib_end"`
	ReplayMemorySource string     `json:"replay_memory_source"`
	ResidualCoresLow   *float64   `json:"residual_cores_low"`
	ResidualCoresHigh  *float64   `json:"residual_cores_high"`
	ResidualMemoryGiB  *float64   `json:"residual_memory_gib"`
	Missing            []string   `json:"missing,omitempty"`
}

// cpBucket is one outage bucket and the replay's minutes at the same offset.
type cpBucket struct {
	Label            string   `json:"label"`
	OffsetMin        int      `json:"offset_min"`
	OutageCoresMean  float64  `json:"outage_cores_mean"`
	OutageCoresMax   float64  `json:"outage_cores_max"`
	OutageMemGiBMean float64  `json:"outage_mem_gib_mean"`
	OutageMemGiBMax  float64  `json:"outage_mem_gib_max"`
	ReplayMinutes    int      `json:"replay_minutes"`
	ReplayCoresMean  *float64 `json:"replay_cores_mean"`
	ReplayCoresMax   *float64 `json:"replay_cores_max"`
	ReplayWSGiBMean  *float64 `json:"replay_working_set_gib_mean"`
	ReplayWSGiBMax   *float64 `json:"replay_working_set_gib_max"`
	ReplayMissing    string   `json:"replay_missing,omitempty"`
}

func gibOf(b float64) float64 { return b / (1 << 30) }

// shareCell is a share as a percentage with two decimals, "-" for nil.
func shareCell(p *float64) string {
	if p == nil {
		return "-"
	}
	return num(100**p, 2) + "%"
}

// dispatchCores is the cgroup's Dispatch cores_mean over the window, or why there is none.
func dispatchCores(cf *cgroupFile) (*float64, string) {
	r, why := dispatchRole(cf)
	switch {
	case why != "":
		return nil, why
	case r.CoresMean == nil:
		return nil, "cgroup.json's dispatch cores_mean is null (no good sample brackets the window's ends)"
	default:
		return r.CoresMean, ""
	}
}

// dispatchRole is cf's dispatch role, or why there is none.
func dispatchRole(cf *cgroupFile) (cgroupRole, string) {
	if cf == nil {
		return cgroupRole{}, "no cgroup.json (loadgen cgroup-merge)"
	}
	r, ok := cf.Roles["dispatch"]
	if !ok {
		return r, "cgroup.json has no dispatch role"
	}
	return r, ""
}

// workingSetAtEnd is the Dispatch working set at the window's end: the last minute's mean, else the
// window's maximum; the source says which, or why there is none.
func workingSetAtEnd(cf *cgroupFile) (*float64, string) {
	r, why := dispatchRole(cf)
	if why != "" {
		return nil, why
	}
	if n := len(r.Minutes); n > 0 && r.Minutes[n-1] != nil {
		return new(r.Minutes[n-1].WorkingSetBytesMean), fmt.Sprintf("the last minute's working_set_bytes_mean (minute %d of cgroup.json's dispatch minutes)", n)
	}
	if r.WorkingSetBytesMax != nil {
		return new(float64(*r.WorkingSetBytesMax)), "working_set_bytes_max over the window; the last minute has no figure"
	}
	return nil, "cgroup.json's dispatch role has no working set (no good sample in the window)"
}

// cpuByEntry assigns every sample of a CPU profile to one entry point, with cores from the
// cgroup's cores_mean (nil: the cores column is null).
func cpuByEntry(p *profile.Profile, cores *float64) (*cpCPU, error) {
	si, err := valueIndex(p, "samples")
	if err != nil {
		return nil, err
	}
	ci, err := valueIndex(p, "cpu")
	if err != nil {
		return nil, err
	}
	type acc struct{ samples, ns int64 }
	byEntry, byStart := map[string]acc{}, map[string]acc{}
	var total acc
	var frames []string
	for _, s := range p.Sample {
		frames = stackFrames(frames[:0], s)
		e := entryPoint(frames)
		a := byEntry[e]
		a.samples += s.Value[si]
		a.ns += s.Value[ci]
		byEntry[e] = a
		total.samples += s.Value[si]
		total.ns += s.Value[ci]
		if e == entryOther {
			g := goroutineStart(frames)
			b := byStart[g]
			b.samples += s.Value[si]
			b.ns += s.Value[ci]
			byStart[g] = b
		}
	}
	row := func(name string, a acc) cpCPURow {
		r := cpCPURow{Row: name, Samples: a.samples, CPUNs: a.ns, CPUS: float64(a.ns) / 1e9}
		if total.samples > 0 {
			r.Share = new(float64(a.samples) / float64(total.samples))
			if cores != nil {
				r.Cores = new(*r.Share * *cores)
			}
		}
		return r
	}
	out := &cpCPU{DurationS: float64(p.DurationNanos) / 1e9, Samples: total.samples, CPUS: float64(total.ns) / 1e9, CgroupCoresMean: cores}
	for _, e := range entryOrder(byEntry, func(e string) int64 { return byEntry[e].samples }) {
		out.Rows = append(out.Rows, row(e, byEntry[e]))
	}
	starts := slices.SortedFunc(maps.Keys(byStart), func(a, b string) int {
		return cmp.Or(cmp.Compare(byStart[b].samples, byStart[a].samples), strings.Compare(a, b))
	})
	for _, g := range starts[:min(10, len(starts))] {
		out.OtherByStart = append(out.OtherByStart, row(g, byStart[g]))
	}
	return out, nil
}

// entryOrder is the table order of the entry points: the HTTP handlers in have, the largest weight
// first, then every fixed row in precedence order, `other` last.
func entryOrder[V any](have map[string]V, weight func(string) int64) []string {
	var handlers []string
	for e := range have {
		if strings.HasPrefix(e, "HTTP ") {
			handlers = append(handlers, e)
		}
	}
	slices.SortFunc(handlers, func(a, b string) int {
		return cmp.Or(cmp.Compare(weight(b), weight(a)), strings.Compare(a, b))
	})
	return append(handlers, fixedEntries()...)
}

// subsystems counts each sample once toward every subsystem its stack holds a frame of.
func subsystems(p *profile.Profile, cores *float64) ([]cpSubsystem, error) {
	si, err := valueIndex(p, "samples")
	if err != nil {
		return nil, err
	}
	ci, err := valueIndex(p, "cpu")
	if err != nil {
		return nil, err
	}
	samples, ns := make([]int64, len(subsystemRules)), make([]int64, len(subsystemRules))
	var total int64
	var frames []string
	for _, s := range p.Sample {
		frames = stackFrames(frames[:0], s)
		total += s.Value[si]
		for i, r := range subsystemRules {
			if slices.ContainsFunc(frames, r.match) {
				samples[i] += s.Value[si]
				ns[i] += s.Value[ci]
			}
		}
	}
	out := make([]cpSubsystem, len(subsystemRules))
	for i, r := range subsystemRules {
		row := cpSubsystem{cpCPURow: cpCPURow{Row: r.name, Samples: samples[i], CPUNs: ns[i], CPUS: float64(ns[i]) / 1e9}, WritePath: r.writePath}
		row.InProfile = slices.ContainsFunc(p.Function, func(f *profile.Function) bool { return r.match(f.Name) })
		if total > 0 {
			row.Share = new(float64(samples[i]) / float64(total))
			if cores != nil {
				row.Cores = new(*row.Share * *cores)
			}
		}
		out[i] = row
	}
	return out, nil
}

// heapByEntry sums a heap profile's inuse_space and alloc_space by entry point.
func heapByEntry(p *profile.Profile) (inuse, alloc map[string]int64, inuseTotal int64, err error) {
	ii, err := valueIndex(p, "inuse_space")
	if err != nil {
		return nil, nil, 0, err
	}
	ai, err := valueIndex(p, "alloc_space")
	if err != nil {
		return nil, nil, 0, err
	}
	inuse, alloc = map[string]int64{}, map[string]int64{}
	var frames []string
	for _, s := range p.Sample {
		frames = stackFrames(frames[:0], s)
		e := entryPoint(frames)
		inuse[e] += s.Value[ii]
		alloc[e] += s.Value[ai]
		inuseTotal += s.Value[ii]
	}
	return inuse, alloc, inuseTotal, nil
}

// memStats is the part of heap.txt's runtime.MemStats the memory table needs. HeapReleased is nil
// when the block has no HeapReleased line.
type memStats struct {
	HeapSys, HeapInuse, StackSys, Sys float64
	HeapReleased                      *float64
}

// errNoMemStats is parseMemStats' error for a heap.txt with no `# runtime.MemStats` block: the
// block ends /debug/pprof/heap?debug=1, so a fetch cut before its end has none.
var errNoMemStats = errors.New("no `# runtime.MemStats` block")

// parseMemStats reads the `# runtime.MemStats` block of /debug/pprof/heap?debug=1: `# Key = value`
// lines, and `# Stack = <inuse> / <sys>`. HeapSys, HeapInuse, Stack and Sys are required;
// HeapReleased is read when present.
func parseMemStats(text []byte) (memStats, error) {
	_, blockBytes, ok := bytes.Cut(text, []byte("# runtime.MemStats\n"))
	if !ok {
		return memStats{}, errNoMemStats
	}
	block := string(blockBytes)
	got := map[string]float64{}
	for line := range strings.SplitSeq(block, "\n") {
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "# "), " = ")
		if !ok {
			continue
		}
		if key == "Stack" {
			_, value, ok = strings.Cut(value, " / ")
			if !ok {
				return memStats{}, fmt.Errorf("Stack line %q is not <inuse> / <sys>", line)
			}
			key = "StackSys"
		}
		switch key {
		case "HeapSys", "HeapInuse", "HeapReleased", "StackSys", "Sys":
			v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return memStats{}, fmt.Errorf("%s: %w", key, err)
			}
			got[key] = float64(v)
		}
	}
	for _, k := range []string{"HeapSys", "HeapInuse", "StackSys", "Sys"} {
		if _, ok := got[k]; !ok {
			return memStats{}, fmt.Errorf("the runtime.MemStats block has no %s", k)
		}
	}
	ms := memStats{HeapSys: got["HeapSys"], HeapInuse: got["HeapInuse"], StackSys: got["StackSys"], Sys: got["Sys"]}
	if v, ok := got["HeapReleased"]; ok {
		ms.HeapReleased = new(v)
	}
	return ms, nil
}

// memoryInputs are what the memory table reads; each nil when missing, with why.
type memoryInputs struct {
	heapEnd, heapStart       *profile.Profile
	heapEndWhy, heapStartWhy string
	ms                       *memStats
	msWhy                    string
	ws                       *float64
	wsSource                 string
}

// memoryTable splits the working set at the window's end: the Go heap in use by entry point
// (inuse_space shares of HeapInuse), heap idle retained, stacks, other runtime memory and non-Go
// memory. HeapReleased, the heap pages the runtime has returned to the OS, is in HeapSys and Sys
// but not in the working set, so it is taken out of heap idle and out of Sys before non-Go memory.
func memoryTable(in memoryInputs) (cpMemory, error) {
	m := cpMemory{WorkingSetBytes: in.ws, WorkingSetSource: in.wsSource}
	if in.ms != nil {
		m.HeapInuse, m.HeapSys, m.HeapReleased, m.StackSys, m.Sys = new(in.ms.HeapInuse), new(in.ms.HeapSys), in.ms.HeapReleased, new(in.ms.StackSys), new(in.ms.Sys)
	} else {
		m.Notes = append(m.Notes, "heap.txt: "+in.msWhy+", so every row from runtime.MemStats shows `-`.")
	}
	if in.ws == nil {
		m.Notes = append(m.Notes, "Working set: "+in.wsSource+", so non-Go memory and the shares of the working set show `-`.")
	}
	var inuse, allocEnd, allocStart map[string]int64
	var inuseTotal int64
	if in.heapEnd != nil {
		var err error
		if inuse, allocEnd, inuseTotal, err = heapByEntry(in.heapEnd); err != nil {
			return m, fmt.Errorf("heap-end: %w", err)
		}
	} else {
		m.Notes = append(m.Notes, "heap-end: "+in.heapEndWhy+", so the Go heap in use is not split by entry point.")
	}
	if in.heapStart != nil {
		var err error
		if _, allocStart, _, err = heapByEntry(in.heapStart); err != nil {
			return m, fmt.Errorf("heap-start: %w", err)
		}
	} else {
		m.Notes = append(m.Notes, "heap-start: "+in.heapStartWhy+", so the window's allocation shows `-`.")
	}
	share := func(b *float64) *float64 {
		if b == nil || in.ws == nil || *in.ws == 0 {
			return nil
		}
		return new(*b / *in.ws)
	}
	add := func(r cpMemoryRow) {
		r.ShareOfWorkingSet = share(r.Bytes)
		m.Rows = append(m.Rows, r)
	}
	if in.heapEnd != nil {
		seen := map[string]bool{}
		for e := range inuse {
			seen[e] = true
		}
		for e := range allocEnd {
			seen[e] = true
		}
		for _, e := range entryOrder(seen, func(e string) int64 { return inuse[e] }) {
			r := cpMemoryRow{Row: "Go heap in use: " + e}
			if inuseTotal > 0 {
				r.InuseShare = new(float64(inuse[e]) / float64(inuseTotal))
				if in.ms != nil {
					r.Bytes = new(*r.InuseShare * in.ms.HeapInuse)
				}
			}
			if allocStart != nil {
				r.WindowAllocBytes = new(float64(allocEnd[e] - allocStart[e]))
			}
			add(r)
		}
		if inuseTotal == 0 {
			m.Notes = append(m.Notes, "heap-end holds no inuse_space, so the Go heap rows show `-`.")
		}
	} else {
		r := cpMemoryRow{Row: "Go heap in use (HeapInuse, not split)"}
		if in.ms != nil {
			r.Bytes = new(in.ms.HeapInuse)
		}
		add(r)
	}
	var idle, stacks, runtimeOther, nonGo *float64
	idleRow, nonGoRow := "heap idle retained (HeapSys - HeapInuse - HeapReleased)", "non-Go memory (working set - (Sys - HeapReleased))"
	if in.ms != nil {
		released := 0.0
		negative := "Non-Go memory is negative: Sys - HeapReleased exceeds the working set, because the runtime's Sys still counts address space it has mapped but never touched, which the working set does not hold."
		if in.ms.HeapReleased != nil {
			released = *in.ms.HeapReleased
		} else {
			idleRow, nonGoRow = "heap idle (HeapSys - HeapInuse)", "non-Go memory (working set - Sys)"
			negative = "Non-Go memory is negative: the runtime's Sys counts address space it has reserved and released to the kernel (HeapReleased) or never touched, which the working set does not hold, so Sys exceeds the working set."
			m.Notes = append(m.Notes, "heap.txt's runtime.MemStats block has no HeapReleased, so heap idle is HeapSys - HeapInuse and non-Go memory is working set - Sys: both count the heap pages the runtime has returned to the OS, which are not in the working set, so heap idle is high and non-Go memory low by that unknown amount.")
		}
		idle, stacks, runtimeOther = new(in.ms.HeapSys-in.ms.HeapInuse-released), new(in.ms.StackSys), new(in.ms.Sys-in.ms.HeapSys-in.ms.StackSys)
		if in.ws != nil {
			nonGo = new(*in.ws - (in.ms.Sys - released))
			if *nonGo < 0 {
				m.Notes = append(m.Notes, negative)
			}
		}
	}
	add(cpMemoryRow{Row: idleRow, Bytes: idle})
	add(cpMemoryRow{Row: "stacks (StackSys)", Bytes: stacks})
	add(cpMemoryRow{Row: "other runtime memory (Sys - HeapSys - StackSys)", Bytes: runtimeOther})
	add(cpMemoryRow{Row: nonGoRow, Bytes: nonGo})
	return m, nil
}

// outageBucket is one row of the outage buckets file.
type outageBucket struct {
	Label                                      string
	OffsetMin                                  int
	CoresMean, CoresMax, MemGiBMean, MemGiBMax float64
}

// parseOutageBuckets reads `label,offset_min,cores_mean,cores_max,mem_gib_mean,mem_gib_max` rows; a
// first row whose label is "label" is the header.
func parseOutageBuckets(source, text string) ([]outageBucket, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = 6
	r.Comment = '#'
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("outage buckets %s: %w", source, err)
	}
	var out []outageBucket
	for i, rec := range recs {
		if i == 0 && rec[0] == "label" {
			continue
		}
		b := outageBucket{Label: rec[0]}
		if b.OffsetMin, err = strconv.Atoi(rec[1]); err != nil {
			return nil, fmt.Errorf("outage buckets %s row %d: offset_min %q is not a whole number of minutes", source, i+1, rec[1])
		}
		for j, dst := range []*float64{&b.CoresMean, &b.CoresMax, &b.MemGiBMean, &b.MemGiBMax} {
			v, err := strconv.ParseFloat(rec[2+j], 64)
			if err != nil || !finite(v) || v < 0 {
				return nil, fmt.Errorf("outage buckets %s row %d: %q is not a non-negative number", source, i+1, rec[2+j])
			}
			*dst = v
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("outage buckets %s holds no bucket", source)
	}
	return out, nil
}

// replayBucket is the replay's figures over minutes start to start+outageBucketMinutes of the
// Dispatch role's minutes, as production's are: the mean and maximum of the 1-minute points (the
// working set's maximum is the highest minute mean). A minute outside the series or null is skipped.
func replayBucket(b outageBucket, minutes []*cgroupMinute, start int) cpBucket {
	out := cpBucket{Label: b.Label, OffsetMin: b.OffsetMin, OutageCoresMean: b.CoresMean, OutageCoresMax: b.CoresMax,
		OutageMemGiBMean: b.MemGiBMean, OutageMemGiBMax: b.MemGiBMax}
	var cores, ws float64
	coresMax, wsMax := math.Inf(-1), math.Inf(-1)
	for i := start; i < start+outageBucketMinutes; i++ {
		if i < 0 || i >= len(minutes) || minutes[i] == nil {
			continue
		}
		out.ReplayMinutes++
		cores += minutes[i].Cores
		ws += minutes[i].WorkingSetBytesMean
		coresMax = max(coresMax, minutes[i].Cores)
		wsMax = max(wsMax, minutes[i].WorkingSetBytesMean)
	}
	if n := float64(out.ReplayMinutes); n > 0 {
		out.ReplayCoresMean, out.ReplayCoresMax = new(cores/n), new(coresMax)
		out.ReplayWSGiBMean, out.ReplayWSGiBMax = new(gibOf(ws/n)), new(gibOf(wsMax))
	} else {
		out.ReplayMissing = fmt.Sprintf("replay minutes %d-%d are outside its %d minutes or null", start, start+outageBucketMinutes-1, len(minutes))
	}
	return out
}

// outageTable puts the replay beside the outage in o, which holds the outage's figures, the
// buckets' source, the window's offset and the working set's source: each bucket at the same offset
// from 22:55 (the replay's window starting o.WindowOffsetMin minutes after 22:55), then the totals
// (Dispatch's cores, or coresWhy, and its working set ws) and the residual.
func outageTable(o cpOutage, buckets []outageBucket, cf *cgroupFile, cores *float64, coresWhy string, ws *float64) cpOutage {
	r, why := dispatchRole(cf)
	for _, b := range buckets {
		row := replayBucket(b, r.Minutes, b.OffsetMin-o.WindowOffsetMin)
		if why != "" {
			row.ReplayMissing = why
		}
		o.Buckets = append(o.Buckets, row)
	}
	o.ReplayCores = cores
	if cores != nil {
		o.ResidualCoresLow, o.ResidualCoresHigh = new(o.OutageCoresLow-*cores), new(o.OutageCoresHigh-*cores)
	} else {
		o.Missing = append(o.Missing, "replay cores: "+coresWhy)
	}
	if ws != nil {
		o.ReplayMemoryGiB = new(gibOf(*ws))
		o.ResidualMemoryGiB = new(o.OutageMemoryGiB - *o.ReplayMemoryGiB)
	} else {
		o.Missing = append(o.Missing, "replay memory: "+o.ReplayMemorySource)
	}
	return o
}

// mdListLine matches a markdown list item line.
var mdListLine = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)

// readResidualNotes is the markdown list lines of path, verbatim.
func readResidualNotes(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-residual-notes: %w", err)
	}
	var out []string
	for line := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if mdListLine.MatchString(line) {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-residual-notes %s holds no markdown list line", path)
	}
	return out, nil
}

// --- the command -----------------------------------------------------------------------------

// cpFile resolves one input file: the flag's path when given (it must exist), else the first of
// the step directory's defaults that exists; missing says why there is none.
func cpFile(flagPath, step string, defaults ...string) (path, missing string, err error) {
	if flagPath != "" {
		if _, err := os.Stat(flagPath); err != nil {
			return "", "", err
		}
		return flagPath, "", nil
	}
	if step == "" {
		return "", "not named, and no -step", nil
	}
	for _, d := range defaults {
		p := filepath.Join(step, d)
		if _, err := os.Stat(p); err == nil {
			return p, "", nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
	}
	return "", "no " + strings.Join(defaults, " or ") + " in the step directory", nil
}

func readProfileFile(path string) (*profile.Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p, err := profile.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func readCgroupPath(path string) (*cgroupFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf cgroupFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cf, nil
}

// parseOutageCores reads -outage-cores: "low,high".
func parseOutageCores(s string) (low, high float64, err error) {
	a, b, ok := strings.Cut(s, ",")
	if ok {
		low, err = strconv.ParseFloat(strings.TrimSpace(a), 64)
		if err == nil {
			high, err = strconv.ParseFloat(strings.TrimSpace(b), 64)
		}
	}
	if !ok || err != nil || !finite(low) || !finite(high) || low < 0 || high < low {
		return 0, 0, fmt.Errorf("-outage-cores %q is not <low>,<high> cores with low <= high", s)
	}
	return low, high, nil
}

func runCodepaths(args []string) error {
	fs := flag.NewFlagSet("codepaths", flag.ExitOnError)
	step := fs.String("step", "", "the replay step directory; each file flag defaults to its file there")
	cpuPath := fs.String("cpu", "", "the window's CPU profile (default <step>/cpu.pprof)")
	heapStartPath := fs.String("heap-start", "", "the heap profile at the window's start (default <step>/heap-start.pprof)")
	heapEndPath := fs.String("heap-end", "", "the heap profile at the window's end (default <step>/heap-end.pprof, else <step>/heap.pprof)")
	heapTxtPath := fs.String("heap-txt", "", "/debug/pprof/heap?debug=1 at the window's end, for runtime.MemStats (default <step>/heap.txt)")
	cgroupPath := fs.String("cgroup", "", "the step's cgroup.json (default <step>/cgroup.json)")
	outageCores := fs.String("outage-cores", "11.1,13.8", "the outage's Dispatch cores, <low>,<high> (10-minute means)")
	outageMemory := fs.Float64("outage-memory", 13.8, "the outage's Dispatch memory at its end, GiB")
	bucketsPath := fs.String("outage-buckets", "", "CSV label,offset_min,cores_mean,cores_max,mem_gib_mean,mem_gib_max (default the committed "+defaultOutageBucketsName+")")
	windowOffset := fs.Duration("window-offset", 0, "how long after 22:55 the replay's window starts, in whole minutes")
	notesPath := fs.String("residual-notes", "", "a markdown file whose list lines are what the replay does not reproduce")
	out := fs.String("out", "", "the directory that receives CODEPATHS.md and codepaths.json (required)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen codepaths -step <step dir> -out <dir> [flags]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *out == "" || fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("name -out, and no argument")
	}
	coresLow, coresHigh, err := parseOutageCores(*outageCores)
	if err != nil {
		return err
	}
	if !finite(*outageMemory) || *outageMemory < 0 {
		return fmt.Errorf("-outage-memory %v is not a non-negative number of GiB", *outageMemory)
	}
	if *windowOffset%time.Minute != 0 {
		return fmt.Errorf("-window-offset %s is not a whole number of minutes", *windowOffset)
	}
	bucketsSource, bucketsText := defaultOutageBucketsName+" (committed)", defaultOutageBucketsText
	if *bucketsPath != "" {
		b, err := os.ReadFile(*bucketsPath)
		if err != nil {
			return fmt.Errorf("-outage-buckets: %w", err)
		}
		bucketsSource, bucketsText = *bucketsPath, string(b)
	}
	buckets, err := parseOutageBuckets(bucketsSource, bucketsText)
	if err != nil {
		return err
	}
	rep := codepathsReport{}
	if *step != "" {
		rep.Step = filepath.Base(filepath.Clean(*step))
	}
	if *notesPath != "" {
		if rep.NotReproduced, err = readResidualNotes(*notesPath); err != nil {
			return err
		}
	}

	type input struct {
		name, flag string
		defaults   []string
		path, why  string
	}
	cpuIn := &input{name: "cpu", flag: *cpuPath, defaults: []string{"cpu.pprof"}}
	startIn := &input{name: "heap-start", flag: *heapStartPath, defaults: []string{"heap-start.pprof"}}
	endIn := &input{name: "heap-end", flag: *heapEndPath, defaults: []string{"heap-end.pprof", "heap.pprof"}}
	txtIn := &input{name: "heap-txt", flag: *heapTxtPath, defaults: []string{"heap.txt"}}
	cgIn := &input{name: "cgroup", flag: *cgroupPath, defaults: []string{"cgroup.json"}}
	for _, in := range []*input{cpuIn, startIn, endIn, txtIn, cgIn} {
		if in.path, in.why, err = cpFile(in.flag, *step, in.defaults...); err != nil {
			return fmt.Errorf("-%s: %w", in.name, err)
		}
		rep.Inputs = append(rep.Inputs, cpInput{Name: in.name, Path: in.path, Missing: in.why})
	}

	var cf *cgroupFile
	if cgIn.path != "" {
		if cf, err = readCgroupPath(cgIn.path); err != nil {
			return err
		}
	}
	cores, coresWhy := dispatchCores(cf)
	if cgIn.path == "" {
		coresWhy = "cgroup.json: " + cgIn.why
	}
	if cpuIn.path != "" {
		p, err := readProfileFile(cpuIn.path)
		if err != nil {
			return err
		}
		if rep.CPU, err = cpuByEntry(p, cores); err != nil {
			return fmt.Errorf("%s: %w", cpuIn.path, err)
		}
		rep.CPU.Profile, rep.CPU.CoresMissing = cpuIn.path, coresWhy
		if rep.Subsystems, err = subsystems(p, cores); err != nil {
			return fmt.Errorf("%s: %w", cpuIn.path, err)
		}
	}

	mi := memoryInputs{heapEndWhy: endIn.why, heapStartWhy: startIn.why, msWhy: txtIn.why}
	if endIn.path != "" {
		if mi.heapEnd, err = readProfileFile(endIn.path); err != nil {
			return err
		}
	}
	if startIn.path != "" {
		if mi.heapStart, err = readProfileFile(startIn.path); err != nil {
			return err
		}
	}
	if txtIn.path != "" {
		b, err := os.ReadFile(txtIn.path)
		if err != nil {
			return err
		}
		ms, err := parseMemStats(b)
		switch {
		case errors.Is(err, errNoMemStats):
			mi.msWhy = fmt.Sprintf("%s in its %d bytes (the block ends the profile, so a fetch cut short has none)", err, len(b))
		case err != nil:
			return fmt.Errorf("%s: %w", txtIn.path, err)
		default:
			mi.ms = &ms
		}
	}
	mi.ws, mi.wsSource = workingSetAtEnd(cf)
	if cgIn.path == "" {
		mi.wsSource = "cgroup.json: " + cgIn.why
	}
	if rep.Memory, err = memoryTable(mi); err != nil {
		return err
	}
	rep.Outage = outageTable(cpOutage{BucketsSource: bucketsSource, WindowOffsetMin: int(*windowOffset / time.Minute),
		OutageCoresLow: coresLow, OutageCoresHigh: coresHigh, OutageMemoryGiB: *outageMemory, ReplayMemorySource: mi.wsSource},
		buckets, cf, cores, coresWhy, mi.ws)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "codepaths.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "CODEPATHS.md"), []byte(renderCodepaths(rep)), 0o644); err != nil {
		return err
	}
	summary := "no CPU profile"
	if rep.CPU != nil {
		summary = fmt.Sprintf("%d CPU samples in %d entry-point rows", rep.CPU.Samples, len(rep.CPU.Rows))
	}
	fmt.Fprintf(os.Stderr, "loadgen codepaths: wrote CODEPATHS.md and codepaths.json in %s: %s, %d memory rows, %d outage buckets\n",
		*out, summary, len(rep.Memory.Rows), len(rep.Outage.Buckets))
	return nil
}

// --- CODEPATHS.md ----------------------------------------------------------------------------

func renderCodepaths(rep codepathsReport) string {
	var md markdown
	title := "Code paths"
	if rep.Step != "" {
		title += ": " + rep.Step
	}
	md.line("# %s\n", title)
	md.line("The code-path table: Dispatch's CPU by entry point and by subsystem, its working set at the window's end, and the replay beside the 2026-10-26 outage. `-` is a number an input did not give, never 0. Every number is also in codepaths.json.\n")
	md.line("Inputs:\n")
	for _, in := range rep.Inputs {
		if in.Path != "" {
			md.line("- %s: `%s`", in.Name, in.Path)
		} else {
			md.line("- %s: missing (%s)", in.Name, in.Missing)
		}
	}
	cpuEntryMD(&md, rep.CPU)
	subsystemMD(&md, rep.CPU, rep.Subsystems)
	memoryMD(&md, rep.Memory)
	outageMD(&md, rep.Outage, rep.NotReproduced)
	return md.String()
}

func cpuRowCells(r cpCPURow) []string {
	return []string{r.Row, num(float64(r.Samples), 0), shareCell(r.Share), num(r.CPUS, 2), numPtr(r.Cores, 2)}
}

func cpuEntryMD(md *markdown, c *cpCPU) {
	md.line("\n## CPU by entry point (exclusive)\n")
	if c == nil {
		md.line("No CPU profile (see Inputs), so this table and the subsystem table have no rows.")
		return
	}
	md.line("Every sample of `%s` (%s CPU s over a %s s profile) is assigned to exactly one row by its stack, in this precedence (the production profile's §2a rule): the HTTP handler named by the stack's handler frame (walking from the root, the first named function of Dispatch's API package below Go's HTTP server; the document websocket's handler is the docs service's ServeHTTP); the docs cold-read fold goroutine (`coldRead`'s singleflight closure, `foldDocument`); GC background mark workers (`runtime.gcBgMarkWorker`); GC sweep and scavenge (`runtime.bgsweep`, `runtime.bgscavenge`); the room observer (`docs.(*renderedReplica)`), settlement (`settleRoom`, `settleLastPeer`, the settle timer, `RunSettlementResumption`) and compaction (`Compact`, `CompactAll`); the rest of the document room server (ygo's websocket provider, the persistence adapter, stale-room eviction); the outbox (package `outbox`); SSE `/api/v1/events` streams (`streamEvents`, the event stream relay); profiling (`net/http/pprof`, `runtime/pprof`: the CPU profile, heap profiles and memory statistics the harness reads from Dispatch during the window); Go's HTTP server outside a handler (middleware, the access log, reading requests); scheduler and netpoll (a runtime scheduling frame, or a stack of runtime frames only); other. %q, %q and %q are not rows of §2a; they keep those goroutines out of `other`. The rows sum to the profile's total.\n",
		c.Profile, num(c.CPUS, 2), num(c.DurationS, 0), entryRoomsOther, entryProfiling, entryHTTPServer)
	if c.CgroupCoresMean != nil {
		md.line("Cores = the row's share × the Dispatch container's cgroup cores_mean over the window, %s.\n", num(*c.CgroupCoresMean, 2))
	} else {
		md.line("Cores show `-`: %s.\n", c.CoresMissing)
	}
	md.header("entry point", "samples", "share", "CPU s (profile)", "cores")
	for _, r := range c.Rows {
		md.row(cpuRowCells(r)...)
	}
	total := cpCPURow{Row: "**total**", Samples: c.Samples, CPUS: c.CPUS, Cores: c.CgroupCoresMean}
	if c.Samples > 0 {
		total.Share = new(1.0)
	}
	md.row(cpuRowCells(total)...)
	if len(c.OtherByStart) > 0 {
		md.line("\n`other`, by the function its goroutine started in (the outermost frame outside the runtime), the 10 largest:\n")
		md.header("goroutine start", "samples", "share of the profile", "CPU s (profile)", "cores")
		for _, r := range c.OtherByStart {
			r.Row = "`" + r.Row + "`"
			md.row(cpuRowCells(r)...)
		}
	}
}

func subsystemMD(md *markdown, c *cpCPU, subs []cpSubsystem) {
	if c == nil {
		return
	}
	md.line("\n## CPU by subsystem (inclusive)\n")
	md.line("A sample counts toward every subsystem on its stack, so the shares add up to more than 100%%. The subsystems are the production profile's §2b; the rows marked *(write path, overlapping)* are INFRA-512's write-path functions, matched by function name suffix (`len16` is `pmdoc.len16`; `loadRoom` is ygo's `(*Server).loadRoom`, `fold` is `docs.fold`), which overlap the subsystems and each other. `(not in profile)`: no function of the profile has that name.\n")
	md.header("subsystem", "samples", "share", "CPU s (profile)", "cores")
	for _, s := range subs {
		r := s.cpCPURow
		if s.WritePath {
			r.Row += " *(write path, overlapping)*"
		}
		cells := cpuRowCells(r)
		if !s.InProfile && c.Samples > 0 {
			cells[2] += " (not in profile)"
		}
		md.row(cells...)
	}
}

func memoryMD(md *markdown, m cpMemory) {
	md.line("\n## Memory at the window's end\n")
	ws := "-"
	if m.WorkingSetBytes != nil {
		ws = num(gibOf(*m.WorkingSetBytes), 2) + " GiB"
	}
	md.line("The Dispatch container's working set, %s (%s), split into the Go heap in use by entry point (the CPU table's rule applied to heap-end's `inuse_space` samples, each row's bytes = its `inuse_space` share × runtime.MemStats HeapInuse), heap idle retained, stacks, other runtime memory and non-Go memory, from heap.txt's runtime.MemStats. The rows sum to the working set. \"allocated in window\" is the entry point's heap-end minus heap-start `alloc_space`.\n",
		ws, m.WorkingSetSource)
	md.header("part", "MiB", "GiB", "share of working set", "inuse_space share", "allocated in window GiB")
	for _, r := range m.Rows {
		md.row(r.Row, mibCell(r.Bytes), gibCell(r.Bytes, 3), shareCell(r.ShareOfWorkingSet), shareCell(r.InuseShare), gibCell(r.WindowAllocBytes, 3))
	}
	total := (*float64)(nil)
	if m.WorkingSetBytes != nil {
		total = new(1.0)
	}
	md.row("**working set**", mibCell(m.WorkingSetBytes), gibCell(m.WorkingSetBytes, 3), shareCell(total), "", "")
	if m.HeapReleased != nil {
		md.line("\nHeapReleased, %s MiB (%s GiB): heap pages the runtime has returned to the OS. They are in HeapSys and Sys but not in the working set, so no row holds them: heap idle retained and non-Go memory leave them out.", mibCell(m.HeapReleased), gibCell(m.HeapReleased, 3))
	}
	for _, n := range m.Notes {
		md.line("\n%s", n)
	}
}

func mibCell(b *float64) string {
	if b == nil {
		return "-"
	}
	return num(mib(*b), 0)
}

func gibCell(b *float64, d int) string {
	if b == nil {
		return "-"
	}
	return num(gibOf(*b), d)
}

func outageMD(md *markdown, o cpOutage, notes []string) {
	md.line("\n## Beside the outage\n")
	md.line("Production's outage pod in 10-minute buckets (%s; the production profile's §5, Cloud Monitoring: cores and non-evictable memory, the mean and maximum of each bucket's 1-minute points), beside the replay's Dispatch container over the same offsets from 22:55 (cgroup.json's dispatch minutes; the replay's window starts %d minutes after 22:55): its cores and its working set, the mean and maximum of the 1-minute figures (the working set's maximum is the highest minute mean).\n",
		o.BucketsSource, o.WindowOffsetMin)
	md.header("bucket", "offset min", "outage cores mean / max", "replay cores mean / max", "outage memory GiB mean / max", "replay working set GiB mean / max", "replay minutes")
	var missing []string
	for _, b := range o.Buckets {
		md.row(b.Label, strconv.Itoa(b.OffsetMin), num(b.OutageCoresMean, 2)+" / "+num(b.OutageCoresMax, 2),
			numPtr(b.ReplayCoresMean, 2)+" / "+numPtr(b.ReplayCoresMax, 2), num(b.OutageMemGiBMean, 2)+" / "+num(b.OutageMemGiBMax, 2),
			numPtr(b.ReplayWSGiBMean, 2)+" / "+numPtr(b.ReplayWSGiBMax, 2), fmt.Sprintf("%d of %d", b.ReplayMinutes, outageBucketMinutes))
		if b.ReplayMissing != "" {
			missing = append(missing, fmt.Sprintf("%s: %s", b.Label, b.ReplayMissing))
		}
	}
	if len(missing) > 0 {
		md.line("\nThe replay's cells show `-` where it has no minute: %s.", strings.Join(missing, "; "))
	}
	md.line("\nTotals: the replay's cores_mean over its window against the outage's 10-minute means, and its working set at the window's end (%s) against the outage's memory at its end. The residual is the outage's figure minus the replay's.\n", o.ReplayMemorySource)
	md.header("row", "Dispatch cores", "Dispatch memory GiB")
	md.row("outage", num(o.OutageCoresLow, 2)+"-"+num(o.OutageCoresHigh, 2), num(o.OutageMemoryGiB, 2))
	md.row("replay", numPtr(o.ReplayCores, 2), numPtr(o.ReplayMemoryGiB, 2))
	residual := "-"
	if o.ResidualCoresLow != nil {
		residual = num(*o.ResidualCoresLow, 2) + " to " + num(*o.ResidualCoresHigh, 2)
	}
	md.row("**residual** (outage - replay)", residual, numPtr(o.ResidualMemoryGiB, 2))
	for _, m := range o.Missing {
		md.line("\n%s, so its cells show `-`.", m)
	}
	if len(notes) == 0 {
		md.line("\n## Not reproduced: none given (no -residual-notes)")
		return
	}
	md.line("\n## Not reproduced\n")
	for _, n := range notes {
		md.line("%s", n)
	}
}
