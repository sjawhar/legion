---
title: Troubleshooting
description: The failures Legion reports, what each means, and the command that shows it.
sidebar:
  order: 5
---

Legion refuses loudly rather than carrying on in a state it cannot vouch for: a refusal names what
is wrong and, usually, what to do. This page collects the ones an operator or a user meets, grouped
by where they show up. The commands assume `legion.yaml` and the operator token are in the current
directory; `<KEY>` is an issue key such as `WIDGETS-12`.

## The daemon will not start

**Run the configuration check first.** It makes every refusal the boot would make from the file,
the environment and the files the configuration names, and starts nothing:

```sh
legion start --config legion.yaml --check-config
```

| It says | What to do |
| --- | --- |
| `runtime.kubernetes.image must be pinned by digest (@sha256:…)` | Pin the image by digest, never a tag ([The worker image](/legion/legion/running-legion/#the-worker-image)). |
| `bind 127.0.0.1 is not an address a pod can reach, …` (or the same for `daemon_url`, `envoy_url`, `dispatch_url`, a `nats_urls` entry, or `advertise_host`) | Every address handed to a pod must be one a pod reaches: the daemon host's own address, never loopback or `0.0.0.0` — except `bind`, which may be `0.0.0.0` once `advertise_host` names the stable address pods dial instead. |
| `bind 127.0.0.1 is loopback, where no pod reaches the worker stream, whatever advertise_host names; …` | With `advertise_host` set, `bind` is where the daemon listens: `0.0.0.0`, or the daemon host's own address. |
| `advertise_host must be an IP address or a DNS name, …` or `advertise_host is not used when runtime is tmux: …` | `advertise_host` is the host alone (no scheme, port, path or brackets), and only `runtime: kubernetes` reads it. |
| `<key> is required when runtime is kubernetes: …` | Add the key; the message says why a pod needs it. |
| `projects must configure <PROJECT>, the daemon's own project` | `project` must also be a key of `projects`. |
| `postgres_dsn is required (or set LEGION_POSTGRES_DSN)` | Give the daemon its database. |
| `unknown key <key>` | A typo, or a setting Legion no longer has; the message says which when it knows. |
| `omp_invocation is not used when runtime is kubernetes: …` | Remove it: every pod runs the worker image's Oh My Pi. |
| `<PROJECT> is already running (pid <n>)` (from `legion start` itself) | A daemon for this project is already registered on the machine: `legion status <PROJECT>`, `legion legions`. |
| `capability <name> is open: <detail>; to record a decision, add to legion.yaml: capabilities.decided.<name>: "<reason>"` (after the `Config OK` line, exit 0) | A report, not a refusal: the deployment leaves a worker capability open. The check names the rows the file alone decides — `resource-limits` while a role lacks CPU and memory in both requests and limits, `secrets` while no broker is configured — and the daemon logs the same line at boot for every open row, `model-fallback` included when the probe read `retry.modelFallback` false under your overlay. Close the gap, or add the `capabilities.decided.<name>: "<reason>"` line it prints to record your decision. `legion state --json` lists every row under `capabilities`, and the controller's daily report names each open one. |

**At boot, after the check passes**, the daemon checks the cluster and the image:

- **Agent Sandbox is missing.** The refusal names `the Sandbox CRD sandboxes.agents.x-k8s.io` or
  `the Agent Sandbox controller Deployment agent-sandbox-system/agent-sandbox-controller`, and
  whether the API answered 404 or 403. A 403 means the daemon's identity lacks the get the
  [RBAC list](/legion/legion/running-legion/#the-cluster) names.

  ```sh
  kubectl get crd sandboxes.agents.x-k8s.io
  kubectl -n agent-sandbox-system get deployment agent-sandbox-controller
  ```

- **The image probe refuses.** The daemon runs the worker image's launch probes in a probe Sandbox,
  `legion-probe-<project>-<digest prefix>`, and quotes the probe's log. The causes it names:
  - the image's plugin speaks another daemon contract than this binary: run the `legion` binary
    from the same commit as the image ([Upgrade](/legion/legion/running-legion/#upgrade));
  - an agent's model does not resolve (`task agent <name> … role <role> is not configured`), or its
    key does not work: name the role in your overlay's `modelRoles`, re-run `apply.sh`, and check
    the credential your `models.yml` reads;
  - the probe pod cannot mount the providers Secret (`… cannot mount the providers Secret
    legion-<project>-providers …`): create the Secret with every key `provider_keys` names;
  - the image names another NATS user than the daemon's seed: put the agents' seed in the providers
    Secret's `NATS_NKEY_SEED` key;
  - `capability <name> is missing: <detail>` in the quoted log: the image lacks a tool the capability
    check looks for (`chromium`, a language server, `go`, …), or an operator pod env such as
    `PUPPETEER_EXECUTABLE_PATH` names one that does not run. Rebuild the image from a commit whose
    Dockerfile carries it, or fix the variable; the worker-image workflow fails the same way on an
    image that lacks one;
  - `Succeeded without checking the capability list (its legion CLI predates the check)`: the image's
    `legion` is older than this daemon's capability check. Build the image from this daemon's
    commit.

  A model key that fails at boot refuses the boot even when the cause is a passing network blip,
  since the probe cannot tell the two apart. Start the daemon again once the cause is gone.

## The daemon stops

On SIGTERM (`legion stop`, a rollout, a node drain) the daemon logs `legion daemon stopping`, with
`deciding` naming each claim whose decision is in flight and its event. It cuts short every
relaunch and suspension it started itself and, once it has recorded its boot, the boot steps still
running. A request in flight from the operator or an agent gets up to 8 seconds to finish, so a
suspension or stop whose agent is already exiting is recorded. An operator request that would
change a claim and that the daemon accepted before the stop began, but had not started deciding,
is answered `503` (`<request> refused: the daemon is stopping; ask again once it is back`); one
sent once the stop has begun gets the same 503, or finds no daemon listening once the daemon has
closed its API listener. Repeat either once the daemon is back. It logs `legion daemon stopped`
once it has recorded the boot's end. It waits at most 10 seconds for its own work before it records
that, so it stops well inside a pod's termination grace. It ends no agent: the next boot re-adopts
every pod or pane still running and relaunches each launch the stop cut short.

A signal that comes before the boot is recorded ends the boot step under way only when that step
is the plugin gate, which runs first, the GitHub App token mint, the image probe, or the NATS and
Dispatch readiness checks; the daemon then stops without recording a boot. The cluster check,
opening and migrating the store, building the runtime and recording the boot are not cut short:
each runs to its end, with at most 30 seconds between one of the steps that end at the signal and
the next, and the daemon stops at the next such step, or once its boot is recorded, which it then
stamps.

- **`legion daemon stopped draining the API`.** A request was still in flight after those 8
  seconds. The daemon cut it short, the caller got an error, and the next boot takes its claim up;
  repeat the request once the daemon is back.
- **`legion daemon stopped waiting for its work`.** Some of the daemon's work had not ended when that
  wait ran out, which takes a call into a process that does not answer; `deciding` names each claim
  whose decision was still running, with its event. The daemon stopped anyway, and the next boot
  takes those claims up.

## The controller

- **The daemon logs `controller not registered; run legion controller start`.** Nobody is running
  the controller. Start it ([Start the controller](/legion/legion/running-legion/#start-the-controller)).
- **The daemon logs `controller not registered; run legion controller start only under controller:
  operator; this daemon launches its own controller and relaunches it`.** The controller the daemon
  launches has not registered, or its session is gone. The line's `claimState` says where its claim
  is: `launching` while a launch is in flight, `failed` or `retired` while the daemon waits to retry
  it. For a launch that keeps failing, see "The daemon-launched controller keeps failing" below.
- **`legion controller start` is refused: `this daemon launches the project's controller itself
  (controller: daemon), so legion controller start has none to start`.** The daemon runs its own
  controller as a pod; reach it through Dispatch instead.
- **The daemon-launched controller keeps failing.** `legion claims list` shows its claim,
  `legion-<project>-controller`, and the daemon logs why each launch failed and when it retries a
  failed one. `kubectl -n <namespace> describe pod legion-<project>-controller` and its logs show the
  pod's own side; a `workspace-init` exit 3 means its volume lost the session, and the daemon starts
  a fresh controller.
- **The daemon refuses to boot: `stop legion-<project>-controller, the controller an earlier boot
  under controller: daemon launched, since this daemon leaves the controller to its operator
  (controller: operator): …`.** `legion.yaml` was switched back to `controller: operator`, and the
  daemon could not stop the pod it launched before; the end of the line says why (most often the
  cluster refused the Sandbox's deletion). The refusal leaves the pod and its registration as they
  were. Fix the cause and start the daemon again, or set `controller: daemon` back, which re-adopts
  that controller with its registration, grants and wakes still working.
- **The daemon refuses to boot: `end the registration of legion-<project>-controller, the controller
  an earlier boot under controller: daemon launched, since this daemon leaves the controller to its
  operator (controller: operator): …` (or `read the controller record to end the registration of
  …`).** The daemon stopped its controller's pod but could not write the controller record; the end
  of the line is the store's error. Start the daemon again once Postgres answers: the next boot finds
  the claim retired and ends the registration before it serves anything. The record names the
  stopped pod's session until then, but no refused boot serves the state; it shows as the state's
  `controllerLocator` only if you set `controller: daemon` back before an operator boot succeeds.
  Nothing can act as that session, since the process that held its registration secret ended with
  the pod; the pod's Secret held no registration secret, only its boot token, which an operator's
  daemon refuses (409), and the launch's Envoy and Dispatch bearers.
- **`… the daemon answered 403 Forbidden: Invalid operator token — the operator token does not match
  the daemon's operator_token_file`.** Your `operator_token_file` holds a different value than the
  daemon's.
- **`could not reach the Legion daemon at <daemon_url>: …; is the port-forward running?`** Fix
  `daemon_url`, pass `--daemon-url`, or start the tunnel; the command never tries another address.
- **`… is readable by its group or others (mode 0640); chmod 0600 it`.** The operator token and the
  NATS seed must be readable by you alone.
- **`unknown key "<key>" in the controller configuration; …`** The controller reads its own small
  file, not `legion.yaml`; compare with `deploy/kubernetes/daemon/controller.yaml.example`.
- **The plugin refusal.** `legion controller start` probes your Oh My Pi before it asks the daemon
  for anything, and refuses one that does not load Legion's plugin or loads one speaking another
  daemon contract. Install the plugin from the same image as the daemon.

Whether the daemon sees a controller:

```sh
legion state --config legion.yaml --json | jq .controllerLocator
```

## An issue does not start

Check, in order:

1. **The label.** The issue must carry the Dispatch label `legion`. A root without it is never
   admitted, whatever its status.
2. **The status.** A root runs from `todo`. In `triage` it waits for the controller; in `backlog` or
   `icebox` it does not run.
3. **The project.** The issue's project must be the daemon's, under `projects` in `legion.yaml`.
4. **The slots.** When every slot is taken the issue waits its turn:

   ```sh
   legion state --config legion.yaml --json | jq .admission
   ```

   `active` lists the running roots, `waiting` the ones in line, `cap` the slots (`admission_cap`).
5. **GitHub refuses its branch.** Before the architect starts, the daemon creates the issue's
   branch, `legion/<KEY>`, on GitHub at `main` as the implement App, and again before each tree
   member's planner starts, so no agent's push is the one that creates it (GitHub can refuse that
   push on a large repository). A branch GitHub already has is kept as it is. While GitHub refuses
   the create, the architect or planner waiting for it does not start. The daemon retries the
   create, backing off to about a minute, and logs each refusal as an error whose `msg` is
   `outbox row failed`, with `kind` `issue_branch` and an `error` naming the repository, the ref,
   GitHub's status and its body:

   ```text
   {"level":"ERROR","msg":"outbox row failed","row":81,"kind":"issue_branch","error":"create the branch of WIDGETS-12, which its roles' starts wait for: create refs/heads/legion/WIDGETS-12 on acme/widgets at <main's commit>: GitHub answered POST /git/refs with 403: {\"message\":\"Resource not accessible by integration\",…}"}
   ```

   Fix what GitHub names, such as the implement App's permissions or a ruleset on `legion/*`
   branches. The next attempt then creates the branch, and the tree starts.
6. **Someone else holds it.** The architect's first act is to claim the issue in Dispatch. When a
   person or another session already holds the claim, the architect starts nothing and asks the
   holder, on the issue, to release it or take the issue back; the tree keeps its slot meanwhile.

## A tree waits at the design gate

The root's phase stays `admitted` until a person approves the spec at its current version:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"] | {phase, designGate}'
```

`designGate.approvedVersion` is `null`, or lower than `currentVersion`, until someone approves the
latest version from the document's header or the Inbox. An approval of an earlier version does not
count: the spec changed since. The merger's `READY` waits on the same approval.

## An issue is held

When an agent's budget runs out (launches that failed, deaths with work outstanding, prompts it
never took a turn on) the daemon stops relaunching it and holds the issue. The daemon logs JSON
lines; the one to look for is the error whose `msg` is `supervise: claim failed`, and its `why`
says which budget ran out (`launch failures ran out`, `deaths with work outstanding ran out`,
`prompt retirements ran out`):

```text
{"level":"ERROR","msg":"supervise: claim failed","why":"launch failures ran out","launchFailures":3,"deaths":0,…}
```

Before it come `supervise: launch failed` or `supervise: process died` warnings, one per attempt.
The state shows it too:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"] | {phase, holdReason, architect, workers}'
```

`phase` is `held` and the failed claim's `state` is `failed`. A held phase worker is its
architect's to retry or escalate; `holdReason: "escalated"` means it went to the controller. An
architect that failed (`architect.state` is `failed`) leaves its tree with nobody to act inside it,
so the controller is woken to re-admit it. To do that by hand, park the root and admit it again,
which starts a new generation from its architect:

```sh
legion status <KEY> backlog --config legion.yaml --operator-token-file operator-token
legion status <KEY> todo    --config legion.yaml --operator-token-file operator-token
```

To see why the launches failed, read the failed role's container log in the issue's pod:

```sh
kubectl -n legion get pods -l legion.dev/issue=<KEY>
kubectl -n legion logs <pod> -c <role>   # e.g. -c implementer
kubectl -n legion describe pod <pod>     # scheduling, image pulls, mounts
```

## A pod does not come up

- **`Pending`.** Usually scheduling: a tree's pods must share one node, and different trees never
  share a node, so a pool without a free node of the right size leaves the tree's next pod
  unscheduled. `kubectl -n legion describe pod <pod>` names the reason. Keep `admission_cap` within
  what the pool can hold ([The cluster](/legion/legion/running-legion/#the-cluster)).
- **The `workspace-init` container fails.** It provisions the issue's workspace on the tree volume,
  and its refusals name what to do, sometimes a `jj` command to run against the tree's shared clone
  (a local bookmark deleted and never pushed, or a conflicted one). Read it with

  ```sh
  kubectl -n legion logs <pod> -c workspace-init
  ```

  Nothing is registered before a refusal, so every later attempt refuses the same way until the
  repository is fixed. Run the commands it names from a shell in one of the tree's running pods
  (`kubectl -n legion exec -it <pod> -c architect -- sh`).
- **The tree volume was lost.** The daemon logs
  `supervise: the tree volume was lost with the session; relaunching a fresh session`. The agent
  comes back as a new session in a new workspace, which holds `.legion/<issue>/workspace-recovered.json`
  naming the issue's branch to reconcile from.
- **`worker-stream: rejected hello (stale worker generation)`** in the daemon's log is the fence
  working: a pod from an older generation of a claim tried to connect after a newer one replaced it.
  Nothing to do.
- **`worker-stream: could not resolve a hello's boot token; the shim redials`** means the daemon's
  Postgres did not answer while a pod's shim said hello. The hello is not refused: the shim
  redials, and the hello is accepted once the store answers. If the line keeps coming, look at the
  database, not the pod.
- **`supervise: the agent registered and never said it was ready; retired its process`** means an
  agent registered and its ready never came within the registration deadline
  (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`, 6 minutes at the
  defaults), often because the registration's answer never reached it. The daemon resumes the same
  session one generation later and counts a launch failure; after `launch_failure_limit` of them
  the claim fails.

## The work loops or stops

These arrive as messages on the Dispatch issue, and the architect is told:

- **`Issue reached review_round_cap=3.`** The implementer's round count reached three
  ([what counts a round](/legion/legion/concepts/#review-signalling)). The work goes on, and the
  architect decides what happens next, often with a question to you.
- **`Pull request #<n> reached max_fix_attempts=3.`** A check or workflow the base branch requires
  stayed red through three fix attempts.
- **A review round that no review decides.** Legion's reviewer must approve the head or request
  changes; a plain comment leaves the issue in `needs_review`, and the architect asks the reviewer
  for the decision. Only the review App or an account with write access to the repository decides a
  round, so an approval or request for changes from anyone else leaves it undecided too. A review
  from the review App decides a round only when it comes from the reviewer's own session: the
  daemon reads the review's Legion footer
  (`<!-- legion: {"session":"…","phase":"review"} -->`) and requires the session it names to match
  the session recorded on the reviewer's claim. A review-App review with no footer, or whose
  footer names another session (the controller's, an architect's), is set aside and decides
  nothing, logged at warn as `workflow: a review decides nothing: the review App submitted it from
  a session that is not the reviewer's`, with `footer_session`, `reviewer_session` and
  `body_truncated`. While the reviewer's session is not yet recorded, the login alone decides,
  logged as `workflow: the reviewer's session is not recorded; the review App's login decides`.
  Envoy caps review bodies at 2048 runes, so the daemon restores a truncated review-App body from
  GitHub before reading its footer; a review it cannot restore (no review id) stays set aside with
  `body_truncated=true`. The remedy for this stall: the reviewer re-submits its review with the
  footer naming its own session. The daemon reads the author's permission from GitHub before it
  applies the review: an account GitHub answers `404` for, or a `403` that is not its rate limit,
  has no write access, and stands so for five minutes; write access is read again for every review.
  Any other failed read is retried, a rate limit once the wait GitHub names has passed, and while
  that wait stands the daemon reads nothing from GitHub. The daemon logs each review that decides
  nothing for permission reasons as `workflow: a review decides nothing: its author is neither the
  review App nor an account with write access to the repository`, with the author's login, and logs
  a permission it read as no write access with GitHub's own answer. A `403` that is not a rate limit
  is logged at error as `workflow: GitHub refuses the review App's installation a review author's
  repository permission`, naming the installation's owner: the review App's installation cannot
  read the repository's collaborators, so until it can, no review but the review App's decides a
  round.
- **`READY` refused.** The merger's `READY` is refused while the pull request's head still carries
  the issue's handoffs, `.legion/<issue>/`, which retro's last commit removes (the issue goes back
  to `retro`); until every check the base branch requires has succeeded on the head, and every
  workflow its rulesets require has a run on the head that succeeded; and while the design gate is
  closed. The refusal names the head and the directory, the check or workflow, or the spec version
  that needs approval. A refusal saying GitHub's read of `.legion/<issue>/` failed is GitHub's
  failure, and the merger completes again. None of the head checks applies to a pull request a
  person already merged: its `READY` is published unread, and the issue goes on to its production
  check. If that merge carried `.legion/<issue>/` onto the base, the architect asks whoever merged
  for a pull request that deletes it.

An issue back in `in_progress` after its `READY`, while it awaited its merge, had a required check
or workflow turn red on the head itself, or its head conflicts with its base (GitHub computes no
merge ref for a conflicting head and runs no checks on it at all): the daemon sent it back to the
implementer, told the architect which checks or that the head conflicts, and posted on the issue,
and told the project's `merge_queue_role` when one is set, that the `READY` is withdrawn; the work
comes back through testing, review and a new `READY`.

An issue that stays in `needs_review` while a review workflow the project declares
(`projects.<KEY>.review_workflows`) is red: only declared review workflows are red, so the reviewer
is adjudicating their findings, resolving the threads it accepted and re-running the failed run. If
the re-run stays red after that, the architect asks you to decide, naming the pull request, the
head and the workflow. A red required workflow the project does not declare sends the issue back to
the implementer instead, as a red required check does.

The pull request's state as the daemon sees it, its `checksVerdict` judged only by the checks and
workflows the base branch requires:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"].pullRequest'
```

No `checksVerdict` on a pull request whose checks have settled means the daemon has not read the
base branch's required checks yet, or a required workflow's run on the head is still going, has
not happened, or has not been read since the head settled. Its log names each read GitHub refused,
with the repository, the pull request and the HTTP status (`read the checks a pull request's base
branch requires`); the daemon reads again every two minutes.

## A status change did not stick

The daemon owns a running tree's status. A status an agent session writes on a running root is set
back, and the tree's architect is told who wrote it. A person's move in the Dispatch dashboard, the
controller's `set_status`, or `legion status` from an operator's shell, is what takes a tree out.

When the daemon's own status writes to Dispatch are failing, `legion state` says so on its third
line:

```text
pending Dispatch status writes: 2; the oldest, for WIDGETS-12, has failed 4 attempts, next at 2026-10-03T10:41:07Z: …
```

The writes are retried in order; the error at the end of the line is Dispatch's answer.

## GitHub, NATS and the Secrets Broker

- **`github_app_not_installed: <role> not installed on <owner>`.** Install that GitHub App (the
  implement or the review App) on the repository's owner.
- **`NATS refused the daemon a permission: its NATS user lacks that grant`** (an error line with
  the `operation` and `subject`). The daemon's NATS user is missing a grant;
  [Envoy](/legion/dispatch/envoy/) lists the subjects it needs. A daemon can boot healthy and
  consume events with one grant missing, so search the log for this line after a NATS change.
- **`agent-secrets machine login: enter code XXXX-XXXX on the Dispatch credential page (approver:
  <operator>); pod enrollment is held until approved`.** With `runtime.kubernetes.agent_secrets`
  set, the daemon logs in to the [Secrets Broker](/legion/broker/) at boot and waits for the
  `operator` to approve the code on Dispatch's credential page; pods are not enrolled until then.

  ```sh
  legion state --config legion.yaml --json | jq .agentSecretsLogin
  ```
