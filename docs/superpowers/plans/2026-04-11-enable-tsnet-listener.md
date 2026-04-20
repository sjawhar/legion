> **[HISTORICAL]** The Pulumi project this plan modifies (`packages/envoy/infra/`) has been migrated out of this repo to `~/.dotfiles/envoy/`. File-path references below are no longer valid in this repo. Kept for historical reference.

# Enable tsnet for sami-agents-mx Listener — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Secure the publicly accessible listener port 9020 on sami-agents-mx by enabling tsnet, restricting `/v1/*` API routes to Tailscale TLS only.

**Architecture:** Single-file Pulumi config change. PR #313 already implemented tsnet code — the listener reads `listener.tsnet` from `MachineConfig` and conditionally serves `/v1/*` on the tsnet HTTPS interface while restricting legacy port 9020 to `/healthz` only. This plan enables that existing code path via config.

**Tech Stack:** Pulumi (TypeScript/ts-node), Docker over Tailscale SSH

---

## Assumptions

1. Tailscale mesh and ACLs already permit intended callers to reach the listener's new tsnet identity (all machines are already connected via Tailscale SSH for Docker provider transport)
2. Scope is strictly `sami-agents-mx` — other machines (`sami`, `sami-claude`, `ghost-wispr`) are NOT in scope
3. No external clients depend on public `http://78.12.245.82:9020/v1/*` — all legitimate Envoy callers are on the Tailscale mesh
4. The `envoy:tsnetAuthKey` Pulumi secret will either already be set, or the implementer will set it from SOPS before deploying (SOPS key: `TS_AUTHKEY`)

## File Structure

- **Modify:** `packages/envoy/infra/Pulumi.prod.yaml` — add tsnet config to sami-agents-mx listener

No new files. No code changes. No test changes.

---

### Task 1: Edit Pulumi.prod.yaml — enable tsnet for sami-agents-mx — Independent

**Files:**
- Modify: `packages/envoy/infra/Pulumi.prod.yaml:10` (the `listener: {}` line for sami-agents-mx)

- [ ] **Step 1: Edit the listener config for sami-agents-mx**

In `packages/envoy/infra/Pulumi.prod.yaml`, change the `sami-agents-mx` entry's listener from:

```yaml
      listener: {}
```

To:

```yaml
      listener:
        tsnet:
          hostname: envoy-listener-sami-agents-mx
          stateDir: /var/lib/envoy-tsnet/listener-sami-agents-mx
```

**Naming conventions** (from `packages/envoy/infra/README.md`):
- Hostname: `envoy-{service}-{machineName}` → `envoy-listener-sami-agents-mx`
- State dir: `/var/lib/envoy-tsnet/{service}-{machineName}` → `/var/lib/envoy-tsnet/listener-sami-agents-mx`

**CRITICAL:** Only change `sami-agents-mx`. Do NOT touch:
- `sami` (line 19: `listener: {}`)
- `sami-claude` (line 25: `listener: {}`)
- `ghost-wispr` (line 29: `listener: {}`)

The full `sami-agents-mx` entry should now read:

```yaml
    - name: sami-agents-mx
      machineId: sami-agents-mx
      nats:
        serverName: sami-agents-mx
      listener:
        tsnet:
          hostname: envoy-listener-sami-agents-mx
          stateDir: /var/lib/envoy-tsnet/listener-sami-agents-mx
      receivers:
        github: true
        slack: true
```

- [ ] **Step 2: Verify the diff is minimal and correct**

```bash
jj diff --git
```

Expected output: Only `packages/envoy/infra/Pulumi.prod.yaml` changed. The diff should show approximately:
- Removal of `listener: {}` (1 line)
- Addition of 4 lines (`listener:`, `tsnet:`, `hostname:`, `stateDir:`)
- No changes to any other machine entries or any other files

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "security: enable tsnet for sami-agents-mx listener

Closes #426

