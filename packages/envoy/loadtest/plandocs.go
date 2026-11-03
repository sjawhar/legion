package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// docVersions is one document's planned version writes in a step's window: the edits and the
// uploads of the step's event schedule that land on it. Those two kinds write a document version
// each (events.go); the other three write comments.
type docVersions struct {
	Artifact string `json:"artifact"`
	Edits    int    `json:"edits"`
	Uploads  int    `json:"uploads"`
	// Versions is Edits + Uploads, the versions the step plans for the document.
	Versions int `json:"versions"`
}

// planVersionDocs is every document the step's in-window edits and uploads write to, most
// versions first, ties by artifact id. It walks the very schedule loadgen run sends (planWrites
// over openLoopPlan's stream 1, each write drawn by planEvent), so the counts are the run's own
// plan: a pure function of the seed file, -sessions, -events, -warmup, -duration, -event-mix and
// -rng-seed. It plans, so it counts no write that the run then loses to an error.
func planVersionDocs(cfg runConfig, sf seedFile) []docVersions {
	st := newStep(cfg, sf, log.New(io.Discard, "", 0), 0)
	// Any start gives the same plan: every arrival is start plus a sum of drawn nanoseconds, and
	// the window is start plus the warm-up, so the in-window arrivals are the same ones.
	st.openWindow(time.Time{})
	byDoc := map[string]*docVersions{}
	st.planWrites(func(p eventPlan, in bool) {
		if !in || (p.kind != kindEdit && p.kind != kindUpload) {
			return
		}
		id := sf.Issues[p.issue].ArtifactID
		d := byDoc[id]
		if d == nil {
			d = &docVersions{Artifact: id}
			byDoc[id] = d
		}
		if p.kind == kindEdit {
			d.Edits++
		} else {
			d.Uploads++
		}
		d.Versions++
	})
	out := make([]docVersions, 0, len(byDoc))
	for _, d := range byDoc {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b docVersions) int {
		if a.Versions != b.Versions {
			return b.Versions - a.Versions
		}
		return strings.Compare(a.Artifact, b.Artifact)
	})
	return out
}

// runPlanDocs prints the documents a step's in-window edits and uploads write the most versions
// to, without a stack: the choice cluster.sh's browser typing needs before the step starts.
func runPlanDocs(args []string) error {
	fs := flag.NewFlagSet("plan-docs", flag.ExitOnError)
	seedPath := fs.String("seed", "", "seed.json path (default $LOADTEST_ROOT/seed.json)")
	events := fs.Float64("events", -1, "the step's events per second (required)")
	sessions := fs.Int("sessions", -1, "the step's sessions (required)")
	warmup := fs.Duration("warmup", 30*time.Second, "the step's warm-up before the measured window")
	duration := fs.Duration("duration", 180*time.Second, "the step's measured window")
	mixFlag := fs.String("event-mix", defaultEventMix, "the step's event mix: a preset ("+eventMixPresetNames()+") or kind=weight,...")
	rngSeed := fs.Uint64("rng-seed", 1, "the step's seed of the event schedule")
	top := fs.Int("top", 20, "documents to print, the most written first")
	asJSON := fs.Bool("json", false, "print a JSON array: each document's artifact id and its planned edits, uploads and versions")
	_ = fs.Parse(args)

	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	mix, err := parseEventMix(*mixFlag)
	if err != nil {
		return err
	}
	switch {
	case *events < 0 || !finite(*events):
		return fmt.Errorf("-events is required and must be 0 or more")
	case *sessions < 1:
		return fmt.Errorf("-sessions is required and must be at least 1")
	case *warmup < 0 || *duration <= 0:
		return fmt.Errorf("-warmup must be 0 or more, -duration more than 0")
	case *top < 1:
		return fmt.Errorf("-top must be at least 1")
	}
	if *seedPath == "" {
		*seedPath = filepath.Join(loadtestRoot(), "seed.json")
	}
	sf, err := readSeed(*seedPath)
	if err != nil {
		return err
	}
	if err := checkSeedMix(sf, mix, *seedPath); err != nil {
		return err
	}
	docs := planVersionDocs(runConfig{
		sessions: *sessions, events: *events, warmup: *warmup, duration: *duration,
		mix: mix, rngSeed: *rngSeed,
	}, sf)
	if len(docs) > *top {
		docs = docs[:*top]
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(docs)
	}
	for _, d := range docs {
		fmt.Println(d.Artifact)
	}
	return nil
}
