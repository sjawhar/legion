package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A gate's registration and each artifact event that opens or closes it write one Info line
// naming the tree, the issue, the document and the version, so the journal says what the design
// gate did without a read of the daemon's state.
func TestTheDesignGateLogsItsRegistrationAndEachOpenOrClose(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", Rank: "U"})
	var logged bytes.Buffer
	engine := New(record.NewStore(), Config{
		Project: "LEGION", DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, MaxFixAttempts: 3, Linger: time.Hour,
		Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
	}, slog.New(slog.NewTextHandler(&logged, nil)))
	apply := func(id string, fact intake.Fact) []string {
		t.Helper()
		logged.Reset()
		if _, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{}); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		lines := []string{}
		for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
			if strings.Contains(line, "design gate") {
				lines = append(lines, line[strings.Index(line, "msg="):])
			}
		}
		return lines
	}
	for _, step := range []struct {
		name string
		fact intake.Fact
		want []string
	}{
		{"registration", intake.GateRegistered{Issue: "LEGION-1", ArtifactID: "art-1", Version: 2},
			[]string{`msg="workflow: design gate registered" tree=LEGION-1 issue=LEGION-1 artifact=art-1 version=2 open=false policy=root-issues`}},
		{"changes requested on the closed gate", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactChangesRequested, Version: 2, Reason: "tighten it"},
			[]string{`msg="workflow: design gate closed" tree=LEGION-1 issue=LEGION-1 artifact=art-1 event=changes_requested version=2`}},
		{"a new version", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactVersion, Version: 3},
			nil},
		{"the approval", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactApproved, Version: 3},
			[]string{`msg="workflow: design gate opened" tree=LEGION-1 issue=LEGION-1 artifact=art-1 event=approved version=3`}},
		{"a version after the approval", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactVersion, Version: 4},
			[]string{`msg="workflow: design gate closed" tree=LEGION-1 issue=LEGION-1 artifact=art-1 event=version version=4`}},
	} {
		if got := apply(step.name, step.fact); strings.Join(got, "\n") != strings.Join(step.want, "\n") {
			t.Fatalf("%s logged %q; want %q", step.name, got, step.want)
		}
	}
}
