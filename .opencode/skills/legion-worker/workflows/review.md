# Review Workflow

Deep PR review with line-level comments. Code is already local in the workspace.

**Your job is to protect the codebase.** Be direct, be specific, and do not soften your findings. If the code is wrong, say it's wrong. If a requirement was dropped, that's a CRITICAL issue — not a suggestion. Do not praise the implementer for effort. Focus exclusively on whether the code meets the requirements and is correct.

## Identity

When GitHub Apps are configured, you run as `legion-review[bot]` — a separate identity from
the implementer (`legion-impl[bot]`). This means you CAN use GitHub's native review API.
If Apps are not configured, fall back to PR draft status signaling (legacy).

## Workflow

### 0.5. Load Repo Config

Read repo config from workspace root:

```bash
cat .legion/config.yml 2>/dev/null || true
```

Apply @references/config.md semantics for `review` mode:
- Parse recognized keys
- Merge `phases.review.*` overrides on top of top-level values
- Echo recognized keys/effective values
- Missing/malformed config falls back to defaults

If config sets `skills.required` (or `phases.review.skills.required`), invoke those skills additively with plan handoff `requiredSkills.review`.

### 1. Gather Context

Fetch the issue:

**GitHub:**

```
gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO
```

**Linear:**

```
linear_linear(action="get", id=$LEGION_ISSUE_ID)
```

Extract:
- Original requirements from description
- Implementation plan (from comments)
- Acceptance criteria
- Project-specific skills that define domain-specific quality standards (check `.opencode/skills/` or `.claude/skills/`)

**Verify every acceptance criterion has corresponding implementation.** If an acceptance criterion from the issue is not addressed in the diff, that is a CRITICAL/P1 issue — the implementer dropped a requirement.

**Check for silent shortcuts:** Did the implementer skip hard parts, leave TODOs, hardcode values that should be dynamic, or implement a simpler version of what was asked? These are P1 issues, not suggestions.

Fetch the PR metadata:
```bash
gh pr view "$LEGION_ISSUE_ID" --json title,body,headRefName
```

**The code is already in the workspace** - review locally, no need to fetch diff remotely.

Also read all prior handoffs for full context chain:

```bash
legion handoff read --workspace . 2>/dev/null || echo '{}'
```

If present, implementer's `trickyParts` and `deviations` can highlight areas to review more carefully. This is advisory.

Also check for plan-phase context and apply it explicitly during review:
- If `plan.concerns` exists, verify the implementation addresses each concern or document why a concern is not applicable.
- If `plan.learningsInjected` references solution docs/patterns, verify the implementation follows those patterns (or justify deviations).
- If `plan.workflowRecommendation` includes review-relevant guidance, follow it.

Plan-phase fields are optional/advisory — absence must not block review execution. Continue using issue requirements and implementation evidence.

**Skill loading from plan handoff:** If the plan handoff includes a `requiredSkills.review` array, invoke each listed skill before proceeding to step 2. This replaces the manual skill check above for this run.

If `requiredSkills` is absent or the plan handoff is missing, rely on the manual skill check above as the fallback (current behavior, no regression).

Also check for cross-phase messages:

```bash
legion handoff messages --workspace . 2>/dev/null || echo '[]'
```

Messages may contain warnings, blockers, or context from earlier workers.

### 1.6. Check Automated Bot Review Comments

Before running the review, check whether automated code reviewers (Claude, GitHub Copilot,
Codex, etc.) have already posted review comments on the PR:

```bash
PR_NUMBER=$(gh pr view "$LEGION_ISSUE_ID" --json number --jq '.number' -R $OWNER/$REPO)
BOT_COMMENTS=$(gh api repos/$OWNER/$REPO/pulls/$PR_NUMBER/comments \
  --jq '.[] | select(.user.type == "Bot") | {author: .user.login, file: .path, line: .line, body: .body}')
echo "$BOT_COMMENTS"
```

If bot comments exist:

1. **Note which bot comments have been addressed** by the implementer (check for reply threads
   or code changes at the flagged locations) vs which remain unaddressed.
2. **Classify unaddressed bot findings:** Unaddressed bot comments that identify real bugs,
   security issues, or correctness problems are **P1 issues** — treat them the same as if you
   discovered the bug yourself.
