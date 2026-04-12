# Fix CI: Docker Build Independent of Conventional Commits

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decouple Docker image builds from conventional commit gating so every push to main that changes envoy/contracts/plugin paths produces a fresh multi-arch Docker image.

**Architecture:** The `release-envoy-and-plugin.yaml` workflow currently gates all downstream jobs on a single `should_release` boolean derived from conventional commit scanning. We split this into two concerns: (1) docker build eligibility (always, on workflow trigger) and (2) versioned release eligibility (conventional commits required). The `envoy` job must also be ungated because `docker` depends on it — GitHub Actions skips downstream jobs when `needs` targets are skipped.

**Tech Stack:** GitHub Actions YAML, shell scripting (bash)

**Key Learnings:**
- `docs/solutions/ci/envoy-plugin-release-versioning.md`: Documents the conventional commit bumping pattern and its gotchas. Warns against over-extraction.
- `docs/solutions/envoy/ghcr-docker-multiarch-ci.md`: Documents Docker multi-arch build setup. Notes that docker depends on envoy for `gen:go` validation gating.

**Metis Pre-Analysis Key Findings:**
- Removing only docker's `if:` is insufficient — docker `needs: [version, envoy]` and envoy is also gated on `should_release`. Must ungated envoy too.
- The envoy job serves as a validation gate for contracts (`gen:go`). Running it always is the simplest approach.
- Plugin and release jobs should remain gated on conventional commits.
- Keep the fix local to `release-envoy-and-plugin.yaml` — no composite action extraction or release.yaml refactoring.

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `.github/workflows/release-envoy-and-plugin.yaml` | Modify | Remove `should_release` gate from `envoy` and `docker` jobs |
| `.github/scripts/verify-envoy-release-workflow.sh` | Create | Agent-executable workflow invariant check |

---

## Task 1: Remove `should_release` gate from `envoy` and `docker` jobs — Independent

**Files:**
- Modify: `.github/workflows/release-envoy-and-plugin.yaml:153-156` (envoy job gate)
- Modify: `.github/workflows/release-envoy-and-plugin.yaml:183-186` (docker job gate)

- [ ] **Step 1: Read the current workflow file and identify all job-level `should_release` gates**

Run:
```bash
grep -n 'if: needs.version.outputs.should_release' .github/workflows/release-envoy-and-plugin.yaml
```

Expected output showing 4 job-level gates:
- Line ~102: `if: needs.version.outputs.should_release == 'true'` (plugin job — KEEP)
- Line ~155: `if: needs.version.outputs.should_release == 'true'` (envoy job — REMOVE)
- Line ~185: `if: needs.version.outputs.should_release == 'true'` (docker job — REMOVE)
- Line ~208: `if: needs.version.outputs.should_release == 'true'` (release job — KEEP)

- [ ] **Step 2: Remove the `if:` condition from the `envoy` job**

In `.github/workflows/release-envoy-and-plugin.yaml`, delete the line:
```yaml
    if: needs.version.outputs.should_release == 'true'
```
from the `envoy` job (line ~155). The `envoy` job should keep `needs: version` for dependency ordering but run unconditionally.

Result — the `envoy` job block should look like:
```yaml
  envoy:
    needs: version
    runs-on: ubuntu-24.04
    strategy:
      fail-fast: true
      matrix:
        arch: [amd64, arm64]
    steps:
```

- [ ] **Step 3: Remove the `if:` condition from the `docker` job**

In `.github/workflows/release-envoy-and-plugin.yaml`, delete the line:
```yaml
    if: needs.version.outputs.should_release == 'true'
```
from the `docker` job (line ~185). The `docker` job should keep `needs: [version, envoy]` for dependency ordering but run unconditionally.

Result — the `docker` job block should look like:
```yaml
  docker:
    needs: [version, envoy]
    runs-on: ubuntu-24.04
    steps:
```

- [ ] **Step 4: Verify `plugin` and `release` jobs still have the `should_release` gate**

Run:
```bash
grep -n 'if: needs.version.outputs.should_release' .github/workflows/release-envoy-and-plugin.yaml
```

Expected: Exactly 2 remaining job-level gates:
1. Plugin job gate (`if: needs.version.outputs.should_release == 'true'`)
2. Release job gate (`if: needs.version.outputs.should_release == 'true'`)

