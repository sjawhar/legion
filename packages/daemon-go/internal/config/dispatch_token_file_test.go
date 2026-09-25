package config

import (
	"strings"
	"testing"
)

// workflowFile is a configuration a workflow boot accepts: Dispatch with its bearer, NATS, the
// daemon's own project among projects, and both GitHub Apps (whose key commands the validating
// loader never runs).
const workflowFile = `project: DEMO
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: ./state
dispatch_url: https://dispatch.test
dispatch_token_file: ./dispatch-token
nats_urls: [nats://127.0.0.1:4222]
projects:
  DEMO: { repo: acme/widgets }
github_apps:
  implement: { app_id: "1", private_key_command: "exit 1" }
  review: { app_id: "2", private_key_command: "exit 1" }
`

// Boot refuses a workflow with no Dispatch bearer file (internal/daemon/workflow.go bind), so the
// validating loader refuses it too, with boot's words — `legion start --check-config` never says OK
// to a file `legion start` then refuses for it. Load refuses it before running a key command.
func TestLoadRefusesDispatchWithoutItsTokenFile(t *testing.T) {
	if _, err := LoadForValidation(writeConfigFile(t, workflowFile), nil); err != nil {
		t.Fatalf("the bootable workflow file: %v", err)
	}
	body := strings.Replace(workflowFile, "dispatch_token_file: ./dispatch-token\n", "", 1)
	const want = "dispatch_token_file is required when dispatch_url is configured"
	for name, load := range map[string]func(string, func(string) string) (Config, error){"LoadForValidation": LoadForValidation, "Load": Load} {
		if _, err := load(writeConfigFile(t, body), nil); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}
