---
title: "A kubernetes-only configuration key is proven in a pod at the load-and-project seam, with the previous round's code overlaid on the same file as the negative control"
category: testing
tags:
  - worker-pod
  - kubernetes
  - proof-environment
  - config-load
  - negative-control
  - real-binary-proof
date: 2026-10-21
status: active
module: packages/daemon
related_issues:
  - "LEGION-791"
  - "sjawhar/legion#2096"
---

# A kubernetes-only configuration key is proven in a pod at the load-and-project seam, with the previous round's code overlaid on the same file as the negative control

Extends docs/solutions/testing/the-pods-api-only-daemon-runs-no-outbox-lanes-a-tmux-stub-and-a-pgx-runner-boot-and-seed-it-for-a-state-proof-LEGION-694.md.

That note boots the real binary under tmux in a pod. A `runtime: kubernetes` file cannot boot there
(no cluster reaches a pod), and docs/solutions/testing/prove-a-daemon-only-kubernetes-change-before-an-image-exists-the-branch-daemon-on-the-host-against-published-pods-and-a-node-side-kill.md
is the route for kubernetes *behaviour*. A kubernetes-only *configuration key* that only the state
projects needs neither:

- Prove it at the exact seam `serve` calls — `config.Load` of a kubernetes `legion.yaml` →
  `api.SettingsOf` → `json.Marshal` — in a throwaway probe test inside the package (deleted after
  the run; `jj status` clean), and cite the committed golden the same input regenerates, read back
  through the strict TypeScript schema.
- The negative control is the previous round's code on the identical input: `jj restore --from
  <previous head> <files>` into the working copy, run the probe, record the leak, `jj restore` back.
  A proof that shows only the fixed output cannot be told from a fixture that never held the
  credential.
- Self-test a sweep regex before trusting it: `jq -n '"<planted value>" | test("<regex>")'` must
  print `true`, and the masked form `false`.
- Say in `Not proven / risk` that no booted kubernetes daemon served the key; the tmux real-binary
  proof covers the route, this covers the projection.

## Evidence

sjawhar/legion#2096 (LEGION-791), round 3: `runtime.kubernetes.plugins.pi_envoy` planted as
`https://legion:<password>@plugins.internal.example/pi-envoy-10.4.0.tgz?token=<token>`. With round
2's `settings.go` restored over the same fixture the probe logged the password and the token
verbatim; at the fix head it logged `https://xxxxx@plugins.internal.example/pi-envoy-10.4.0.tgz`.
The tester drove the same seam independently with `go test -overlay` on a file of its own (another
host, an uppercase scheme, a fragment) and read the regenerated `state.json` through
`LegionStateResponse`. The tmux binary at the same head re-served the four address keys masked.
