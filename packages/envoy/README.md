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
- `ENVOY_TSNET_OAUTH_CLIENT_ID` — Tailscale OAuth client ID (preferred for tsnet auth)
- `ENVOY_TSNET_OAUTH_CLIENT_SECRET` — Tailscale OAuth client secret (preferred for tsnet auth)
- `ENVOY_TSNET_AUTH_KEY` — legacy Tailscale auth key (mutually exclusive with OAuth)

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

## Contract source of truth

The authoritative event contract lives in `packages/contracts/`.
Run `bun run gen:go` in that package before validating or releasing `packages/envoy`.
