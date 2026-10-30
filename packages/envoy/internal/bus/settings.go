package bus

import (
	"fmt"
	"strconv"
	"strings"
)

// ReplicasEnvVar and StreamMaxBytesEnvVar are the deployment settings WithReplicas and
// WithStreamMaxBytes carry. Each program reads them itself and hands the bus their values; the bus
// names them in a server's refusal of a stream or a bucket that carries one (refusedSettings).
const (
	ReplicasEnvVar       = "ENVOY_NATS_REPLICAS"
	StreamMaxBytesEnvVar = "ENVOY_STREAM_MAX_BYTES"
)

// MaxReplicas is the most copies NATS keeps of a stream or a bucket, and so the largest
// ReplicasEnvVar ParseNATSReplicas takes. aws:WebhookListener's rendering
// (github.com/sjawhar/envoy/infra/aws/rendering) refuses a larger natsReplicas by this bound, so a
// component never deploys a listener its configuration would refuse.
const MaxReplicas = 5

// ParseNATSReplicas reads ReplicasEnvVar, the deployment's replica count: unset or blank is 1,
// and otherwise a whole number from 1 to MaxReplicas. Anything else (0, a negative number, more
// than NATS keeps, a word) is refused naming the variable and the value, at load rather than as a
// server's refusal at the stream. envoy-listener (config.Load in
// github.com/sjawhar/envoy/internal/config) and envoy-dispatch read the setting through it. Every
// program of a deployment carries the same value: one with a lower value can lower the stream
// until the others' keepers raise it back (ConnectOwningStream).
func ParseNATSReplicas(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 1, nil
	}
	replicas, err := strconv.Atoi(value)
	if err != nil || replicas < 1 || replicas > MaxReplicas {
		return 0, fmt.Errorf("invalid %s %q: want a whole number of copies from 1 to %d", ReplicasEnvVar, value, MaxReplicas)
	}
	return replicas, nil
}

// ParseStreamMaxBytes reads StreamMaxBytesEnvVar, the stream's byte limit: unset or blank is 0,
// which sets no limit (a deployed stream keeps the one it has), and otherwise a positive whole
// number of bytes. 0, a negative number or a word is refused naming the variable and the value.
// envoy-listener (config.Load) and envoy-dispatch read the setting through it. A deployed limit is
// never lowered (WithStreamMaxBytes), so programs with different limits leave the largest.
func ParseStreamMaxBytes(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("invalid %s %q: want a positive whole number of bytes, or unset for no limit", StreamMaxBytesEnvVar, value)
	}
	return limit, nil
}
