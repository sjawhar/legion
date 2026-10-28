package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// readCgroupFile reads the step directory's cgroup.json (loadgen cgroup-merge); nil when the
// directory holds none.
func readCgroupFile(dir string) (*cgroupFile, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("step %s: %w", dir, err)
	}
	var cf cgroupFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("step %s: cgroup.json: %w", dir, err)
	}
	return &cf, nil
}

// readFailedKinds is the kinds of the reasons in the step directory's step-failed.json, each once
// in file order; failed is false when the directory holds no step-failed.json. A file that is not
// readable still fails the step, with the kind "unreadable step-failed.json".
func readFailedKinds(dir string) (kinds []string, failed bool) {
	b, err := os.ReadFile(filepath.Join(dir, "step-failed.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	var doc struct {
		Reasons []stepFailedReason `json:"reasons"`
	}
	if err != nil || json.Unmarshal(b, &doc) != nil {
		return []string{"unreadable step-failed.json"}, true
	}
	for _, r := range doc.Reasons {
		if r.Kind != "" && !slices.Contains(kinds, r.Kind) {
			kinds = append(kinds, r.Kind)
		}
	}
	return kinds, true
}

// failedMark is the mark of a step whose directory holds step-failed.json: "(failed: <kinds>)".
func failedMark(kinds []string) string {
	if len(kinds) == 0 {
		return "(failed)"
	}
	return "(failed: " + strings.Join(kinds, ", ") + ")"
}

// failedNote defines failedMark under a table, when a row has it.
func failedNote(md *markdown, marked bool) {
	if marked {
		md.line("\n(failed: <kinds>): the step directory holds step-failed.json, whose `reasons[].kind` say why the step failed (`restart`: a container of the copy restarted during the step; `sampler_gap`: loadgen cgroup-merge found a role over 30 s without a good cgroup sample in the window), so its row, in every table here, does not measure the load.")
	}
}

// The marks of a Resources row whose cells come from cgroup.json, as their notes name them, and
// the working-set column's header.
const (
	cgroupFailedMark     = "(cgroup failed)"
	cgroupUnmeasuredMark = "(no cgroup cores: <roles>)"
	cgroupWorkingSetH    = "Dispatch working set max MiB"
)

// cgroupResources are the Resources cells that a step's cgroup.json gives: Dispatch cores mean /
// peak 5 s, Postgres, listener, NATS (every nats-* role) and loadgen (role runner) cores, and the
// Dispatch working set max; dispatchCPUS is Dispatch's CPU-seconds over the window. failed is
// cgroup.json's failed, which leaves every cell "-"; unmeasured are the roles whose cores_mean is
// null.
type cgroupResources struct {
	dispatch, postgres, listener, nats, loadgen, workingSet string
	dispatchCPUS                                            *float64
	failed                                                  bool
	unmeasured                                              []string
}

// resources is the Resources cells of cf; a role cgroup.json does not hold shows "-".
func (cf *cgroupFile) resources() cgroupResources {
	c := cgroupResources{dispatch: "-", postgres: "-", listener: "-", nats: "-", loadgen: "-", workingSet: "-"}
	if cf.Failed {
		c.failed = true
		return c
	}
	// cores is a role's cores_mean cell; a null one is "-" and the role is unmeasured.
	cores := func(name string, d int) string {
		r, ok := cf.Roles[name]
		if !ok {
			return "-"
		}
		if r.CoresMean == nil {
			c.unmeasured = append(c.unmeasured, name)
			return "-"
		}
		return num(*r.CoresMean, d)
	}
	if d := cores("dispatch", 2); d != "-" {
		peak := "-"
		if p := cf.Roles["dispatch"].CoresPeak5s; p != nil {
			peak = num(*p, 2)
		}
		c.dispatch = d + " / " + peak
	}
	c.postgres, c.listener, c.loadgen = cores("postgres", 2), cores("listener", 3), cores("runner", 2)
	var natsRoles []string
	for name := range cf.Roles {
		if strings.HasPrefix(name, "nats-") {
			natsRoles = append(natsRoles, name)
		}
	}
	slices.Sort(natsRoles)
	sum, measured := 0.0, len(natsRoles) > 0
	for _, name := range natsRoles {
		if r := cf.Roles[name]; r.CoresMean != nil {
			sum += *r.CoresMean
			continue
		}
		c.unmeasured = append(c.unmeasured, name)
		measured = false
	}
	if measured {
		c.nats = num(sum, 3)
	}
	if d, ok := cf.Roles["dispatch"]; ok {
		if d.WorkingSetBytesMax != nil {
			c.workingSet = num(float64(*d.WorkingSetBytesMax)/(1<<20), 0)
		}
		c.dispatchCPUS = d.CPUS
	}
	return c
}

// mark is the marks of the row that carries c's cells.
func (c cgroupResources) mark() string {
	var m []string
	if c.failed {
		m = append(m, cgroupFailedMark)
	}
	if len(c.unmeasured) > 0 {
		m = append(m, "(no cgroup cores: "+strings.Join(c.unmeasured, ", ")+")")
	}
	return strings.Join(m, " ")
}

// cgroupNotes says, under the Resources table, where its cgroup cells come from when a step holds
// cgroup.json, and defines the marks its rows carry.
func cgroupNotes(md *markdown, present, failed, unmeasured bool) {
	if present {
		md.line("\nA step whose directory holds cgroup.json (loadgen cgroup-merge of the cluster sampler's cgroup v2 reads) takes its Dispatch, Postgres, listener, NATS (the sum of the nats-* pods) and loadgen (the runner pod) cores, its %s (memory.current less inactive_file) and, when summary.json has none, its Dispatch CPU-s per operation from cgroup.json.", cgroupWorkingSetH)
	}
	if failed {
		md.line("\n%s: cgroup.json marks the step failed (`failed`, `failures`: a role went over 30 s without a good sample in the window, or had none), so every cell taken from cgroup.json shows `-`, not a measured number.", cgroupFailedMark)
	}
	if unmeasured {
		md.line("\n%s: cgroup.json has no good sample of those roles at or before window_start, or at or after window_end (`cores_mean` null), so their cores cells show `-`, not 0.", cgroupUnmeasuredMark)
	}
}
