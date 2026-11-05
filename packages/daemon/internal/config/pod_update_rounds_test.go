package config

import (
	"testing"
)

// pod_update_max_open_rounds and pod_update_rounds_per_minute cap how many issues hold an open
// pod-update round at once and how many rounds one sweep opens in a minute. Each is a positive
// integer, and each is absent from a file that wants no cap at all: there is no default, so the
// resolved config carries nil rather than a number nobody chose.
func TestPodUpdateRoundsKeys(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.PodUpdateMaxOpenRounds != nil || cfg.PodUpdateRoundsPerMinute != nil {
		t.Errorf("PodUpdateMaxOpenRounds = %v, PodUpdateRoundsPerMinute = %v; want nil for both, the no-limit a file that names neither key asks for",
			cfg.PodUpdateMaxOpenRounds, cfg.PodUpdateRoundsPerMinute)
	}

	cfg, err = LoadForValidation(writeConfigFile(t,
		minimalFile+"pod_update_max_open_rounds: 3\npod_update_rounds_per_minute: 2\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if cfg.PodUpdateMaxOpenRounds == nil || cfg.PodUpdateRoundsPerMinute == nil {
		t.Fatalf("PodUpdateMaxOpenRounds = %v, PodUpdateRoundsPerMinute = %v; want the file's 3 and 2",
			cfg.PodUpdateMaxOpenRounds, cfg.PodUpdateRoundsPerMinute)
	}
	if *cfg.PodUpdateMaxOpenRounds != 3 || *cfg.PodUpdateRoundsPerMinute != 2 {
		t.Errorf("PodUpdateMaxOpenRounds = %d, PodUpdateRoundsPerMinute = %d; want the file's 3 and 2",
			*cfg.PodUpdateMaxOpenRounds, *cfg.PodUpdateRoundsPerMinute)
	}

	// A cap of none is the absent key, never a number, so zero and a negative are refused, as is
	// a value that is no integer; each refusal names the key it refuses, since a file may set
	// both.
	for _, key := range []string{"pod_update_max_open_rounds", "pod_update_rounds_per_minute"} {
		for _, tc := range []struct{ value, want string }{
			{"0", key + " must be a positive integer"},
			{"-1", key + " must be a positive integer"},
			{"2.5", key + " must be an integer"},
			{"three", key + " must be an integer"},
		} {
			t.Run(key+" "+tc.value, func(t *testing.T) {
				_, err := LoadForValidation(writeConfigFile(t, minimalFile+key+": "+tc.value+"\n"), noEnv)
				if err == nil || err.Error() != tc.want {
					t.Errorf("LoadForValidation error = %v, want %q", err, tc.want)
				}
			})
		}
	}
}
