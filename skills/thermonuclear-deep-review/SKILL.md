---
name: thermonuclear-deep-review
description: Use when asked for a thermonuclear or deep review, or for a security and correctness audit of a branch or PR diff. Covers bugs, breaking changes, security issues, developer-experience regressions and feature-gate leaks.
---

# Thermonuclear Deep Review

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

## Verdict

The first line of the report is the verdict, in exactly this form, because a PR gate reads only that line:

- `Verdict: CHANGES at <sha>` when the report has a Medium or High finding.
- `Verdict: APPROVE at <sha>` when it has none. Minor findings do not block.

Priority: trace each finding end to end first. Minor changes no behavior, hides no error and breaks no gate; any other finding is Medium, or High when the report holds its trace, because developers who get false High findings stop listening.

`<sha>`: `git rev-parse --short=8 HEAD` at the head you reviewed, or the sha your brief names when you have no shell.

## Scope

Audit the checked-out branch. Report issues in code its diff adds or modifies, in the unchanged consumers check 2 finds, and in the PR body's claims (check 6), because the author fixes only this diff and the diff never shows them.

## Checks

1. **Breakage.** Trace each change through its callers and contracts, across packages and modules, because breaks show outside the changed lines.
2. **Consumers.** For each behavior the diff changes, name its consumers and show that each one still works. Behaviors: log level, status code, exception type, metric or field name, payload shape, identity or uniqueness key, timing, retry, default. Tracing callers misses consumers, many outside the diff, the package or the repository:
   - alert and monitor predicates (a monitor on `status:error` stops when a line goes down to WARNING)
   - log-level contracts, structured-log field names, log-derived metric filters
   - status codes and error-type strings in request logs, webhooks and audit records
   - identity or uniqueness keys that a persistence layer arbitrates on
   - dashboards, SLOs, saved queries, downstream parsers

   If you cannot find a consumer, say so, because an assumed-away consumer breaks silently. A change that removes its own alarm is High, because it also removes the signal that would catch it.
3. **Developer experience.** Find changes to how developers run or build the code locally, because no test catches them. Examples: where or how the code reads secrets, new or renamed environment variables, remapped ports or networking, a new script that must run for a feature to keep working. Not a break: a new alternative way to run or build, or a new package-manager dependency, unless it needs a step the README or AGENTS.md does not list, such as a manual install from a website or app store.
4. **Feature leaks.** Trace each feature flag or internal-only check and its callers, because a leak ships a feature to users a gate excludes.
5. **Intended breakage.** A high-risk effect the PR body names and bounds (who it affects, how to undo it) is not a defect, because the author chose it. Report one it does not, or one that removes a safeguard or feature gate.
6. **PR body claims.** Attack each safety or correctness claim in the PR body, because the diff does not show whether a claim in prose is true. Attack these first:
   - Order, not coverage: after a pipeline sanitizes, can any later pass create or recreate what the sanitizer neutralized?
   - An input called safe because another program escapes it depends on that program: rate the finding as if it did not.

## Security Guidelines

For each row whose surface the diff touches, answer with a file:line citation, because an answer without one cannot be checked.

| Tag | If the diff touches… | Answer, with file:line |
| --- | --- | --- |
| `authz` | an authorization or refusal check, or a new route, command, or tool | who may call it, who may not, where the diff enforces that, and what the unauthorized caller gets |
| `secret` | a token, grant, secret file, credential helper, or its lifetime | what widened: who can read it, for how long, in which process |
| `untrusted-input` | a subprocess, argv, path, template, or query built from text an outside party controls (an issue body, a PR comment, a webhook payload, model output) | the boundary that neutralizes it, and whether any later pass can re-create what it neutralized (order, not coverage) |
| `prompt` | a prompt that embeds untrusted text into an agent's instructions | what delimits the untrusted region, and what the agent may do if it obeys that text (OWASP LLM01 prompt injection, LLM06 excessive agency) |
| `supply-chain` | a dependency, lockfile, base image, or GitHub Action | the version, the pin (a digest or a SHA checked against its tag), and the permissions the workflow runs with |
| `sandbox` | a sandbox or pod manifest, a capability, or a network policy | which isolation property changed, and against whom |
| `agent-def` | an agent definition, skill, role prompt, `.omp/` config, or `AGENTS.md` | whether this diff can steer its own reviewers, and, if so, what a reviewer must check by hand |
| `transport` | TLS or certificate verification, a signature, an HMAC, a randomness source, or a read or write that untrusted input sizes with no bound | what changed, the check or bound it relies on, and the ASVS identifier it maps to |

- Cite an OWASP ASVS v5.0.0 identifier where one applies (`v5.0.0-1.2.5` style), so the author can find it.
- A security finding with no stated exploit path is hardening, ranked Minor. One that states an exploit path is its own finding at its real priority, so no exploit is filed as optional.
- Report at most two hardening items, ranked, and put the rest in one paragraph, so they do not bury findings.
- Start each security finding with its row's tag, `Security[<tag>]:`, so readers can count findings by row.
- End the report with one `Security:` line, because the Legion reviewer copies it: each touched row's tag and its answer with file:line, or `Security: no sensitive surface in this diff.` when the diff touches none.

## Findings

- Never report a finding whose open question you can settle by reading code, because the author cannot act on a guess. When you cannot settle it, say what you searched and where.

## PR discussion

Only after your own audit, so others' findings do not steer it, and only with a Medium or High finding on a branch that has a PR:

1. Read the PR/MR's comments and review comments, bots' included, with `gh` or `glab`.
2. Check their findings in the code: a comment is not evidence. Include the valid ones you missed; for one you share, add their evidence if yours lacks it.
3. Mark each finding that came from that discussion, so readers can tell it from yours.
