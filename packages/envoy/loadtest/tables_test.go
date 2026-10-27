package main

import (
	"strings"
	"testing"
	"time"
)

func TestTablesMarkAStepWhoseStackProcessExited(t *testing.T) {
	busy := 0.42
	ok := stepData{name: "step-1x", sum: summary{Config: summaryConfig{Step: "step-1x"}, Resources: &resourcesSummary{}},
		meta: &metaFile{BoxBusyBefore: &busy}}
	killed := stepData{name: "killed", sum: summary{
		Config:     summaryConfig{Step: "killed"},
		StackExits: []stackExit{{Name: "dispatch", PID: 4242, FirstSeenGone: time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)}},
	}}
	md := renderTables([]stepData{ok, killed})
	if !strings.Contains(md, "| killed (dispatch exited) |") {
		t.Fatalf("the killed step's row is not marked:\n%s", md)
	}
	if strings.Contains(md, "step-1x (") {
		t.Fatalf("a step with no stack exit is marked:\n%s", md)
	}
	if !strings.Contains(md, "\n(<process> exited): ") {
		t.Fatalf("no note defines the mark:\n%s", md)
	}
	if !strings.Contains(md, "| box busy | box busy before |") || !strings.Contains(md, "| 0% | 42% |") {
		t.Fatalf("the Resources table lacks box busy before 42%%:\n%s", md)
	}
}
