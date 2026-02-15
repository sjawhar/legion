# Workspace Path Validation + Persistence Design

## Goal
Analyze operability/performance implications of validating and canonicalizing workspace paths, persisting them to `~/.legion/<team>/workers.json`, and sending the `x-opencode-directory` header. Define minimal mitigations that reduce risk without harming performance.

## Context
- Workspaces are currently passed from CLI → daemon → `spawnServe(cwd=workspace)` with no validation or canonicalization.
- `workers.json` is persisted via atomic write but does not include workspace paths today.
- `x-opencode-directory` is planned to be sent on `POST /session` during `initializeSession` to set session directory.
- Daemon binds to `127.0.0.1` (local trust boundary), but the `/workers` endpoint still accepts arbitrary `workspace` values from callers.

## Requirements & Non-Goals
**Requirements**
- Prevent obvious path misuse (relative paths, traversal via `..`, control character injection).
- Keep worker spawn latency minimal; avoid filesystem I/O unless necessary.
- Preserve operator intent while avoiding persistence breakage on older `workers.json`.

**Non-Goals**
- Full sandboxing or multi-tenant hardening.
- Enforcing a single base directory for all workspaces (optional mitigation only).

## Design Decisions

### 1) Validation & Canonicalization (Boundary)
**Decision:** Normalize via `path.resolve(workspace)` and enforce syntactic safety checks only.

Validation rules (minimal, low-cost):
- Must be absolute after `path.resolve`.
- Must not include control characters (reject `\r`/`\n`, optionally other ASCII control chars).
- Must be within a reasonable length cap to avoid abuse (e.g., 4096).
- No `..` traversal after normalization (implied by `path.resolve` + check of resolved string).

**Rationale:**
- `path.resolve` is CPU-only and negligible in cost.
- Avoid `realpath`/`stat` by default to prevent slow mounts and symlink pinning surprises.
- Existence checks can be limited to cases where the daemon is about to create a workspace directory (optional path).

### 2) Header Flow (`x-opencode-directory`)
**Decision:** Send `x-opencode-directory` only on `POST /session` and use the same resolved path as `spawnServe(cwd=...)`.

**Rationale:**
- This aligns the OpenCode session directory with the actual worker process cwd.
- The daemon does not reflect inbound headers, so the risk is limited to misuse of the `workspace` string.

### 3) Persistence to `workers.json`
**Decision:** Persist the workspace path as optional fields (e.g., `workspaceRaw`, `workspaceResolved`), and never trust persisted values for runtime decisions.

**Rationale:**
- Existing state files lack `workspace`; treating it optional avoids type/restore breakage.
- Persisting both raw and resolved values preserves operator intent and aids debugging.

## Operability & Performance Implications

### Performance
- `path.resolve` is negligible (< microseconds) and safe to run on every spawn.
- `stat`/`realpath` are blocking syscalls and can introduce latency on network/slow disks. Avoid by default.
- Persisting a string field to `workers.json` adds minimal write size overhead.

### Operability
- Canonicalization without `realpath` avoids pinning to symlink targets that may move.
- Optional allowlisting under `legionDir` can reduce risk but may break multi-repo or custom workspaces.
- Older state files will have no workspace value; treating it optional avoids confusion during adoption.

## Minimal Mitigations (Recommended Baseline)
1. **Absolute path + normalization**: `const resolved = path.resolve(workspace)` and require `path.isAbsolute(resolved)`.
2. **Control character rejection**: reject `\r`/`\n` and optionally other ASCII control chars.
3. **Length cap**: reject excessively long strings (e.g., > 4096 chars).
4. **Optional existence check**: only when a workspace is about to be created or when explicitly requested; skip in hot paths.
5. **Persist workspace as optional**: include `workspaceRaw` and `workspaceResolved`, but do not rely on them for runtime safety.

## Alternatives Considered
- **Always `realpath`**: stronger canonicalization but higher I/O and symlink pinning; rejected as default.
- **Allowlist under `legionDir`**: strong isolation, but breaks legitimate workflows (multi-repo, shared dirs). Optional for untrusted daemon usage.

## Testing Strategy
- Unit test validation behavior (absolute/relative, traversal, control chars, length).
- Unit test header use to ensure `x-opencode-directory` matches resolved path.
- Migration test: adopt state without workspace fields and ensure no runtime assumptions.

## Open Questions
- Whether to expose an opt-in allowlist mode for untrusted deployments.
- Whether to include a feature flag for `realpath` if symlink confusion is observed.

## Success Criteria
- Worker spawn uses a safe, normalized workspace path with minimal overhead.
- No regressions in spawn latency or daemon restarts.
- Persisted state remains compatible with older `workers.json` files.