The `envoy` and `docker` jobs should NOT appear in the results.

- [ ] **Step 5: Validate YAML syntax**

Run:
```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release-envoy-and-plugin.yaml'))" && echo "YAML valid"
```

Expected: `YAML valid`

- [ ] **Step 6: Do NOT describe or advance yet**

All changes will accumulate in the current working copy (`@`). Task 2 will add more files to this same change before we describe the final commit.

---

## Task 2: Add workflow invariant verification script — Independent

**Files:**
- Create: `.github/scripts/verify-envoy-release-workflow.sh`

- [ ] **Step 1: Create the scripts directory and verification script**

Run:
```bash
mkdir -p .github/scripts
```

Then create `.github/scripts/verify-envoy-release-workflow.sh`:

```bash
#!/usr/bin/env bash
# Verify invariants of release-envoy-and-plugin.yaml
# Agent-executable: run after any workflow modification to catch regressions.
set -euo pipefail

WORKFLOW=".github/workflows/release-envoy-and-plugin.yaml"

if [ ! -f "$WORKFLOW" ]; then
  echo "FAIL: $WORKFLOW not found"
  exit 1
fi

PASS=0
FAIL=0

check() {
  local desc="$1" result="$2"
  if [ "$result" = "true" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    FAIL=$((FAIL + 1))
  fi
}

echo "Checking workflow invariants: $WORKFLOW"
echo

TEXT=$(cat "$WORKFLOW")

# --- Docker job invariants ---
echo "Docker job:"
# Docker tags must include SHA and latest
check "docker tags include github.sha" \
  "$(echo "$TEXT" | grep -q 'ghcr.io/sjawhar/legion/envoy:\${{ github.sha }}' && echo true || echo false)"
check "docker tags include latest" \
  "$(echo "$TEXT" | grep -q 'ghcr.io/sjawhar/legion/envoy:latest' && echo true || echo false)"
# Docker must NOT be gated on should_release
# Extract the docker job block (from "  docker:" to the next top-level job or EOF)
DOCKER_BLOCK=$(echo "$TEXT" | sed -n '/^  docker:/,/^  [a-z]/p' | head -n -1)
check "docker job not gated on should_release" \
  "$(echo "$DOCKER_BLOCK" | grep -q "should_release" && echo false || echo true)"

echo
echo "Envoy job:"
ENVOY_BLOCK=$(echo "$TEXT" | sed -n '/^  envoy:/,/^  [a-z]/p' | head -n -1)
check "envoy job not gated on should_release" \
  "$(echo "$ENVOY_BLOCK" | grep -q "should_release" && echo false || echo true)"

echo
echo "Plugin job:"
PLUGIN_BLOCK=$(echo "$TEXT" | sed -n '/^  plugin:/,/^  [a-z]/p' | head -n -1)
check "plugin job gated on should_release" \
  "$(echo "$PLUGIN_BLOCK" | grep -q "should_release" && echo true || echo false)"

echo
echo "Release job:"
RELEASE_BLOCK=$(echo "$TEXT" | sed -n '/^  release:/,/^  [a-z]/p')
check "release job gated on should_release" \
  "$(echo "$RELEASE_BLOCK" | grep -q "should_release" && echo true || echo false)"

echo
echo "Trigger paths:"
check "trigger includes packages/envoy/**" \
  "$(echo "$TEXT" | grep -q 'packages/envoy/\*\*' && echo true || echo false)"
check "trigger includes packages/contracts/**" \
  "$(echo "$TEXT" | grep -q 'packages/contracts/\*\*' && echo true || echo false)"
check "trigger includes packages/envoy-plugin/**" \
  "$(echo "$TEXT" | grep -q 'packages/envoy-plugin/\*\*' && echo true || echo false)"

echo
echo "Dependencies:"
check "docker depends on envoy" \
  "$(echo "$DOCKER_BLOCK" | grep -q 'needs:.*envoy' && echo true || echo false)"
check "docker depends on version" \
  "$(echo "$DOCKER_BLOCK" | grep -q 'needs:.*version' && echo true || echo false)"

echo
echo "---"
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "PASS: all workflow invariants verified"
```