Enable tsnet in Pulumi.prod.yaml for the sami-agents-mx listener.
When deployed, /v1/* endpoints will be served exclusively on the
tsnet TLS interface (Tailscale peers only), and the legacy HTTP
port 9020 will only serve /healthz.

PR #313 implemented the tsnet code — this activates it via config."
jj new
```

### Task 2: Set tsnet auth key (if not already configured) — Independent

This is an **operational step** — the auth key value comes from SOPS, not from the codebase. The implementer must check whether the secret already exists and set it if missing.

**Files:**
- Modify: `packages/envoy/infra/Pulumi.prod.yaml` (Pulumi adds the encrypted secret line automatically)

- [ ] **Step 1: Check if tsnetAuthKey secret exists**

Run from `packages/envoy/infra/`:
```bash
pulumi config get envoy:tsnetAuthKey --stack prod 2>&1 || echo "NOT_SET"
```

- If a value is returned → the key exists, skip to Task 3.
- If "configuration key 'envoy:tsnetAuthKey' not found" or "NOT_SET" → proceed to Step 2.

- [ ] **Step 2: Set the auth key from SOPS**

```bash
TS_KEY=$(../deploy/scripts/read-secret.sh TS_AUTHKEY)
pulumi config set --secret envoy:tsnetAuthKey "$TS_KEY" --stack prod
```

If `read-secret.sh` does not have `TS_AUTHKEY` or returns empty:
- **ESCALATE**: Post to issue #426: "Blocker: `TS_AUTHKEY` not found in SOPS. Need a Tailscale auth key (reusable, not ephemeral) for initial listener registration on sami-agents-mx."
- Add `user-input-needed` label, remove `worker-active`, exit.

- [ ] **Step 3: Verify the secret decrypts correctly**

```bash
pulumi config get envoy:tsnetAuthKey --stack prod
```

Expected: Returns a non-empty value. If decryption fails, the `PULUMI_CONFIG_PASSPHRASE` environment variable may be missing or wrong.

- [ ] **Step 4: If the auth key was newly set, include the Pulumi.prod.yaml change in the commit**

The `pulumi config set --secret` command modifies `Pulumi.prod.yaml` by adding an encrypted line. If this happened, amend the commit description (which already covers the listener config change):

```bash
jj squash
```

This squashes the auth key addition into the parent commit (which has the listener config change).

---

## Testing Plan

### Setup
- Ensure `PULUMI_CONFIG_PASSPHRASE` is exported (from SOPS: `~/.dotfiles/secrets.env`)
- `cd packages/envoy/infra && npm install`
- Ensure Tailscale is connected (`tailscale status` shows peers)

### Pre-Deploy Verification (RED — prove the vulnerability exists)

Before deploying, confirm the public endpoint is currently exposed:

```bash
curl -s -o /dev/null -w "%{http_code}" http://78.12.245.82:9020/healthz
# Expected: 200

curl -s -o /dev/null -w "%{http_code}" -X POST http://78.12.245.82:9020/v1/messages/publish -d '{"topic":"test.security","message":"probe"}' -H 'Content-Type: application/json'
# Expected: 200 (or other success) — this is the vulnerability
```

### Deploy

```bash
cd packages/envoy/infra
pulumi preview --stack prod --diff
# Verify: changes only to sami-agents-mx listener container + new tsnet volume
# No changes to sami, sami-claude, ghost-wispr, or any receiver containers

pulumi up --stack prod
```

### Health Check
- `curl -sf http://127.0.0.1:9020/healthz` returns 200
- Retry for up to 90s after deploy (container restart + tsnet bootstrap)

### Post-Deploy Verification (GREEN — prove the fix works)

1. **Legacy port security (CRITICAL)**
   - Action: `curl -s -o /dev/null -w "%{http_code}" -X POST http://78.12.245.82:9020/v1/messages/publish -d '{}' -H 'Content-Type: application/json'`
   - Expected: 404 (route no longer registered on legacy port)
   - Tool: curl from non-Tailscale network

2. **Healthz still available on legacy port**
   - Action: `curl -sf http://78.12.245.82:9020/healthz`
   - Expected: 200 OK
   - Tool: curl

3. **tsnet API available to Tailscale peers**
   - Action: `curl -sf https://envoy-listener-sami-agents-mx.<tailnet>.ts.net/v1/messages/publish -X POST -d '{"topic":"test.health","message":"ping"}' -H 'Content-Type: application/json'`
   - Expected: 200 OK (or appropriate success response) from authorized Tailscale peer
   - Tool: curl from Tailscale-connected machine
   - Note: Replace `<tailnet>` with the actual tailnet name

4. **Other machines unaffected**
   - Action: Verify sami, sami-claude, ghost-wispr listeners still serve `/v1/*` on port 9020
   - Expected: No changes — their `listener: {}` config is untouched

### Failure Scenarios

- **Auth key missing**: If `envoy:tsnetAuthKey` is not set and no prior tsnet state exists, the listener will fail to register on the Tailscale network. Check container logs: `docker logs envoy-listener` on sami-agents-mx.
- **ACL mismatch**: If Tailscale ACLs don't permit the new `envoy-listener-sami-agents-mx` node, peers won't be able to reach it. Check `tailscale status` on the machine.

### Tools Needed
- curl for HTTP verification
- Tailscale peer access for tsnet verification
- Pulumi CLI with `PULUMI_CONFIG_PASSPHRASE` for deploy

### Skills to Invoke
- `/envoy` — Envoy infrastructure patterns and deployment conventions

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `envoy` |
| Test | (none — verification is curl-based, no project-specific test skills) |
| Review | (none) |

Workers: invoke these skills at the start of your workflow before beginning work.
If a skill is unavailable in your environment, proceed without it.
