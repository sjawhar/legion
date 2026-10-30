package main

// The replay's per-run record: summary.json's `replay` block and meta.json's `replay` record. The
// field names and types of replayRecord and replayInputs are a contract another team's tool writes
// too: never rename them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// replayInputs are the inputs a replay run's result depends on.
type replayInputs struct {
	RateFactor    float64            `json:"rate_factor"`
	ReadsPerWrite float64            `json:"reads_per_write"`
	ReadMix       map[string]float64 `json:"read_mix"`
	UploadShare   float64            `json:"upload_share"`
	RNGSeed       uint64             `json:"rng_seed"`
	FromS         float64            `json:"from_s"`
	ToS           float64            `json:"to_s"`
	// BundleSHA256 is the sha256 of the bundle's bundle.meta.json, which holds every other
	// file's sha256.
	BundleSHA256 string `json:"bundle_sha256"`
	// SeedDumpSHA256 is the restored seed dump's, from $LOADTEST_ROOT/replay/last-reset.json; ""
	// without that file.
	SeedDumpSHA256 string `json:"seed_dump_sha256"`
	// GOMAXPROCS is Dispatch's: from its environment, else the stack's setting.
	GOMAXPROCS int `json:"gomaxprocs"`
	// DispatchCommit is the commit $LOADTEST_ROOT/bin/envoy-dispatch.json says Dispatch was built
	// from.
	DispatchCommit string `json:"dispatch_commit"`
}

type replayWriteCount struct {
	Planned int `json:"planned"`
	Sent    int `json:"sent"`
	Failed  int `json:"failed"`
}

type replaySubstitutes struct {
	Quote int `json:"quote"`
	Edit  int `json:"edit"`
}

type replayEdits struct {
	Diff       int `json:"diff"`
	Substitute int `json:"substitute"`
}

// replayBaseDocs is summary.json's replay.base_docs: how the client's copy of each document the
// plan modelled was matched to the build under test before the window (readBaseTexts).
type replayBaseDocs struct {
	// Read and Unread count the plan's documents whose current markdown the build served and
	// those it did not (no local id, or no 2xx answer).
	Read   int `json:"read"`
	Unread int `json:"unread"`
	// Differed counts the documents the build serves otherwise than the bundle holds them, which
	// the client models from the build's text instead; Replanned says the plan was built again
	// from those texts.
	Differed  int  `json:"differed"`
	Replanned bool `json:"replanned"`
	// Documents are those documents' logged ids, sorted.
	Documents []string `json:"documents"`
}

// replayRecord is summary.json's replay block.
type replayRecord struct {
	Inputs  replayInputs `json:"inputs"`
	WindowS float64      `json:"window_s"`
	// CPUSource is "proc" (Dispatch's /proc/<pid>/stat), or "none" when no pid was readable
	// (-allow-foreign); the CPU fields are null then.
	CPUSource                  string   `json:"cpu_source"`
	DispatchCPUS               *float64 `json:"dispatch_cpu_s"`
	CoresFullRate              *float64 `json:"cores_full_rate"`
	IdleCoresBefore            *float64 `json:"idle_cores_before"`
	CoresFullRateIdleCorrected *float64 `json:"cores_full_rate_idle_corrected"`
	DrainCPUS                  *float64 `json:"drain_cpu_s"`
	// Writes are the write actions by kind; SkippedMissingDep the actions (reads included) not
	// sent because an object they name was never created.
	Writes            map[string]replayWriteCount `json:"writes"`
	SkippedMissingDep map[string]int              `json:"skipped_missing_dep"`
	Substitutes       replaySubstitutes           `json:"substitutes"`
	Edits             replayEdits                 `json:"edits"`
	InflightAtEnd     int                         `json:"inflight_at_end"`
	Timeouts          int                         `json:"timeouts"`
	Saturated         bool                        `json:"saturated"`
	SaturatedReasons  []string                    `json:"saturated_reasons"`
	// SaturationCores is the cores the CPU criterion was measured against (saturationCores), and
	// SaturationCoresFrom names which limit set it: "gomaxprocs", "nproc" or "cgroup_cpu_max".
	SaturationCores     float64 `json:"saturation_cores"`
	SaturationCoresFrom string  `json:"saturation_cores_from"`
	Roster              int     `json:"roster"`
	// RosterSkipped counts the bundle roster's ids the listener refuses as a key (natsKey), which
	// synthetic sessions replace.
	RosterSkipped int `json:"roster_skipped"`
	// DocOrderWaits are the writes that found the previous write on their document still running
	// and waited for it; DocOrderWaitS their waits in seconds.
	DocOrderWaits int  `json:"doc_order_waits"`
	DocOrderWaitS stat `json:"doc_order_wait_s"`
	// BaseDocs says how the client's copy of each document was matched to the build under test
	// before the window; null when the plan modelled no document.
	BaseDocs *replayBaseDocs `json:"base_docs"`
	// EndState digests the documents the run wrote, read after the drain; null when that read
	// failed (no Postgres reachable).
	EndState *replayEndState `json:"end_state"`
}

