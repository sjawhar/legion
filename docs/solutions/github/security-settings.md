---
title: "Security settings and the report-only window"
category: github
tags: ["github", "security", "secret-scanning", "codeql", "zizmor"]
date: "2026-09-30"
status: "active"
---

# Security settings and the report-only window

## The repository settings

| Setting | What it does | Who can change it |
| --- | --- | --- |
| Secret scanning | GitHub scans pushed content for credentials and raises alerts | a repository admin |
| Push protection | refuses a push (git or REST) that contains a supported credential pattern, for every pusher | a repository admin |
| Private vulnerability reporting | gives an outside reporter a private *Report a vulnerability* channel | a repository admin |

Push protection is repository-wide with delegated bypass off: there is no bypass list.
Non-provider (generic) patterns such as private keys stay disabled, so the fake PEM in
`packages/daemon/src/daemon/__tests__/config.test.ts` is not refused.

Only an admin reads or writes `security_and_analysis` and enables private vulnerability
reporting. An agent session's `gh` acts as the owner's GitHub App: it reads
`security_and_analysis` as `null`, GitHub refuses its `PATCH` with 403, and it can read the
private-vulnerability-reporting status but not change it.

`scripts/security-settings.sh` is the recorded form of these settings. It reads the current
state, prints it, and by default prints the command it would run:

```bash
scripts/security-settings.sh                                    # secret scanning and push protection
scripts/security-settings.sh --private-vulnerability-reporting  # private vulnerability reporting
APPLY_SECURITY_SETTINGS=1 scripts/security-settings.sh          # applies, then refuses unless a readback shows it
```

The two commands it runs, which an admin can also run by hand from a shell where
`gh auth status` shows their own login:

```bash
gh api --method PATCH repos/sjawhar/legion --input - <<<'{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}' --jq .security_and_analysis
gh api --method PUT repos/sjawhar/legion/private-vulnerability-reporting
```

It also warns when push protection's delegated bypass is on, and when CodeQL default setup is
configured.

## CodeQL

`.github/workflows/codeql.yaml` is CodeQL *advanced* setup, run on pushes to `main`, weekly, and
by hand. It creates no check run on a pull request, so nothing waits on it. Default setup must
stay off: while it is configured GitHub refuses the advanced workflow's uploads.

The alerts carry file locations, which are write-permission data on GitHub; nothing this
repository publishes lists them. The Security workflow's scheduled job reads them as counts by
rule, severity and state.

## The report-only window

`.github/workflows/security.yaml` runs zizmor over the workflows and composite actions, and
osv-scanner and govulncheck over the dependencies, on every pull request, merge group and push to
`main`, and daily. On a pull request or a merge group zizmor is judged only on findings new
against the base (`.github/scripts/zizmor-findings.sh` fingerprints both sides), and those show as
annotations on the changed files. A pull request's base is its merge commit's first parent, which
is `main`'s tip when GitHub built the merge; the event's `pull_request.base.sha` does not follow
`main`, so findings `main` gained since would read as the pull request's. A merge group's base is
its `base_sha`.

`.github/security-window.json` holds one flag per check:
`{"report_only": {"zizmor": true, "dependencies": true}}`. While a check's flag is `true`, its
job's single Gate step (`workflows` for zizmor, `dependencies` for osv-scanner and govulncheck) runs
under `continue-on-error`, so the job concludes `success` and blocks no merge; a tool that fails to
install or run records a tool error rather than failing its job. A check is blocking only where
the file sets its flag to `false`. A missing file, a document that is not an object with a
`report_only` key, a missing key and a value that is not `true` or `false` each read as
report-only, and the `window` job's step summary and an annotation name the problem. A bare
boolean (`{"report_only": true}`, the file's first form) applies to both checks. The `security` job
gathers each run's counts and both flags into the `security-report` artifact and the step summary.

A pull request or merge group reads the flags from its base's copy of the file, not its own; a
push to `main` and the daily run read `main`'s. So a promotion takes effect on `main` from its
merge, the promotion pull request's own run stays report-only, and a pull request that sets a
promoted check back to `true` is still judged by the base's `false`.

The window is 14 days from the workflow's first push run on `main`. `scripts/security-report.sh`
prints the window's numbers: `main`'s daily runs, the merged pull requests' new zizmor findings
and whether each was fixed or ignored, tool errors, CodeQL alert counts, secret-scanning alert
counts, and the `Security[<tag>]:` review threads per rubric row. Once the window has closed it
ends in a `DECISION:` block with a line per check, decided on its own; its header carries the
rules. The daily scheduled run runs it, and while any check is still report-only the block makes
that run fail, which is the signal to open the promotion pull request that applies the decision
and sets each promoted check's flag to `false`. A check already set to `false` reads as `already
blocking` and is not decided again.

```bash
scripts/security-report.sh                        # sjawhar/legion, 14-day window
scripts/security-report.sh --decision force       # print the decision block now
```

From a devbox the App reads the Actions runs and artifacts, and the alert rows read `BLOCKED`:
it cannot read code-scanning or secret-scanning alerts. The scheduled run's token reads
code-scanning alerts.
