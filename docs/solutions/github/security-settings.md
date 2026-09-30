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
| Push protection | blocks a push (git, the web editor or REST) that contains a supported credential pattern | a repository admin |
| Private vulnerability reporting | gives an outside reporter a private *Report a vulnerability* channel | a repository admin |

Push protection is a block with a bypass, not a hard refusal. By default anyone with write access
can push past a block by giving a reason (used in tests, a false positive, or fix it later), and
GitHub records each bypass as a secret-scanning alert and in the audit log, and emails the
repository's watching admins. Delegated bypass is the setting that narrows who may bypass; the
settings script reports its state and never changes it. Non-provider (generic) patterns such as
private keys stay disabled, so the fake PEM in
`packages/daemon/src/daemon/__tests__/config.test.ts` is not blocked.

Only an admin reads or writes `security_and_analysis` and enables private vulnerability
reporting. An agent session's `gh` acts as the owner's GitHub App: it reads
`security_and_analysis` as `null`, GitHub refuses its `PATCH` with 403, and it can read the
private-vulnerability-reporting status but not change it.

`scripts/security-settings.sh` is the recorded form of these settings. It reads the current
state, prints it, and by default prints the exact command it would send; that dry run is the one
copy of the commands, which an admin can also run by hand from a shell where `gh auth status`
shows their own login:

```bash
scripts/security-settings.sh                                    # secret scanning and push protection
scripts/security-settings.sh --private-vulnerability-reporting  # private vulnerability reporting
APPLY_SECURITY_SETTINGS=1 scripts/security-settings.sh          # applies, then refuses unless a readback shows it
scripts/security-settings.sh --help                             # its modes
```

It also warns when CodeQL default setup is configured.

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
install or run records a tool error rather than failing its job. A missing file reads as
report-only for both checks, since the file lands with the workflow and a base from before it has
none. A file that is there fails closed: a check is report-only only where the file sets its flag
to `true`, so a file that is not JSON, not `{"report_only": {…}}`, missing a check's key or holding
a value that is not `true` or `false` leaves each check it fails to set blocking. The `window`
job's step summary and an annotation name each problem. A pull request's or merge group's own copy
is validated and never obeyed: a copy not in its shape fails the `window` job, so a malformed file
cannot reach `main`. The `security` job gathers each run's counts and both flags into the
`security-report` artifact and the step summary. Its last step enforces: once a check's flag is
not `true`, `security` fails whenever that check's job did not succeed, so a ruleset that requires
the `security` check refuses what a promoted check found; while the flag is `true`, `security`
stays `success` whatever the check found. It also fails whenever the `window` job did not succeed,
since then neither scanner job runs, and on a flag the `window` job never produced.

A pull request or merge group reads the flags from its base's copy of the file, not its own; a
push to `main` and the daily run read `main`'s. So a promotion takes effect on `main` from its
merge, the promotion pull request's own run stays report-only, and a pull request that sets a
promoted check back to `true` is still judged by the base's `false`.

The window is 14 days from the workflow's first run on `main`, which `scripts/security-report.sh`
reads one day at a time from the workflow's creation, so GitHub's 1,000-result cap on a filtered
run listing cannot move it as `main`'s runs pile up. The report
prints the window's numbers: `main`'s daily runs, the merged pull requests' new zizmor findings
and whether each was fixed or ignored, tool errors, CodeQL alert counts, secret-scanning alert
counts, and the `Security[<tag>]:` review threads per rubric row. Once the window has closed it
ends in a `DECISION:` block with a line per check, decided on its own; its header carries the
rules. The daily scheduled run runs it, and while any check is still report-only the block makes
that run fail, which is the signal to open the promotion pull request that applies the decision
and sets each promoted check's flag to `false`. A check already set to `false` reads as `already
blocking` and is not decided again. The dependency scanners are promoted only when `main`'s newest
run has none of the findings their Gate fails on, osv findings with a fix and reachable govulncheck
findings; the report names both counts, since promoting a gate that fails on `main` would fail
every pull request.

Rule 4 counts a `Security[<tag>]:` review thread and its `Accepted:` reply only from a
collaborator with write, maintain or admin access, or from a GitHub App bot, and the report names
every thread and reply it ignores. Every Legion reviewer is an installed App, which GitHub reads as
permission `none`, so a permission check alone would count none of them; only an App the owner
installed can comment here, and no workflow runs on `pull_request_target`, so an outsider cannot
make a bot comment.

```bash
scripts/security-report.sh                        # sjawhar/legion, 14-day window
scripts/security-report.sh --decision force       # print the decision block now
```

From a devbox the App reads the Actions runs and artifacts, and the alert rows read `BLOCKED`:
it cannot read code-scanning or secret-scanning alerts. The scheduled run's token reads
code-scanning alerts.
