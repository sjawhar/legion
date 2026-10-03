package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Profiling the cold edit (LEGION-504). Not a bound: a measurement the memory tests' servers take
// when DISPATCH_EDIT_PROFILE_OUT names a directory.

const (
	editProfileOutEnv     = "DISPATCH_EDIT_PROFILE_OUT"
	editProfileRepeatsEnv = "DISPATCH_EDIT_PROFILE_REPEATS"
	editProfileShapesEnv  = "DISPATCH_EDIT_PROFILE_SHAPES"
	// heapProfileDirEnv makes a memory test's server sample its live heap: each time the heap
	// grows a step past what the last collection left, it collects and, where the live heap is the
	// most its phase has seen, writes the heap profile as peak-<phase>.pb.gz. The phase is what
	// the test last wrote to the directory's phase file.
	heapProfileDirEnv = "DISPATCH_MEMORY_PROFILE_DIR"
)

func init() {
	dir := os.Getenv(heapProfileDirEnv)
	if os.Getenv(memoryTestServeEnv) != "1" || dir == "" {
		return
	}
	runtime.MemProfileRate = 16 << 10
	// Let a heap reference tool attach (Yama's ptrace_scope 1 admits only a tracer this names).
	const prSetPtracer, prSetPtracerAny = 0x59616d61, ^uintptr(0)
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetPtracer, prSetPtracerAny, 0)
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		panic(err)
	}
	go sampleLiveHeap(dir)
}

func sampleLiveHeap(dir string) {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/gc/heap/live:bytes"}}
	timeline, err := os.Create(filepath.Join(dir, "timeline.tsv"))
	if err != nil {
		panic(err)
	}
	started := time.Now()
	peaks := map[string]uint64{}
	phase := "start"
	var floor uint64
	for {
		time.Sleep(time.Millisecond)
		if current := readPhase(dir); current != phase {
			phase = current
			writeProfile(filepath.Join(dir, "allocs-at-"+phase+".pb.gz"), "allocs")
			if phase == "final" {
				runtime.GC()
				writeProfile(filepath.Join(dir, "final.pb.gz"), "heap")
			}
		}
		metrics.Read(samples[:1])
		heap := samples[0].Value.Uint64()
		if heap < floor {
			floor = heap
		}
		if heap < floor+max(8<<20, floor/16) {
			continue
		}
		runtime.GC()
		metrics.Read(samples)
		live := samples[1].Value.Uint64()
		floor = live
		elapsed := time.Since(started).Milliseconds()
		fmt.Fprintf(timeline, "%d\t%s\t%d\t%d\n", elapsed, phase, heap>>20, live>>20)
		if os.Getenv("DISPATCH_MEMORY_PROFILE_EVERY") == "1" && phase != "idle" && phase != "start" {
			writeProfile(filepath.Join(dir, fmt.Sprintf("heap-%06d-%s-%03d.pb.gz", elapsed, phase, live>>20)), "heap")
		}
		if live > peaks[phase] {
			peaks[phase] = live
			writeProfile(filepath.Join(dir, "peak-"+phase+".pb.gz"), "heap")
			fmt.Fprintf(timeline, "#peak %s %d MiB\n", phase, live>>20)
		}
	}
}

func readPhase(dir string) string {
	phase, err := os.ReadFile(filepath.Join(dir, "phase"))
	if err != nil {
		return "start"
	}
	return strings.TrimSpace(string(phase))
}

func writeProfile(path, name string) {
	file, err := os.Create(path + ".tmp")
	if err != nil {
		panic(err)
	}
	if err := pprof.Lookup(name).WriteTo(file, 0); err != nil {
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		panic(err)
	}
}

// coldEditDocument is a document the profile edits: the heaviest the element limit admits of a
// shape, or one of the repository's own.
type coldEditDocument struct {
	name, markdown, artifactID string
}