// replayMetaRecord is meta.json's replay record.
type replayMetaRecord struct {
	Inputs replayInputs `json:"inputs"`
	// CgroupCPUMax is /sys/fs/cgroup/cpu.max; "" when absent.
	CgroupCPUMax string `json:"cgroup_cpu_max"`
	// HeapStartAt and HeapEndAt are when heap-start.pprof and heap-end.pprof were requested.
	HeapStartAt time.Time `json:"heap_start_at,omitzero"`
	HeapEndAt   time.Time `json:"heap_end_at,omitzero"`
	// DispatchBuild is $LOADTEST_ROOT/bin/envoy-dispatch.json (dispatch_commit, dispatch_tree,
	// dispatch_pprof_overlay, dispatch_binary_sha256, go_version, ...).
	DispatchBuild json.RawMessage `json:"dispatch_build,omitempty"`
	// LastReset is $LOADTEST_ROOT/replay/last-reset.json, stack.sh reset --replay's record: the
	// template, the build key, seed_dump_sha256, the checksum of database dispatch before Dispatch
	// started, the migration and whether the block index owed nothing.
	LastReset json.RawMessage `json:"last_reset,omitempty"`
}

// cpuReading is one read of Dispatch's utime+stime ticks.
type cpuReading struct {
	at    time.Time
	ticks float64
	ok    bool
}

// replayCPU are the run's reads: before the idle interval, at the window start and end, and once
// the drain ends.
type replayCPU struct {
	idle, start, end, drain cpuReading
}

// Saturation reasons.
const (
	saturatedCPU      = "dispatch_cpu"
	saturatedTimeouts = "timeouts"
	saturatedInflight = "inflight_at_end"
)

// collectReplayMeta records the inputs only the stack knows: the seed dump, Dispatch's GOMAXPROCS
// and build, the cgroup's CPU limit.
func collectReplayMeta(in replayInputs, pid int, root string) replayMetaRecord {
	m := replayMetaRecord{}
	if b, err := os.ReadFile(filepath.Join(root, "replay", "last-reset.json")); err == nil && json.Valid(b) {
		m.LastReset = b
		var r struct {
			SeedDump string `json:"seed_dump_sha256"`
			Dump     string `json:"dump_sha256"`
		}
		if json.Unmarshal(b, &r) == nil {
			in.SeedDumpSHA256 = r.SeedDump
			if in.SeedDumpSHA256 == "" {
				in.SeedDumpSHA256 = r.Dump
			}
		}
	}
	in.GOMAXPROCS = dispatchGOMAXPROCS(pid)
	if bld, ok := readDispatchBuild(root); ok {
		m.DispatchBuild = bld.raw
		in.DispatchCommit = bld.commit
	}
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		m.CgroupCPUMax = strings.TrimSpace(string(b))
	}
	m.Inputs = in
	return m
}