3. **Factor bot findings into your review** rather than duplicating them. If a bot already
   flagged an issue and it's still present, reference the bot's comment in your review finding
   instead of writing a new description from scratch.
4. **Dismiss bot style suggestions** that conflict with project conventions — these are not
   review issues.

### 1.7. Inject Relevant Learnings

Follow the injection algorithm in @references/knowledge-injection.md using these keyword sources:

| Keyword Source | Fallback |
|---------------|----------|
| Issue title | — (always available) |
| Implement handoff `filesChanged[]` | Issue title only |
| Implement handoff `trickyParts[]` | Issue title only |

Extract keywords from all available sources above. Match against `docs/solutions/index.json` to surface patterns, known pitfalls, and review-relevant institutional knowledge for the code under review.

Output the injected learnings visibly in the session before proceeding to the review. If no relevant learnings are found, output "No relevant learnings found." and continue.

**Graceful degradation:** If `docs/solutions/index.json` is missing, invalid, or handoff data is unavailable, skip silently and proceed to step 2.

### 1.5. Protected Files — Do NOT Flag

The `.legion/` directory contains **structured handoff data** committed intentionally as part of the Legion pipeline. Files include: `architect.json`, `plan.json`, `implement.json`, `test.json`, `review.json`, `retro.json`.

- They carry context between pipeline phases (architect → planner → implementer → tester → reviewer)
- They provide human reviewers with visibility into what information was passed between agents
- They are NOT build artifacts, NOT tooling debris, and must NOT be added to `.gitignore`

**Do NOT flag `.legion/` files for removal or suggest gitignoring them.** They are part of the PR deliverable.

### 2. Run Review

Invoke `/ce:review` with the branch name.

Pass the context gathered in step 1. The review skill will:
- Dispatch multiple reviewer agents in parallel
- Analyze the code in the workspace
- Check against requirements
- Identify issues by severity (CRITICAL/P1, IMPORTANT/P2, MINOR/P3)

### 2.5. Check CI Status

Check whether CI is passing on the PR:
```bash
gh pr checks "$LEGION_ISSUE_ID"
```

Include the CI status in your review summary (step 3). If CI is failing, note which
checks are failing and treat it as a P1 issue — the implementer should have fixed this
before opening the PR.

### 3. Post Summary Comment

Post a top-level PR comment with the review summary:

```bash
gh pr comment "$LEGION_ISSUE_ID" --body "## Review Summary

**CRITICAL (P1):** N issues
**IMPORTANT (P2):** N issues
**MINOR (P3):** N suggestions

[Brief verdict: approved to merge / needs changes]

---
[Detailed summary of key findings]

### Evidence
- CI status: [paste \`gh pr checks\` output]
- [For each P1/P2: code snippet showing the issue, or test output demonstrating the problem]"
```

### 4. Post Line-Level Comments

For each finding, post a line-level comment:

```bash
gh api repos/{owner}/{repo}/pulls/{pr_number}/comments \
  --method POST \
  --field body="**[SEVERITY]:** [description]" \
  --field commit_id="$(gh pr view $LEGION_ISSUE_ID --json headRefOid -q .headRefOid)" \
  --field path="[file_path]" \
  --field line=[line_number]
```

Group related issues when they affect the same area.

### 4.5. Write Handoff Data

Write handoff data (non-blocking) — BEFORE setting PR draft status:

First, assess which injected learnings were helpful:

> Review the learnings injected at the start of this phase. For each, assess: did this learning materially influence your work, prevent a mistake, or provide useful context for this phase? List only those canonical paths in `learningsHelpful`. If none were helpful, use an empty array. If no learnings were injected, omit both fields from handoff.

```bash
legion handoff write --phase review --workspace . <<'HANDOFF' 2>/dev/null || true
{
  "critical": 0,
  "important": 2,
  "minor": 3,
  "verdict": "approved",
  "keyFindings": [
    {"severity": "P2", "file": "src/auth.ts", "description": "Missing null check on line 45"},
    {"severity": "P2", "file": "src/session.ts", "description": "Session TTL should be configurable"}
  ],
  "learningsInjected": ["<canonical docs/solutions/ paths injected into this phase>"],
  "learningsHelpful": ["<subset that materially helped>"]
}
HANDOFF
```

