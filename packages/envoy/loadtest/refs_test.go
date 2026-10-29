package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRefsSeedFile writes a seed.json with the keys the seed makes: Dispatch numbers each
// project's issues from 1, so LGA holds LGA-1 to LGA-400 and LGB, LGC and LGD hold 300, 200 and
// 100 of their own.
func writeRefsSeedFile(t *testing.T) seedFile {
	t.Helper()
	var sf seedFile
	for _, p := range seedProjects {
		sf.Projects = append(sf.Projects, p.key)
		for n := 1; n <= p.issues; n++ {
			sf.Issues = append(sf.Issues, seedIssue{Key: fmt.Sprintf("%s-%d", p.key, n), Project: p.key})
		}
	}
	path := filepath.Join(t.TempDir(), "seed.json")
	b, err := json.Marshal(sf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := readSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// TestMakeRefPlan pins the committed sample and the order of the plan's draws: the plan the seed
// writes must not change under anyone's edit, or two seeds would hold different references.
func TestMakeRefPlan(t *testing.T) {
	sample, err := refsSample()
	if err != nil {
		t.Fatal(err)
	}
	if len(sample) != 160 {
		t.Fatalf("the sample holds %d issues, want 160", len(sample))
	}
	p, err := makeRefPlan(writeRefsSeedFile(t), sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.targets) != refTargetCount {
		t.Errorf("plan holds %d targets, want %d", len(p.targets), refTargetCount)
	}
	versions := 0
	for i, target := range p.targets {
		versions += target.versions
		if target.n != i {
			t.Fatalf("target %d is numbered %d: a source's citation names the target at its index", i, target.n)
		}
		project, _, _ := strings.Cut(target.issue, "-")
		switch project {
		case "LGB", "LGC", "LGD":
		default:
			t.Fatalf("target %d lives on %s; a target lives on an issue of LGB, LGC or LGD, where no step writes a %s document", target.n, target.issue, refTargetPrefix+"*")
		}
		if len(target.writers) < 1 || len(target.writers) > 3 {
			t.Fatalf("target %d has %d writers, want 1 to 3", target.n, len(target.writers))
		}
		if len(target.order) != target.versions {
			t.Fatalf("target %d has %d versions and %d writers of them", target.n, target.versions, len(target.order))
		}
		if target.versions > 0 && target.order[0] != target.writers[0] {
			t.Fatalf("target %d is created by writer %d, not its first writer %d", target.n, target.order[0], target.writers[0])
		}
		for _, w := range append(append([]int(nil), target.writers...), target.order...) {
			if w < 0 || w >= refWriterPool {
				t.Fatalf("target %d names writer %d, outside the pool of %d", target.n, w, refWriterPool)
			}
		}
	}
	if versions != 19471 {
		t.Errorf("the plan writes %d target versions, want 19,471", versions)
	}
	cited := 0
	for _, key := range p.lga {
		if len(p.sources[key]) > 0 {
			cited++
		}
	}
	if cited != 310 {
		t.Errorf("%d LGA issues cite a target, want 310", cited)
	}
	if len(p.lga) != 400 || len(p.sources) != 400 {
		t.Errorf("the plan holds %d LGA issues and %d source entries, want 400 of each", len(p.lga), len(p.sources))
	}
	if digest := refPlanDigest(p); digest != refPlanSHA256 {
		t.Errorf("the plan hashes to %s, want %s: a draw changed, so this seed's references differ from the ones measured", digest, refPlanSHA256)
	}
}

// refPlanSHA256 is refPlanDigest of the plan the committed sample and seed draw. It pins the whole
// plan, not its totals: a change of which targets an issue cites, or of which writer writes a
// version, moves it while every total stays.
const refPlanSHA256 = "7414809c6e1617d6a0e25749a60ae834966a410ca911d691ff4747b4a756279b"

// refPlanDigest is a sha256 over the whole plan in a fixed serialization: every target's number,
// issue, version count, writers and writer of each version, in target order; then every LGA key,
// in the plan's order, with the targets it cites, in the plan's order.
func refPlanDigest(p refPlan) string {
	h := sha256.New()
	for _, t := range p.targets {
		fmt.Fprintf(h, "target %d %s %d %v %v\n", t.n, t.issue, t.versions, t.writers, t.order)
	}
	for _, key := range p.lga {
		fmt.Fprintf(h, "source %s %v\n", key, p.sources[key])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestRefWriterAuthorBytes pins the padding: a version row's author list must encode to the bytes
// production's does, or the answers are smaller than production's.
func TestRefWriterAuthorBytes(t *testing.T) {
	for w := range refWriterPool {
		b, err := json.Marshal([]refActor{refWriter(w)})
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != refAuthorBytes {
			t.Fatalf("the author list of writer %d encodes to %d bytes, want %d", w, len(b), refAuthorBytes)
		}
	}
}
