> **[HISTORICAL]** References to `packages/envoy/infra/` point to code that has been migrated out of this repo to `~/.dotfiles/envoy/`. Kept here for historical reference.

# Implementation Plan: Ghost Wispr Webhook Receiver (#186)

## Status: Near-Complete

Previous workers (architect + implementer) have completed ~95% of this work. This plan documents the full implementation for reference and lists the remaining task(s).

## Assumptions

1. Ghost Wispr webhook push capability exists and sends the documented payload format to a configured URL.
2. The Ghost Wispr machine (`ghost-wispr`, Tailscale IP `100.103.243.89`) already runs the Envoy listener and has SSH access configured.
3. Port 9012 is used for the receiver (following GitHub=9010, Slack=9011).
4. The signing secret is optional — when `ENVOY_GHOSTWISPR_SIGNING_SECRET` env var is empty/unset, signature verification is skipped. Deliberate: Ghost Wispr runs on localhost behind Tailscale.
5. Session IDs from Ghost Wispr are timestamp strings like `20260326041405` — safe for NATS subject segments.

## Already Implemented (Verify Only)

The following tasks are already complete on this branch. The implementer should verify they are correct, not re-implement them.

### Contracts Fix (#268 scope — DONE)
- ✅ `packages/contracts/src/subject.ts`: `ghostWisprSubject(sessionId, kind)` — renamed from `recordingId`
- ✅ `packages/contracts/scripts/gen-go.ts`: keep block uses `sessionId`
- ✅ `packages/envoy/internal/contracts/generated.go`: regenerated with `sessionId`
- ✅ `packages/envoy/internal/contracts/normalize_test.go`: tests use `sessionId` with correct kinds (`session.started`, `session.ended`, `summary.ready`)
- ✅ `packages/contracts/src/envelope.test.ts`: positional args, compatible
- ✅ `packages/envoy/internal/routing/match_test.go`: topic strings, compatible

### Signature Verification (#268 scope — DONE)
- ✅ `packages/envoy/internal/verify/ghostwispr.go`: HMAC SHA256, optional (returns true if no secret)
- ✅ `packages/envoy/internal/verify/ghostwispr_test.go`: covers valid, invalid, empty-secret cases

### Envelope Normalization (#268 scope — DONE)
- ✅ `packages/envoy/internal/contracts/normalize.go`: `GhostWisprEnvelopeInput` + `GhostWisprEnvelope()`
- ✅ `packages/envoy/internal/contracts/normalize_test.go`: tests for all event types, missing session_id, summary JSON

### Webhook Receiver (#268 scope — DONE)
- ✅ `packages/envoy/cmd/ghostwispr/main.go`: port 9012, optional signing, correct unknown event handling (`ghostWisprSkip` returns true for unknown events)
- ✅ `packages/envoy/docker/Dockerfile`: ghostwispr binary built and copied
- ✅ `packages/envoy/deploy/compose/ghostwispr.compose.yml`: compose config created

### Pulumi Infrastructure (#269 scope — DONE)
- ✅ `packages/envoy/infra/machines.ts`: `ghostwispr?: boolean` in `ReceiverConfig`
- ✅ `packages/envoy/infra/services.ts`: `ghostWisprSigningSecret?: pulumi.Output<string>` in `ServiceSecrets`, `createGhostWisprReceiver()` function
- ✅ `packages/envoy/infra/index.ts`: conditional creation wired up, `ghostWisprSigningSecret` via `cfg.getSecret()`
- ✅ `packages/envoy/infra/Pulumi.prod.yaml`: ghost-wispr machine has `receivers: { ghostwispr: true }`

### Envoy Skill Documentation (#269 scope — DONE)
- ✅ `.opencode/skills/envoy/SKILL.md`: Ghost Wispr section with topic format, parameters, examples

## Remaining Tasks

### Task 1: Update `packages/envoy/AGENTS.md` with Ghost Wispr receiver — Independent

**Files:**
- `packages/envoy/AGENTS.md`

**Changes:**

1. Line 9 — add Ghost Wispr to the overview:
```
- ingests Slack/GitHub/Ghost Wispr/agent events
```

2. Line 20 — add `cmd/ghostwispr/main.go` to receiver behavior row:
```
| Receiver behavior      | `cmd/github/main.go`, `cmd/slack/main.go`, `cmd/ghostwispr/main.go` | HTTP ingress, signature verification, publish path |
```

**Verify:**
Visual inspection — no build step needed.

**Commit:** `jj describe -m "docs(envoy): add Ghost Wispr receiver to AGENTS.md" && jj new`

### Task 2: Clean up build artifact — Independent

**Files:**
- `packages/envoy/ghostwispr` (compiled binary, 9.6MB)

