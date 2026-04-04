---
title: "Pulumi remote Docker deployment patterns over Tailscale SSH"
category: envoy
tags:
  - pulumi
  - docker
  - tailscale
  - deployment
  - infrastructure-as-code
  - deleteBeforeReplace
  - nats
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "204"
symptoms:
  - "pulumi up fails with port conflict on container replace"
  - "new container can't bind port while old container still running"
  - "Docker Compose volume name doesn't match Pulumi volume name"
---

# Pulumi Remote Docker Deployment Patterns over Tailscale SSH

## Provider-per-Machine Pattern

One `docker.Provider` per target machine, using SSH transport over Tailscale:

```typescript
const provider = new docker.Provider(`docker-${machine.name}`, {
  host: "ssh://user@tailscale-hostname",
});
```

All resources for that machine pass `{ provider }` in resource options.

## deleteBeforeReplace Is Mandatory

Pulumi's default create-before-delete strategy fails for port-mapped or host-networked
containers — the new container can't bind ports that the old one still holds.

Set `deleteBeforeReplace: true` on **every** container:

```typescript
new docker.Container("my-container", { ... }, {
  provider,
  deleteBeforeReplace: true,
});
```

This causes a brief downtime window during replacement. For NATS clusters, this is
acceptable because the cluster maintains quorum as long as a majority of peers are up.

## Version Pinning

Pin `@pulumi/docker` to an exact version (e.g., `4.11.0`, not `^4.11.0`). The SSH
transport implementation has had regressions in minor versions. Test upgrades explicitly.

## Docker Compose Volume Migration

Docker Compose prefixes volume names with the project name (the directory containing the
compose file). For zero-copy migration:

1. Identify the actual volume name: `docker volume ls --filter name=nats`
2. Match it exactly in Pulumi: `new docker.Volume("nats-data", { name: "nats_nats_data" })`

The naming is: `{compose_project}_{volume_name}`. For `compose/nats/peer.compose.yml`
with volume `nats_data`, the project is `nats`, so the volume is `nats_nats_data`.

**Always verify per-machine during preflight** — manual docker-compose invocations may
have used a different project name.

## Config Rendering via uploads

NATS config can be injected via the `uploads` property instead of bind-mounting a
rendered file:

```typescript
uploads: [{
  content: renderNatsConf(serverName, routes),
  file: "/etc/nats/nats.conf",
}]
```

Note: changing `content` triggers container replacement (combined with `deleteBeforeReplace`,
this means a container restart). This is acceptable for rare config changes.

## Rolling Deploys

Use `--target` with `--target-dependents` to deploy one machine at a time:

```bash
pulumi up --target 'urn:pulumi:prod::envoy::pulumi:providers:docker::docker-sami' --target-dependents
```

For NATS clusters, migrate one peer at a time and verify quorum between each step.

## Conditional Image Pulls

Only pull images a machine actually needs — don't pull NATS image on listener-only machines:

```typescript
if (machine.nats) {
  result.nats = new docker.RemoteImage(...);
}
```

Use `keepLocally: true` on all `RemoteImage` resources to prevent image deletion on
`pulumi destroy`.

## Secrets

Use `pulumi.interpolate` for secret values in container env vars. This keeps them marked
as secret in Pulumi state and masked in logs:

```typescript
envs: [
  pulumi.interpolate`ENVOY_GITHUB_WEBHOOK_SECRET=${secrets.githubWebhookSecret}`,
]
```

Store secrets via `pulumi config set --secret envoy:githubWebhookSecret <value>`.
