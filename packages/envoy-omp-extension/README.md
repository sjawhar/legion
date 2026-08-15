# OMP Envoy Extension

Tracked OMP extension for Envoy messaging. It shares the Envoy HTTP client, tool contract, envelope
display data, and subject helpers with the other Legion adapters while keeping OMP's direct NATS
subscription and Pi follow-up delivery local.

## Development install

From the Legion repository root, link the tracked entry into OMP:

```sh
ln -sfn "$PWD/packages/envoy-omp-extension/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

Released installs package the extension and its `nats` dependency. The symlink is only for local
development.
