# Legion Cloud Deployment Plan

**Date:** 2026-04-09
**Status:** Draft

## Context

Legion currently has no cloud deployment for the daemon. Envoy (webhook receivers, NATS, listener) is deployed via Pulumi + Docker over Tailscale to bare Linux machines. The daemon is run manually on a dev machine.

The goal is to deploy Legion into METR's existing AWS stack (Hawk), which uses Pulumi (Python), ECS Fargate, EKS, and Tailscale.

## Current Architecture

```
Hawk AWS Stack (Pulumi/Python, us-west-2):
  - ECS Fargate: Hawk API (2 replicas), Middleman (1 task)
  - EKS: Eval runners, GPU workloads (Karpenter)
  - ALB: Host-based routing, HTTPS
  - Aurora PostgreSQL Serverless v2
  - Tailscale: Private service access
  - Secrets Manager + KMS
  - GitHub Actions CI/CD

Envoy Stack (Pulumi/TS, Tailscale machines):
  - envoy-github (port 9010): GitHub webhook receiver
  - envoy-slack (port 9011): Slack event receiver
  - envoy-listener (port 9020): NATS subscriber + session delivery
  - NATS JetStream: Message bus
```

## Phase 1: Local Validation

Get Legion working locally with the claude-code runtime.

- [x] Install prerequisites (bun, claude, tmux, jj)
- [x] Fix session ID format for claude-code runtime (ses_ -> UUID)
- [x] Fix controller workspace for skill discovery
- [ ] Test full loop: controller picks up issue, dispatches worker, worker completes
- [ ] Validate sentry label routing end-to-end
- [ ] Confirm handoff between phases (plan -> implement -> test -> review)

## Phase 2: Dockerize the Daemon

Create a Docker image for `packages/daemon`.

### Container Contents
- Bun runtime
- OpenCode binary
- jj (Jujutsu) binary
- Legion daemon code (`packages/daemon/`)
- Legion skills (`.opencode/skills/`)
- OpenCode plugin (`packages/opencode-plugin/`)

### Required Secrets (runtime injection)
- `LINEAR_API_TOKEN` or GitHub App credentials
- `ANTHROPIC_API_KEY` (for OpenCode workers)
- `GITHUB_TOKEN` (for PR operations)
- `ENVOY_URL` (for webhook delivery)

### Health Check
- `GET /health` on port 13370 (already exists)
- Returns `{status, uptime, workerCount, runtime}`

### Dockerfile Considerations
- Multi-stage build: install bun + opencode + jj, copy daemon code
- OpenCode needs writable home dir for SQLite session storage
- Workers need git/jj access to clone repos and push branches

## Phase 3: Deploy to ECS Fargate

Add Legion as a Pulumi component in the hawk infra stack.

### ECS Task Definition
- Follow pattern from `infra/hawk/api.py`
- Single task (not replicated — one daemon per Linear team)
- Sizing: 2048 CPU / 4096 MB memory (similar to Middleman)
- CloudWatch Logs (non-blocking)
- Restart policy: always (daemon is long-lived)

### Networking
- Private subnet (no public exposure needed)
- Tailscale sidecar or VPN for Envoy connectivity
- No ALB rule needed — only Envoy listener talks to the daemon
- Security group: allow inbound on 13370 (daemon API) + 13381 (OpenCode serve)

### Secrets Management

All secrets go into **AWS Secrets Manager** (same pattern as Hawk API). The ECS task
definition references them via `secrets` block — IAM role controls access.

| Secret | Consumer | Purpose | Scope |
|--------|----------|---------|-------|
| `LINEAR_API_TOKEN` | Daemon | Team resolution, issue polling | Daemon process |
| `ANTHROPIC_API_KEY` | OpenCode serve | LLM calls for all workers | Serve process (inherited by workers) |
| `GH_TOKEN` or GitHub App private keys | Workers | PR creation, git push, code review | Per-worker (role-based: impl vs review) |
| `SENTRY_AUTH_TOKEN` | Sentry fix workers | Assign issues, post comments, resolve | Serve process (inherited by workers) |
| `GITHUB_APP_ID_IMPL` | Daemon | GitHub App identity for impl role | Daemon process |
| `GITHUB_APP_PRIVATE_KEY_IMPL` | Daemon | GitHub App auth for impl role | Daemon process (file mount or env) |
| `GITHUB_APP_ID_REVIEW` | Daemon | GitHub App identity for review role | Daemon process |
| `GITHUB_APP_PRIVATE_KEY_REVIEW` | Daemon | GitHub App auth for review role | Daemon process (file mount or env) |

