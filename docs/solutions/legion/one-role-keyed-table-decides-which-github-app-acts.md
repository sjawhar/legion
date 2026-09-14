> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "One exhaustive role-keyed table decides which GitHub App a Legion role acts as; both Apps are required at config load and proven at boot; the review App's real permission set and what a tester's verdict artifact is"
category: legion
tags:
  - github-apps
  - legion-roles
  - appRoleForLegionRole
  - exhaustive-record
  - config
  - boot-probe
  - checks-api
  - commit-status
  - review-app
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#1021"
  - "LEGION-42"
  - "LEGION-21"
  - "LEGION-22"
  - "LEGION-34"
symptoms:
  - "a tester's `legion gh api …/check-runs` answers 403 `Resource not accessible by integration`"
  - "a tester's commit status or PR comment is attributed to `legion-implementer[bot]`"
  - "`legion start --check-config` prints `Config OK` for a legion.yaml the daemon then refuses at boot"
---

# One exhaustive role-keyed table decides which GitHub App a Legion role acts as

## Context

Legion acts on GitHub through two Apps: `legion-implementer` (the code-writing App) and
`legion-reviewer` (the review App). Until LEGION-42 the daemon held **two** functions that decided
which App a role acts as, and they disagreed:

| function | where | used by | mapping |
| --- | --- | --- | --- |
| `appRoleForLegionRole(role)` | `github-apps.ts` | `/gh-token`, `/git-credential`, the `/worker/started` git-identity lease | `reviewer → review`, every other role → implement |
| `modeToRole(mode)` over `MODE_TO_ROLE` | `github-apps.ts` | the worker catch-up (`catchup.ts`), through its own `WORKER_MODE: Record<LegionRole, string>` | `test/plan/architect/review → review`, `implement/merge → implement` |

The credential routes used the first, so a tester's `legion gh` ran as `legion-implementer[bot]`:
its commit statuses carried the code-writing bot's name and `POST /check-runs` was refused 403
(`checks:write` is a review-App permission). LEGION-21's and LEGION-22's testers both hit it. The
second table — the one with a unit test pinning `test → review` — was the intended mapping, and it
was only ever consulted for catch-up reads.

## The pattern that ended the drift

One table, keyed by the validated role type, exhaustive by construction:

```ts
// github-apps.ts
const APP_ROLE_FOR_LEGION_ROLE: Record<LegionRole, GitHubAppRole> = {
  architect: "review",
  planner: "review",
  implementer: "implement",
  tester: "review",
  reviewer: "review",
  merger: "implement",
};

export function appRoleForLegionRole(role: LegionRole | "controller"): GitHubAppRole {
  return role === "controller" ? "implement" : APP_ROLE_FOR_LEGION_ROLE[role];
}
```

Three properties make this hold:

1. **`Record<LegionRole, …>` is exhaustive.** Adding a `LegionRole` in `@legion/contracts`
   fails to compile until it is placed here. There is no "unknown mode" runtime branch and no
   default, because there is no way to reach the function with an unmapped role.
2. **Callers pass a `LegionRole`, never a mode string.** Every caller already held a validated
   role (`legionRole()` in `api/http.ts`; `parseRoleToken`/`isLegionRole`), so the
   `WORKER_MODE` role→mode→App indirection was pure translation and went away with `MODE_TO_ROLE`,
   `modeToRole`, and `WORKER_MODE`.
