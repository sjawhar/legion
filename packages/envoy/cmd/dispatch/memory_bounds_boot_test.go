package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A real Dispatch process derives the document service's memory bounds from the Go memory limit
// it holds and names each of them in a line of its boot log, so what an operator reads there is
// what that pod's rooms, applies and decodes are held to. The two limits are the production pod's
// - 90 per cent of its 32Gi, where the room budget is 16 GiB and the budget admits hundreds of
// mebibyte documents rather than thirty-seven - and the 4 GiB pod these figures were calibrated
// on, whose bounds are the constants every drive under the `memory` tag is written against.
//
// Both are given as GOMEMLIMIT rather than left to the container, as the memory drives give it
// (podGoMemoryLimit): a test runner's own control group is not a pod's.
func TestTheBootLineNamesTheBoundsItsGoMemoryLimitGives(t *testing.T) {
	database := storetest.Open(t)
	for _, row := range []struct {
		name   string
		limit  int64
		bounds map[string]int64
	}{
		{"the production pod's limit", 30_923_764_531, map[string]int64{
			"go_memory_limit_bytes":     30_923_764_531,
			"room_budget_bytes":         17_179_869_185,
			"document_read_cache_bytes": 268_435_456,
			"pressure_reserve_bytes":    2_147_483_648,
			"apply_heap_bytes":          3_775_992_080,
			"copy_heap_bytes":           3_775_992_082,
			"heap_slack_bytes":          3_775_992_080,
			"apply_too_heavy_bytes":     11_327_976_242,
			"peer_write_queue_bytes":    62_914_560,
		}},
		{"the 4 GiB pod the figures are calibrated on", 3_865_470_566, map[string]int64{
			"go_memory_limit_bytes":     3_865_470_566,
			"room_budget_bytes":         2_147_483_648,
			"document_read_cache_bytes": 268_435_456,
			"pressure_reserve_bytes":    268_435_456,
			"apply_heap_bytes":          393_705_335,
			"copy_heap_bytes":           393_705_336,
			"heap_slack_bytes":          393_705_335,
			"apply_too_heavy_bytes":     1_181_116_006,
			"peer_write_queue_bytes":    62_914_560,
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
				Env: map[string]string{"GOMEMLIMIT": strconv.FormatInt(row.limit, 10)},
			})
			process.waitHealthy(t)
			logged := memoryBoundsLine(t, process.Output.String())
			for _, field := range []string{
				"go_memory_limit_bytes", "room_budget_bytes", "document_read_cache_bytes",
				"pressure_reserve_bytes", "apply_heap_bytes", "copy_heap_bytes",
				"heap_slack_bytes", "apply_too_heavy_bytes", "peer_write_queue_bytes",
			} {
				got, named := logged[field]
				if !named {
					t.Errorf("the boot line names no %s", field)
					continue
				}
				if want := row.bounds[field]; got != want {
					t.Errorf("the boot line names %s=%d, want %d", field, got, want)
				}
			}
			if len(logged) != len(row.bounds) {
				t.Errorf("the boot line names %d figures, want the %d the bounds hold: %v", len(logged), len(row.bounds), logged)
			}
		})
	}
}

// memoryBoundsFields reads the figures of one logfmt line, each a key and a whole number.
var memoryBoundsFields = regexp.MustCompile(`([a-z_]+)=(\d+)`)

// memoryBoundsLine is the figures of the boot line naming the document service's memory bounds,
// read from a process's output.
func memoryBoundsLine(t *testing.T, output string) map[string]int64 {
	t.Helper()
	const message = `msg="dispatch: the document service's memory bounds"`
	line := ""
	for _, candidate := range strings.Split(output, "\n") {
		if index := strings.Index(candidate, message); index >= 0 {
			line = candidate[index+len(message):]
			break
		}
	}
	if line == "" {
		t.Fatalf("no memory bounds line in the process's output:\n%s", output)
	}
	figures := make(map[string]int64)
	for _, match := range memoryBoundsFields.FindAllStringSubmatch(line, -1) {
		value, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			t.Fatalf("read %s off the boot line: %v", match[1], err)
		}
		figures[match[1]] = value
	}
	return figures
}
