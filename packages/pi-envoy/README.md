# Pi Envoy Extension

Tracked Oh My Pi extension for Envoy messaging. It shares the Envoy HTTP client, tool contract, envelope
display data, and subject helpers with the other Legion adapters while keeping OMP's direct NATS
subscriptions and Pi steering delivery local (inbound messages steer an in-flight turn instead of queueing behind it).

Normal topic subscriptions are direct NATS subscriptions owned by this extension. A role claim is
different: the listener arbitrates the core-NATS role lane for the current live holder, then sends
a receipt-backed request with the original role topic to the holder's direct agent subject. The
agent pump replies after accepting the envelope; without a receipt within two seconds the listener
emits a `delivery_failed` exception. Role messages are live only; they are not retained for a later
claimant.

`envoy_list()` shows the union of the local subscriptions and the listener's persisted interest
registry. Each reported interest identifies whether it is `live`, `registry`, or `both`, so
temporary registration drift does not hide the extension's actual delivery state.

## Session identity

Run `/whoami` to copy the active session ID to the clipboard. OMP copies through its host
clipboard API, which sends OSC 52
first for tmux and SSH sessions. The notification shows the session ID even if the copy fails.

For tmux to accept OSC 52 clipboard writes, enable clipboard support in the tmux server:

```tmux
set -g set-clipboard on
```

## Development install

This package declares two OMP extension entries in `package.json`: `extensions/envoy.ts`
(Envoy messaging, subscriptions, and steering delivery) and `extensions/legion.ts` (the
Legion lifecycle: root bootstrap, worker spawning, budgets, and daemon capabilities).
Loading the package directory with OMP's `--extension` flag — as the Legion daemon does
when it launches trees and workers — loads both.

For local development of the messaging extension alone, link the entry into OMP:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

The repository root `package.json` likewise loads only `extensions/envoy.ts` for dev
sessions inside this repo: the Legion extension is meant to be loaded by the daemon with
its environment prepared, not by ambient dev sessions.

Released installs package the extension and its `nats` dependency. The symlink is only for local
development.
