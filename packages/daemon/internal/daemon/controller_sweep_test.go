package daemon

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// liveTrees is a Sandbox runtime's store in which every tree's lifecycle is open, so the sweep
// keeps every issue Sandbox it consults one for: the controller's Sandbox, when the sweep deletes
// it, is deleted on its claim's account alone, never for want of a live tree.
type liveTrees struct{}

func (liveTrees) IssueHasSessions(context.Context, string, string) (bool, error) { return false, nil }
func (liveTrees) TreeLive(context.Context, string, string) (bool, error)         { return true, nil }

// The orphan sweep is told what knownClaims makes of the daemon's claims, and the Sandbox runtime
// keeps the controller's Sandbox exactly while a known claim is the controller's. So the
// controller's Sandbox, which holds its saved session, survives a sweep while the controller's
// claim is suspended (known, with no locator) and goes once the claim is retired (unknown). This
// runs the daemon's own knownClaims over the controller's claim in each state, as the store holds
// it after the stop, and hands its answer to a real Sandbox runtime (sandbox.New) over an API that
// holds the controller's Sandbox, at the periodic sweep's grace.
func TestTheOrphanSweepKeepsASuspendedControllersSandboxAndDeletesARetiredOnes(t *testing.T) {
	controller := claim.ControllerToken("legion")
	for _, tc := range []struct {
		state supervise.ClaimState
		known []runtime.Known
		want  string
	}{
		{supervise.StateSuspended, []runtime.Known{{Claim: controller}}, "Running"},
		{supervise.StateRetired, nil, "deleted"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			api := &issueSandboxAPI{object: map[string]any{
				"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
				"metadata": map[string]any{
					"name": "legion-legion-controller", "namespace": "legion", "uid": "sandbox-uid", "resourceVersion": "1",
					"creationTimestamp": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
					"labels":            map[string]any{"legion.dev/project": "legion", "legion.dev/role": "controller"},
				},
				"spec": map[string]any{"operatingMode": "Running"},
			}}
			server := httptest.NewServer(api)
			t.Cleanup(func() { cancel(); server.Close() })
			rt, err := sandbox.New(ctx, &rest.Config{Host: server.URL}, sandbox.Options{
				Namespace: "legion", Project: "legion", Store: liveTrees{}, Image: "ghcr.io/example/worker@sha256:" + strings.Repeat("a", 64),
				StorageClass: "standard", IssueVolume: resource.MustParse("1Gi"), StreamURL: "tcp://127.0.0.1:13371",
				Tools:       sandbox.Tools{GH: "/usr/bin/gh", Git: "/usr/bin/git", JJ: "/usr/bin/jj", Legion: "/opt/legion/bin/legion", AgentSecrets: "/opt/legion/bin/agent-secrets"},
				BootTimeout: time.Second, TerminationGrace: time.Second, ProbeInterval: time.Hour, AdoptTimeout: time.Second,
				Tokens: issueProvisionTokens{}, Conns: fake.NewConns(), Log: quietLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			stopped := supervise.Claim{
				Token: controller, Project: "legion", Role: claim.RoleController, State: tc.state, Generation: 4,
				Session: "ses_controller", SessionFile: "/home/legion/.omp/agent/sessions/--legion--/controller.jsonl",
			}
			known := knownClaims([]supervise.Claim{stopped})
			if !reflect.DeepEqual(known, tc.known) {
				t.Fatalf("knownClaims tells the sweep %+v of a %s controller, want %+v", known, tc.state, tc.known)
			}
			if err := rt.ReconcileOrphans(ctx, known, orphanGrace); err != nil {
				t.Fatal(err)
			}
			if got := api.mode(); got != tc.want {
				t.Fatalf("the %s controller's Sandbox after the sweep is %s, want %s", tc.state, got, tc.want)
			}
		})
	}
}
