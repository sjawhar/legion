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

Also check for repo-specific skills that may define domain-specific testing procedures. If the issue domain has a dedicated skill (e.g., frontend, backend, security, performance), invoke it — it may require additional verification steps beyond functional testing.

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

### 2. Spec Compliance Check

Before booting anything, verify the code even attempts to address the spec. Dispatch a spec compliance subagent using the template from `/superpowers/subagent-driven-development` (spec-reviewer-prompt.md):

- **What Was Requested:** the acceptance criteria from the issue
- **What Implementer Claims They Built:** the PR title and body
- **Code to inspect:** the workspace (already checked out)

The subagent reads the actual diff and verifies each acceptance criterion has corresponding code. It checks for:
- Missing requirements (criterion has no code addressing it)
- Extra/unneeded work (code that wasn't requested)
- Misunderstandings (right feature, wrong approach)

**If ✅ spec compliant:** proceed to step 3.

**If ❌ issues found:** include the findings in your test results and **fail immediately** — skip booting the environment. There's no point smoke-testing code that doesn't even attempt to implement the spec.

### 2.5. Critique the Tests

Before running the app, review the **implementer's tests** with a critical eye. Unit tests passing and CI green is necessary but not sufficient. Look for:

- **Excessive mocking**: Are tests mocking out the very thing they should be testing? A test that mocks the database, the API, the filesystem, and the business logic is testing nothing.
- **Circular logic**: Does the test just re-implement the production code and assert they match? That proves consistency, not correctness.
- **Happy-path-only coverage**: Are there tests for error cases, edge cases, and boundary conditions? If every test is `expect(result).toBe(expectedValue)` with no failure scenarios, the test suite is decorative.
- **Missing integration tests**: Are there only unit tests when the feature clearly requires integration testing (e.g., API endpoints, database queries, multi-component flows)?
- **Hardcoded expected values**: Are expected values copied from implementation output rather than derived from the spec? This just locks in whatever the code happens to do, right or wrong.

**Include your test critique in the PR comment.** If the tests are weak, that's a finding — note it as a P2 issue. The implementer should have tests that actually catch regressions, not tests that give a false sense of coverage.

**This does NOT replace running the app.** Test critique tells you whether the safety net is real. Smoke testing (steps 4-5) tells you whether the feature actually works. Both are required.

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

**Long-running commands:** Some verification commands (eval runs, build pipelines, integration suites) exceed the bash tool's default timeout (~120s). If a command is killed by timeout:
- Re-run with a longer `timeout` parameter if supported by your tool
- Or run in tmux: `tmux new-session -d -s test '<command>'`, then poll with `tmux capture-pane -t test -p | tail -5`
- Do NOT treat a timeout as a test failure — it means the command didn't finish, not that it failed

### 5. Execute Acceptance Criteria

Work through each criterion from the testing plan. Use appropriate tools:

- **Playwright / agent-browser** for web UIs (navigate, click, fill forms, verify results)
- **curl / HTTP requests** for APIs (hit endpoints, verify responses)
- **CLI commands** for command-line tools (run commands, verify output)
- **Subprocess execution** for scripts, build tools

For each criterion, capture concrete evidence:
- Screenshots (for UI tests)
- Command output (for CLI/API tests)
- Log excerpts (for backend behavior)

Do NOT accept "it looks like it works" — capture actual artifacts.

**Fail hard, fail fast.** If a criterion fails:
- Mark it ❌ immediately with the specific failure evidence
- Do NOT retry hoping it will pass — one clear failure = FAIL
- Do NOT downgrade a failure to a "minor issue" or "observation"
- Do NOT write "mostly works" or "partially passes" — it either passes or it fails
- If you cannot verify a criterion because you lack access to reference material, external services, or credentials — FAIL the test and explain what's missing. Silent degradation is not acceptable.

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
| [criterion 1] | ✅/❌ | [output/screenshot] |

### Test Quality Critique
- [Are the implementer's tests meaningful or decorative?]
- [Excessive mocking? Circular logic? Happy-path-only?]
- [Missing integration/edge-case coverage?]

### Documentation Feedback
- [Was it easy to understand the feature from docs?]
- [Were setup instructions accurate?]
- [What was missing or confusing?]

### Observations
- [UX issues, error messages, edge cases noticed]" -R $OWNER/$REPO
```

**Linear:**

**Linear** (posts to issue — Linear doesn't have PR-level comments):
```
linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="## Behavioral Test Results

### Summary
[PASS/FAIL] — [N/M] acceptance criteria verified

### Results
| Criterion | Status | Evidence |
|-----------|--------|----------|
| [criterion 1] | ✅/❌ | [output/screenshot] |

### Test Quality Critique
- [Are the implementer's tests meaningful or decorative?]
- [Excessive mocking? Circular logic? Happy-path-only?]
- [Missing integration/edge-case coverage?]

### Documentation Feedback
- [Was it easy to understand the feature from docs?]
- [Were setup instructions accurate?]
- [What was missing or confusing?]

### Observations
- [UX issues, error messages, edge cases noticed]")
```

### 7. Signal Completion

**CRITICAL: The labels are how the controller knows you finished.** If you skip this,
the issue silently stalls. This is the MOST IMPORTANT step.

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
