package config

import (
	"strings"
	"testing"
)

func TestLoadModelsMaxFixAttempts(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.MaxFixAttempts != 3 {
		t.Fatalf("MaxFixAttempts = %d, want shipped default 3", cfg.MaxFixAttempts)
	}

	configured, err := Load(writeConfigFile(t, minimalFile+"max_fix_attempts: 5\n"), noEnv)
	if err != nil {
		t.Fatalf("Load max_fix_attempts: %v", err)
	}
	if configured.MaxFixAttempts != 5 {
		t.Fatalf("MaxFixAttempts = %d, want 5", configured.MaxFixAttempts)
	}

	_, err = Load(writeConfigFile(t, minimalFile+"max_fix_attempts: 0\n"), noEnv)
	if err == nil || !strings.Contains(err.Error(), "max_fix_attempts must be a positive integer") {
		t.Fatalf("Load zero max_fix_attempts error = %v, want positive-integer refusal", err)
	}
}