**Changes:**
A compiled Go binary was left in the workspace. Either:
- Add `ghostwispr` to `packages/envoy/.gitignore`, OR
- Delete the binary: `rm packages/envoy/ghostwispr`

The binary is blocking jj snapshot (exceeds 1MB max-new-file-size).

**Verify:**
```bash
jj status  # Should not show snapshot warnings
```

**Commit:** Part of the AGENTS.md commit or standalone.

## Testing Plan

### Setup
```bash
# 1. Install dependencies
cd packages/contracts && bun install
cd packages/envoy && go mod download
```

### Health Check
```bash
# Verify Go compiles
cd packages/envoy && go build ./...

# Verify TS compiles
cd packages/contracts && bun run tsc --noEmit

# Verify Pulumi TS compiles
cd packages/envoy/infra && npx tsc --noEmit
```

### Unit Tests
```bash
# 1. TS contract tests
cd packages/contracts && bun test

# 2. Go contract tests (normalize, subject helpers)
cd packages/envoy && go test ./internal/contracts/...

# 3. Go verify tests (signature verification)
cd packages/envoy && go test ./internal/verify/...

# 4. Go routing tests (topic matching)
cd packages/envoy && go test ./internal/routing/...

# 5. All Go tests
cd packages/envoy && go test ./...

# 6. Infra tests
cd packages/envoy/infra && bun test
```

### Lint
```bash
# Go vet
cd packages/envoy && go vet ./...

# gofmt check
cd packages/envoy && gofmt -l .

# Biome
cd packages/contracts && bunx biome check src/
cd packages/envoy/infra && bunx biome check .
```

### Docker Build
```bash
cd packages/envoy && docker build -f docker/Dockerfile -t envoy-test .
docker run --rm envoy-test ls -la /usr/local/bin/envoy-ghostwispr
```

### Integration Smoke Test (requires NATS)
```bash
# 1. Start receiver (no signing)
cd packages/envoy && NATS_URLS=nats://127.0.0.1:4222 go run ./cmd/ghostwispr/

# 2. Health check
curl http://127.0.0.1:9012/healthz

# 3. Valid webhook
curl -X POST http://127.0.0.1:9012/webhook/ghostwispr \
  -H "Content-Type: application/json" \
  -H "X-GhostWispr-Event: summary_ready" \
  -H "X-GhostWispr-Delivery: test-del-1" \
  -d '{"id":19,"event_type":"summary_ready","payload":{"session_id":"20260326041629","status":"completed","title":"Test session","type":"summary_ready","version":1},"created_at":"2026-03-26T04:17:03Z"}'
# Expected: 200 OK, "ok"

# 4. Missing headers
curl -X POST http://127.0.0.1:9012/webhook/ghostwispr \
  -H "Content-Type: application/json" -d '{}'
# Expected: 400, "missing ghostwispr headers"

# 5. Unknown event (should be accepted but not published)
curl -X POST http://127.0.0.1:9012/webhook/ghostwispr \
  -H "Content-Type: application/json" \
  -H "X-GhostWispr-Event: live_transcript" \
  -H "X-GhostWispr-Delivery: test-del-2" \
  -d '{"event_type":"live_transcript","payload":{"session_id":"test"}}'
# Expected: 200 OK, "ok" (with log line, no NATS publish)
```

### Acceptance Criteria Verification
| Criterion | How to verify |
|-----------|---------------|
| `recordingId` → `sessionId` rename | `grep -r "recordingId" packages/contracts/ packages/envoy/internal/contracts/` returns no results |
| Ghost Wispr verify with optional secret | `go test ./internal/verify/...` passes |
| Ghost Wispr envelope normalization | `go test ./internal/contracts/...` passes |
| Receiver builds and runs | `go build ./cmd/ghostwispr/` succeeds |
| Docker image includes binary | `docker build` succeeds |
| Compose file valid | `docker compose -f packages/envoy/deploy/compose/ghostwispr.compose.yml config` |
| Pulumi types check | `npx tsc --noEmit` in infra/ succeeds |
| Pulumi config has ghost-wispr receiver | `grep ghostwispr packages/envoy/infra/Pulumi.prod.yaml` shows `ghostwispr: true` |
| Envoy skill docs updated | Ghost Wispr section present in `.opencode/skills/envoy/SKILL.md` |
| AGENTS.md updated | `grep ghostwispr packages/envoy/AGENTS.md` |
| All existing tests pass | `go test ./...` and `bun test` both green |

### Tools Needed
- **Bun** — TS compilation, test runner
- **Go 1.24+** — Go compilation, testing, vet
- **Docker** — Image build verification
- **curl** — Manual smoke testing (optional)

## Dependency Graph

```
Task 1 (AGENTS.md) — Independent
Task 2 (cleanup binary) — Independent
```

Both tasks are independent and can run in parallel. All other work is already complete.