Replace the example counts and findings with actual review results:
- `critical`: count of CRITICAL/P1 issues found
- `important`: count of IMPORTANT/P2 issues found
- `minor`: count of MINOR/P3 suggestions found
- `verdict`: "approved" if no CRITICAL issues, "changes_requested" if any CRITICAL issues found
- `keyFindings`: list of `{"severity": "P1"|"P2"|"P3", "file": "path", "description": "..."}`
- `learningsInjected`: Canonical `docs/solutions/` file paths of learnings presented to the worker at the start of the phase (omit if none were injected)
- `learningsHelpful`: Subset of `learningsInjected` that materially helped this phase's output (empty array if none were helpful; omit if no learnings were injected)

You MUST attempt the handoff write before setting PR draft status or signaling completion. The `|| true` ensures CLI failures don't block you, but skipping this step entirely is not acceptable. If the write fails, note it in your PR comment.


### 5. Submit Review

**Order matters:** Submit review BEFORE `worker-done` to avoid race condition with controller.

Every review MUST signal its outcome. Use native GitHub review when running under a separate identity:

```bash
# Check if running as App-based reviewer (LEGION_APP_ROLE set by daemon credential injection)
if [ "$LEGION_APP_ROLE" = "review" ]; then
  # Native review — running as legion-review[bot], separate identity from implementer
  if [[ $CRITICAL_COUNT -gt 0 ]]; then
    gh pr review "$LEGION_ISSUE_ID" --request-changes --body "Changes requested: $CRITICAL_COUNT critical issue(s) found. See review comments." -R $OWNER/$REPO
  else
    gh pr review "$LEGION_ISSUE_ID" --approve --body "Approved. No critical issues found." -R $OWNER/$REPO
  fi
else
  # Legacy fallback — same identity as implementer, can't use review API
  if [[ $CRITICAL_COUNT -gt 0 ]]; then
    gh pr ready "$LEGION_ISSUE_ID" --undo -R $OWNER/$REPO
  else
    gh pr ready "$LEGION_ISSUE_ID" -R $OWNER/$REPO
  fi
fi
```

### 6. Signal Completion

**CRITICAL: The `worker-done` label is how the controller knows you finished.** If you skip this,
the issue silently stalls. This is the MOST IMPORTANT step.

Before adding labels, verify:
1. Summary comment posted (step 3)
2. Line-level comments posted (step 4)
3. Handoff write attempted (step 4.5)
4. PR draft status set (step 5)

If any were skipped, go back and do them.

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --remove-label "worker-active" -R $OWNER/$REPO
# Verify the label was actually applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  echo "WARNING: worker-done label not applied, retrying"
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done"])
```

Then notify the controller via Envoy (best-effort, exactly one notification):
```
envoy_send(target_session="$CONTROLLER_SESSION_ID", message="Worker done: $ISSUE_NUMBER review completed. See PR for review outcome.")
```
If `envoy_send` fails, continue — the label is the source of truth.

## Common Mistakes

| Mistake | Correction |
|---------|------------|
| Approving with "minor" issues that are actually P1 | If it breaks behavior or drops a requirement, it's CRITICAL. Severity inflation is better than severity deflation. |
| Reviewing only code quality, not spec compliance | Check every acceptance criterion against the diff. Missing requirements are the most common failure mode. |
| Softening language ("consider", "might want to", "could improve") | Be direct: "This is wrong because X. Fix it." |
| Passing a PR that has CI failures | CI failures are P1. The implementer's job was to ship with green CI. |
| Not checking for dropped requirements | Cross-reference every acceptance criterion. A missing criterion is a CRITICAL issue. |
| Flagging `.legion/` handoff files for removal | `.legion/` files are intentional pipeline data committed by design. Do NOT suggest removing them or adding `.legion/` to `.gitignore`. |
| Posting review findings without evidence | Include code snippets, CI output, or reproduction steps for every P1/P2 finding. |
| Ignoring automated bot review comments | Check bot comments (`user.type == "Bot"`) before reviewing. Unaddressed bot comments identifying correctness issues are P1 — the implementer should have fixed them. |
