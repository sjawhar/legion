package legionagents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/legionagents"
)

// A project's answering record is stored under project.<legion_project> and read back whole, a
// rewrite replaces it, a project with none reads as not found, and the agent listings never list it.
func TestProjectRecordRoundTrips(t *testing.T) {
	r := openRegistry(t)
	ctx := context.Background()
	if _, found, err := r.store.Project(ctx, "infra"); err != nil || found {
		t.Fatalf("a project with no record = found %t, %v; want not found", found, err)
	}
	first := legionagents.Project{Answering: true, UpdatedAt: "2026-10-26T04:00:00.000Z"}
	if stored, err := r.store.PutProject(ctx, "infra", first); err != nil || stored != first {
		t.Fatalf("PutProject = %#v, %v; want %#v", stored, err, first)
	}
	next := legionagents.Project{Answering: false, UpdatedAt: "2026-10-26T04:01:00Z"}
	if _, err := r.store.PutProject(ctx, "infra", next); err != nil {
		t.Fatalf("rewrite the record: %v", err)
	}
	got, found, err := r.store.Project(ctx, "infra")
	if err != nil || !found || got != next {
		t.Fatalf("Project = %#v, %t, %v; want %#v", got, found, err, next)
	}
	if held := r.contents(t); held["project.infra"] != `{"answering":false,"updated_at":"2026-10-26T04:01:00Z"}` || len(held) != 1 {
		t.Fatalf("the bucket holds %v, want the one project key", held)
	}
}

// A name that is not a Legion project token, or an updated_at that is not an RFC 3339 UTC time, is
// refused with ErrInvalid and writes nothing.
func TestProjectRecordRefusesWhatTheRegistryDoesNotTake(t *testing.T) {
	r := openRegistry(t)
	ctx := context.Background()
	valid := legionagents.Project{Answering: true, UpdatedAt: "2026-10-26T04:00:00Z"}
	for _, tc := range []struct {
		name    string
		project string
		record  legionagents.Project
	}{
		{"upper-case project", "Infra", valid},
		{"dotted project", "in.fra", valid},
		{"empty project", "", valid},
		{"project past 64 bytes", "a123456789012345678901234567890123456789012345678901234567890123x", valid},
		{"local time", "infra", legionagents.Project{Answering: true, UpdatedAt: "2026-10-26T04:00:00+01:00"}},
		{"no time", "infra", legionagents.Project{Answering: true}},
		{"not a time", "infra", legionagents.Project{Answering: true, UpdatedAt: "yesterday"}},
	} {
		if _, err := r.store.PutProject(ctx, tc.project, tc.record); !errors.Is(err, legionagents.ErrInvalid) {
			t.Errorf("%s: PutProject = %v, want ErrInvalid", tc.name, err)
		}
	}
	if _, _, err := r.store.Project(ctx, "In.fra"); !errors.Is(err, legionagents.ErrInvalid) {
		t.Errorf("Project of an invalid token = %v, want ErrInvalid", err)
	}
	if held := r.contents(t); len(held) != 0 {
		t.Fatalf("the refusals wrote %v", held)
	}
}
