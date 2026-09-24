package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// linger_hours reaches the workflow engine as the duration it names, a decimal included: the
// Stage 4b proof lingers 0.3 hours so a tree closes inside one run, and an hour count truncated to
// zero would reach the engine as nothing, which it takes for its 72-hour default.
func TestLingerHoursReachesTheEngineAsItsDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legion.yaml")
	body := "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nlinger_hours: 0.3\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}

	if got := engineConfig(cfg).LingerHours; got != 18*time.Minute {
		t.Errorf("the engine's linger = %s, want 18m0s", got)
	}
}
