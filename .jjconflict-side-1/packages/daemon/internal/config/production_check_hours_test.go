package config

import (
	"testing"
	"time"
)

// production_check_hours is read as linger_hours is: a positive number of hours, a decimal among
// them, so a proof can drive the check's bound inside one run; 14 days when the file names none.
func TestProductionCheckHoursTakesAPositiveDecimalAndDefaultsToFourteenDays(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.ProductionCheckBound != 336*time.Hour {
		t.Errorf("ProductionCheckBound = %s, want the 14-day default", cfg.ProductionCheckBound)
	}
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0.1", 6 * time.Minute},
		{"1.5", 90 * time.Minute},
		{"336", 336 * time.Hour},
		{"596", 596 * time.Hour},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"production_check_hours: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.ProductionCheckBound != tc.want {
				t.Errorf("ProductionCheckBound = %s, want %s", cfg.ProductionCheckBound, tc.want)
			}
			if cfg.Linger != 72*time.Hour {
				t.Errorf("Linger = %s, want its own default untouched by production_check_hours", cfg.Linger)
			}
		})
	}
}

func TestProductionCheckHoursRefuses(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"0", "production_check_hours must be a positive number"},
		{"-0.5", "production_check_hours must be a positive number"},
		{"596.5", "production_check_hours must be at most 596"},
		{"soon", "production_check_hours must be a number"},
		{"\"336\"", "production_check_hours must be a number"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+"production_check_hours: "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