**Secret injection architecture:**

```
AWS Secrets Manager
  └── ECS task definition (secrets block)
        └── Daemon process env
              ├── LINEAR_API_TOKEN (daemon uses directly)
              ├── ANTHROPIC_API_KEY (passed to OpenCode serve)
              ├── SENTRY_AUTH_TOKEN (passed to OpenCode serve)
              ├── GitHub App keys (daemon generates per-worker GH_TOKENs)
              └── OpenCode serve process
                    ├── Controller session (inherits env)
                    └── Worker sessions (inherit env + per-worker GH_TOKEN)
```

**Principle:** Secrets flow downward from the ECS task env. The daemon is the trust
boundary — it generates short-lived GitHub App installation tokens per worker and
scrubs ambient credentials from worker environments (see `SCRUBBED_ENV_KEYS` in
`github-apps.ts`). Workers never see the raw GitHub App private keys.

**Sentry MCP OAuth (read operations):** The Sentry MCP server uses OAuth for read
operations (search_issues, get_issue_details). In production, this either needs a
service account OAuth token or falls back to the REST API with `SENTRY_AUTH_TOKEN`.
For the initial deployment, `SENTRY_AUTH_TOKEN` covers both read and write operations.

**Rotation:** GitHub App installation tokens auto-expire (~1 hour). The daemon's
`TokenManager` refreshes them. `LINEAR_API_TOKEN`, `ANTHROPIC_API_KEY`, and
`SENTRY_AUTH_TOKEN` are long-lived and should be rotated via Secrets Manager
rotation policies.

### Storage
- OpenCode uses SQLite for session state
- Options:
  - **EFS mount** — persistent across task restarts (recommended)
  - **Ephemeral** — sessions lost on restart, daemon re-creates them (acceptable for v1)

### Pulumi Component
```
infra/hawk/legion.py  (new)
  - ECS task definition
  - ECS service (desired_count=1)
  - CloudWatch log group
  - IAM role (Secrets Manager read, ECR pull)
  - Security group

infra/hawk/__main__.py  (modify)
  - Add LegionStack instantiation
```

## Phase 4: Connect Envoy

Route Envoy's listener to the Fargate task's OpenCode serve port.

### Option A: Tailscale (recommended)
- Legion Fargate task runs Tailscale sidecar
- Envoy listener already supports tsnet delivery
- Session registry (NATS KV) maps session -> Tailscale hostname + port
- No VPC peering or special networking needed

### Option B: Same VPC
- Move Envoy into hawk's VPC
- Listener reaches OpenCode via private IP + security group rules
- Simpler networking but couples Envoy to hawk's infra

### NATS
- Keep existing NATS cluster on Tailscale machines, OR
- Run NATS as ECS task in hawk VPC (sidecar or separate service)

## Open Questions

1. **Persistent storage**: Does OpenCode need persistent SQLite across task restarts? If yes, EFS mount is required. If sessions can be re-created (daemon already handles this on restart), ephemeral is fine.

2. **Concurrent workers**: How many simultaneous workers? Affects Fargate task sizing. Each OpenCode session uses memory for its context window.

3. **Multi-team**: One Fargate task per Linear team, or one task handling multiple teams? Current architecture assumes one daemon per team.

4. **Cost**: Fargate pricing for a long-running 2048/4096 task is ~$70/month. Spot pricing could reduce this but risks interruption.

5. **Envoy migration**: Should Envoy move into the hawk stack entirely, or keep it on current Tailscale machines? Moving it simplifies networking but adds to the hawk infra surface area.

6. **GitHub App credentials**: Workers need different GitHub identities per mode (impl vs review). Currently handled by daemon's RoleServeManager with per-role OpenCode serves. This needs to work in the container.

## Dependencies

- OpenCode must be installable in a Docker image (check binary distribution)
- jj must be available for Linux amd64 (confirmed: available via GitHub releases)
- Bun Docker images exist officially (`oven/bun`)
