---
title: "A Docker-free rig: a scratch Go module that renders the real authorization, and go run server@pin"
category: testing
tags:
  - proofs
  - go
  - nats
  - rigs
  - no-docker
date: 2026-10-22
status: active
module: envoy
related_issues:
  - "INFRA-113"
  - "sjawhar/legion#2105"
---

# A Docker-free rig: a scratch Go module that renders the real authorization, and `go run server@pin`

Extends docs/solutions/testing/render-the-artefact-and-run-it-INFRA-31.md.

That page states the rule: call the renderer, never retype it; execute what it produces in
production's shape; execute the failure you claim is handled. This is the Go-shaped way to do it
in an issue pod, where the repository's own harness cannot run, plus what made the result
discriminating rather than merely green.

An issue pod has no container runtime — `which docker podman nerdctl` finds nothing and there is
no `/var/run/docker.sock` — and `packages/envoy/internal/testnats` starts NATS through
testcontainers, so every testnats-backed suite dies with `panic: rootless Docker not found` and a
package-wide run reports nothing at all. That is not a reason to fall back to unit tests.

## The shape

A scratch module outside the workspace, `replace`d into the checkout, so the renderer is the
production renderer:

```
module rig
go 1.27.1
require github.com/sjawhar/envoy/infra v0.0.0
replace github.com/sjawhar/envoy/infra => <checkout>/packages/envoy/infra
replace github.com/sjawhar/envoy       => <checkout>/packages/envoy
```

Its `main` calls the production functions and writes their output to disk — nothing is retyped:

```go
listener, session := core.ListenerPermissions(machineID), core.SessionPermissions()
config, _ := core.Authorization{Users: []core.User{…}, NoAuthUser: "example-sessions"}.Render(nil)
env, _ := core.ListenerConfig{MachineID: machineID, Port: port}.Env([]string{natsURL})
```

The upstream server runs without a container, pinned to the version the deployment runs:

```
go run github.com/nats-io/nats-server/v2@v2.15.1 -c <rendered>.conf -js --store_dir <dir> -p <port> -m <monitorPort>
```

and the binary under test is the real one, started with only the rendered environment:
`env -i PATH=… HOME=… $(cat listener.env) ./listener`. The subscriber is a real client connecting
as the server's own `no_auth_user`, so nothing in the path is a fake.

## What made it discriminating

- **Two independent source-tree parameters.** The script takes the tree whose module renders the
  grant and, separately, the tree the binary is built from. Two changes were suspected of causing
  one symptom; only a rig that can mix them can attribute the fix. Run the cross legs, or say in
  the proof that you did not and what stands in for them.
- **Instrument both sides in the same leg.** The `main`/`main` leg read the client's answer
  (`http=500 time_total=5.003272s {"error":"context deadline exceeded"}`) *and* the server's own
  log (`Publish Violation - Subject "notifications.team.deploy-squad.all"`). Two causes produced
  one user-visible symptom; the client's answer alone attributes nothing.
- **Measure a refusal by effect, not by status.** The rig curls the server's monitoring endpoint
  (`/jsz?streams=1`) for the role-claim bucket's message count before and after the publish loop.
  A 400 proves the handler refused; a count that does not move proves nothing was published.

## The finding that fell out of it

A rig holding a real server answers adversarial questions nobody asked. Publishing
`{"topic":"$KV.envoy_roles.example-role"}` through the real `/v1/messages/publish` at `main`
answered `http=200` and moved that bucket from 0 messages to 1: any holder of the listener's bearer
could write the records that say which session holds a role, and reach `$JS.API.*` the same way,
because the handler passed a caller-supplied subject to a client whose grant covers the listener's
own bookkeeping. That is now refused with
`400 {"error":"topic must begin with notifications.","expected":["topic"]}`.

Generalised: **any handler that accepts a routing key, subject, path or bucket name from a caller
is as privileged as the process behind it.** Against a real server, send it the keys its grant
allows but its contract never meant it to reach, and read the side effect rather than the status.

## Where the rig lives

Under `.legion/<issue>/`, which retro removes, so it never reaches the default branch and nothing
of it survives but this page. If the shape is worth keeping for a repository, promote it to
`scripts/` in its own issue; otherwise quote the skeleton in the learning, as above.
