package workflow

import (
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

var allWorkflowPhases = []phase.Phase{
	phase.Admitted, phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing,
	phase.Retro, phase.Merging, phase.AwaitingMerge, phase.ProductionCheck, phase.Done, phase.Held,
}

var allWorkflowTriggers = []TriggerKind{
	TriggerGateOpened, TriggerPlannerCompleted, TriggerImplementationReady, TriggerTesterPassed,
	TriggerTesterFailed, TriggerReviewApproved, TriggerReviewRejected, TriggerRetroCompleted,
	TriggerReady, TriggerPullRequestMerged, TriggerSignOff, TriggerBackward,
}

func TestEveryPhaseAndTriggerHasARowOrNamedIgnore(t *testing.T) {
	if err := validateTable(Table); err != nil {
		t.Fatal(err)
	}
}

func TestDeletingAnyTransitionRowBreaksCoverage(t *testing.T) {
	for index := range Table {
		rows := append([]Row{}, Table[:index]...)
		rows = append(rows, Table[index+1:]...)
		if err := validateTable(rows); err == nil {
			t.Fatalf("deleting %s/%s -> %s left the table covered", Table[index].From, Table[index].Trigger, Table[index].To)
		}
	}
}

func validateTable(rows []Row) error {
	for _, current := range allWorkflowPhases {
		for _, trigger := range allWorkflowTriggers {
			hasRow := false
			for _, row := range rows {
				if row.From != current || row.Trigger != trigger {
					continue
				}
				hasRow = true
				if row.To == "" || row.Guard == nil || len(row.Effects) == 0 {
					return fmt.Errorf("%s/%s is not a complete transition row", current, trigger)
				}
			}
			if workflowTransitionRequired(current, trigger) {
				if !hasRow {
					return fmt.Errorf("%s/%s is missing a transition row", current, trigger)
				}
				continue
			}
			if hasRow {
				return fmt.Errorf("%s/%s has a row where %q says it is ignored", current, trigger, namedIgnore(current, trigger))
			}
			if namedIgnore(current, trigger) == "" {
				return fmt.Errorf("%s/%s has neither a row nor a named ignore", current, trigger)
			}
		}
	}
	for _, from := range allWorkflowPhases {
		for _, to := range requiredBackwardTargets(from) {
			if !hasBackwardRow(rows, from, to) {
				return fmt.Errorf("%s/backward -> %s is missing a transition row", from, to)
			}
		}
	}
	for _, row := range rows {
		if !workflowTransitionRequired(row.From, row.Trigger) {
			return fmt.Errorf("%s/%s is not a declared transition", row.From, row.Trigger)
		}
		if row.Trigger == TriggerBackward && !containsPhase(requiredBackwardTargets(row.From), row.To) {
			return fmt.Errorf("%s/backward -> %s is not a declared backward edge", row.From, row.To)
		}
	}
	return nil
}

func hasBackwardRow(rows []Row, from, to phase.Phase) bool {
	for _, row := range rows {
		if row.From == from && row.Trigger == TriggerBackward && row.To == to {
			return true
		}
	}
	return false
}

func workflowTransitionRequired(from phase.Phase, trigger TriggerKind) bool {
	switch trigger {
	case TriggerGateOpened:
		return from == phase.Admitted
	case TriggerPlannerCompleted:
		return from == phase.Planning
	case TriggerImplementationReady:
		return from == phase.Implementing
	case TriggerTesterPassed, TriggerTesterFailed:
		return from == phase.Testing
	case TriggerReviewApproved, TriggerReviewRejected:
		return from == phase.Reviewing
	case TriggerRetroCompleted:
		return from == phase.Retro
	case TriggerReady:
		return from == phase.Merging
	case TriggerPullRequestMerged:
		return from == phase.AwaitingMerge
	case TriggerSignOff:
		return from == phase.ProductionCheck
	case TriggerBackward:
		return len(requiredBackwardTargets(from)) != 0
	default:
		return false
	}
}

func requiredBackwardTargets(from phase.Phase) []phase.Phase {
	ordered := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro, phase.Merging, phase.ProductionCheck}
	for index, candidate := range ordered {
		if candidate == from {
			return ordered[:index]
		}
	}
	return nil
}

func containsPhase(phases []phase.Phase, wanted phase.Phase) bool {
	for _, candidate := range phases {
		if candidate == wanted {
			return true
		}
	}
	return false
}

func namedIgnore(current phase.Phase, trigger TriggerKind) string {
	return fmt.Sprintf("%s does not advance %s", trigger, current)
}
