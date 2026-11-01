package config

import (
	"strings"
	"testing"
	"time"
)

// A Kubernetes deployment that says nothing about idle release gets it anyway, on the daemon's
// own periods: every deployment on a cluster that charges for a pod's requests pays for an idle
// pod, so release is the default rather than a key an operator must find.
func TestIdleReleaseIsOnByDefaultForEveryKubernetesDeployment(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	got := cfg.Runtime.Kubernetes.IdleRelease
	if got != (IdleReleaseConfig{IdleMinutes: 10, BackstopMinutes: 60}) {
		t.Fatalf("idle_release = %#v, want the 10 and 60 defaults", got)
	}
	if got != DefaultIdleRelease() {
		t.Fatalf("idle_release = %#v, want DefaultIdleRelease() %#v", got, DefaultIdleRelease())
	}
	if !got.Releases() || got.Idle() != 10*time.Minute || got.Backstop() != time.Hour {
		t.Fatalf("idle_release = %#v, want release on at 10m with an hour's backstop", got)
	}
}

// Each member settles on its own: the file's where it sets one, the default for the other.
func TestIdleReleaseSettlesEachMemberTheFileLeavesOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  IdleReleaseConfig
	}{
		{"idle alone", "    idle_release: {idle_minutes: 25}\n", IdleReleaseConfig{IdleMinutes: 25, BackstopMinutes: 60}},
		{"backstop alone", "    idle_release: {backstop_minutes: 180}\n", IdleReleaseConfig{IdleMinutes: 10, BackstopMinutes: 180}},
		{"both", "    idle_release: {idle_minutes: 30, backstop_minutes: 30}\n", IdleReleaseConfig{IdleMinutes: 30, BackstopMinutes: 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if got := cfg.Runtime.Kubernetes.IdleRelease; got != tc.want {
				t.Fatalf("idle_release = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// `idle_minutes: 0` turns release off, and is the one zero the block accepts: the deployment's
// pods then live from an issue's first role start to its close.
func TestIdleMinutesZeroTurnsReleaseOff(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"    idle_release: {idle_minutes: 0}\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	got := cfg.Runtime.Kubernetes.IdleRelease
	if got.Releases() || got.Idle() != 0 {
		t.Fatalf("idle_release = %#v, want release off", got)
	}
	if got.BackstopMinutes != 60 {
		t.Fatalf("backstop_minutes = %d, want the default kept", got.BackstopMinutes)
	}
}

// What the loader refuses, each refusal naming the key an operator has to edit, so
// `legion start --check-config` exits 1 saying where to look.
func TestIdleReleaseRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{
			"backstop below idle", "    idle_release: {idle_minutes: 30, backstop_minutes: 29}\n",
			"runtime.kubernetes.idle_release.backstop_minutes (29) is below runtime.kubernetes.idle_release.idle_minutes (30)",
		},
		{
			"backstop below the default idle", "    idle_release: {backstop_minutes: 5}\n",
			"runtime.kubernetes.idle_release.backstop_minutes (5) is below runtime.kubernetes.idle_release.idle_minutes (10)",
		},
		{
			"a negative idle period", "    idle_release: {idle_minutes: -1}\n",
			"runtime.kubernetes.idle_release.idle_minutes must be a positive number of minutes, or 0 to release no pod",
		},
		{
			"a zero backstop", "    idle_release: {backstop_minutes: 0}\n",
			"runtime.kubernetes.idle_release.backstop_minutes must be a positive integer",
		},
		{
			"an unknown member", "    idle_release: {idle_seconds: 600}\n",
			"unknown key runtime.kubernetes.idle_release.idle_seconds",
		},
		{
			"not a mapping", "    idle_release: 10\n",
			"runtime.kubernetes.idle_release must be a mapping",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err == nil {
				t.Fatalf("LoadForValidation accepted %q", tc.block)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadForValidation = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
