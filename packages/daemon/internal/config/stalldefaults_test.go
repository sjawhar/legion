package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixture this package pins")

// stallDefaults is the fixture's shape. Milliseconds, because that is the unit of the plugin's
// own numbers and of every LEGION_STALL_* variable; one field per default, named as the
// plugin's `StallLimits` names it, so the bun test reads the two side by side.
type stallDefaults struct {
	CallGraceMs int64 `json:"callGraceMs"`
	CallBoundMs int64 `json:"callBoundMs"`
	StepMs      int64 `json:"stepMs"`
	// CallBoundMsByRole is the bound's default for every role a session can hold, the controller
	// among them, since the bound's default is the role's and the daemon's floor is per role.
	CallBoundMsByRole map[claim.Role]int64 `json:"callBoundMsByRole"`
}

// The numbers a worker's stall watch runs on live once, in
// `packages/pi-shared/src/stall-limits.ts`; this package holds the copy the daemon's own backstop
// floor is computed from (stalldefaults.go). This golden is where the two meet: Go writes it
// (`-update`), and `packages/pi-shared/src/stall-limits.test.ts` reads it back and holds it equal
// to DEFAULT_STALL_LIMITS and to the per-role bounds the plugin resolves, so a number moved on
// one side alone fails a test on the other.
func TestStallDefaultsGolden(t *testing.T) {
	// RoleConfigRoles is every issue role and then the controller: the same set a session's
	// LEGION_ROLE can hold, and so the set the bound has a default for.
	byRole := make(map[claim.Role]int64, len(RoleConfigRoles))
	for _, role := range RoleConfigRoles {
		byRole[role] = StallCallBoundFor(role).Milliseconds()
	}
	golden(t, "stall-defaults.json", stallDefaults{
		CallGraceMs:       StallCallGrace.Milliseconds(),
		CallBoundMs:       StallCallBound.Milliseconds(),
		StepMs:            StallStep.Milliseconds(),
		CallBoundMsByRole: byRole,
	})
}

// golden pins what this package writes for the plugin to read: internal/api's helper with this
// package's command in its message, as internal/projection's copy carries its own. A fixture that
// differs from what Go now writes is stale.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
	if *updateGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/config/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}
