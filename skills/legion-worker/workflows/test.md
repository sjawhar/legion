# Test Workflow

Behavioral verification of implemented features against running infrastructure.

You are a fresh agent with no prior context about the implementation. Your job is to verify that the feature works by exercising it against real running infrastructure — not by reading code or running unit tests.

**Your default stance is skepticism.** Assume the implementation is wrong until you have concrete evidence it's right. A test that doesn't actively try to break things is not a test. Do not soften failures, do not make excuses for the implementer, and do not say "good effort" when something fails. Your job is to protect the user from shipping broken code.

## Rebase First

```bash
jj git fetch
jj rebase -d main
```

Resolve any conflicts before proceeding.

### 0.5. Load Repo Config

Read repo config from workspace root:

```bash
if [ -f .legion/config.yml ]; then cat .legion/config.yml; fi
```

Apply @references/config.md semantics:
- Parse and apply recognized keys for `test` mode
- Merge `phases.test.*` overrides on top of top-level values
- Echo recognized keys/effective values for auditability
- Missing/malformed config falls back to defaults

Test-mode config keys:
- `testing.require_specific_task`
- `testing.require_taiga_evidence`
- `skills.required` (plus `phases.test.skills.required`)

## Workflow

### 1. Fetch Context

**GitHub:**

```
gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO
```

**Linear:**

```
linear_linear(action="get", id=$LEGION_ISSUE_ID)
```

Extract:
- Acceptance criteria from the issue body
- Testing plan from issue comments (look for `## Testing Plan` section posted by planner)
- PR number from issue comments or linked PRs

**Mandatory: Discover and invoke repo-specific skills.**

You MUST list all available skills in the project's skill directories (`.opencode/skills/`, `.claude/skills/`, or equivalent) and invoke any that match the domain of your test. Specifically look for:

- **`smoke-testing`** — Required when changes touch environments, infrastructure, or eval pipeline. Runs the real system on the deployment platform, not just local tests.
- **`testing`** — May define repo-specific test conventions, fixtures, and verification requirements beyond what this generic workflow covers.
- **`pr-screenshots`** — Required for uploading visual evidence. Screenshots saved to local paths (`/tmp/...`) are NOT acceptable in PR comments — upload them and use the resulting URLs.

Do not skip this step. You are a fresh agent with no prior knowledge of repo conventions. These skills contain critical domain-specific requirements that you will miss without reading them. If the testing plan from the planner lists specific skills to invoke, follow that list.

**Skill loading from plan handoff:** Read the plan handoff for pre-identified testing skills:

```bash
legion handoff read --phase plan --workspace . 2>/dev/null || echo '{}'
```

If the plan handoff includes a `requiredSkills.test` array, invoke each listed skill before proceeding. This replaces the repo-specific skill check above for this run.

If `requiredSkills` is absent or the plan handoff is missing, rely on the repo-specific skill check above as the fallback (current behavior, no regression).

If config sets `skills.required`, invoke those skills additively with plan handoff skills.

Then fetch PR metadata and check out the branch:

**GitHub:**

```
gh pr list --search "$LEGION_ISSUE_ID" --json number,title,body,files,headRefName -R $OWNER/$REPO
gh pr view $PR_NUMBER --json title,body,files,headRefName -R $OWNER/$REPO
```

Check out the PR branch so you're testing the actual changes:

```bash
jj new $PR_BRANCH  # Use the headRefName from the PR metadata
```

**If no testing plan is found:** Construct one from the acceptance criteria using this structure:
- Setup: look at README or package.json scripts for how to run the app
- Health check: look for health endpoints or process readiness signals
- Verification: map each acceptance criterion to a concrete action + expected outcome

Note the missing plan in your results — the planner workflow should have produced one.

Also read all prior handoffs for full context chain:

```bash
legion handoff read --workspace . 2>/dev/null || echo '{}'
```

**WARNING: Handoff data may be stale** — from a different issue that previously used this workspace. Verify the handoff's issue ID matches YOUR issue before trusting it. If it references a different issue, IGNORE it and rely on the issue comments and PR.

