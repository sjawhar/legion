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
`main`, and daily. On a pull request zizmor is judged only on findings new against the base
commit (`.github/scripts/zizmor-findings.sh` fingerprints both sides), and those show as
annotations on the changed files.

`.github/security-window.json` holds one flag, `report_only`. While it is `true`, each scanner
job's single Gate step runs under `continue-on-error`, so every job concludes `success` and
nothing blocks a merge; a tool that fails to install or run records a tool error rather than
failing its job. The `security` job gathers each run's counts into the `security-report`
artifact and the step summary.

The window is 14 days from the workflow's first push run on `main`. `scripts/security-report.sh`
prints the window's numbers: `main`'s daily runs, the merged pull requests' new zizmor findings
and whether each was fixed or ignored, tool errors, CodeQL alert counts, secret-scanning alert
counts, and the `Security[<tag>]:` review threads per rubric row. Once the window has closed it
ends in a `DECISION:` block; its header carries the rules. The daily scheduled run runs it, and
while `report_only` is still `true` the block makes that run fail, which is the signal to open
the promotion pull request that applies the decision and sets `report_only` to `false`.

```bash
scripts/security-report.sh                        # sjawhar/legion, 14-day window
scripts/security-report.sh --decision force       # print the decision block now
```

From a devbox the App reads the Actions runs and artifacts, and the alert rows read `BLOCKED`:
it cannot read code-scanning or secret-scanning alerts. The scheduled run's token reads
code-scanning alerts.
