package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The route the model probe's tests name, and the model the image's profile answers from.
const (
	testRoute = "https://middleman.legion.internal/anthropic"
	testModel = "anthropic/claude-fable-5-1-legion"
)

// modelSteps are what the fake Oh My Pi answers the model round trip with: the turn's JSON event
// stream as a real Oh My Pi prints it in `--mode json` (a `message_end` per message), its stderr,
// and its exit.
var modelSteps = map[string]string{
	"answers": `printf '%s\n' '{"type":"session","version":3}' \
  '{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"Reply with the single word ok."}]}}' \
  '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-fable-5-1-legion","stopReason":"stop"}}' \
  '{"type":"agent_end","isTerminal":true,"message":null}'; exit 0`,
	"bedrock": `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"api":"bedrock-converse-stream","provider":"amazon-bedrock","model":"us.anthropic.claude-opus-4-8","stopReason":"stop"}}'; exit 0`,
	"nokey": `echo 'cat: /var/run/legion/gateway/token: No such file or directory' >&2
echo 'No model available matching enabledModels (anthropic/*-legion) with usable credentials. Configure auth for an allowed provider or adjust enabledModels.' >&2; exit 1`,
	"not-found":    `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"api":"anthropic-messages","provider":"anthropic","model":"claude-fable-5-1-legion","stopReason":"error","errorMessage":"404 {\"type\":\"error\",\"error\":{\"type\":\"not_found_error\",\"message\":\"model not found\"}}"}}'; exit 1`,
	"unauthorized": `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"api":"anthropic-messages","provider":"anthropic","model":"claude-fable-5-1-legion","stopReason":"error","errorMessage":"401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid api key\"}}"}}'; exit 1`,
	"overloaded":   `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"api":"anthropic-messages","provider":"anthropic","model":"claude-fable-5-1-legion","stopReason":"error","errorMessage":"529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}}'; exit 1`,
	"unreachable":  `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"api":"anthropic-messages","provider":"anthropic","model":"claude-fable-5-1-legion","stopReason":"error","errorMessage":"Unable to connect. Is the computer able to access the url?"}}'; exit 1`,
	"dies":         `echo 'database is locked' >&2; exit 1`,
	"silent":       `exit 0`,
	"hang":         `exec sleep 30`,
}

// planModel gives the fake Oh My Pi's model round trip its plan, one step per attempt (the last
// repeats), as newImageOmp does for the three launch probes.
func (f imageOmp) planModel(t *testing.T, steps ...string) {
	t.Helper()
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncase \"$1\" in\n")
	for name, body := range modelSteps {
		script.WriteString(name + ")\n" + body + "\n;;\n")
	}
	script.WriteString("esac\necho \"fake omp: no model step $1\" >&2; exit 65\n")
	if err := os.WriteFile(filepath.Join(f.dir, "model.sh"), []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "model.plan"), []byte(strings.Join(steps, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func modelGate(t *testing.T, f imageOmp, attempts int) pluginGate {
	t.Helper()
	gate, _ := imageGateUnder(t, f, contractCurrent, attempts)
	gate.model, gate.route = testModel, testRoute
	return gate
}

// The image probe's fourth probe runs only when the profile is routed through a gateway: one
// print-mode turn on the profile's default role, as a phase worker's Oh My Pi runs it — the
// profile's models, roles and pins, no plugin, no tools, no session — last, after the launch
// probes, under the image's own environment.
func TestTheModelProbeMakesOneTurnThroughTheProfile(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	f.planModel(t, "answers")
	gate := modelGate(t, f, 0)

	if err := gate.verifyImage(context.Background()); err != nil {
		t.Fatalf("verifyImage = %v, want a pass", err)
	}

	if got := strings.Join(f.calls(t), " "); got != "agents load session model" {
		t.Fatalf("probes ran as %q, want the three launch probes, then the model round trip", got)
	}
	argv := strings.TrimSpace(f.read(t, "model.argv.1"))
	if want := "-p --mode json --no-session --no-tools --no-extensions --no-skills --no-rules --no-lsp --no-title Reply with the single word ok."; argv != want {
		t.Errorf("the round trip ran `omp %s`, want `omp %s`", argv, want)
	}
	for name, value := range gate.env {
		if !strings.Contains(f.read(t, "model.env.1"), name+"="+value+"\n") {
			t.Errorf("the round trip ran without the image's %s=%s", name, value)
		}
	}

	// Without a route (the image build, a tmux host) there is nothing to prove a round trip through.
	f = newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	gate, _ = imageGateUnder(t, f, contractCurrent, 0)
	if err := gate.verifyImage(context.Background()); err != nil {
		t.Fatalf("verifyImage without a route = %v", err)
	}
	if got := strings.Join(f.calls(t), " "); got != "agents load session" {
		t.Errorf("probes ran as %q without a route, want no model round trip", got)
	}
}

// What answered the turn, and how, is the verdict. Only the profile's default model through the
// gateway's anthropic provider passes. A turn answered by any other provider or model, a profile
// with no usable model (the key command failing), and the gateway refusing the key or the model
// are answers no retry changes, refused naming what the turn said and the route; the gateway
// overloaded or unreachable, Oh My Pi dying before it answered, or a turn cut off, are waited out.
func TestTheModelProbeClassifiesWhatAnsweredTheTurn(t *testing.T) {
	for _, testCase := range []struct {
		plan     []string
		attempts int
		refusal  []string
	}{
		{[]string{"answers"}, 1, nil},
		{[]string{"bedrock"}, 1, []string{"answered by amazon-bedrock/us.anthropic.claude-opus-4-8", "not " + testModel, testRoute}},
		{[]string{"nokey"}, 1, []string{"no usable model", "/var/run/legion/gateway/token: No such file or directory", "No model available matching enabledModels", testRoute}},
		{[]string{"not-found"}, 1, []string{"the gateway refused", "404", "model not found", testRoute}},
		{[]string{"unauthorized"}, 1, []string{"the gateway refused", "401", "invalid api key"}},
		{[]string{"silent"}, 1, []string{"answered nothing"}},
		{[]string{"overloaded", "unreachable", "answers"}, 3, nil},
		{[]string{"dies", "hang", "answers"}, 3, nil},
	} {
		t.Run(strings.Join(testCase.plan, ","), func(t *testing.T) {
			f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
			f.planModel(t, testCase.plan...)
			gate := modelGate(t, f, 0)

			err := gate.verifyImage(context.Background())

			if testCase.refusal == nil && err != nil {
				t.Fatalf("verifyImage = %v, want a pass", err)
			}
			for _, want := range testCase.refusal {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("verifyImage = %v, want a refusal saying %q", err, want)
				}
			}
			if n := f.attempts(t, "model"); n != testCase.attempts {
				t.Errorf("the round trip ran %d times, want %d", n, testCase.attempts)
			}
		})
	}
}

// The round trip's retry is the image probe's, bounded: a gateway that never answers ends the
// probe naming the budget and what the last attempt said.
func TestTheModelProbeGivesUpAfterItsAttempts(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	f.planModel(t, "overloaded")
	gate := modelGate(t, f, 3)

	err := gate.verifyImage(context.Background())

	for _, want := range []string{"the model route probe never completed within its retry budget (3 attempts)", "529", "Overloaded", testRoute} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("verifyImage = %v, want an error saying %q", err, want)
		}
	}
	if n := f.attempts(t, "model"); n != 3 {
		t.Errorf("the round trip ran %d times, want the bound, 3", n)
	}
}