Use this precedence order when handoff/context sources overlap:
1. Issue body acceptance criteria (primary)
2. Issue comments testing plan (`## Testing Plan`) (primary)
3. Implement handoff (`trickyParts`, `deviations`, `openQuestions`) (implementation context)
4. Plan handoff (`concerns`, `workflowRecommendation`) (advisory, additive)

If present, note implement handoff `trickyParts`, `deviations`, and `openQuestions` to focus your testing accordingly.

If present, use plan handoff `concerns` to shape stress/edge scenarios (for example, if the planner flagged race conditions, include concurrent-path verification).

If present, follow relevant plan handoff `workflowRecommendation` guidance while executing tests.

Plan handoff fields are optional/advisory — absence must not block execution. Continue with issue acceptance criteria and implement handoff context.

Also check for cross-phase messages from earlier workers:

```bash
legion handoff messages --workspace . 2>/dev/null || echo '[]'
```

Messages may contain important context, warnings, or clarifications from the architect, planner, or implementer that aren't captured in the phase handoff data.

### 1.5. Inject Relevant Learnings

Follow the injection algorithm in @references/knowledge-injection.md using these keyword sources:

| Keyword Source | Fallback |
|---------------|----------|
| Issue title | — (always available) |
| Acceptance criteria from issue body | Issue title only |
| Implement handoff `filesChanged[]` | Issue title + acceptance criteria only |

Extract keywords from all available sources above. Match against the assembled index from `docs/solutions/.index/` to surface patterns, testing pitfalls, and known issues relevant to the code under test.

Output the injected learnings visibly in the session before proceeding to spec compliance check. If no relevant learnings are found, output "No relevant learnings found." and continue.

**Graceful degradation:** If `docs/solutions/.index/` is missing, empty, or handoff data is unavailable, skip silently and proceed to step 2.

### 2. Spec Compliance Check

Before booting anything, verify the code even attempts to address the spec. Dispatch a spec compliance subagent using the template from `/superpowers/subagent-driven-development` (spec-reviewer-prompt.md):

- **What Was Requested:** the acceptance criteria from the issue
- **What Implementer Claims They Built:** the PR title and body
- **Code to inspect:** the workspace (already checked out)
- **timeout_seconds: 180** (3 minutes — auto-cancels if the subagent stalls)

