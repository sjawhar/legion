package main

import (
	"os"
	"strings"
	"testing"
)

// TestPlanHistoryBundle runs planHistory over every document of the bundle LOADTEST_REPLAY_BUNDLE
// names (skipped without it) and fails on any version it cannot reach. It logs keys and numbers
// only, never a text.
func TestPlanHistoryBundle(t *testing.T) {
	dir := os.Getenv("LOADTEST_REPLAY_BUNDLE")
	if dir == "" {
		t.Skip("LOADTEST_REPLAY_BUNDLE is not set")
	}
	b, err := readBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	docs, versions, flipped, unreachable, missing, runs := 0, 0, 0, 0, 0, 0
	for i := range b.State.Issues {
		is := &b.State.Issues[i]
		for j := range is.Artifacts {
			a := &is.Artifacts[j]
			if a.Kind != "doc" || a.TextPath == "" {
				continue
			}
			raw, err := b.readFile(a.TextPath)
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			p, err := docPlan(is, a, text)
			if err != nil {
				t.Errorf("%s %s: %v", is.Key, a.ID, err)
				continue
			}
			docs++
			versions += a.Version
			flipped += p.Flipped
			missing += p.Missing
			if p.RunEnd > p.RunStart {
				runs++
			}
			if p.Unreachable > 0 || p.Flipped > 0 {
				unreachable += p.Unreachable
				smallest := len(text)
				for _, s := range p.Sizes[:max(0, p.Restore-1)] {
					smallest = min(smallest, s)
				}
				askBytes := 0
				for _, line := range strings.Split(text, "\n") {
					if strings.HasPrefix(line, ":::ask") {
						askBytes += len(line)
					}
				}
				t.Logf("short %s doc %s: %d unreachable, %d sign flipped, of %d versions; text %d bytes, removed run %d bytes, base %d bytes, smallest planned %d, ask directive lines %d bytes, ask step versions %d, restore at %d",
					is.Key, a.ID[:8], p.Unreachable, p.Flipped, a.Version, len(text), p.RunEnd-p.RunStart, len(baseText(text, p.RunStart, p.RunEnd)), smallest, askBytes, len(p.AskAt), p.Restore)
			}
		}
	}
	t.Logf("documents %d, versions %d, with a removed run %d, sign flipped %d, unreachable %d, sizes missing %d", docs, versions, runs, flipped, unreachable, missing)
	if unreachable > 0 {
		t.Errorf("%d versions unreachable", unreachable)
	}
}
