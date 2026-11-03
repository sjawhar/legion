package bus_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// ENVOY_NATS_REPLICAS is the deployment's replica count, 1 when unset or blank and otherwise a
// whole number from 1 to bus.MaxReplicas.
func TestParseNATSReplicasReadsACountFrom1ToMaxReplicas(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 1},
		{" ", 1},
		{"1", 1},
		{" 3 ", 3},
		{"5", 5},
	} {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			got, err := bus.ParseNATSReplicas(tc.raw)
			if err != nil || got != tc.want {
				t.Fatalf("ParseNATSReplicas(%q) = %d, %v; want %d", tc.raw, got, err, tc.want)
			}
		})
	}
	if bus.MaxReplicas != 5 {
		t.Fatalf("MaxReplicas = %d, want 5, the most copies NATS keeps", bus.MaxReplicas)
	}
}

// ENVOY_STREAM_MAX_BYTES is the stream's byte limit, 0 (none set) when unset or blank.
func TestParseStreamMaxBytesReadsAPositiveByteCount(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"", 0},
		{" ", 0},
		{"1", 1},
		{"10737418240", 10 << 30},
		{" 4096 ", 4096},
	} {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			got, err := bus.ParseStreamMaxBytes(tc.raw)
			if err != nil || got != tc.want {
				t.Fatalf("ParseStreamMaxBytes(%q) = %d, %v; want %d", tc.raw, got, err, tc.want)
			}
		})
	}
}

// A replica count outside 1 to MaxReplicas or a byte limit that is not a positive number is
// refused, naming the variable and the value.
func TestTheSettingsParsersRefuseACountOutside1To5AndAByteLimitThatIsNotPositive(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{bus.ReplicasEnvVar, "0"},
		{bus.ReplicasEnvVar, "-1"},
		{bus.ReplicasEnvVar, "6"},
		{bus.ReplicasEnvVar, "x"},
		{bus.ReplicasEnvVar, "2.5"},
		{bus.StreamMaxBytesEnvVar, "0"},
		{bus.StreamMaxBytesEnvVar, "-5"},
		{bus.StreamMaxBytesEnvVar, "x"},
		{bus.StreamMaxBytesEnvVar, "10GB"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			var err error
			if tc.name == bus.ReplicasEnvVar {
				_, err = bus.ParseNATSReplicas(tc.value)
			} else {
				_, err = bus.ParseStreamMaxBytes(tc.value)
			}
			if want := fmt.Sprintf("invalid %s %q", tc.name, tc.value); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want a refusal naming %s and %q", err, tc.name, tc.value)
			}
		})
	}
}
