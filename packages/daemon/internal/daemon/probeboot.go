package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// imageProber is the image probe ProbeBoot runs: sandbox.Prober's.
type imageProber interface {
	ProbeImage(ctx context.Context, p sandbox.ImageProbe) (bootprobe.ImageReport, error)
}

// ProbeBoot is `legion probe-boot`: the part of a Kubernetes daemon's boot its worker pod can refuse,
// run from the daemon's own configuration with nothing the workflow needs. It makes boot's reads of
// the configuration, the caller's environment and the files they name (readBoot), then runs the
// cluster check and the image probe a booting daemon runs (sandboxBootOf: one value for both), the
// probe on a runtime built from the Options boot reads (sandbox.NewProber), under the credentials
// runtime.kubernetes names. The probe Sandbox it creates is the one a booting daemon creates, under
// a name of its own run: built from the operator pod's ServiceAccount, volumes, mounts and
// environment, so a pod the cluster would not admit, mount or start fails here. It skips every boot
// stage that reads nothing of the worker pod — the store and its claims fence and migration and the
// GitHub App mint, which a booting daemon runs before the probe, and the NATS connection and the
// Dispatch listing it runs after — and writes nothing but its probe Sandbox, which it deletes.
func ProbeBoot(ctx context.Context, cfg config.Config, lookup func(string) (string, bool), log *slog.Logger) (bootprobe.ImageReport, error) {
	return probeBoot(ctx, cfg, lookup, log, func(ctx context.Context, rc *rest.Config, opts sandbox.Options) (imageProber, error) {
		return sandbox.NewProber(ctx, rc, opts)
	})
}

// probeBoot is ProbeBoot over newProber, which builds the probe's runtime.
func probeBoot(ctx context.Context, cfg config.Config, lookup func(string) (string, bool), log *slog.Logger,
	newProber func(context.Context, *rest.Config, sandbox.Options) (imageProber, error),
) (bootprobe.ImageReport, error) {
	if cfg.Runtime.Name != "kubernetes" {
		return bootprobe.ImageReport{}, fmt.Errorf("probe-boot runs the Kubernetes runtime's boot probe, and runtime %q has none", cfg.Runtime.Name)
	}
	reads, err := readBoot(cfg, lookup, os.Getenv, true, log)
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	boot, err := sandboxBootOf(cfg, reads.sandbox)
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	checking, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	if err := boot.clusterCheck(checking); err != nil {
		return bootprobe.ImageReport{}, err
	}
	// The prober's informers run until this context ends, so they stop when the probe returns.
	probing, stop := context.WithCancel(ctx)
	defer stop()
	prober, err := newProber(probing, reads.sandbox.client, reads.sandbox.opts)
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	return prober.ProbeImage(probing, boot.probe)
}