3. **No second entry point survives.** The last implicit `"implement"` default — the unused
   `GitHubService.gh(command, appRole = "implement")` — was deleted with the `runner` plumbing
   that existed only for it (`LegionApiDeps.runner`, `RouteContext.runner`, and `api.ts`'s
   `deps.runner ?? defaultRunner` fallback; `defaultRunner` itself stays exported from
   `state/fetch.ts` for the CLI and the daemon's own command runner).

The daemon's own GitHub uses are deliberately **not** role-keyed and are written as the implement
App literal, named as such in `packages/daemon/src/daemon/AGENTS.md` (GitHub Apps): workspace
provisioning (`ProcessManager`'s `provisioningToken` and `/legion/v1/provisioning-credential`),
the CI reads (`createCiStatusFetcher`), and the first of the two boot leases. A literal
`"implement"` anywhere else is the smell this document exists to catch — a role acting should
never pick its own App.

`github-apps.test.ts` pins the whole table (`Object.fromEntries(LEGION_ROLES.map(…))` equals the
expected record) plus the one line the bug was about: `appRoleForLegionRole("tester") === "review".

## Both Apps are required — at config load *and* at boot

Once the root architect (the first role to act) runs as the review App, a deployment with only
one App can do nothing. Two independent gates enforce it, and the order matters:

- **Config load** (`loadGitHubApps`, `config.ts`): a `github_apps` section missing either role
  throws `github_apps.<role> is required`; `resolveDaemonConfig` throws `github_apps is required`
  when neither the file nor a CLI override supplies the section at all, and
  `github_apps.<role> is required` when a CLI override supplies it without both roles (the file
  path never reaches that second check — the loader already refused). The presence check runs
  **before** any role's key is resolved, so a missing App is refused before a
  `private_key_command` or
  `secrets` call could execute — and `legion start --check-config` (`resolveSecrets: false`)
  reports it without running anything. Before LEGION-42 the loader `continue`d past an absent
  role and an absent section resolved to `{}`, so `--check-config` printed `Config OK` for a file
  the daemon then refused at boot.
- **Boot** (`startDaemonLocked`, `index.ts`): `getToken("implement", owner)` then
  `getToken("review", owner)` before state is loaded or anything network-facing opens. The
  loader proves the *sections* exist; this proves each *key mints a token*. A dead key refuses
  startup with `TokenManager`'s error instead of 500ing on that role's first `legion gh`.

`GitHubAppsConfig` stays `Partial<Record<GitHubAppRole, …>>` on purpose: `TokenManager`'s unit
tests build one-App managers, and six test fixtures construct a `DaemonConfig` with
`githubApps: {}` because they never reach GitHub. The invariant lives in the two loaders that
every production path goes through; a fixture that bypasses them and then reaches
`TokenManager.getToken` fails loudly with `role_not_configured: <role>`, never silently.

**When a config contract tightens, consolidate the fixtures first.** `config.test.ts` carried the
implement-only override literal in 36 places; one `BOTH_APPS` constant, one `REVIEW_APP_YAML`
fragment for file fixtures that exercise the implement App's key-source rules, and one
`resolveWithApps()` helper for the ~28 file fixtures that exercise other settings replaced them.
The cli tests got one `baseYaml`/`bothAppsYaml`/`writeYaml`. When `main` then added four more
implement-only fixtures (#1016, see the rebase note in
`conflict-only-rebases-keep-the-diff-auditable.md`), the follow-through was one line each.

## The review App's real permission set, and the tester's verdict artifact

Read from GitHub itself (`GET /app` with each App's JWT, 2026-09-13):

| App | id | permissions |
| --- | --- | --- |
| `legion-implementer` | 3202636 | actions:write, checks:**read**, contents:write, issues:write, metadata:read, organization_projects:write, packages:read, pull_requests:write, repository_projects:write, **statuses:write**, workflows:write |
| `legion-reviewer` | 3202653 | actions:write, **checks:write**, issues:write, metadata:read, packages:read, pull_requests:write |

Consequences a tester or reviewer meets:

- **A tester creates check runs, not commit statuses.** As the review App its `POST
  /repos/{owner}/{repo}/check-runs` answers 201 (`app.slug: legion-reviewer`); its `POST
  /repos/{owner}/{repo}/statuses/{sha}` answers exactly
  `403 {"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/commits/statuses#create-a-commit-status","status":"403"}`
  — the review App has no `statuses` permission. The tester's verdict artifacts are the **check
  run and the PR comment**; the daemon's CI settlement reads check runs; no skill asks any role
  for a commit status. LEGION-42's spec re-scoped its acceptance from "commit status" to "check
  run + comment" on this evidence and recorded the alternative under *Rejected*: asking for the
  statuses permission on the review App was declined because no Legion role posts one and the
  deployment rule is to decide what an engineer can decide. Do not re-propose it without a design
  that needs commit statuses.
- **The implement App cannot create check runs** (`checks:read`) — `POST /check-runs` from an
  implementer or merger grant is the same 403. That was the pre-fix failure, and it is the
  negative control to keep in any App-identity proof.
- **Only the implement App can push.** Over git the review App's refused push reads
  `remote: Repository not found.` — *not* the REST text. The REST/GraphQL form
  (`Resource not accessible by integration`) appears on `POST /git/refs` or
  `resolveReviewThread`. A worker matching on the REST text will never recognize its own refused
  push. The planner, tester, reviewer, and architects therefore commit their handoff locally and
  it rides the implementer's next push; the merger acts as the implement App but pushes nothing.

## Related

- `app-identity-acceptance-needs-an-in-role-probe.md` — when acceptance depends on which App
  acts, the phase running as that App manufactures the case (updated for this mapping).
- `../daemon/external-red-and-phase-ownership.md` §3 — the review App's push/resolve limits in a
  review round.
- `../testing/isolate-git-from-the-pane-credential-helper-for-identity-proofs.md` — how to prove
  a *different* identity's push from inside a Legion pane without the pane's own grant leaking in.
- `conflict-only-rebases-keep-the-diff-auditable.md` — the MERGEABLE-but-red rebase this
  tightened contract forced.
