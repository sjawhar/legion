package main

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

// dispatchPodGolden is the pod that envoy-infra's gcp:Dispatch component renders for the app
// Deployment, recorded byte for byte, read from this package's directory; dispatchPodGoldenPath
// names the same file from the repository root, for a failure read in CI. The component lives in
// packages/envoy/infra, which is a Go module of its own, so this test reads what it renders
// rather than calling it.
const (
	dispatchPodGolden     = "../../infra/gcp/testdata/dispatch-pod.golden"
	dispatchPodGoldenPath = "packages/envoy/infra/gcp/testdata/dispatch-pod.golden"
)

// dispatchPodContainer is the container of that pod which runs this command.
const dispatchPodContainer = "envoy-dispatch"

// dispatchPod is as much of the rendered pod as this test reads. Every pointer is nil when the
// golden does not carry that field, which is a failure here rather than a zero: the shape this
// test reads is the shape the component is required to render.
type dispatchPod struct {
	Spec *struct {
		TerminationGracePeriodSeconds *int `json:"terminationGracePeriodSeconds"`
		Containers                    []struct {
			Name      string `json:"name"`
			Lifecycle *struct {
				PreStop *struct {
					Exec *struct {
						Command []string `json:"command"`
					} `json:"exec"`
				} `json:"preStop"`
			} `json:"lifecycle"`
		} `json:"containers"`
	} `json:"spec"`
}

// TestDispatchPodGracePeriodCoversStopGrace holds the Kubernetes side of stopGrace, which nothing
// else does: the pod that gcp:Dispatch renders is one of the runtimes that grant these seconds,
// its grace period is written out as a number in another Go module's component, and this test is
// what holds that number to stopGrace. So raising stopGrace here breaks nothing over there, and
// this test is what tells whoever raises it that the pod has to be raised with it.
//
// The kubelet runs the pre-stop hook and the server's shutdown inside the one
// terminationGracePeriodSeconds - it sends SIGTERM only once the hook returns - so the seconds
// left for the shutdown are the grace period less the hook's sleep, and they must cover stopGrace.
func TestDispatchPodGracePeriodCoversStopGrace(t *testing.T) {
	golden, err := os.ReadFile(dispatchPodGolden)
	if err != nil {
		t.Fatalf("read %s, the pod gcp:Dispatch renders: %v", dispatchPodGoldenPath, err)
	}
	var pod dispatchPod
	if err := json.Unmarshal(golden, &pod); err != nil {
		t.Fatalf("parse %s as the pod JSON it records: %v", dispatchPodGoldenPath, err)
	}
	if pod.Spec == nil || pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatalf("%s carries no spec.terminationGracePeriodSeconds; this test cannot tell whether the pod covers stopGrace", dispatchPodGoldenPath)
	}
	grace := *pod.Spec.TerminationGracePeriodSeconds

	var command []string
	found := false
	for _, container := range pod.Spec.Containers {
		if container.Name != dispatchPodContainer {
			continue
		}
		found = true
		if container.Lifecycle == nil || container.Lifecycle.PreStop == nil || container.Lifecycle.PreStop.Exec == nil {
			t.Fatalf("container %s in %s has no lifecycle.preStop.exec; this test cannot tell how much of the grace period the kubelet spends before SIGTERM", dispatchPodContainer, dispatchPodGoldenPath)
		}
		command = container.Lifecycle.PreStop.Exec.Command
	}
	if !found {
		t.Fatalf("%s has no container named %s; this test cannot tell which container's shutdown the grace period has to cover", dispatchPodGoldenPath, dispatchPodContainer)
	}
	if len(command) != 2 || command[0] != "sleep" {
		t.Fatalf("the preStop hook of %s in %s is %q, not a sleep of some seconds; this test reads the wait out of a sleep", dispatchPodContainer, dispatchPodGoldenPath, command)
	}
	wait, err := strconv.Atoi(command[1])
	if err != nil {
		t.Fatalf("the preStop sleep of %s in %s waits %q, which is no whole number of seconds: %v", dispatchPodContainer, dispatchPodGoldenPath, command[1], err)
	}

	if time.Duration(grace-wait)*time.Second < stopGrace {
		t.Fatalf("%s asks for %d seconds of termination grace and spends %d of them on %s's pre-stop sleep, leaving its shutdown %d seconds where stopGrace in packages/envoy/cmd/dispatch/shutdown.go builds it for %s: the pod asks for less than the server's shutdown needs, so the kubelet kills %s mid-shutdown, past its document service's five-second reserve, with a write still waiting on a lock killed mid-commit. Whoever moves one of the two moves the other.",
			dispatchPodGoldenPath, grace, wait, dispatchPodContainer, grace-wait, stopGrace, dispatchPodContainer)
	}
}
