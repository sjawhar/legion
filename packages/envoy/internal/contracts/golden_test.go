package contracts

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var updateGoldens = flag.Bool("update", false, "update golden files")

type githubEnvelopeGolden struct {
	Topic          string `json:"topic"`
	PayloadSummary string `json:"payload_summary"`
	Payload        any    `json:"payload"`
}

func TestGithubEnvelopeGoldens(t *testing.T) {
	fixtures := []struct {
		name  string
		event string
	}{
		{name: "pull-request-opened", event: "pull_request"},
		{name: "pull-request-synchronize", event: "pull_request"},
		{name: "pull-request-closed-merged", event: "pull_request"},
		{name: "issue-comment-created", event: "issue_comment"},
		{name: "issue-comment-pr-created", event: "issue_comment"},
		{name: "push", event: "push"},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			rawPath := filepath.Join("..", "..", "scripts", "fixtures", "github", fixture.name+".json")
			raw, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatalf("read raw fixture: %v", err)
			}

			body := map[string]any{}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode raw fixture: %v", err)
			}
			envelope := GithubEnvelope(GithubEnvelopeInput{
				Event:    fixture.event,
				Delivery: "golden-" + fixture.name,
				Body:     body,
			})
			payload := any(nil)
			if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
				t.Fatalf("decode normalized payload: %v", err)
			}
			got := githubEnvelopeGolden{
				Topic:          envelope.Topic,
				PayloadSummary: envelope.PayloadSummary,
				Payload:        payload,
			}

			goldenPath := filepath.Join("..", "..", "..", "contracts", "fixtures", "github-envelopes", fixture.name+".json")
			if *updateGoldens {
				encoded, err := json.MarshalIndent(got, "", "  ")
				if err != nil {
					t.Fatalf("encode golden: %v", err)
				}
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("create golden directory: %v", err)
				}
				if err := os.WriteFile(goldenPath, append(encoded, '\n'), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			goldenBytes, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			want := githubEnvelopeGolden{}
			if err := json.Unmarshal(goldenBytes, &want); err != nil {
				t.Fatalf("decode golden: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized envelope mismatch\ngot:  %#v\nwant: %#v", got, want)
			}
		})
	}
}