The subagent reads the actual diff and verifies each acceptance criterion has corresponding code. It checks for:
- Missing requirements (criterion has no code addressing it)
- Extra/unneeded work (code that wasn't requested)
- Misunderstandings (right feature, wrong approach)

**If ✅ spec compliant:** proceed to step 3.

**If ❌ issues found:** include the findings in your test results and **fail immediately** — skip booting the environment. There's no point smoke-testing code that doesn't even attempt to implement the spec.

**If spec compliance subagent fails or times out (>3 min):** Proceed to step 3 (boot and test). Note "Spec compliance check skipped (subagent timeout)" in your test results. The behavioral testing in subsequent steps will surface spec gaps through actual test failures. **Do NOT stall waiting for the subagent.**

### 2.5. Critique the Tests

Before running the app, review the **implementer's tests** with a critical eye. Unit tests passing and CI green is necessary but not sufficient. Look for:

- **Excessive mocking**: Are tests mocking out the very thing they should be testing? A test that mocks the database, the API, the filesystem, and the business logic is testing nothing.
- **Circular logic**: Does the test just re-implement the production code and assert they match? That proves consistency, not correctness.
- **Happy-path-only coverage**: Are there tests for error cases, edge cases, and boundary conditions? If every test is `expect(result).toBe(expectedValue)` with no failure scenarios, the test suite is decorative.
- **Missing integration tests**: Are there only unit tests when the feature clearly requires integration testing (e.g., API endpoints, database queries, multi-component flows)?
- **Hardcoded expected values**: Are expected values copied from implementation output rather than derived from the spec? This just locks in whatever the code happens to do, right or wrong.

**Include your test critique in the PR comment.** If the tests are weak, that's a finding — note it as a P2 issue. The implementer should have tests that actually catch regressions, not tests that give a false sense of coverage.

**This does NOT replace running the app.** Test critique tells you whether the implementer's safety net is real. Steps 4-5 (booting the app and walking through acceptance criteria as a user) are the actual test. You MUST do both.

### 3. Read the Documentation

Before doing anything else, try to understand the feature from the repo's documentation alone.

- Read the README, usage guides, and any docs the implementer updated in the PR
- Try to understand what the feature does and how to use it from docs only
- Note any gaps, confusion, or missing information

This is intentional — your first experience mirrors a real user's experience. Documentation quality feedback is part of your output.

### 4. Boot the Environment

Follow the testing plan's setup instructions:

1. Run the setup commands from the testing plan
2. Run the health check to verify the environment is ready (retry for up to 30s)
3. If the environment fails to boot, distinguish **transient infrastructure failures** from **real code bugs**:
   - **Transient** (stale containers, port conflicts, timeout, missing dependency cache): clean up and retry once. If it fails again with the same transient error, try a different setup approach (e.g., rebuild, clear cache, restart Docker).
   - **Real bug** (crash with stack trace, config error, missing file from the PR): that is a test failure — skip to step 6 with the boot error as evidence.
   - If unsure: retry once. If the same error recurs, treat it as a real bug.

```bash
# Example — use the actual health check from the testing plan, not this literal URL
for i in $(seq 1 30); do
  curl -s http://localhost:PORT/health && break
  sleep 1
done
```

**Long-running commands (MANDATORY tmux rule):** Any command expected to take >60 seconds (serve commands, eval runs, build pipelines, integration suites) MUST run in a tmux session. Never block your bash session — tool timeouts will kill it and waste the attempt.

```bash
# Run long-running command in tmux (REQUIRED for >60s commands)
tmux new-session -d -s test '<command>'
# Monitor progress
tmux capture-pane -t test -p | tail -20
# Check if still running
tmux has-session -t test 2>/dev/null && echo "running" || echo "done"
```

This is not optional. 5 consecutive smoke test failures were caused by workers running serve commands directly in bash. Use tmux for the serve, normal bash for test commands (curl, CLI invocations, etc.).

- Do NOT treat a timeout as a test failure — it means the command didn't finish, not that it failed

### 5. Execute Acceptance Criteria (This Is the Actual Test)

**Everything before this step is preliminary. This is your real job.** Boot the app, use the feature as a real user would, and walk through every acceptance criterion end-to-end. If you haven't actually launched the application and interacted with the feature, you haven't tested anything.

For each acceptance criterion, simulate the user story: navigate to the relevant screen, perform the action, verify the outcome. Use appropriate tools:

If `testing.require_specific_task=true`, identify the exact blocked task from the issue/plan and include explicit evidence that this exact task was run (not a generic smoke scenario).

- **Playwright / agent-browser** for web UIs (navigate, click, fill forms, verify results)
- **curl / HTTP requests** for APIs (hit endpoints, verify responses)
- **CLI commands** for command-line tools (run commands, verify output)
- **Subprocess execution** for scripts, build tools
For each criterion, capture concrete evidence:
- Screenshots (for UI tests)
- Command output (for CLI/API tests)
- Log excerpts (for backend behavior)

> **EVIDENCE GATE:** Every acceptance criterion in your PR comment MUST include at least one concrete artifact: screenshot, command output, log excerpt, or — for non-behavioral criteria verifiable only by reading code — "Verified by code inspection: [file:line]". A test result without evidence is not a valid test. One artifact may cover multiple related criteria if explicitly noted. Local file paths are NOT evidence — the reviewer cannot access your filesystem.

Do NOT accept "it looks like it works" — capture actual artifacts.

**Fail hard, fail fast.** If a criterion fails:
- Mark it ❌ immediately with the specific failure evidence
- Do NOT retry hoping it will pass — one clear failure = FAIL
- Do NOT downgrade a failure to a "minor issue" or "observation"
- Do NOT write "mostly works" or "partially passes" — it either passes or it fails
- If you cannot verify a criterion because you lack access to reference material, external services, or credentials — FAIL the test and explain what's missing. Silent degradation is not acceptable.

If `testing.require_taiga_evidence=true` and you cannot include a Taiga job URL in results, mark the run as FAIL.

**Domain-specific verification:** If the issue references external specifications, designs, screenshots, or reference material, verify the implementation matches. Functional correctness alone is insufficient — if the issue says "make it look like X" and you can't verify it looks like X, that's a test failure, not a pass with a note.

### 6. Post Results to PR

Post a structured comment on the PR:

**GitHub:**

```
gh pr comment $PR_NUMBER --body "## Behavioral Test Results

### Summary
[PASS/FAIL] — [N/M] acceptance criteria verified

### Results
| Criterion | Status | Evidence |
|-----------|--------|----------|
| [criterion 1] | ✅/❌ | [REQUIRED: screenshot/output/log/code-ref] |

### Test Quality Critique
- [Are the implementer's tests meaningful or decorative?]
- [Excessive mocking? Circular logic? Happy-path-only?]
- [Missing integration/edge-case coverage?]

### Documentation Feedback
- [Was it easy to understand the feature from docs?]
- [Were setup instructions accurate?]
- [What was missing or confusing?]

### Observations
- [UX issues, error messages, edge cases noticed]

### Config Compliance
- require_specific_task: [true/false] — [evidence]
- require_taiga_evidence: [true/false] — [Taiga URL or not required]" -R $OWNER/$REPO
```

### PR Evidence Checklist

Before posting your results comment, verify it meets these evidence standards:

- [ ] **No local file paths** — PR comment contains zero references to `/tmp/`, local filesystem paths, or non-URL screenshot references. All evidence must be accessible to remote reviewers.
- [ ] **Uploaded screenshots** — If visual evidence was captured, screenshots were uploaded via the repo's screenshot/evidence skill (e.g., `/pr-screenshots`) and referenced by durable URL, not local path.
- [ ] **Smoke test / CI URLs** — If smoke tests or deployment-platform runs were executed, include the job URLs in the comment.
- [ ] **Eval pipeline output** — If the repo has an eval pipeline (e.g., `tl run`) and it was exercised, include the command and score output.
- [ ] **Local evidence is supplementary** — Local test output (Playwright, headless Chrome, pytest) supplements but does not replace deployment-platform evidence when the repo has a smoke-testing skill.

If any checklist item is not met, fix the comment before posting. Evidence that reviewers cannot access is not evidence.

### 6.5. Write Handoff Data

Write handoff data for the next phase:

First, assess which injected learnings were helpful:

> Review the learnings injected at the start of this phase. For each, assess: did this learning materially influence your work, prevent a mistake, or provide useful context for this phase? List only those canonical paths in `learningsHelpful`. If none were helpful, use an empty array. If no learnings were injected, omit both fields from handoff.

```bash
legion handoff write --phase test --workspace . <<'HANDOFF'
{
  "passed": <count of criteria that passed>,
  "failed": <count of criteria that failed>,
  "failures": [{"criterion": "...", "evidence": "..."}],
  "documentationFeedback": "<text about doc quality>",
  "observations": ["<edge case or UX issue>"],
  "learningsInjected": ["<canonical docs/solutions/ paths injected into this phase>"],
  "learningsHelpful": ["<subset that materially helped>"]
}
HANDOFF
```

Verify the handoff was written:

```bash
if [ ! -f .legion/test.json ]; then
  echo "FATAL: Handoff write failed — .legion/test.json not created"
  echo "STOP: Do NOT signal worker-done. Diagnose: Is 'legion' CLI in PATH? Is --workspace correct?"
  echo "If write cannot be fixed, note the failure in your exit comment."
fi
```

You **MUST** attempt the handoff write before signaling completion. The CLI will fail loudly if the write encounters an error. If the write fails, diagnose the issue and note it in your PR comment before continuing.

**Linear:**

**Linear** (posts to issue — Linear doesn't have PR-level comments):
```
linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="## Behavioral Test Results

### Summary
[PASS/FAIL] — [N/M] acceptance criteria verified

### Results
| Criterion | Status | Evidence |
|-----------|--------|----------|
| [criterion 1] | ✅/❌ | [REQUIRED: screenshot/output/log/code-ref] |

### Test Quality Critique
- [Are the implementer's tests meaningful or decorative?]
- [Excessive mocking? Circular logic? Happy-path-only?]
- [Missing integration/edge-case coverage?]

### Documentation Feedback
- [Was it easy to understand the feature from docs?]
- [Were setup instructions accurate?]
- [What was missing or confusing?]

### Observations
- [UX issues, error messages, edge cases noticed]

### Config Compliance
- require_specific_task: [true/false] — [evidence]
- require_taiga_evidence: [true/false] — [Taiga URL or not required]")
```

### 7. Signal Completion

**CRITICAL: The labels are how the controller knows you finished.** If you skip this,
the issue silently stalls. This is the MOST IMPORTANT step.


**Pre-signal checklist — verify before adding labels:**
1. PR results posted (step 6)
2. Handoff write attempted (step 6.5)

If either was skipped, go back and do it now.

**If all criteria pass:**

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --add-label "test-passed" --remove-label "worker-active" -R $OWNER/$REPO
# Verify labels applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" --add-label "test-passed" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done", "test-passed"])
```

Then notify the controller via Envoy (best-effort, exactly one notification):
```
envoy_publish(topic="notifications.role.legion-controller", message="Worker done: $ISSUE_NUMBER test passed.")
```

**If any criterion fails:**

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --add-label "test-failed" --remove-label "worker-active" -R $OWNER/$REPO
# Verify labels applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" --add-label "test-failed" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done", "test-failed"])
```

Then notify the controller via Envoy (best-effort, exactly one notification):
```
envoy_publish(topic="notifications.role.legion-controller", message="Worker done: $ISSUE_NUMBER test failed. See PR comments for details.")
```

If `envoy_publish` fails, continue — the label is the source of truth.

## Blocking on User Input

If the testing plan is ambiguous or infrastructure is fundamentally missing (not a bug — e.g., no way to run the app at all), try `/legion-oracle [your question]` first. If still blocked, follow the escalation pattern from SKILL.md: push, post comment, add `user-input-needed`, remove `worker-active`, exit.

Do NOT escalate for test failures — those are expected outcomes, not blockers.

## Common Mistakes

| Mistake | Correction |
|---------|------------|
| Seeing green CI and declaring the feature works | CI passing means the implementer's tests pass. You still need to boot the app and verify the feature yourself. Unit tests are the implementer's safety net, not yours. |
| Skipping documentation review | Always read docs first — you're the first "user" |
| Accepting "it works" without evidence | Capture screenshots, command output, log excerpts |
| Adding `test-passed` AND `test-failed` | Only one — pass or fail, never both |
| Continuing after boot failure | Boot failure is a test failure — report and exit |
| Testing on main instead of the PR branch | Check out the PR branch first |
| Escalating when tests fail | Test failures are expected outcomes, not blockers |
| Booting the app when spec compliance failed | If the code doesn't attempt to implement the spec, fail immediately |
| Giving up after one infrastructure failure | Retry with cleanup. Transient failures (timeouts, stale state) are common — only fail after confirming the error is from the PR code, not the environment |
| Softening failure language ("mostly works", "partially passes", "good effort but...") | Binary pass/fail only. If it fails, say it fails. No consolation prizes. |
| Silently degrading on environment, infrastructure, or access issues | FAIL and explain what's missing. Never build a pass verdict on incomplete verification. |
| Passing with "observations" that are actually failures | If an observation would make a user unhappy, it's a failure, not an observation |
| Skipping test critique because CI is green | Review the actual test code. Heavily mocked tests, circular logic, and happy-path-only suites are P2 findings. |
| Posting results without evidence artifacts | Every criterion needs at least one: screenshot, command output, log, or code reference. Local file paths are not evidence — the reviewer cannot access your filesystem. "It works" is not evidence. |
