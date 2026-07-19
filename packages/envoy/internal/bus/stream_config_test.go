package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

type streamInfoJetStream struct {
	nats.JetStreamContext
	config nats.StreamConfig
}

func (js *streamInfoJetStream) StreamInfo(_ string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	return &nats.StreamInfo{Config: js.config}, nil
}

func (js *streamInfoJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.config = *cfg
	return &nats.StreamInfo{Config: js.config}, nil
}

func TestEnsureStreamWithConfig_updatesMaxAgeWhenExistingStreamDiffers(t *testing.T) {
	// Given
	oldConfig := *streamCfg
	oldConfig.MaxAge = time.Minute
	js := &streamInfoJetStream{config: oldConfig}
	desiredConfig := oldConfig
	desiredConfig.MaxAge = 72 * time.Hour

	// When
	if err := ensureStreamWithConfig(js, &desiredConfig); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Then
	if js.config.MaxAge != desiredConfig.MaxAge {
		t.Fatalf("stream MaxAge = %s, want %s", js.config.MaxAge, desiredConfig.MaxAge)
	}
}
