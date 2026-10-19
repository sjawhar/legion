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
| `worker_stop_timeout_seconds must be at least 9 when runtime is kubernetes: …` | Raise it to 9 or more. A role's stop gives its agent 5 s to exit, drains its output for 1 s and waits at least 3 s for the CodeGraph warm-up to release its lease on the issue volume; a smaller grace would let the pod kill the shim with the lease held, and the role's relaunch would skip its own build for a minute. |
| `projects must configure <PROJECT>, the daemon's own project` | `project` must also be a key of `projects`. |
| `postgres_dsn is required (or set LEGION_POSTGRES_DSN)` | Give the daemon its database. |
| `unknown key <key>` | A typo, or a setting Legion no longer has; the message says which when it knows. |
| `omp_invocation is not used when runtime is kubernetes: …` | Remove it: every pod runs the worker image's Oh My Pi. |
| `<PROJECT> is already running (pid <n>)` (from `legion start` itself) | A daemon for this project is already registered on the machine: `legion status <PROJECT>`, `legion legions`. |
| `capability <name> is open: <detail>; to record a decision, add to legion.yaml: capabilities.decided.<name>: "<reason>"` (after the `Config OK` line, exit 0) | A report, not a refusal: the deployment leaves a worker capability open. The check names the rows the file alone decides — `resource-limits` while a role lacks CPU and memory in both requests and limits, `secrets` while no broker is configured — and the daemon logs the same line at boot for every open row, `model-fallback` included when the probe read `retry.modelFallback` false under your overlay or could not read it at all (the probe's OK line marks that `model-fallback=unread`, the probe pod's log says why), and `pool-capacity` when the image probe pod was Unschedulable before it scheduled (the detail counts the attempts that waited and quotes the scheduler's reason). Close the gap, or add the `capabilities.decided.<name>: "<reason>"` line it prints to record your decision. `legion state --json` lists every row under `capabilities`, each deployment row carrying `source` (`configuration` or `boot`), and the controller's daily report names each open one. In the daemon's log alone (the check prints no such row), a line without the `; to record a decision` tail — `capability <name> is open: pod <sandbox>/<container> (<role>, <issue>): <fact>`, or `pane <window>:<pane>` under tmux — is a live row (`subagents`, `web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) that a running session's own check at its start failed: the daemon logs the fact alone, with the claim, role, issue and locator as the line's attributes, as the session reports ready, then this pod-naming form at boot and at the first controller tick after the set of open rows changed, and `legion state` shows the row `open` with that detail, naming each failing session. The fact is what the session measured inside its Oh My Pi (`web_search is not a registered tool`, `not connected: <server> (<error>)`, `agents the prompts dispatch that the task tool does not resolve: <names>`, `not discovered: <paths>`, `Envoy tools missing: <names>`, `dispatch read failed: <stderr>`, `gh viewer login failed: <stderr>`, or `did not finish within 8 s`). No `capabilities.decided` line applies: fix what the fact names in the session's environment, and the row heals when that process is relaunched with the gap closed or reports anew at its next boot — a session that booted during a Dispatch, GitHub or search-provider outage reads open until then. The session's own report is under `issues.<KEY>.architect.capabilities` or `issues.<KEY>.workers.<role>.claim.capabilities` in `legion state --json`. |

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
  - the probe pod cannot mount a volume of your `runtime.kubernetes.pod` block that a `volume_mounts`
    entry mounts (`… cannot mount volume "<name>", which runtime.kubernetes.pod.volumes gives it from
    ConfigMap "<name>" …`, or `Secret "<name>"`, quoting the kubelet's `configmap "<name>" not found`,
    `secret "<name>" not found` or the key it could not find): create the object in the daemon's
    namespace, or fix the name or key in the block. The daemon refuses on the kubelet's first
    `FailedMount` (a volume no container mounts is never attempted, so never refused), minutes into
    the boot, and `legion probe-boot` shows the same refusal before a restart; a container waiting
    `CreateContainerConfigError` for an object its environment reads (`… cannot configure its
    container probe (waiting: CreateContainerConfigError) …`) has the same remedy;
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
   branch, `legion/<KEY>`, on GitHub at the repository's default branch, as GitHub names it
   (`default_branch`, read again at most every ten minutes), as the implement App, and again before
   each tree member's planner starts, so no agent's push is the one that creates it (GitHub can
   refuse that push on a large repository). A branch GitHub already has is kept as it is. While
   GitHub refuses the read or the create, the architect or planner waiting for it does not start.
   The daemon retries, backing off to about a minute, and logs each refusal as an error whose `msg`
   is `outbox row failed`, with `kind` `issue_branch` and an `error` naming the repository, the
   ref, GitHub's status and its body:

   ```text
   {"level":"ERROR","msg":"outbox row failed","row":81,"kind":"issue_branch","error":"create the branch of WIDGETS-12, which its roles' starts wait for: create refs/heads/legion/WIDGETS-12 on acme/widgets at <default branch's commit>: GitHub answered POST /git/refs with 403: {\"message\":\"Resource not accessible by integration\",…}"}
   ```

   A read GitHub refuses names it instead (`read the default branch of acme/widgets: GitHub
   answered GET /repos/acme/widgets with 404: …`). Fix what GitHub names, such as the implement
   App's permissions or a ruleset on `legion/*` branches. The next attempt then creates the branch,
   and the tree starts.
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

Before it come `supervise: launch failed` or `supervise: process died` warnings, one per attempt,
each but the last followed by `supervise: waiting before the next launch`: the tries are spaced by
a backoff (1–2 s, then 2–4 s), so a fault that lasts a moment cannot spend the whole budget. Under
the Kubernetes runtime, a launch whose requests the API server answers overloaded (a 429, or a 5xx
such as `etcdserver: throttle: too many requests`) waits the server out and is charged nothing;
its `sandbox runtime: the API server answered a launch's request overloaded` warning comes first,
and the launch fails only when the server is still overloaded at the launch's own deadline. The
state shows it too:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"] | {phase, holdReason, architect, workers}'
```

`phase` is `held` and the failed claim's `state` is `failed`. A held phase worker is its
architect's to retry or escalate; `holdReason: "escalated"` means it went to the controller. An
architect that failed (`architect.state` is `failed`) leaves its tree with nobody to act inside it,
and the controller is woken to re-admit the tree. What brings the architect back depends on where
its tree stands:

- **Its root holds its admission slot and does not linger:** `legion state`'s `claims nothing will
  wake` line names the architect under `failed, retry`. Retry it, as
  [A tree does not move after a restart](#a-tree-does-not-move-after-a-restart) shows: the daemon
  relaunches it on the session it kept, with fresh budgets, and the tree carries on at the
  generation and phase it stands at.
- **Its root is parked or lingers,** the controller's park for a re-admission included: leave the
  claim as it is. The tree's re-admission retries it at a new generation; a retry by hand would run
  it outside the admission cap, and it would be sent no catch-up. To re-admit the tree by hand, park
  the root and admit it again:

  ```sh
  legion status <KEY> backlog --config legion.yaml --operator-token-file operator-token
  legion status <KEY> todo    --config legion.yaml --operator-token-file operator-token
  ```

`legion claims retry` relaunches only a failed architect. It refuses a claim in any other state,
naming the state, and a failed phase worker or controller, naming who retries it.

To see why the launches failed, read the failed role's container log in the issue's pod:

```sh
kubectl -n legion get pods -l legion.dev/issue=<KEY>
kubectl -n legion logs <pod> -c <role>   # e.g. -c implementer
kubectl -n legion describe pod <pod>     # scheduling, image pulls, mounts
```

## A tree does not move after a restart

A restart re-adopts every process still running, but a claim with no process runs again only when
something starts it. A worker is started by the workflow when its phase begins, and a tree's
architect by its admission; a notice to an architect with no process waits for it rather than
starting it. A suspended worker of its issue's current phase that the daemon suspended is resumed
by the daemon at boot. One an operator suspended (`legion claims suspend`, one claim at a time or a
whole fleet before a restart), or whose suspension predates the release that records who suspended
it (`suspendedBy: unknown`), is not; neither is a tree's architect, whoever suspended it, nor a
claim whose launches ran out. Each of those is woken by nothing: the daemon looks healthy and the
state shows the tree active, but the tree stops for good at the worker's phase, or at the first
decision that needs its architect. Once a boot has re-adopted its claims, the daemon names every
such claim in one error line, under the move that brings it back:

```text
{"level":"ERROR","msg":"legion daemon started with claims nothing will wake","project":"WIDGETS","count":4,"resume":["WIDGETS-12 architect"],"suspendedWorkers":["WIDGETS-12 implementer"],"retry":["WIDGETS-14 architect"],"needsWorkflowStart":["WIDGETS-15 reviewer"]}
```

The state names the same claims on every read, after a restart or not, each with its `state`, and
`legion state` prints them on its `claims nothing will wake` line:

```sh
legion state --config legion.yaml --json | jq '.strandedClaims'
```

Act only on the claims this list names, and leave out any you suspended on purpose. A claim's token
is `legion-<project>-<issue, lowercased>-<role>`, as `legion claims list` shows.

- **`resume`** (a suspended architect): resume it. Once it is ready the daemon sends it a catch-up
  of what changed while it was away:

  ```sh
  legion claims resume --config legion.yaml --operator-token-file operator-token --claim legion-widgets-widgets-12-architect
  ```

- **`suspendedWorkers`** (a suspended phase worker): the move depends on who suspended it, which
  `legion claims list --json` shows as `suspendedBy`, and `legion state` points there too.
  Only an architect is sent a catch-up, so a worker resumed with no task relaunches with nothing to
  do, and once it runs the list stops naming it.

  - **`suspendedBy: unknown`** (suspended before this release recorded who did): let the
    workflow start it. Stop the project's daemon, mark the claim as the daemon's, and start the
    daemon again; at boot it queues the workflow's own start for the worker's phase, with the
    right run, phase and, for a merger, approved head:

    ```sql
    update claims set suspended_by = 'daemon' where token = 'legion-widgets-widgets-12-implementer' and state = 'suspended';
    ```

    A task you deliver yourself belongs to no run of the workflow, so such a worker's completion
    can be refused (`HANDOFF_NO_RUN`, or a stale generation) when it never confirmed a task of the
    current run. The boot starts the worker only while its issue is in that phase: when
    `legion state` shows the issue `held`, leave the claim as it is, and its architect's retry of
    the phase starts it ([An issue is held](#an-issue-is-held)).
  - **`suspendedBy: operator`, with a task or a waiting start:** resume it. A worker suspended
    mid-task keeps that task (`legion claims list --json` shows it as the claim's `pending`, and
    the daemon refuses a second delivery), and its task is sent again once it is ready. A worker
    whose phase start reached it while it was suspended has that start waiting for your resume
    (the daemon logged `outbox row waits` for `start <role> of <issue>`, naming the operator's
    suspension); the start gives it the phase's task once it runs, so a delivery would hand it the
    phase twice.
  - **`suspendedBy: operator`, otherwise:** deliver its phase's resume task. An operator's
    delivery resumes the claim and hands it the task. The task is the one the workflow gives a
    worker resuming its phase, with the issue's title, key and phase as `legion state` shows them
    (a merger told no approved head sends the round back for review):

    ```sh
    legion claims deliver --config legion.yaml --operator-token-file operator-token --claim legion-widgets-widgets-12-implementer \
      --task "Continue <title>. Issue: WIDGETS-12. Phase: implementing. Resume the existing phase work."
    ```

- **`retry`** (a failed architect): retry it. The daemon relaunches it on the session it kept,
  with fresh budgets, and sends it a catch-up of its tree once it is ready, so the tree carries on
  at the generation and phase it stands at:

  ```sh
  legion claims retry --config legion.yaml --operator-token-file operator-token --claim legion-widgets-widgets-14-architect
  ```

  When a tree's architect and its workers failed together, retry the architect first: the held
  workers are its to retry once it runs.

- **`needsWorkflowStart`** (a failed phase worker): its issue is held
  ([An issue is held](#an-issue-is-held)), and its tree's architect retries the phase or escalates
  it to the controller. A failed claim ignores `legion claims resume` and `legion claims deliver`,
  and `legion claims retry` refuses a phase worker's.

The list names only claims of trees that hold an admission slot, and leaves out claims that
something will still start: a worker whose phase is not its own is started again when its phase
comes, a parked tree's claims wait for its re-admission, and the architect of a tree re-admitted
while every slot was taken waits for admission to start it once one frees. Never resume or retry by
hand a claim of a lingering tree or of an issue parked out of the workflow, nor that waiting
architect: it would run outside the admission cap, and re-admission or the issue's return to `todo`
starts it.
A claim an operator suspended is listed even when a workflow start is queued for it: that start
waits for the operator's resume. One whose `suspendedBy` is `unknown` is listed only while no start
is queued for it, since the next phase start or re-admission resumes it as before.

## A pod does not come up

- **`Pending`.** Usually scheduling: a pod reserves its roles' cpu and memory (3 CPU and 19 GiB for
  an issue pod at the defaults) and lands wherever the pool has room, so a pool at its limits leaves
  the next pod unscheduled until the daemon reads it dead past `worker_boot_timeout_seconds` and,
  once its launch failures run out, fails the claim. `kubectl -n legion describe pod <pod>` names the
  reason. Keep `admission_cap`, and the children a tree runs at once, within what the pool's limits
  can place ([The cluster](/legion/legion/running-legion/#the-cluster)). A pod relaunched onto
  another node first waits for its volume to detach from the old one (`FailedAttachVolume` or
  `Multi-Attach` events, transient). The image probe pod, `legion-probe-<project>-<digest prefix>`,
  Pending Unschedulable at boot is the one pod the daemon waits on instead: it logs `boot probe is
  waiting on capacity; running it again` with the scheduler's reason and creates the pod again every
  5 minutes, serving nothing until one is placed and passes, and then reports the `pool-capacity`
  capability open with the count and the reason. Make room in the pool, or raise its limits; the
  probe pod itself needs only 250m and 1Gi.
- **The `workspace-init` container fails.** It provisions the issue's workspace on the issue's own
  volume, and its refusals name what to do, sometimes a `jj` command to run against the issue's
  clone (a local bookmark deleted and never pushed, or a conflicted one). Read it with

  ```sh
  kubectl -n legion logs <pod> -c workspace-init
  ```

  Nothing is registered before a refusal, so every later attempt refuses the same way until the
  repository is fixed. Run the commands it names from a shell in the issue's running pod
  (`kubectl -n legion exec -it <pod> -c architect -- sh`), or, when none runs, in a pod of your own
  that mounts the issue's volume (`docs/kubernetes.md` in the repository, "A shell on an issue's
  volume").
- **The issue's volume was lost.** The daemon logs
  `supervise: the issue's volume was lost with the session; relaunching a fresh session`. The loss
  is one issue's: its other roles drop the sessions they kept on that volume, and no other issue of
  the tree is touched. The agent comes back as a new session in a new workspace, which holds
  `.legion/<issue>/workspace-recovered.json` naming the issue's branch to reconcile from.
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
  stayed red through three fix attempts. Red CI within the attempt that ran the count out (the
  counted head and every head pushed on top of it until a push lands on a head CI judged green) no
  longer sends the work back by itself, so the issue waits in testing, review, retro or merging for
  the architect, or for a worker's own decision such as a reviewer's request for changes. A `READY`
  is still withdrawn on red CI.
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
- **`READY` refused.** The merger's `READY` is refused while the pull request's head carries
  anything above the reviewer's approved head but retro's notes under `docs/solutions/` and its
  removal of `.legion/<issue>/` (`READY_UNREVIEWED_CHANGE`, naming the paths; also when the head
  is not built on the approved one, GitHub does not know the approved head, or GitHub's compare
  gives no file list or lists 300 files, the most it lists), or
  the review round recorded no approval (`READY_NOT_APPROVED`): either way the issue goes back to
  `reviewing`. It is refused while the head still carries
  the issue's handoffs, `.legion/<issue>/`, which retro's last commit removes (the issue goes back
  to `retro`); while a check the base branch requires has failed or is missing on the head, or a
  workflow its rulesets require has failed or has no run on it (one still running refuses nothing);
  and while the design gate is closed. The refusal names the head and the directory, the check or workflow, or the spec version
  that needs approval. A refusal saying GitHub's read of `.legion/<issue>/` failed is GitHub's
  failure, and the merger completes again. None of the head checks applies to a pull request a
  person already merged: its `READY` is published unread, and the issue goes on to its production
  check. If that merge carried `.legion/<issue>/` onto the base, the architect asks whoever merged
  for a pull request that deletes it.

An issue back in `in_progress` from `retro` or `merging` had a required check or workflow turn red
on the head: the daemon sent it back to the implementer and told the architect which checks. From
`retro` the implementer kept its turn and was handed the implementing task once that turn ended;
from `merging` the merger's turn was interrupted. One back in `in_progress` after its `READY`,
while it awaited its merge, had a required check or workflow turn red on the head itself, or its
head conflicts with its base (GitHub computes no merge ref for a conflicting head and runs no
checks on it at all): the daemon sent it back to the implementer, told the architect which checks
or that the head conflicts, and posted on the issue, and told the project's `merge_queue_role` when
one is set, that the `READY` is withdrawn; the work comes back through testing, review and a new
`READY`.

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

## An issue's stop or start waits behind its own launch

An issue's lifecycle rows — its starts, stops and suspensions, its branch, its workspace removal —
run one at a time, oldest due first, so a stop, suspension or later start of an issue waits while
the row it names runs. Most often that row is a start whose pod is still launching
([A pod does not come up](/legion/legion/troubleshooting/#a-pod-does-not-come-up)). Other issues
are not held, and the issue's own status writes, messages and notices run meanwhile.
`legion state` says so on its fourth line, naming the row that has waited longest:

```text
held lifecycle rows: 1; the longest waiting, supervise row 802 of WIDGETS-12, has waited 2m30s behind row 801
```

`legion state --json` lists each under `heldOutboxRows`, with its `id`, `kind` and `issue`,
`waitedSeconds` counted from when the row came due, and `waitsOn`, the id of the running row. Once
such a wait passes two minutes the daemon logs a warning, `outbox row held behind its issue's
running lifecycle row`, with `row`, `kind`, `issue`, `waitsOn` and `waited`: search the log for
it, then look at what the row `waitsOn` names is doing.

## GitHub, NATS and the Secrets Broker

- **`github_app_not_installed: <role> not installed on <owner>`.** Install that GitHub App (the
  implement or the review App) on the repository's owner.
- **`NATS refused the daemon a permission: its NATS user lacks that grant`** (an error line with
  the `operation` and `subject`). The daemon's NATS user is missing a grant;
  [Envoy](/legion/dispatch/envoy/) lists the subjects it needs. A daemon can boot healthy and
  consume events with one grant missing, so search the log for this line after a NATS change.
- **`agent-secrets: the token login failed; retrying`** (an error line with the broker's answer).
  With `runtime.kubernetes.agent_secrets` set, the daemon logs in to the
  [Secrets Broker](/legion/broker/) with its own ServiceAccount token, read from `token_file`, at
  every start and again before each credential expires; pods wait to be enrolled until a login
  succeeds. The answer says why: `ACCOUNT_NOT_LISTED` (the broker's `BROKER_SERVICE_LOGINS` does not
  name the daemon's ServiceAccount for `legion-daemon`: add it and redeploy the broker),
  `TOKEN_EXPIRED` or `TOKEN_INVALID` (the token file is stale, or not a token for the broker's
  issuer and audience: check the daemon's projected-token volume), `TOKEN_TOO_LONG_LIVED` (the
  token lives longer than an hour from its issue: set the projected token's `expirationSeconds`,
  or `kubectl create token`'s `--duration`, to at most an hour), `TOKEN_LOGIN_OFF` (the broker
  trusts no Kubernetes token issuer), `read the ServiceAccount token` (the file is missing), a
  `404` from a broker too old to have the token route, or a network fault. `.agentSecretsLogin`
  reads `"state": "failed"` until a login succeeds. Nothing needs restarting: the daemon retries
  30 s after a failed login, doubling to at most 5 minutes, and no person is ever asked.

  ```sh
  legion state --config legion.yaml --json | jq .agentSecretsLogin
  ```
- **`runtime.kubernetes.agent_secrets.token_file is required`.** The daemon logs in with its own
  ServiceAccount token, so the block names where it is mounted.
- **`runtime.kubernetes.agent_secrets.operator was removed (LEGION-664)`.** No person approves the
  daemon's login, so the block names no approver. Delete the key.
- **`supervise: agent-secrets: the broker found the pod token expired; keeping the key for a fresh
  token`** (`403 POD_TOKEN_EXPIRED: the projected token has expired (oidc: token expired at
  <time>)`). The token the daemon presented for the pod expired before the broker checked it. The
  daemon reads a fresh token from the shim when the one it holds is about to expire, so from a
  current daemon this means the broker's clock runs more than a minute ahead of the daemon's. The
  daemon keeps the pod's key, asks the shim for the pod's current token at its next observation,
  and enrolls the key with it; check the two hosts' clocks.
- **`supervise: agent-secrets: enrollment refused; waiting for the shim's next hello`** with
  `403 POD_IDENTITY_MISMATCH: the projected token does not identify this pod`. The token is not
  this pod's: it names another pod UID, it is not signed by the cluster the broker trusts
  (`BROKER_K8S_OIDC_ISSUER`), or it is for another audience. An expired token is answered
  `POD_TOKEN_EXPIRED` instead.
