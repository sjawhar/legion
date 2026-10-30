package sandbox

// The Kubernetes side of dispatch://INFRA-714 R8: the launcher says which of the two session
// refusals its start met, in typed fields of its answer, and the runtime turns each into the
// sentinel the supervisor acts on — the same two the tmux runtime answers with.

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

func TestALauncherStartRefusalCarriesItsSessionVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		result shimwire.LauncherStartResult
		want   error
		other  error
	}{
		"a session the table does not hold": {
			result: shimwire.LauncherStartResult{Error: "resume session /s.jsonl: the session table holds no such session", SessionMissing: true},
			want:   runtime.ErrSessionFileMissing,
			other:  runtime.ErrSessionStore,
		},
		"a store that could not be asked": {
			result: shimwire.LauncherStartResult{Error: "resume session /s.jsonl: the session store could not be asked: connection refused", SessionStoreFault: true},
			want:   runtime.ErrSessionStore,
			other:  runtime.ErrSessionFileMissing,
		},
		"any other refusal": {
			result: shimwire.LauncherStartResult{Error: "generation 3 is still running"},
			other:  runtime.ErrSessionFileMissing,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// The refusal the runtime builds from the launcher's answer (launchers.start).
			refused := startRefusal{token: claim.Token("legion-legion-209-tester"), generation: 4, reason: tc.result.Error,
				sessionMissing: tc.result.SessionMissing, storeFault: tc.result.SessionStoreFault}

			if tc.want != nil && !errors.Is(refused, tc.want) {
				t.Errorf("the refusal %v is not %v, so the supervisor cannot act on it", refused, tc.want)
			}
			if errors.Is(refused, tc.other) {
				t.Errorf("the refusal %v reads as %v, which is the other verdict", refused, tc.other)
			}
			if tc.want == nil && errors.Is(refused, runtime.ErrSessionStore) {
				t.Errorf("the refusal %v reads as a session fault, want a refusal about no session at all", refused)
			}
			if got := refused.Error(); !strings.Contains(got, tc.result.Error) {
				t.Errorf("the refusal reads %q, want it to carry the launcher's own sentence %q", got, tc.result.Error)
			}
		})
	}
}

// The two verdicts travel on the wire: a frame the launcher encodes and the daemon decodes carries
// them, so the daemon never has to read an error's prose to tell them apart.
func TestTheStartResultCarriesItsSessionVerdictOverTheWire(t *testing.T) {
	for _, sent := range []shimwire.LauncherStartResult{
		{ID: "start-4", Error: "no such session", SessionMissing: true},
		{ID: "start-4", Error: "the store did not answer", SessionStoreFault: true},
		{ID: "start-4", OK: true, RunningGeneration: 4},
	} {
		encoded, err := sent.MarshalJSON()
		if err != nil {
			t.Fatalf("encode %+v: %v", sent, err)
		}
		decoded, err := shimwire.Decode(encoded)
		if err != nil {
			t.Fatalf("decode %s: %v", encoded, err)
		}
		got, ok := decoded.(shimwire.LauncherStartResult)
		if !ok {
			t.Fatalf("decoded %T, want a LauncherStartResult", decoded)
		}
		if got != sent {
			t.Fatalf("decoded %+v, want %+v", got, sent)
		}
	}
}
