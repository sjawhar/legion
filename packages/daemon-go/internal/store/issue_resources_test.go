package store

import (
	"context"
	"strings"
	"testing"
)

func TestIssueResourcesFenceCleanupAndReadmission(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	first := IssueResources{Project: "legion", Issue: "LEGION-208", Tree: "LEGION-208", Sandbox: "legion-legion-legion-208", Generation: 1}
	if err := store.EnsureIssueResources(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.IssueResources(ctx, first.Project, first.Issue)
	if err != nil || !found || got.Project != first.Project || got.Issue != first.Issue || got.Generation != 1 || got.CleanupStarted {
		t.Fatalf("resources = %+v, found %t, err %v", got, found, err)
	}
	if resources, began, err := store.BeginIssueCleanup(ctx, first.Project, first.Issue, 2); err != nil || began || resources.Issue != "" {
		t.Fatalf("wrong-generation cleanup = %+v, %t, %v", resources, began, err)
	}
	if resources, began, err := store.BeginIssueCleanup(ctx, first.Project, first.Issue, 1); err != nil || !began || !resources.CleanupStarted || resources.CleanupGeneration != 1 {
		t.Fatalf("first cleanup = %+v, %t, %v", resources, began, err)
	}
	if _, began, err := store.BeginIssueCleanup(ctx, first.Project, first.Issue, 1); err != nil || began {
		t.Fatalf("second cleanup = began %t, err %v", began, err)
	}
	if err := store.ConfirmIssueCleanup(ctx, first.Project, first.Issue, 1); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Generation = 2
	if err := store.EnsureIssueResources(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, found, err = store.IssueResources(ctx, first.Project, first.Issue)
	if err != nil || !found || got.Generation != 2 || got.CleanupStarted || !got.CleanupConfirmedAt.IsZero() {
		t.Fatalf("readmitted resources = %+v, found %t, err %v", got, found, err)
	}
}

func TestIssuePodLayoutRefusesLegacyClaimsAndOtherLayouts(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	if err := store.EnsureIssuePodLayout(ctx, "legion", true); err == nil || !strings.Contains(err.Error(), "legacy per-claim") {
		t.Fatalf("legacy layout = %v", err)
	}
	if err := store.EnsureIssuePodLayout(ctx, "legion", false); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssuePodLayout(ctx, "legion", false); err != nil {
		t.Fatalf("same layout: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update runtime_layouts set layout = 'legacy-v0' where project = 'legion'`); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIssuePodLayout(ctx, "legion", false); err == nil || !strings.Contains(err.Error(), "legacy-v0") {
		t.Fatalf("wrong layout = %v", err)
	}
}
