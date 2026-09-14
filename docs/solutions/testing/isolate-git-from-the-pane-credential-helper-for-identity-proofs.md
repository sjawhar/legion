> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "Any git push from a Legion pane redeems the pane's own grant through the gh-shim credential helper: isolate git with GIT_CONFIG_GLOBAL=/dev/null, GIT_CONFIG_NOSYSTEM=1, and GIT_ASKPASS before proving another identity's push"
category: testing
tags:
  - github-apps
  - credential-helper
  - gh-shim
  - git-askpass
  - identity-proof
  - legion-pane
  - negative-control
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#1021"
  - "LEGION-42"
symptoms:
  - "a push made with a token that has no contents permission succeeds anyway from a Legion pane"
  - "a cross-identity proof's commit lands attributed to the pane's own bot, not the identity under test"
---

# Any git push from a Legion pane redeems the pane's own grant; isolate git before proving another identity

## Context

LEGION-42's production-like proof stood up the branch's real Legion API (`startLegionApi` + real
`TokenManager`, real `api.github.com`) and redeemed a **tester** grant and an **implementer**
grant through `/legion/v1/gh-token`, then used each token against the sandbox repository. One
negative control was "the tester's token cannot push". Run 1 reported the tester push as
`exit=0` — the review App had pushed a commit to GitHub, which its permission set makes
impossible.

## What happened

The proof driver ran in the implementer's own Legion pane. Three facts about that pane combined:

1. The pane's **global git config** routes `https://github.com` through
   `credential.helper = !gh auth git-credential` (the box's dotfiles; `git config --global
   --list` shows it).
2. The `gh` first on a pane's `PATH` is the **Legion shim** (`<state_dir>/worker-bin/gh`), which
   execs `legion gh -- "$@"` and redeems **this pane's** grant per call.
3. Git consults the credential helper whenever the URL carries no usable credential — the
   driver's `GIT_ASKPASS` was set, but a configured helper is asked first.

So `git push` with the tester's token in the environment silently pushed as the pane's
implementer grant, whatever token the command meant to use. The "impossible" success was the
harness measuring itself. The contaminated branch was deleted from the sandbox and the run
repeated.

## The isolation

Every git command in a cross-identity proof runs with:

```sh
GIT_CONFIG_GLOBAL=/dev/null   # no ~/.gitconfig: no credential.helper, no includes
GIT_CONFIG_NOSYSTEM=1         # no /etc/gitconfig either
GIT_ASKPASS=<script>          # answers "Username" with x-access-token and "Password" from an env var
GIT_TERMINAL_PROMPT=0         # never fall back to a tty prompt
```

The askpass script reads the token from the child's environment (`LEGION42_TOKEN`), never from
argv or the remote URL, so it appears in no process list and no transcript. The repo-local config
(`git config user.name/email`) still applies — set it to the identity under test so the commit's
author matches the token's App.

With that in place run 2 gave the expected shape:

```
PUSH     implementer git push … exit=0; commit author.login=committer.login=legion-implementer[bot]
NEGATIVE tester git push exit=128 remote: Repository not found. | remote head after: <sha> (unchanged = refused)
NEGATIVE tester POST /git/refs status=403 message="Resource not accessible by integration"
```

Two details of the negative control:

- **Assert on the remote, not the message.** `git ls-remote origin refs/heads/<branch>` after the
  refused push must still equal the pre-push head. The refusal text is evidence; the unchanged
  remote head is proof.
- **The review App's push refusal reads `remote: Repository not found.`**, not the REST API's
  `Resource not accessible by integration`. GitHub hides the repository from a token without
  `contents` over git; the REST text appears only on API writes (`POST /git/refs`,
  `resolveReviewThread`). A control that greps for the REST text on a git push will report a
  false failure.

The tester's independent round (`.legion/test.json`, PR #1021) used the same isolation and
recorded it in its `surface` line; that is the form to copy.

## When this applies

Any proof, from any Legion pane, that exercises a credential other than the pane's own: a
scratch daemon leasing another role's token, a smoke check that a role *cannot* do something, a
comparison of two Apps' behaviour. If the command is `git`, isolate it; if it is `gh`, call the
real binary by absolute path (the pane's `gh` is the shim) or use `curl`/`fetch` with an explicit
`Authorization` header — the proof driver here used `fetch` for every API call for exactly that
reason.

The same hazard runs the other way: a `task` subagent or background job inside a pane inherits
the pane's grant file and `PATH`, so its `git push` or `gh` also acts as the pane
(`../legion/worker-pane-shell-gotchas.md`, credential facts).

## Related

- `../legion/one-role-keyed-table-decides-which-github-app-acts.md` — which App each role acts
  as, both Apps' real permission sets, and why the review App cannot push.
- `../legion/worker-pane-shell-gotchas.md` §4 — the box's global credential helper hanging a rig
  daemon's clone: the same helper, a different failure.
