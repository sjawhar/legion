---
name: simplify-settled-diff
description: "Simplify settled, recently changed code for clarity, reuse, quality, and efficiency while preserving behavior. Use after implementation and before review."
argument-hint: "[blank to simplify current branch changes, or describe what to simplify]"
---

Simplify recently changed code for clarity, reuse, quality, and efficiency while preserving exact behavior. Prioritize readable, explicit code over compact code — fewer lines is not the goal.

Legion's fork of the MIT-licensed Compound Engineering `ce-simplify-code` skill (LICENSE beside this file), renamed so a session that also installs the upstream pool loads one of the two by name.

## Step 1: Identify scope

Resolve the simplification scope in this order:

1. **User-named scope** is authoritative; do not widen it.
2. **Otherwise, in git**, use the current branch versus its base. Without a usable base, use staged and unstaged changes (`git diff HEAD`).
3. **Outside git or without a diff**, use files the user named or that were edited earlier in the conversation.

If none of the above produces a non-empty scope, stop and ask the user what to simplify rather than guessing, with the host's blocking question tool, matched by capability and not by another host's name. A tool in the current list exists; never call a question tool to ask whether it exists. Where the list names one that is not loaded, load it with the host's tool-discovery primitive. Fall back to numbered options on the user-visible chat surface only where the host lists no such tool or a real call to it errors. Never silently skip the question.

**Preflight.** Every file of the scope is in the pass: documentation, workflow and configuration files as much as code, and a scope that holds no code at all still runs. A pass that drops the documentation drops the files a wrong procedure lives in, which is the failure this rule exists to stop. Read generated, vendored and lockfile content for what it shows and edit none of it. This is never a size gate: explicit small scopes still run, and callers own any size or cost threshold.

Report the review, apply and verification outcomes together; do not open one task per reviewer.

## Step 2: Launch 3 review agents in parallel

Dispatch three generic subagents — code-reuse, code-quality, and efficiency reviewers — via the platform's subagent primitive (`Agent`/`Task` in Claude Code, `spawn_agent` in Codex) where available; otherwise run the reviews inline or serially. For each reviewer, read its prompt asset from this skill's directory and pass the **full file content** as the subagent's prompt, together with the resolved scope (the full diff or file set) so it has complete context:

- `references/personas/code-reuse-reviewer.md`
- `references/personas/code-quality-reviewer.md`
- `references/personas/efficiency-reviewer.md`

Do not paraphrase these rubrics from memory — read each file and pass it verbatim, or the reviewer loses the gating rules that keep the pass behavior-preserving.

**Bounded dispatch.** Launch only as many reviewers as the harness accepts at once, and read a concurrency or active-agent-limit error as backpressure: leave the reviewer queued and retry when a slot frees, rather than record it as a reviewer failure. Where a dispatch fails for a reason that survives correcting the call, run that reviewer's pass inline with the same prompt asset and say so in one line.

**Agent.** In Oh My Pi, dispatch each persona as the bundled reviewer, `task(agent="reviewer")`. Each runs as a job of its own, so `wait` for its result and read the findings from what it returns. Which model that agent runs on is the operator's configuration.

**Permission mode.** Omit the `mode` parameter on the dispatch call so the user's configured permission settings apply.

## Step 3: Fix issues

Proceed only after all three review outcomes are complete, whether returned by subagents or produced inline. Apply worthwhile findings directly; record false positives and low-value findings as skipped without asking the user.

Inspect beyond the resolved scope when needed to evaluate a finding, but edit only that scope and its necessary import/export seams. For a user-named file or directory scope, those seams must also be inside it; skip any fix that would edit outside the mutation boundary.

Each fix must preserve outputs, errors, side effects, and ordering. If that cannot be established, skip it.

**A defect you see is reported, never dropped.** A problem outside what this pass may change — a wrong procedure in a document, a workflow step that cannot work, a check that cannot fail — is written into the summary with its file, its line and what is wrong, and never recorded as out of scope. This pass applies only behaviour-preserving fixes, so the caller makes that fix outside the pass and proves it again. The pass that stays silent is the one that let the defect reach review.

An interface or data shape that existed only in an earlier iteration of the current unshipped scope is not protected behavior once you verify it has no deployed, persisted, public, external, dependent-branch, or in-repo caller outside the resolved scope. Remove that compatibility path only when every required caller update fits the existing mutation boundary; otherwise preserve it.

**Never simplify away a safety check.** Preserve trust-boundary validation, data-loss protection, security checks, and accessibility affordances. Skip any finding that would thin or remove one.

**Honor a plan the caller passed as context.** A plan the brief names as context is not scope: preserve each decision it marks `session-settled:`, including a duplication or a separation it made on purpose.

## Step 4: Verify behavior is preserved

Run this step only when a fix was applied. Run project-wide typecheck and lint. Run tests matched to blast radius: scoped tests for local changes, broader tests for shared or wide-reach changes, and the full suite when the runner cannot scope tests. A pass that applied nothing left the head as it was, so there is nothing to verify: record `no fix applied; head unchanged, no verification run` in the summary and go to Step 5.

Report failures with the check name and relevant output. Fix simplification-caused failures or revert the responsible change; never relax assertions, weaken types, or skip tests.

If no test suite, lint, or typecheck is configured, state that explicitly in the summary; do not silently skip verification.

## Step 5: Summarize

Summarize what was already sound and what improved. Report applied counts by reuse, quality, and efficiency; skipped count; defects reported but not fixed, each with its file and line; and check outcomes. Name the agent (or model) that produced each reviewer's findings, so the reader can see which model reviewed. If nothing changed, say so. Do not use net lines removed as the success metric.