// TestProfileTheColdEditOfTheHeaviestDocuments stores the heaviest documents of every shape the
// element limit admits, and some of the repository's own, then makes a same-length one-word edit
// of each on servers that have not loaded it: repeats of the edit and of the settlement after it,
// each peak logged above idle, then one run that samples the server's live heap.
func TestProfileTheColdEditOfTheHeaviestDocuments(t *testing.T) {
	out := os.Getenv(editProfileOutEnv)
	if out == "" {
		t.Skip(editProfileOutEnv + " names where the profiles go")
	}
	repeats := 3
	if value := os.Getenv(editProfileRepeatsEnv); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		repeats = parsed
	}
	only := map[string]bool{}
	for name := range strings.SplitSeq(os.Getenv(editProfileShapesEnv), ",") {
		if name != "" {
			only[name] = true
		}
	}
	var documents []coldEditDocument
	for _, shape := range heaviestAdmittedShapes(t) {
		documents = append(documents, coldEditDocument{name: shape.name, markdown: withSentinelWord(t, shape.markdown)})
	}
	for _, path := range []string{"docs/superpowers/plans/2026-09-05-dispatch-conversations.md", "packages/envoy/AGENTS.md", "scripts/e2e/README.md", "packages/envoy/README.md"} {
		text, err := os.ReadFile(filepath.Join("..", "..", "..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, coldEditDocument{name: path, markdown: string(text) + "\n\nThe wurd ends it.\n"})
	}
	memory := newMemoryHarness(t)
	writer := memory.start(t)
	var stored []coldEditDocument
	for _, document := range documents {
		if len(only) > 0 && !only[document.name] {
			continue
		}
		upload := writer.upload(t, "json", memory.issue, document.markdown)
		if upload.status != http.StatusCreated {
			t.Logf("%s: upload answered %d %.300s; not profiled", document.name, upload.status, upload.body)
			continue
		}
		memory.waitForSettlement(t, upload)
		document.artifactID = upload.artifactID
		stored = append(stored, document)
	}
	writer.stop(t)
	words := [2]string{"wurd", "wird"}
	edits := map[string]int{}
	edit := func(server *dispatchProcess, document coldEditDocument) time.Duration {
		find, with := words[edits[document.name]%2], words[(edits[document.name]+1)%2]
		edits[document.name]++
		started := time.Now()
		answer := server.edit(t, document.artifactID, map[string]any{"op": "replace", "find": find, "with": with})
		if answer.status != http.StatusOK {
			t.Fatalf("%s: the edit answered %d %.300s", document.name, answer.status, answer.body)
		}
		return time.Since(started).Round(time.Millisecond)
	}
	for _, document := range stored {
		for run := range repeats {
			server := memory.start(t)
			var elapsed time.Duration
			editPeak := server.peakAboveIdle(t, func() { elapsed = edit(server, document) })
			settlePeak := server.peakAboveIdle(t, func() { memory.waitForSettled(t, document.artifactID) })
			server.stop(t)
			t.Logf("RESULT\t%s\trun %d\tedit %d MiB in %s\tsettlement %d MiB", document.name, run, editPeak>>20, elapsed, settlePeak>>20)
		}
		dir := filepath.Join(out, strings.NewReplacer("/", "_", " ", "_").Replace(document.name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		setPhase := func(phase string) {
			if err := os.WriteFile(filepath.Join(dir, "phase"), []byte(phase), 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		setPhase("idle")
		memory.env = []string{heapProfileDirEnv + "=" + dir, "DISPATCH_MEMORY_PROFILE_EVERY=" + os.Getenv("DISPATCH_MEMORY_PROFILE_EVERY")}
		server := memory.start(t)
		memory.env = nil
		time.Sleep(500 * time.Millisecond)
		setPhase("edit")
		elapsed := edit(server, document)
		setPhase("settle")
		memory.waitForSettled(t, document.artifactID)
		setPhase("done")
		time.Sleep(2 * time.Second)
		setPhase("final")
		time.Sleep(time.Second)
		if os.Getenv("DISPATCH_MEMORY_PROFILE_HOLD") == "1" {
			// A reference tool attaches now; it writes release when it is done.
			for {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
				time.Sleep(time.Second)
			}
		}
		server.stop(t)
		t.Logf("PROFILED\t%s\tedit in %s\t%s", document.name, elapsed, dir)
	}
}
