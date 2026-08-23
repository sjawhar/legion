# OMP Envoy Extension

Tracked OMP extension for Envoy messaging. It shares the Envoy HTTP client, tool contract, envelope
display data, and subject helpers with the other Legion adapters while keeping OMP's direct NATS
subscription and Pi steering delivery local (inbound messages steer an in-flight turn instead of queueing behind it).

## Session identity

Run `/whoami` to copy the active session ID to the clipboard (matching the `/whoami` command the
OpenCode envoy plugin registers). OMP copies through its host clipboard API, which sends OSC 52
first for tmux and SSH sessions. The notification shows the session ID even if the copy fails.

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
