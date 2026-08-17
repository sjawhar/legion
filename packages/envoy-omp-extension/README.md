# OMP Envoy Extension

Tracked OMP extension for Envoy messaging. It shares the Envoy HTTP client, tool contract, envelope
display data, and subject helpers with the other Legion adapters while keeping OMP's direct NATS
subscription and Pi follow-up delivery local.

## Envoy identity

Run `/envoy_whoami` to show the active session ID, machine ID, and working directory. The command
also asks OMP to copy the session ID through its host clipboard API, which sends OSC 52 first for
tmux and SSH sessions. The identity remains visible if the clipboard request fails.

For tmux to accept OSC 52 clipboard writes, enable clipboard support in the tmux server:

```tmux
set -g set-clipboard on
```

## Development install

From the Legion repository root, link the tracked entry into OMP:

```sh
ln -sfn "$PWD/packages/envoy-omp-extension/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

Released installs package the extension and its `nats` dependency. The symlink is only for local
development.
