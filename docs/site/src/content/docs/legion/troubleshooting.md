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
| `bind 127.0.0.1 is not an address a pod can reach, …` (or the same for `daemon_url`, `envoy_url`, `dispatch_url`, a `nats_urls` entry) | Every address handed to a pod must be one a pod reaches: the daemon host's own address, never loopback or `0.0.0.0`. |
| `<key> is required when runtime is kubernetes: …` | Add the key; the message says why a pod needs it. |
| `projects must configure <PROJECT>, the daemon's own project` | `project` must also be a key of `projects`. |
| `postgres_dsn is required (or set LEGION_POSTGRES_DSN)` | Give the daemon its database. |
| `unknown key <key>` | A typo, or a setting Legion no longer has; the message says which when it knows. |
| `omp_invocation is not used when runtime is kubernetes: …` | Remove it: every pod runs the worker image's Oh My Pi. |
| `<PROJECT> is already running (pid <n>)` (from `legion start` itself) | A daemon for this project is already registered on the machine: `legion status <PROJECT>`, `legion legions`. |

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
    Secret's `NATS_NKEY_SEED` key.

  A model key that fails at boot refuses the boot even when the cause is a passing network blip,
  since the probe cannot tell the two apart. Start the daemon again once the cause is gone.

## The controller

- **The daemon logs `controller not registered; run legion controller start`.** Nobody is running
  the controller. Start it ([Start the controller](/legion/legion/running-legion/#start-the-controller)).
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
5. **Someone else holds it.** The architect's first act is to claim the issue in Dispatch. When a
   person or another session already holds the claim, the architect starts nothing and asks the
   holder, on the issue, to release it or take the issue back; the tree keeps its slot meanwhile.
6. **GitHub refuses its branch.** Before the architect starts, the daemon creates the issue's
   branch, `legion/<KEY>`, on GitHub at `main` as the implement App, so no agent's push is the one
   that creates it (GitHub can refuse that push on a large repository). A branch GitHub already has
   is kept as it is. While GitHub refuses the create, the architect does not start, and neither does
   a child's planner whose branch is refused. The daemon retries the create, backing off to about a
   minute, and logs each refusal as an error whose `msg` is `outbox row failed`, with `kind`
   `issue_branch` and an `error` naming the repository, the ref, GitHub's status and its body:

   ```text
   {"level":"ERROR","msg":"outbox row failed","row":81,"kind":"issue_branch","error":"create the branch of WIDGETS-12, which its roles' starts wait for: create refs/heads/legion/WIDGETS-12 on acme/widgets at <main's commit>: GitHub answered 403: {\"message\":\"Resource not accessible by integration\",…}"}
   ```

   Fix what GitHub names, such as the implement App's permissions or a ruleset on `legion/*`
   branches. The next attempt then creates the branch, and the tree starts.

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

To see why the launches failed, read the pod's logs:

```sh
kubectl -n legion get pods -l legion.dev/issue=<KEY>
kubectl -n legion logs <pod> -c worker
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
  (`kubectl -n legion exec -it <pod> -c worker -- sh`).
- **The tree volume was lost.** The daemon logs
  `supervise: the tree volume was lost with the session; relaunching a fresh session`. The agent
  comes back as a new session in a new workspace, which holds `.legion/workspace-recovered.json`
  naming the issue's branch to reconcile from.
- **`worker-stream: rejected hello (stale worker generation)`** in the daemon's log is the fence
  working: a pod from an older generation of a claim tried to connect after a newer one replaced it.
  Nothing to do.

## The work loops or stops

These arrive as messages on the Dispatch issue, and the architect is told:

- **`Issue reached review_round_cap=3.`** Three review rounds sent the change back. The architect
  decides what happens next, often with a question to you.
- **`Pull request #<n> reached max_fix_attempts=3.`** CI stayed red through three fix attempts.
- **A review round that no review decides.** Legion's reviewer must approve the head or request
  changes; a plain comment leaves the issue in `needs_review`, and the architect asks the reviewer
  for the decision.
- **`READY` refused.** The merger's `READY` is refused until every check the base branch requires
  has succeeded on the pull request's head, and while the design gate is closed. The refusal names
  the head and the check, or the spec version that needs approval.

The pull request's state as the daemon sees it:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"].pullRequest'
```

## A status change did not stick

The daemon owns a running tree's status. A status an agent session writes on a running root is set
back, and the tree's architect is told who wrote it. A person's move in the Dispatch dashboard, or
`legion status` from the controller or an operator's shell, is what takes a tree out.

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
