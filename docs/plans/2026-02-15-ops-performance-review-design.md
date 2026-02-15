# Ops/Performance Review: Path Headers + State JSON Persistence Design

## Goal
Provide low-overhead mitigations for risks around reflecting absolute paths in headers and persisting daemon state JSON, with no new dependencies.

## Current State
- HTTP headers used today are limited to `content-type` (and Linear `Authorization`); no absolute paths appear in headers.
- Absolute paths flow via request bodies (`workspace`) and process env (`LEGION_DIR`), not headers.
- `workers.json` persists only small worker metadata (no workspace paths); writes happen on lifecycle events.

## Constraints
- No new dependencies.
- Minimize overhead (CPU, I/O, log volume).
- Avoid proxy/header limits and cache-key bloat.

## Options Considered
1. **Keep paths out of headers (recommended).** Paths remain in body/env only.
2. **Header carries a fixed-length workspace ID.** Hash of canonical path, no absolute path exposure.
3. **Header carries absolute path with caps + redaction.** Only if contractually required.

## Decision & Prioritized Mitigations
1. **Do not send absolute paths in headers by default.** Avoid proxy limits, cache-key explosion, and log amplification.
2. **If a header is required, send a fixed-length workspace ID** derived from a canonical path (e.g., SHA-256 hex, truncated).
3. **Cap and redact any path reflection** (headers/logs) to prevent oversized headers and noisy logs.
4. **Keep state JSON path-free.** Persist only lifecycle metadata; avoid storing workspace paths unless required for operators.

## Data Flow (Recommended)
- CLI sends `workspace` in the request body only.
- Daemon canonicalizes the path once at ingress (e.g., `path.resolve`) and uses it for `spawnServe(cwd=...)`.
- If a header is required for downstream systems, emit a fixed-length `workspaceId` (not the path) and keep it optional.

## Error Handling
- Reject invalid paths early (control characters, excessive length) with `400`.
- If a header is required and the derived value exceeds a fixed cap, reject with `400` or `413`.

## Testing Strategy
- Unit tests for path validation and workspace ID derivation.
- Regression tests to ensure no absolute-path header is emitted by default.
- Verify state JSON remains unchanged (no workspace paths persisted).

## Success Criteria
- No absolute paths in headers under default configuration.
- No measurable overhead in spawn or persistence paths.
- State JSON remains small and event-driven, with no log bloat.
