package docs

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// On every spec of the write corpus (runWriteCorpus), warm and cold, each anchored MarkQuote reads
// its fork twice, before and after its write, and each read joined to the write - every
// SnapshotVersion after a write, and the suggestion's SuggestionKind - and each ProjectMark's
// margin write take the write's rendering in place of reading the fork, and every rendering the
// write hands on is a fresh read and render of its fork (checkRendering). The test skips unless
// DISPATCH_TOKEN_CORPUS names the directory.
func TestWriteCorpusReadsOnce(t *testing.T) {
	dir := os.Getenv("DISPATCH_TOKEN_CORPUS")
	if dir == "" {
		t.Skip("DISPATCH_TOKEN_CORPUS unset; set it to a directory of markdown specs to count the writes' reads on them")
	}
	names, err := tokenCorpusSpecs(dir)
	if err != nil {
		t.Fatalf("DISPATCH_TOKEN_CORPUS: %v", err)
	}
	markQuote := forkReads{read: 2}
	reused := forkReads{renderingReused: 1}
	want := map[string]forkReads{
		"comment/MarkQuote":          markQuote,
		"comment/SnapshotVersion":    reused,
		"comment/ProjectMark":        reused,
		"suggestion/MarkQuote":       markQuote,
		"suggestion/SnapshotVersion": reused,
		"suggestion/SuggestionKind":  reused,
		"suggestion/ProjectMark":     reused,
		"edit/SnapshotVersion":       reused,
	}
	for _, name := range names {
		source, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(name, func(t *testing.T) {
			service := newWriteCorpusService(t)
			checks := checkRenderings(service)
			// Each step's reads are the counts' move since the call before it, which for a
			// transaction's first operation is its begin.
			got := map[string]forkReads{}
			last := readForkReads(service)
			observe := func(step string) {
				now := readForkReads(service)
				if !strings.HasSuffix(step, "/begin") {
					got[step] = now.minus(last)
				}
				last = now
			}
			runWriteCorpus(t, service, name, string(source), observe)
			checks.requireNoRenderingFailures(t)
			for _, mode := range writeCorpusModes {
				for _, step := range slices.Sorted(maps.Keys(want)) {
					reads := want[step]
					observed, ok := got[mode+"/"+step]
					if !ok {
						t.Errorf("%s/%s did not run", mode, step)
						continue
					}
					requireForkReads(t, mode+"/"+step, observed, reads)
				}
			}
		})
	}
}
