---
title: "A pod drives the rendered configuration itself, injects its driver with go test -overlay, and sends production's own Host — and a plan's 'CI only' is a claim to check against docs/solutions"
category: testing
tags:
  - nats
  - live-proof
  - worker-pod
  - go-overlay
  - cloud-config
  - capability-claim
date: 2026-10-22
status: active
module: envoy-infra
related_issues:
  - "INFRA-127"
  - "sjawhar/legion#2108"
---

# A pod drives the rendered configuration itself, and "CI only" is a claim

Extends docs/solutions/testing/prove-a-nats-grant-in-a-docker-less-pod-by-rendering-it-from-the-branchs-own-code-INFRA-117.md.

That page states the capability: a worker pod runs no Docker, so every `internal/testnats`
suite is CI's, but a real `nats-server` installs from the Go proxy
(`GOBIN=/tmp/natsbin go install github.com/nats-io/nats-server/v2@v2.15.1`, the version
`testnats.HubImage` pins) and the real binary runs beside it. Four additions.

- **Drive the rendered configuration, not a hand-built one.** Start the server from the
  `nats-server.conf` the component rendered and the client from the `-e` variables of the
  rendered unit's own `ExecStart` (`gcp.CloudConfigOf`), so the whole component is under test
  rather than a struct the test filled in. A proof that builds the config itself proves the
  binary's behaviour and says nothing about what the deployment will render.
- **Send the request production sends.** A rendered address carries its port
  (`example-hub.example-tailnet.ts.net:9020`), and a `Host` header with a port is not the one a
  test usually types. Drive the real shape; the CI test that types the bare name does not.
- **Inject the driver with `go test -overlay <overlay.json>`.** A throwaway file placed into an
  existing package reaches its unexported surface, and nothing is written into the shared
  workspace: no rig to delete, no `jj status` to clean, no commit to avoid. This is how a mutation
  control runs in a workspace five other roles share.
- **A plan's "no surface can reach this" is a claim like any other.** Search `docs/solutions` for
  the surface before you write it, and cite or check. The claim outranks nobody: it reaches the
  later phases as a settled constraint, and the phase that could disprove it in ten minutes is
  usually reading it as given.

**Choose where the composed value lives by which package the live lane can run.** If the proof can
only run in package X and the value under test is composed in package Y, export the composition
from X and have Y call it. Otherwise the live test re-spells the value as a literal, and a wrong
composition in Y ships while the proof stays green.

## Evidence (INFRA-127, sjawhar/legion#2108)

The plan's decision D7 read that the behavioural proof could run only in CI, because no Legion pod
has a container runtime. The planner wrote it, the architect endorsed it, and the implementer built
the phase on it and repeated it in the pull request body — "no Legion worker machine has a
container runtime, so this proof runs in CI and nowhere else". It was already false and already
written down: the INFRA-117 page above, dated the same day and present at this branch's fork point,
gives the install recipe. Only testcontainers needs the runtime. The tester tested it, installed
the pinned `nats-server` in its own pod, and drove the whole rendered hub; the body's clause was
narrowed to testcontainers in the next round. Three phases took a recorded decision as settled
until someone spent ten minutes on it — that is the part of this entry worth remembering.

What the pod's run reached that the CI test does not: the server started from the rendered
`nats-server.conf`, the listener started from the rendered unit's own `-e` variables, the
`listener API hosts:` boot line read back, and a `Host` of `example-hub.example-tailnet.ts.net:9020`
with its port. Both mutants of the agreement rules were injected with `go test -overlay` into
`core_test`, `gcp_test` and `provider_test`, leaving the workspace untouched.

The composition rule is why `core.TailnetHostName(hostname, tailnetDNSName)` exists at all. CI runs
the `natslive` tag over `./core/` and `./seed/` only, and `gcp`'s `resolveListener` is unexported
and hard-codes port 9020 and `nats://127.0.0.1:4222`, so a core test cannot reach it. With the join
left in `gcp`, the live test could only re-spell the address as a literal and a reversed or dotless
join would have passed the proof and shipped the 421. The hub, the golden's assertion and the live
test now call the one function, and three further hand-written joins in `gcp/dispatch.go` and
`gcp/dispatch_kube.go` were moved onto it in the same change — hand composition is the drift that
produced this bug.