// dispatchGOMAXPROCS is GOMAXPROCS from Dispatch's environment, else the stack's setting
// (LOADTEST_GOMAXPROCS, default nproc, as stack.sh starts Dispatch).
func dispatchGOMAXPROCS(pid int) int {
	if pid > 0 {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
			for kv := range strings.SplitSeq(string(b), "\x00") {
				if v, ok := strings.CutPrefix(kv, "GOMAXPROCS="); ok {
					if n, err := strconv.Atoi(v); err == nil && n > 0 {
						return n
					}
				}
			}
		}
	}
	if n, err := strconv.Atoi(os.Getenv("LOADTEST_GOMAXPROCS")); err == nil && n > 0 {
		return n
	}
	return runtime.NumCPU()
}

// replayCoresFullRate is Dispatch's cores at the full rate: CPU-seconds over the window, divided
// by the rate factor.
func replayCoresFullRate(cpuS, windowS, rate float64) float64 {
	return cpuS / windowS / rate
}

// replayIdleCorrected is replayCoresFullRate with the idle cores before the window taken off the
// window's CPU first.
func replayIdleCorrected(cpuS, idleCores, windowS, rate float64) float64 {
	return (cpuS - idleCores*windowS) / windowS / rate
}

// dispatchCoreSamples are Dispatch's cores over each interval between consecutive in-window 5 s
// samples.
func dispatchCoreSamples(ss []sample) []float64 {
	var out []float64
	var prev *sample
	for i := range ss {
		s := &ss[i]
		if !s.InWindow {
			continue
		}
		if _, ok := s.CPUTicks["dispatch"]; !ok {
			continue
		}
		if prev != nil {
			if dt := s.At.Sub(prev.At).Seconds(); dt > 0 {
				out = append(out, (s.CPUTicks["dispatch"]-prev.CPUTicks["dispatch"])/clkTck/dt)
			}
		}
		prev = s
	}
	return out
}

// cgroupQuotaCores is the CPU quota of a /sys/fs/cgroup/cpu.max text ("<quota> <period>", both in
// microseconds) in cores; ok is false when no quota is set ("max") or the text is not that shape.
func cgroupQuotaCores(cpuMax string) (float64, bool) {
	q, p, ok := strings.Cut(strings.TrimSpace(cpuMax), " ")
	if !ok {
		return 0, false
	}
	quota, qErr := strconv.ParseFloat(q, 64)
	period, pErr := strconv.ParseFloat(strings.TrimSpace(p), 64)
	if qErr != nil || pErr != nil || quota <= 0 || period <= 0 {
		return 0, false
	}
	return quota / period, true
}

// saturationCores are the cores Dispatch can in fact use: the least of its GOMAXPROCS, the box's
// nproc and the cgroup's CPU quota in cores when one is set. GOMAXPROCS alone is not a limit a
// 2-core box can reach (--gomaxprocs 3 there), and neither is a GOMAXPROCS above the cgroup quota.
// from names which of the three set the figure.
func saturationCores(gomaxprocs, nproc int, cgroupCPUMax string) (float64, string) {
	cores, from := float64(gomaxprocs), "gomaxprocs"
	if nproc > 0 && float64(nproc) < cores {
		cores, from = float64(nproc), "nproc"
	}
	if q, ok := cgroupQuotaCores(cgroupCPUMax); ok && q < cores {
		cores, from = q, "cgroup_cpu_max"
	}
	return cores, from
}

// replaySaturation says whether the run saturated and why: Dispatch's 5 s cores reached 0.9 x
// satCores (saturationCores) in at least 10% of the window's samples, an action hit its deadline,
// or more than 20 actions were in flight at the window end.
func replaySaturation(cores []float64, satCores float64, timeouts, inflightAtEnd int) (bool, []string) {
	reasons := []string{}
	if len(cores) > 0 && satCores > 0 {
		high := 0
		for _, c := range cores {
			if c >= 0.9*satCores {
				high++
			}
		}
		if float64(high) >= 0.1*float64(len(cores)) {
			reasons = append(reasons, saturatedCPU)
		}
	}
	if timeouts > 0 {
		reasons = append(reasons, saturatedTimeouts)
	}
	if inflightAtEnd > 20 {
		reasons = append(reasons, saturatedInflight)
	}
	return len(reasons) > 0, reasons
}

