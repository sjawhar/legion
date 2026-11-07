package admit

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The admission cap is read at every decision that needs one (Admission.capacity), not copied
// when admission was built: a cap raised through the settings route — the same Apply an operator
// call makes — is the cap the next admission decision promotes under, with nothing rebuilt. The
// change says as much to the caller that asked for it: a decision-class path, taking effect at
// once, naming no holder.
func TestTheAdmissionCapChangedThroughTheSettingsHolderIsUsedByTheNextDecision(t *testing.T) {
	pool := migratedPool(t)
	live := liveCap(t, 1)
	admission := newAdmissionReading(t, func() int { return live.Snapshot().AdmissionCap }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedWaiting(t, pool, "LEGION-A", "A")
	seedWaiting(t, pool, "LEGION-B", "B")
	seedWaiting(t, pool, "LEGION-C", "C")
	summaries := []dispatch.IssueSummary{
		{Key: "LEGION-A", ResolvedRepo: "acme/widgets", Title: "A", Status: "todo", Rank: "A", HandedOver: handed},
		{Key: "LEGION-B", ResolvedRepo: "acme/widgets", Title: "B", Status: "todo", Rank: "B", HandedOver: handed},
		{Key: "LEGION-C", ResolvedRepo: "acme/widgets", Title: "C", Status: "todo", Rank: "C", HandedOver: handed},
	}

	reconcile(t, pool, admission, summaries)
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow}})

	applied, refused, err := live.Apply(config.SourceRoute, "someone", map[string]any{"admission_cap": 3})
	if err != nil || len(refused) != 0 {
		t.Fatalf("apply admission_cap=3: %v, refused %+v", err, refused)
	}
	if len(applied) != 1 || applied[0].Path != "admission_cap" || applied[0].New != "3" {
		t.Fatalf("the apply reported %+v, want one admission_cap change to 3", applied)
	}
	if applied[0].Class != config.ClassDecision || applied[0].Outcome != config.OutcomeAtOnce || applied[0].Holder != "" {
		t.Fatalf("the change is %+v, want a decision-class change taking effect at once with no holder", applied[0])
	}

	reconcile(t, pool, admission, summaries)
	assertSlots(t, pool, []record.Slot{
		{Issue: "LEGION-A", Index: 0, AdmittedAt: fixedNow},
		{Issue: "LEGION-B", Index: 1, AdmittedAt: fixedNow},
		{Issue: "LEGION-C", Index: 2, AdmittedAt: fixedNow},
	})
}

// liveCap is a settings holder over a real legion.yaml whose admission_cap is cap: a route change
// is applied by reading that file again under the overlay, so the test needs the file and not a
// Config it built in memory.
func liveCap(t *testing.T, cap int) *config.Live {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "legion.yaml")
	body := fmt.Sprintf("project: %s\nstate_dir: %s\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nadmission_cap: %d\n",
		testProject, dir, cap)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return live
}
