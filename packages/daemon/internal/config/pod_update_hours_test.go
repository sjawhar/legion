package config

import (
	"testing"
	"time"
)

// pod_update_retask_hours and pod_update_notice_hours are read as production_check_hours is: a
// positive number of hours, a decimal among them, so a live proof and the production check can
// drive each bound of an update round inside one run; an hour and four hours when the file names
// none.
func TestPodUpdateHoursTakePositiveDecimalsAndDefaultToAnHourAndFourHours(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.PodUpdateRetask != time.Hour {
		t.Errorf("PodUpdateRetask = %s, want the one-hour default", cfg.PodUpdateRetask)
	}
	if cfg.PodUpdateNotice != 4*time.Hour {
		t.Errorf("PodUpdateNotice = %s, want the four-hour default", cfg.PodUpdateNotice)
	}
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0.1", 6 * time.Minute},
		{"1.5", 90 * time.Minute},
		{"4", 4 * time.Hour},
		{"596", 596 * time.Hour},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t,
				minimalFile+"pod_update_retask_hours: "+tc.value+"\npod_update_notice_hours: "+tc.value+"\n"), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.PodUpdateRetask != tc.want || cfg.PodUpdateNotice != tc.want {
				t.Errorf("PodUpdateRetask = %s, PodUpdateNotice = %s, want %s for both",
					cfg.PodUpdateRetask, cfg.PodUpdateNotice, tc.want)
			}
			if cfg.ProductionCheckBound != 336*time.Hour || cfg.Linger != 72*time.Hour {
				t.Errorf("ProductionCheckBound = %s, Linger = %s, want their own defaults untouched by the update-round keys",
					cfg.ProductionCheckBound, cfg.Linger)
			}
		})
	}
}

// Each key is bounded on its own, so one named alone leaves the other at its default.
func TestPodUpdateHoursAreBoundSeparately(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+"pod_update_retask_hours: 0.25\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.PodUpdateRetask != 15*time.Minute || cfg.PodUpdateNotice != 4*time.Hour {
		t.Errorf("PodUpdateRetask = %s, PodUpdateNotice = %s, want 15m and the four-hour default",
			cfg.PodUpdateRetask, cfg.PodUpdateNotice)
	}
}

func TestPodUpdateHoursRefuse(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value string
		want  string
	}{
		{"pod_update_retask_hours", "0", "pod_update_retask_hours must be a positive number"},
		{"pod_update_retask_hours", "-0.5", "pod_update_retask_hours must be a positive number"},
		{"pod_update_retask_hours", "596.5", "pod_update_retask_hours must be at most 596"},
		{"pod_update_retask_hours", "soon", "pod_update_retask_hours must be a number"},
		{"pod_update_retask_hours", "\"1\"", "pod_update_retask_hours must be a number"},
		{"pod_update_notice_hours", "0", "pod_update_notice_hours must be a positive number"},
		{"pod_update_notice_hours", "-4", "pod_update_notice_hours must be a positive number"},
		{"pod_update_notice_hours", "596.5", "pod_update_notice_hours must be at most 596"},
		{"pod_update_notice_hours", "later", "pod_update_notice_hours must be a number"},
		{"pod_update_notice_hours", "\"4\"", "pod_update_notice_hours must be a number"},
	} {
		t.Run(tc.key+" "+tc.value, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.key+": "+tc.value+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
			}
		})
	}
}