// record is the run's replay block, from its CPU reads, counters and the sampler's samples;
// windowS is the configured window, used when the /proc reads are missing.
func (rr *replayRun) record(ss []sample, windowS float64) *replayRecord {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rec := &replayRecord{
		Inputs: rr.meta.Inputs, WindowS: windowS, CPUSource: "none", Writes: map[string]replayWriteCount{}, SkippedMissingDep: map[string]int{},
		Substitutes: rr.subs, Edits: rr.edits, InflightAtEnd: rr.inflightAtEnd, Timeouts: int(rr.timeouts.Load()), Roster: len(rr.setup.roster),
		RosterSkipped: rr.setup.rosterSkipped,
		DocOrderWaits: len(rr.docWaits), DocOrderWaitS: mkStat(rr.docWaits), BaseDocs: rr.baseDocs, EndState: rr.endState,
	}
	for k, w := range rr.writes {
		rec.Writes[k] = *w
	}
	for k, n := range rr.skipped {
		rec.SkippedMissingDep[k] = n
	}
	rate := rr.setup.opt.rate
	if c := rr.cpu; c.start.ok && c.end.ok {
		rec.CPUSource = "proc"
		if dt := c.end.at.Sub(c.start.at).Seconds(); dt > 0 {
			rec.WindowS = dt
		}
		cpu := (c.end.ticks - c.start.ticks) / clkTck
		cores := replayCoresFullRate(cpu, rec.WindowS, rate)
		rec.DispatchCPUS, rec.CoresFullRate = &cpu, &cores
		if dt := c.start.at.Sub(c.idle.at).Seconds(); c.idle.ok && dt > 0 {
			idle := (c.start.ticks - c.idle.ticks) / clkTck / dt
			corrected := replayIdleCorrected(cpu, idle, rec.WindowS, rate)
			rec.IdleCoresBefore, rec.CoresFullRateIdleCorrected = &idle, &corrected
		}
		if c.drain.ok {
			drain := (c.drain.ticks - c.end.ticks) / clkTck
			rec.DrainCPUS = &drain
		}
	}
	rec.SaturationCores, rec.SaturationCoresFrom = saturationCores(rec.Inputs.GOMAXPROCS, runtime.NumCPU(), rr.meta.CgroupCPUMax)
	rec.Saturated, rec.SaturatedReasons = replaySaturation(dispatchCoreSamples(ss), rec.SaturationCores, rec.Timeouts, rec.InflightAtEnd)
	return rec
}

// replayEndState is summary.json's replay.end_state: the documents the run wrote and the sha256
// of their end-state lines (endStateDigest).
type replayEndState struct {
	Documents int    `json:"documents"`
	SHA256    string `json:"sha256"`
}

// replayEndAnchor is an unresolved anchored root comment's anchor: its quote's sha256, its block
// id and where that block stands in the document (endStateBlockIndexes). The digest covers
// block_index, not block_id: Dispatch mints a fresh uuid for every block a replay write creates,
// so two runs that did the same work hold the same anchor on different block ids. block_id is
// kept for debugging.
type replayEndAnchor struct {
	QuoteSHA256 string `json:"quote_sha256"`
	BlockID     string `json:"block_id"`
	BlockIndex  string `json:"block_index"`
}

// endStateBlockUnknown is the block index of an anchor whose block the document no longer holds
// (an orphaned anchor).
const endStateBlockUnknown = "-1"

