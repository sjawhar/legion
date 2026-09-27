package daemon

import (
	"fmt"
	"os"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// launchSecret is one secret every launch's spec carries (specs.SpawnSpec), by its name, and how
// the daemon reads it.
type launchSecret struct {
	name string
	read func() (string, error)
}

// launchSecrets are the secrets every launch's spec carries, in name order: the Envoy bearer, when
// the daemon has one, and the NATS nkey seed, when the configuration or the daemon's environment
// (lookup) names one (natsauth.Seed) — the seed the daemon's own NATS connection authenticates
// with. Which of them there are is known from the configuration and the environment alone, so
// `legion start --check-config` names them without reading a file.
func launchSecrets(cfg config.Config, lookup func(string) (string, bool)) []launchSecret {
	var secrets []launchSecret
	if cfg.EnvoyTokenFile != "" {
		secrets = append(secrets, launchSecret{"ENVOY_TOKEN", func() (string, error) {
			return config.ReadSecretPointer("envoy_token_file", cfg.EnvoyTokenFile)
		}})
	}
	if natsauth.Configured(cfg.NatsNkeySeedFile, lookup) {
		secrets = append(secrets, launchSecret{natsauth.SeedVariable, func() (string, error) {
			return natsauth.Seed(cfg.NatsNkeySeedFile, lookup)
		}})
	}
	return secrets
}

// launchSecretNames are the names of the secrets every launch's spec carries (launchSecrets), which
// a provider key and the operator's pod may not collide with.
func launchSecretNames(cfg config.Config, lookup func(string) (string, bool)) []string {
	var names []string
	for _, secret := range launchSecrets(cfg, lookup) {
		names = append(names, secret.name)
	}
	return names
}

// CheckOperatorConfig refuses, over the configuration and the daemon's environment (lookup), what
// the operator configured that collides with Legion's own, before anything is opened: boot runs
// it, and so does `legion start --check-config`, which starts no runtime. On either runtime, a
// provider key may not name a launch secret, which every launch hands the agent behind its
// `<NAME>_FILE` pointer alone (a tmux pane would refuse every launch; a pod's shim skips the key).
// Under runtime: kubernetes, the operator's pod and provider keys are then held to the Sandbox
// runtime's own refusals (sandbox.CheckPod).
func CheckOperatorConfig(cfg config.Config, lookup func(string) (string, bool)) error {
	names := launchSecretNames(cfg, lookup)
	for _, key := range cfg.ProviderKeys {
		if slices.Contains(names, key.Env) {
			return fmt.Errorf("provider_keys names %s, the launch secret every launch carries behind its %s_FILE pointer: a provider key may not name a launch secret", key.Env, key.Env)
		}
	}
	if cfg.Runtime.Kubernetes == nil {
		return nil
	}
	return sandbox.CheckPod(sandbox.Pod(cfg.Runtime.Kubernetes.Pod), providerSecretKeys(cfg.ProviderKeys),
		workerImageTools, names, providersSecrets(cfg, lookup))
}

// environLookup is os.LookupEnv over environ, the daemon's environment or a test's.
func environLookup(environ []string) func(string) (string, bool) {
	return func(name string) (string, bool) { return envValue(environ, name) }
}

// environment is o.environ, or the process's when a test replaced none.
func (o overrides) environment() []string {
	if o.environ == nil {
		return os.Environ()
	}
	return o.environ
}
