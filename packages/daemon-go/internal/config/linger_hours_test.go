package config

import (
	"testing"
	"time"
)

// linger_hours takes a decimal, so a proof can watch a tree's node released and then the tree
// closed inside one run; every integer stays valid, under the same bound.
func TestLingerHoursTakesAPositiveDecimal(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0.3", 18 * time.Minute},
		{"1.5", 90 * time.Minute},
		{"72", 72 * time.Hour},
		{"596", 596 * time.Hour},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"linger_hours: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Linger != tc.want {
				t.Errorf("Linger = %s, want %s", cfg.Linger, tc.want)
			}
		})
	}
}

func TestLingerHoursRefuses(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"0", "linger_hours must be a positive number"},
		{"-0.5", "linger_hours must be a positive number"},
		{".nan", "linger_hours must be a positive number"},
		{"596.5", "linger_hours must be at most 596"},
		{".inf", "linger_hours must be at most 596"},
		{"soon", "linger_hours must be a number"},
		{"\"0.3\"", "linger_hours must be a number"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"linger_hours: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