// replayEndDoc is one written document's end state, by its logged id (stable across runs, unlike
// the local id of a document the run creates): its highest version number and that version's
// size, both 0 when the document does not exist locally, and its anchors.
type replayEndDoc struct {
	ArtifactID string            `json:"artifact_id"`
	Version    int               `json:"version"`
	Size       int               `json:"size"`
	Anchors    []replayEndAnchor `json:"anchors"`
}

// endStateDigestFields says which fields the sha256 covers, for a reader of end_state.json.
const endStateDigestFields = "sha256 over, per document sorted by artifact_id: artifact_id, version, size, " +
	"and each anchor's quote_sha256@block_index sorted; block_id is not covered, as Dispatch mints " +
	"a fresh uuid for every block a write creates"

// replayEndStateFile is end_state.json: ids, numbers and hashes only.
type replayEndStateFile struct {
	Documents []replayEndDoc `json:"documents"`
	SHA256    string         `json:"sha256"`
	Digest    string         `json:"digest"`
}

// endStateDigest sorts the documents by id and each one's anchors, and returns the sha256 of one
// line per document: "<artifact id> <version> <size> <quote sha256>@<block index>,...".
func endStateDigest(docs []replayEndDoc) string {
	slices.SortFunc(docs, func(a, b replayEndDoc) int { return strings.Compare(a.ArtifactID, b.ArtifactID) })
	h := sha256.New()
	for _, d := range docs {
		slices.SortFunc(d.Anchors, func(a, b replayEndAnchor) int {
			if c := strings.Compare(a.QuoteSHA256, b.QuoteSHA256); c != 0 {
				return c
			}
			return strings.Compare(a.BlockIndex, b.BlockIndex)
		})
		anchors := make([]string, len(d.Anchors))
		for i, a := range d.Anchors {
			anchors[i] = a.QuoteSHA256 + "@" + a.BlockIndex
		}
		fmt.Fprintf(h, "%s %d %d %s\n", d.ArtifactID, d.Version, d.Size, strings.Join(anchors, ","))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// endStateBlockIndexes is, by block id, where each of a document's blocks stands in document
// order: "<i>" for the i-th top-level block, "<i>.<r>" for its r-th table row and "<i>.<r>.<c>"
// for that row's c-th cell. A block the live document has not stamped with an id is left out.
func endStateBlockIndexes(served []servedBlock) map[string]string {
	ix := make(map[string]string, len(served))
	for i, b := range served {
		top := strconv.Itoa(i)
		if b.ID != "" {
			ix[b.ID] = top
		}
		if b.Table == nil {
			continue
		}
		for r, row := range b.Table.Rows {
			rp := top + "." + strconv.Itoa(r)
			if row.ID != "" {
				ix[row.ID] = rp
			}
			for c, cell := range row.Cells {
				if cell.ID != "" {
					ix[cell.ID] = rp + "." + strconv.Itoa(c)
				}
			}
		}
	}
	return ix
}

// readEndState reads, over the stack's Postgres URL, the end state of every document the run
// wrote, and over the run's own Dispatch client where each anchor's block stands in its document,
// writes end_state.json and sets the record's end_state; a failed read leaves it null.
func (rr *replayRun) readEndState(a *api) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rr.mu.Lock()
	logged := slices.Sorted(maps.Keys(rr.wrote))
	rr.mu.Unlock()
	docs, local, err := rr.queryEndState(ctx, logged)
	if err == nil {
		err = readEndStateBlocks(ctx, a, docs, local)
	}
	if err != nil {
		rr.lg.Printf("replay: end state not read (end_state is null): %v", err)
		return
	}
	f := replayEndStateFile{Documents: docs, SHA256: endStateDigest(docs), Digest: endStateDigestFields}
	writeJSONFile(filepath.Join(rr.out, "end_state.json"), f, 0o644, rr.lg)
	rr.mu.Lock()
	rr.endState = &replayEndState{Documents: len(docs), SHA256: f.SHA256}
	rr.mu.Unlock()
	rr.lg.Printf("replay: end state of %d documents, sha256 %s", len(docs), f.SHA256)
}

// readEndStateBlocks sets each anchor's block index from the blocks of its document in document
// order (GET /api/v1/artifacts/{id}/blocks, over the run's agent bearer), endStateBlockUnknown
// for a block id the document no longer holds. local is the local id of each document by its
// logged id; a document with no anchor is not read.
func readEndStateBlocks(ctx context.Context, a *api, docs []replayEndDoc, local map[string]string) error {
	for i := range docs {
		d := &docs[i]
		if len(d.Anchors) == 0 {
			continue
		}
		id, ok := local[d.ArtifactID]
		if !ok {
			return fmt.Errorf("artifact %s blocks: no local id", d.ArtifactID)
		}
		res := a.callRetry(ctx, "GET", "/api/v1/artifacts/"+url.PathEscape(id)+"/blocks", who{}, nil)
		if !res.ok() {
			return fmt.Errorf("artifact %s blocks: status %d %s: %v", d.ArtifactID, res.status, res.errorCode(), res.err)
		}
		var served []servedBlock
		if err := json.Unmarshal(res.body, &served); err != nil {
			return fmt.Errorf("artifact %s blocks: %w", d.ArtifactID, err)
		}
		ix := endStateBlockIndexes(served)
		for j := range d.Anchors {
			bi, ok := ix[d.Anchors[j].BlockID]
			if !ok {
				bi = endStateBlockUnknown
			}
			d.Anchors[j].BlockIndex = bi
		}
	}
	return nil
}

// queryEndState reads each written document's version, size and anchors over Postgres, and
// returns the local id of every document that exists locally, by its logged id.
func (rr *replayRun) queryEndState(ctx context.Context, logged []string) ([]replayEndDoc, map[string]string, error) {
	if rr.pg == "" {
		return nil, nil, errors.New("no Postgres URL")
	}
	conn, err := pgx.Connect(ctx, rr.pg)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: %w", err)
	}
	defer conn.Close(context.Background())
	docs := make([]replayEndDoc, 0, len(logged))
	locals := make(map[string]string, len(logged))
	for _, id := range logged {
		d := replayEndDoc{ArtifactID: id, Anchors: []replayEndAnchor{}}
		if local, ok := rr.localNow(replayRef{idArtifact, id}); ok {
			locals[id] = local
			if err := conn.QueryRow(ctx, `select coalesce(max(number), 0),
				coalesce((select coalesce(size, octet_length(markdown), octet_length(content), 0) from artifact_versions
					where artifact_id = $1 order by number desc limit 1), 0)
				from artifact_versions where artifact_id = $1`, local).Scan(&d.Version, &d.Size); err != nil {
				return nil, nil, fmt.Errorf("artifact_versions: %w", err)
			}
			rows, err := conn.Query(ctx, `select coalesce(anchor->>'quote', ''), coalesce(anchor->>'block_id', '') from comments
				where anchor is not null and reply_to is null and not resolved and anchor->>'artifact_id' = $1`, local)
			if err != nil {
				return nil, nil, fmt.Errorf("comments: %w", err)
			}
			var quote, block string
			if _, err := pgx.ForEachRow(rows, []any{&quote, &block}, func() error {
				d.Anchors = append(d.Anchors, replayEndAnchor{QuoteSHA256: sha256Hex([]byte(quote)), BlockID: block})
				return nil
			}); err != nil {
				return nil, nil, fmt.Errorf("comments: %w", err)
			}
		}
		docs = append(docs, d)
	}
	return docs, locals, nil
}

// localNow is the local id of a logged object once the run is over: the response of the run's
// own action that created it, else loadtest.replay_ids.
func (rr *replayRun) localNow(ref replayRef) (string, bool) {
	if f, ok := rr.futures[ref]; ok {
		select {
		case <-f.done:
			return f.local, f.ok
		default:
			return "", false
		}
	}
	v, ok := rr.ids[[2]string{ref.Kind, ref.Logged}]
	return v, ok
}
