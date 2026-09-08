# envoy

Docker-first cross-machine notification routing for AI agent sessions.

## Goals

- Route GitHub, Slack, WhatsApp, and agent-to-agent notifications to the correct opencode session
- Keep all envoy-owned code and infrastructure in this repo
- Run every envoy component inside Docker containers
- Avoid host installs except Docker on remote machines that need it
- Inject secrets only at runtime via the `secrets` wrapper from `~/.dotfiles/shims/secrets`

## Repo boundaries

This repo owns:

- NATS cluster config and container packaging
- Envoy shared code
- Envoy listener / router / webhook receivers
- Compose files, deploy scripts, and Dockerfiles

This repo does **not** silently absorb changes to other maintained software.

If envoy needs changes in `opencode`, `@sjawhar/whatsapp-mcp`, or another maintained repo:

- make that change in the other repo
- keep it as a separate clear jj change/commit
- do **not** bundle it into an existing `sami` octopus merge
- document the dependency here under `docs/external-repos.md`

## Secrets

Secrets are never stored in this repo. Runtime injection uses:

```bash
secrets ENVOY_SLACK_SIGNING_SECRET ENVOY_GITHUB_WEBHOOK_SECRET -- <command>
```

Current secret names:

- `ENVOY_SLACK_SIGNING_SECRET`
- `ENVOY_GITHUB_WEBHOOK_SECRET`

## Layout

- `internal/contracts/generated.go` — generated Go contract output from `packages/contracts/scripts/gen-go.ts`
- `internal/config` — env parsing
- `internal/verify` — GitHub / Slack signature verification
- `cmd/listener` — per-machine listener service
- `cmd/github` — GitHub webhook receiver
- `cmd/slack` — Slack Events receiver
- `docker/github.Dockerfile` — GitHub receiver image
- `docker/slack.Dockerfile` — Slack receiver image
- `docker/listener.Dockerfile` — listener image
- `deploy` — compose files and deploy scripts
- `docs` — architecture, constraints, external repo touchpoints
- `.envoy` — local execution notes and machine-readable work tracker

## Local end-to-end

Run the reusable local acceptance driver from the repository root:

```bash
packages/envoy/scripts/e2e-local.sh
```

It builds `cmd/listener`, starts a throwaway `nats:2.10-alpine` container, and
submits signed public GitHub fixtures plus a three-paragraph direct agent
message. The driver asserts the one-warning response for a never-seen GitHub
repository, rejects an unheld role publish before proving the fake session can
claim and receive that role, validates settled pull-request checks, lifecycle
and comment topics, PR-less workflow behavior, obsolete-topic absence, and
both renderers' direct-message summary/body contract.

Docker downloads `nats:2.10-alpine` automatically on the first run when it is
not already cached.

Evidence remains in `packages/envoy/out/e2e/` after the run:

- `envelopes.jsonl` captures the raw NATS frames.
- `session-prompts.jsonl` captures raw fake-session `prompt_async` bodies.
- `rendered-ts.txt` is every frame rendered by
  `@legion/envoy-client`'s `renderInbound`.
- `rendered-go.txt` is the equivalent Go `Deliverer.Text` output.

Set `E2E_NATS_PORT`, `E2E_PORT`, or `E2E_SESSION_PORT` when the default local
ports are occupied. The driver removes only its literal `envoy-e2e-nats`
container.

## Contract source of truth

The authoritative event contract lives in `packages/contracts/`.
Run `bun run gen:go` in that package before validating or releasing `packages/envoy`.
