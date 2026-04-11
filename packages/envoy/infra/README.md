# Envoy Fleet Infrastructure (Pulumi)

Declarative fleet deployment for Envoy services via Pulumi + Docker over Tailscale SSH.

## Prerequisites

- [Pulumi CLI](https://www.pulumi.com/docs/install/) installed
- Node.js (Pulumi uses ts-node, not Bun)
- Docker on each target machine
- Tailscale mesh connecting all machines
- SSH access to remote machines via Tailscale

## Quick Start

```bash
cd packages/envoy/infra
npm install
export PULUMI_CONFIG_PASSPHRASE="<your-passphrase>"
pulumi up --stack prod
```

## Secrets Management

Pulumi encrypts secrets in `Pulumi.prod.yaml` using a passphrase. This passphrase **must** be available as an environment variable whenever you run any Pulumi command.

### Setting the Passphrase

```bash
export PULUMI_CONFIG_PASSPHRASE="<your-passphrase>"
```

> **Persist across reboots:** Store the passphrase in your SOPS-encrypted dotfiles (`~/.dotfiles/secrets.env` as `PULUMI_CONFIG_PASSPHRASE=...`) and source it in your shell profile. Without this variable set, `pulumi up` / `pulumi preview` will fail to decrypt secrets.

### After a Server Reboot

1. Re-export the passphrase:
   ```bash
   export PULUMI_CONFIG_PASSPHRASE="<your-passphrase>"
   ```
2. Verify secrets decrypt correctly:
   ```bash
   pulumi config get envoy:githubWebhookSecret --stack prod
   ```
3. Run the deployment:
   ```bash
   pulumi up --stack prod
   ```

### Setting or Rotating Secrets

```bash
# Set a new secret value (Pulumi encrypts it automatically)
pulumi config set --secret envoy:githubWebhookSecret "<value>" --stack prod
pulumi config set --secret envoy:slackSigningSecret "<value>" --stack prod
pulumi config set --secret envoy:ghcrToken "<value>" --stack prod
```

### If the Passphrase Is Lost

If you've lost the passphrase, the encrypted values in `Pulumi.prod.yaml` cannot be recovered. You must:

1. Remove the encrypted secret entries and `encryptionsalt` from `Pulumi.prod.yaml`
2. Set a new passphrase: `export PULUMI_CONFIG_PASSPHRASE="<new-passphrase>"`
3. Retrieve secret values from SOPS and re-set them:
   ```bash
   # Retrieve from SOPS-encrypted dotfiles (from packages/envoy/infra/)
   WEBHOOK=$(../deploy/scripts/read-secret.sh ENVOY_GITHUB_WEBHOOK_SECRET)
   SLACK=$(../deploy/scripts/read-secret.sh ENVOY_SLACK_SIGNING_SECRET)
   GHCR=$(../deploy/scripts/read-secret.sh GHCR_TOKEN)

   # Re-encrypt into Pulumi stack config
   pulumi config set --secret envoy:githubWebhookSecret "$WEBHOOK" --stack prod
   pulumi config set --secret envoy:slackSigningSecret "$SLACK" --stack prod
   pulumi config set --secret envoy:ghcrToken "$GHCR" --stack prod
   ```

The passphrase itself has no external recovery path — if lost, you must choose a new one and re-encrypt all secrets as shown above.

## Current Secrets

| Config Key | SOPS Key | Purpose |
|---|---|---|
| `envoy:githubWebhookSecret` | `ENVOY_GITHUB_WEBHOOK_SECRET` | Validates incoming GitHub webhook payloads |
| `envoy:slackSigningSecret` | `ENVOY_SLACK_SIGNING_SECRET` | Validates incoming Slack event payloads |
| `envoy:ghcrToken` | `GHCR_TOKEN` | Pulls container images from GHCR |
| `envoy:tsnetAuthKey` | `TS_AUTHKEY` | Tailscale auth key for tsnet node registration (optional — only for initial setup) |
## Stack Configuration

Non-secret config in `Pulumi.prod.yaml`:

| Key | Description |
|---|---|
| `envoy:registry` | Container image registry (e.g. `ghcr.io/sjawhar/legion`) |
| `envoy:imageTag` | Envoy image tag to deploy |
| `envoy:natsImage` | NATS image (default: `nats:2.11-alpine`) |
| `envoy:machines` | Array of machine definitions (see below) |

## Machine Configuration

Each machine in the `envoy:machines` array:

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Machine identifier — must match the machine's Tailscale MagicDNS hostname (used for NATS cluster routing) |
| `machineId` | Yes | Machine ID passed to Envoy services as `ENVOY_MACHINE_ID` |
| `sshHost` | No | SSH URI for Docker provider (e.g. `ssh://user@host`). **Omit for the local machine** — Docker uses the local socket instead. |
| `nats.serverName` | No | NATS server name. Omit to skip NATS peer on this machine. |
| `listener.tsnet.hostname` | No | Tailscale hostname for the listener's tsnet node (e.g., `envoy-listener-sami-agents-mx`). When set, enables tsnet: `/v1/*` served over TLS on the tsnet interface, legacy port restricted to `/healthz`. |
| `listener.tsnet.stateDir` | No | Persistent state directory for the tsnet node (must be unique per service per machine, e.g., `/var/lib/envoy-tsnet/listener-sami-agents-mx/`) |
| `receivers.github` | No | Deploy GitHub webhook receiver on this machine |
| `receivers.slack` | No | Deploy Slack webhook receiver on this machine |
| `receivers.ghostwispr` | No | Deploy Ghost Wispr webhook receiver on this machine |

### tsnet (Tailscale) Integration

When `listener.tsnet` is configured on a machine, the listener creates an embedded Tailscale node via [tsnet](https://pkg.go.dev/tailscale.com/tsnet):

- **Security boundary**: `/v1/*` API routes are served exclusively on the tsnet HTTPS interface (port 443). The legacy HTTP port (`:9020`) only serves `/healthz`.
- **Hostname convention**: `envoy-{service}-{machineName}` (e.g., `envoy-listener-sami-agents-mx`)
- **State directory convention**: `/var/lib/envoy-tsnet/{service}-{machineName}/`
- **Docker capabilities**: None required — tsnet uses userspace networking (gVisor netstack)
- **Auth key lifecycle**: Only needed for initial node registration. Once state is persisted (named Docker volume), the node reconnects without it.
- **MagicDNS**: tsnet sidesteps the Docker bridge DNS limitation (see `docs/solutions/envoy/docker-tailscale-networking.md`) because the Tailscale node is embedded in the Go process itself.
### Local Machine (no sshHost)

When `sshHost` is omitted, the Docker provider connects via the local Docker socket. Use this for the machine where `pulumi up` runs (e.g. `sami-agents-mx`).

## TypeScript Configuration

The `tsconfig.json` uses `module: commonjs` and `moduleResolution: node` because Pulumi uses ts-node (Node.js), not Bun. The `ignoreDeprecations: "6.0"` suppresses a TypeScript 5.x deprecation warning for this combination which remains required for ts-node compatibility.

## Commands

```bash
pulumi preview --stack prod   # Dry-run — see what would change
pulumi up --stack prod        # Deploy to all machines
pulumi destroy --stack prod   # Tear down all resources
pulumi stack output           # View stack outputs
```
