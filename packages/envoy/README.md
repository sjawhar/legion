# envoy

Docker-first cross-machine notification routing for AI agent sessions.

## Goals

- Route GitHub, Slack, WhatsApp, and agent-to-agent notifications to the correct agent session
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

If envoy needs changes in `@sjawhar/whatsapp-mcp` or another maintained repo:

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

The secrets broker (`cmd/broker`) supports these kinds for `agent-secrets secret create --kind` and `secret retag --kind`:

- `github-app`: stores an App id and private key; releases installation tokens for its listed GitHub owners.
- `google-login`: stores an `authorized_user` credential; releases a Google access token, not the refresh token.
- `azure-login`: stores `{"tenant_id":"<tenant>","client_id":"<client>"}`; releases JSON with `tenant_id`, `client_id`, `federated_token` and `expires_at` (RFC3339 UTC, from the token's JWT `exp`).
  Set `BROKER_AZURE_LOGIN_SECRET` to the one agent-secret name allowed to release that token; unset, every Azure release is refused.
  The configured secret must be human tier in the policy snapshot checked for each release, and its grant decision must record a person's approval, never an automatic grant.
  Retagging another secret to `azure-login`, or retagging the configured secret to agent tier, releases no Azure token.
  Each release reads the broker pod's projected ServiceAccount token from `BROKER_AZURE_TOKEN_FILE`, default `/var/run/secrets/azure/tokens/azure-identity-token`.
  Unreadable, malformed or expired tokens refuse the release; token bytes never enter an error or log.
  Mount the token read-only in the broker container alone, with audience `api://AzureADTokenExchange` and a one-hour lifetime.
  Exactly one Entra identity may trust that ServiceAccount subject, since a released token can sign in to every identity that trusts it.

Without a kind, the broker releases the stored value unchanged.

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

It builds `cmd/listener` and a throwaway NATS server of its own
(`internal/testnats/cmd/natsd`), so it needs no container runtime, starts both, and
submits signed public GitHub fixtures plus a three-paragraph direct agent
message to a stand-in session that takes every delivery from its own NATS
consumer. The driver asserts the one-warning response for a never-seen GitHub
repository, refuses a registration naming a port, rejects an unheld role
publish before proving the stand-in session can claim and receive that role,
validates settled pull-request checks, lifecycle and comment topics, PR-less
workflow and Actions-job behavior, obsolete-topic absence, and the
direct-message summary/body contract `renderInbound` prints.

The NATS server is a process of this checkout's own build, stopped on exit with
the listener and the two fake clients.

Evidence remains in `packages/envoy/out/e2e/` after the run:

- `envelopes.jsonl` captures the raw NATS frames.
- `session-frames.jsonl` captures the envelopes the session's own consumer
  delivered to it.
- `rendered-ts.txt` is every frame rendered by
  `@legion/envoy-client`'s `renderInbound`, the one inbound renderer.

The OS chooses the NATS and listener ports, and the run builds each URL from
the port its own process reports. `E2E_NATS_PORT` or `E2E_PORT` sets a port
explicitly: it is used as given, and one already in use fails the run with
`address already in use`. The session opens no port of its own. CI runs the
driver as the `envoy-e2e-local` job of `.github/workflows/ci.yaml`.

## Contract source of truth

The authoritative event contract lives in `packages/contracts/`.
Run `bun run gen:go` in that package before validating or releasing `packages/envoy`.

`.github/workflows/changelog-fold.yaml` folds this package's changelog fragments
(`.changes/unreleased/`) into `CHANGELOG.md` under the `legion-envoy-v` release that first carried
each one.
