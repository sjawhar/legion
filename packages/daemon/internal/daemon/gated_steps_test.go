package daemon

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A boot publishes the design-gate steps it is configured with onto the project's Dispatch row,
// through the outbox: the boot queues one row addressed to the project key and the lane writes
// it. The daemon keeps no record of what it last published, so a boot that finds its own row
// still unpublished queues no second; and because that row can have been queued by a boot on a
// list the operator has changed since, the boot writes its own list into it. A boot after the
// row ran publishes again — which is what lets a deployment that changed its list have the new
// one written without anyone asking.
func TestGatedStepsPublishAtBoot(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	boot := &workflowRuntime{pool: pool, records: records, dispatchProject: "LEGION",
		gatedSteps: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}}

	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the first boot's publish: %v", err)
	}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("a second boot's publish: %v", err)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows = %d, want the one publish the second boot found pending", rows)
	}

	client := &outboxDispatch{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client,
		now: func() time.Time { return time.Now().UTC() }, log: quietLogger()}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the publish: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v, the configured list in the configured order", client.gatedSteps, want)
	}
	if want := []string{"LEGION"}; !reflect.DeepEqual(client.gatedStepsOf, want) {
		t.Fatalf("the publish went to %v, want %v, the one project this daemon serves", client.gatedStepsOf, want)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("outbox rows = %d after the publish ran, want none", rows)
	}

	// An operator changes the list while the row of the boot before is still waiting for the
	// lane. The row waits for as long as Dispatch is down or has yet to take the route, so this
	// boot writes its own list into it rather than leaving the stale one to be published.
	boot.gatedSteps = []record.DesignStep{record.DesignStepSpec}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the boot whose publish waits: %v", err)
	}
	boot.gatedSteps = []record.DesignStep{record.DesignStepCriteria}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the boot on the changed list: %v", err)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows = %d, want the one publish carrying the list of the boot that ran last", rows)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the publish of the changed list: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}, {"criteria"}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v: the boot on a changed list publishes its own", client.gatedSteps, want)
	}

	// The next boot gates nothing, and publishes that rather than leaving the stale list standing.
	boot.gatedSteps = []record.DesignStep{}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the boot that gates nothing: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the third publish: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}, {"criteria"}, {}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v: a deployment that gates nothing publishes the empty list", client.gatedSteps, want)
	}
}

// The publish executor refuses a runner with no Dispatch client rather than dropping the row, as
// every other Dispatch write of the outbox does: the row is retried once a client is there.
func TestGatedStepsPublishWithoutADispatchClientIsRefused(t *testing.T) {
	row := mustOutboxRow(t, "LEGION", record.GatedStepsPublish{Project: "LEGION", Steps: []record.DesignStep{record.DesignStepSpec}}, time.Now())
	err := (&outbox{}).execute(context.Background(), row)
	if err == nil || err.Error() != "gated steps executor has no Dispatch client" {
		t.Fatalf("execute without a Dispatch client = %v, want the refusal naming the missing client", err)
	}
}