- [ ] **Step 2: Make the script executable**

Run:
```bash
chmod +x .github/scripts/verify-envoy-release-workflow.sh
```

- [ ] **Step 3: Run the verification script**

Run:
```bash
bash .github/scripts/verify-envoy-release-workflow.sh
```

Expected output:
```
Checking workflow invariants: .github/workflows/release-envoy-and-plugin.yaml

Docker job:
  PASS: docker tags include github.sha
  PASS: docker tags include latest
  PASS: docker job not gated on should_release
...
Results: 12 passed, 0 failed
PASS: all workflow invariants verified
```

- [ ] **Step 4: Do NOT describe or advance yet**

All changes will accumulate in the current working copy (`@`) alongside Task 1's changes. Task 3 will describe the final commit.
---

## Task 3: Final verification and describe — Depends on: Task 1, Task 2

- [ ] **Step 1: Run the verification script one final time**

Run:
```bash
bash .github/scripts/verify-envoy-release-workflow.sh
```

Expected: `PASS: all workflow invariants verified`

- [ ] **Step 2: Verify YAML validity**

Run:
```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release-envoy-and-plugin.yaml'))" && echo "YAML valid"
```

Expected: `YAML valid`

- [ ] **Step 3: Verify job-level `should_release` gate distribution is correct**

Run:
```bash
grep -c 'if: needs.version.outputs.should_release' .github/workflows/release-envoy-and-plugin.yaml
```

Expected: `2` (plugin gate + release gate only)

- [ ] **Step 4: Describe the commit**

All changes from Tasks 1 and 2 are already in the working copy (`@`). Describe it:

```bash
jj describe -m "fix(ci): decouple docker build from conventional commit gating

Remove should_release gate from envoy and docker jobs so Docker
images are built on every push to main that changes envoy paths.
Versioned releases (npm, GitHub release, git tags) still require
conventional commits.

Add workflow invariant verification script for agent-executable
regression checks.

Fixes #444"
```

---

## Testing Plan

### Setup
- No build environment needed — this is a YAML workflow change with a shell verification script.

### Health Check
- Verify the workflow file exists and is valid YAML:
  ```bash
  python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release-envoy-and-plugin.yaml'))" && echo "YAML valid"
  ```
  Expected: `YAML valid`

### Verification Steps

1. **Docker build no longer gated on conventional commits**
   - Action: Run `bash .github/scripts/verify-envoy-release-workflow.sh`
   - Expected: All checks PASS, including "docker job not gated on should_release" and "envoy job not gated on should_release"
   - Tool: Shell script

2. **Versioned releases still gated on conventional commits**
   - Action: Run `grep -c 'if: needs.version.outputs.should_release' .github/workflows/release-envoy-and-plugin.yaml`
   - Expected: `2` (plugin gate + release gate only — no envoy or docker gates)
   - Tool: grep

3. **Docker tags preserved**
   - Action: Run `grep 'ghcr.io/sjawhar/legion/envoy' .github/workflows/release-envoy-and-plugin.yaml`
   - Expected: Both `${{ github.sha }}` and `latest` tags present
   - Tool: grep

4. **All trigger paths present**
   - Action: Run `grep -c 'packages/envoy' .github/workflows/release-envoy-and-plugin.yaml` and verify `packages/envoy-plugin/**` is included
   - Expected: At least 3 matches covering `packages/envoy/**`, `packages/contracts/**`, and `packages/envoy-plugin/**`
   - Tool: grep

5. **Workflow structure intact**
   - Action: Run `python3 -c "import yaml; y=yaml.safe_load(open('.github/workflows/release-envoy-and-plugin.yaml')); jobs=list(y['jobs'].keys()); print(jobs); assert set(jobs) == {'version','plugin','envoy','docker','release'}, f'Unexpected jobs: {jobs}'"` 
   - Expected: `['version', 'plugin', 'envoy', 'docker', 'release']` with assertion passing
   - Tool: Python one-liner

### Tools Needed
- bash (for verification script)
- python3 with PyYAML (for YAML validation)
- grep (for content checks)

### Skills to Invoke
- No project-specific testing skills needed — this is pure CI workflow verification with shell tools.
