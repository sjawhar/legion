package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A gate's registration and each artifact event that opens or closes it write one Info line
// naming the issue, the document and the version, so the journal says what the design gate did
// without a read of the daemon's state. A changes request on a gate already closed changes nothing
// and has a line of its own. Each line is written once the fact's transaction commits, so a fact
// retried after a failed commit writes it once.
func TestTheDesignGateLogsItsRegistrationAndEachOpenOrClose(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", Rank: "U"})
	var logged bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil)))
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
			[]string{`msg="workflow: design gate registered" issue=LEGION-1 artifact=art-1 version=2 open=false policy=root-issues`}},
		{"changes requested on the closed gate", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactChangesRequested, Version: 2, Reason: "tighten it"},
			[]string{`msg="workflow: design gate changes requested" issue=LEGION-1 artifact=art-1 version=2`}},
		{"a new version", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactVersion, Version: 3},
			nil},
		{"the approval", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactApproved, Version: 3},
			[]string{`msg="workflow: design gate opened" issue=LEGION-1 artifact=art-1 event=approved version=3`}},
		{"a version after the approval", intake.DispatchArtifact{Key: "LEGION-1", ArtifactID: "art-1", Kind: intake.DispatchArtifactVersion, Version: 4},
			[]string{`msg="workflow: design gate closed" issue=LEGION-1 artifact=art-1 event=version version=4`}},
	} {
		if got := apply(step.name, step.fact); strings.Join(got, "\n") != strings.Join(step.want, "\n") {
			t.Fatalf("%s logged %q; want %q", step.name, got, step.want)
		}
	}
}

// A gate fact whose transaction does not commit writes no line: its retry writes it.
func TestAGateFactThatDoesNotCommitLogsNothing(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", Rank: "U"})
	var logged bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logged, nil)))
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := engine.Apply(context.Background(), tx, intake.GateRegistered{Issue: "LEGION-1", ArtifactID: "art-1", Version: 2}); err != nil {
		t.Fatalf("apply the registration: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if strings.Contains(logged.String(), "design gate") {
		t.Fatalf("logged %q for a registration that never committed", logged.String())
	}
}
