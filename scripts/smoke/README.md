# Legion smoke rig

This rig runs the real NATS broker, Envoy listener (including its GitHub webhook receiver), Legion daemon, and the pinned oh-my-pi binary that the daemon places in tmux windows. It targets a dedicated GitHub sandbox repository and the shared live Dispatch server (project `LEGSMOKE`) for the issue lifecycle. `forward` mode receives GitHub webhooks locally; `envoy` mode bridges the production Envoy receiver's NATS envelopes — GitHub events and Dispatch issue events — into the rig.

## Prerequisites

`SMOKE_WEBHOOK_MODE=forward` requires the GitHub CLI webhook extension:

```sh
gh extension install cli/gh-webhook
```

Forwarding is a user-only GitHub CLI feature, so `forward` needs a user-authenticated `gh` identity that administers `SMOKE_REPO`. `none` omits every forwarder and GitHub hook registration. `envoy` receives production envelopes without a personal GitHub identity; the Legion Apps still require installation on the sandbox repository.

`up.sh` requires these non-secret inputs:

| Variable | Meaning | Default |
| --- | --- | --- |
| `SMOKE_REPO` | Dedicated repository as `<owner>/<repo>`. | Required. |
| `DISPATCH_URL` | Base URL of the shared Dispatch server the daemon talks to for the issue lifecycle. `up.sh` never reads a config file for this — export it yourself (e.g. from this box's `~/.config/opencode/envoy.json` → `.dispatch.serverUrl`). | Required; `up.sh` fails loudly naming `DISPATCH_URL`/`DISPATCH_TOKEN` if either is unset. |
| `SMOKE_PROJECT` | Legion's own daemon identity (`LEGION_ID`), as `<owner>/<number>`; unrelated to the Dispatch project. | Required. |
| `SMOKE_WEBHOOK_MODE` | `forward` starts `gh webhook forward` (live GitHub events only); `envoy` starts the production-Envoy NATS bridge (live GitHub events and Dispatch issue events); `none` omits every ingress transport — neither GitHub events nor Dispatch issue events reach the rig NATS. | `forward` when `gh webhook forward --help` is available; otherwise `none`. |
| `SMOKE_UPSTREAM_NATS` | Production NATS source for `envoy` mode. | `nats://envoy-nats.tailb86685.ts.net:4222` |
| `LEGION_IMPLEMENT_APP_ID` | Numeric implementation App ID. | `3202636` |
| `LEGION_REVIEW_APP_ID` | Numeric reviewer App ID. | `3202653` |
| `SMOKE_OMP_LAUNCH_PREFIX` | Whitespace-separated argv prefix written into the generated config's `omp_launch_prefix`, so daemon-spawned panes get provider credentials from `secretsd` instead of the daemon's own environment (see the "OMP invocation" section below). Set to an empty string to disable. | `secrets ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY --` |

`LEGSMOKE` must already exist on the server named by `DISPATCH_URL` before starting the rig (project creation is human-only); it is not configurable. `up.sh` creates this exercise's own root Dispatch issue inside that shared project (or reuses one already recorded under `SMOKE_DIR` from an earlier run of the same rig) and records its key at `${SMOKE_DIR}/root-issue`; checkpoints read that file (or `SMOKE_ROOT_ISSUE`, if set) rather than guessing which of the shared project's parentless issues belongs to this run.

It also requires these secret inputs:

| Variable | Meaning |
| --- | --- |
| `GITHUB_WEBHOOK_SECRET` | Secret used by Envoy and the locally signed listener ping; `forward` mode also supplies it to GitHub webhook forwarding. |
| `DISPATCH_TOKEN` | Bearer token for the Dispatch server named by `DISPATCH_URL` (e.g. this box's `~/.config/opencode/envoy.json` → `.dispatch.token`). |
| `GH_AGENT_APP_PRIVATE_KEY_B64` | Base64-encoded implementation App private key. |
| `GH_REVIEW_APP_PRIVATE_KEY_B64` | Base64-encoded reviewer App private key. |

Provide secrets with the `secrets` wrapper rather than writing a `.env` file. The private keys stay in the process environment; `up.sh` writes only `private_key_command` references into its generated daemon configuration. `up.sh` trims leading and trailing whitespace from `GITHUB_WEBHOOK_SECRET` once during validation and prints `WARNING` when the stored secret contains whitespace. It passes that same normalized secret to the listener and, only in `forward` mode, to `gh webhook forward --secret`.

### Human-controlled gates

| Variable | Required for | Behavior when absent |
| --- | --- | --- |
| `SMOKE_WEBHOOK_MODE=envoy` | Production Envoy ingress | App-only, live GitHub envelope and Dispatch issue-event ingress from the production Envoy receiver; the only mode that reaches checkpoints 1–4 and 12. It never registers a hook or calls GitHub with a personal identity. |
| `SMOKE_WEBHOOK_MODE=forward` | Local GitHub webhook ingress (live GitHub events only) | `gh webhook forward` relays GitHub events only: no Dispatch issue event reaches the rig NATS, so the daemon never admits the root issue `up.sh` creates and `checkpoints.sh` prints `SKIPPED-BLOCKED` with the Dispatch-ingress reason for checkpoints 1–4 and 12 (exit 3); 5–7 and 9–11 are available. This is the default whenever `gh webhook forward --help` is available (otherwise `none`) and needs a user-authenticated `gh` identity. |
| `SMOKE_WEBHOOK_MODE=none` | Deliberately no live ingress: neither GitHub events nor Dispatch issue events reach the rig NATS | Exercises rig start-up, the fail-closed OMP probe, controller spawn, and checkpoint 13. The daemon never admits the root issue `up.sh` creates (its resync skips issue keys it never ingested), so `checkpoints.sh` prints `SKIPPED-BLOCKED` with the Dispatch-ingress reason for checkpoints 1–4 and 12 and with the GitHub-ingress reason for 5–7 and 9–11, exit 3 each. `envoy` is the mode for checkpoints 1–4 and 12. |
| `SMOKE_BRANCH_PROTECTION=1` | Branch protection | `up.sh` configures `main` to require one approving review only when explicitly armed. This needs a user-authenticated `gh` identity. Without it, the rig prints `SKIPPED-BLOCKED`; checkpoints 7–8 exit 3 with the exact missing-ruleset reason. |

The sandbox repository includes the 20-second `ci` check and the `.fail-me`-controlled `fail-on-demand` workflow. Both Legion Apps are installed account-wide for `sjawhar`; no per-repository install step is required.


## Start and stop

```sh
export SMOKE_REPO=example-org/legion-smoke
export SMOKE_PROJECT=example-org/24
export SMOKE_WEBHOOK_MODE=envoy
export DISPATCH_URL=http://localhost:8766

secrets ENVOY_GITHUB_WEBHOOK_SECRET GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 DISPATCH_TOKEN -- \
  bash -c 'GITHUB_WEBHOOK_SECRET="$ENVOY_GITHUB_WEBHOOK_SECRET" exec bash scripts/smoke/up.sh'
```

The `secrets` command injects `ENVOY_GITHUB_WEBHOOK_SECRET`, so a wrapper must map it to the public rig interface name `GITHUB_WEBHOOK_SECRET` without printing it. In `none` mode, `up.sh` builds the Envoy listener binary, starts an isolated NATS container at `127.0.0.1:14222`, waits for listener health at `127.0.0.1:19020/healthz`, proves the listener accepts a locally signed GitHub `ping`, prints the explicit webhook-ingress block, then launches the daemon against the Dispatch server named by `DISPATCH_URL`/`DISPATCH_TOKEN`. The daemon starts its controller, but no Dispatch issue event ever reaches the rig NATS, so the root issue `up.sh` creates is never admitted: a `none` run exercises start-up, the fail-closed OMP probe, controller spawn, and checkpoint 13, and `checkpoints.sh` blocks 1–4 and 12 with the Dispatch-ingress reason. The daemon uses installation tokens minted from the GitHub Apps. In `forward` mode, the same local listener assertion runs before `gh webhook forward` and its GitHub hook registration; the rig waits for the forwarder's `Forwarding Webhook events from GitHub...` tunnel-ready signal before launching the daemon with its normal Bun command (`forward` relays live GitHub events only — Dispatch issue events still never reach the rig NATS, so checkpoints 1–4 and 12 stay blocked exactly as in `none` mode):

In `envoy` mode the bridge starts only after the local daemon is healthy, then subscribes to `notifications.github.<owner>.<repo>.>` and `notifications.dispatch.issue.>` (every Dispatch issue project; the daemon's own reducer filters to its own project by key prefix) on `SMOKE_UPSTREAM_NATS` and republishes each raw NATS message onto the same subject in the rig's isolated NATS. The source chain is GitHub → the production `<github-app-slug>` app → Sami's Envoy receiver → production NATS → bridge → isolated rig NATS. The bridge has no upstream publish path, relays the configured repository's GitHub events and every Dispatch issue project's events, and preserves the incoming NATS message rather than synthesizing a new envelope. It validates the first envelope against the daemon contract; a legacy shape logs exact missing and extra fields, marks the bridge unhealthy, and refuses to send that message to the daemon.

```sh
bun run packages/daemon/src/cli/index.ts start <owner>/<board-number> --config /path/to/legion.yaml
```

The generated configuration uses `omp_invocation: mise x github:sjawhar/oh-my-pi@18.1.15-sami.20260908-220934 -- omp` and `repos: [$SMOKE_REPO]` (required by `config.ts` for credential routing, PR lookups, and workspace provisioning). Before accepting work, the daemon asks `mise env --json` for the complete tool environment, resolves absolute `jj`, `git`, `gh`, and `tmux` paths, and resolves that pinned OMP binary with `mise where` — unless `LEGION_OMP_PATH` is set, which `up.sh` always sets for the daemon (see "Which OMP build the rig runs" below). Its tmux panes receive the resulting full `PATH` and execute the resolved OMP path directly. Startup probes that exact OMP executable for `pi.agents`; it refuses to start before opening NATS or its API if the probe or any required tool fails.

Set `LEGION_MISE_PATH`, `LEGION_JJ_PATH`, `LEGION_GIT_PATH`, `LEGION_GH_PATH`, `LEGION_TMUX_PATH`, or `LEGION_OMP_PATH` to an absolute executable path when a tool cannot be discovered. `omp_invocation` must use the `mise x <tool> -- omp` form; set `LEGION_OMP_PATH` when selecting a direct OMP binary.

**Which OMP build the rig runs.** `up.sh` picks the OMP binary during preflight (`resolve_omp_path`) and exports it as `LEGION_OMP_PATH` in the daemon's environment only. If `LEGION_OMP_PATH` is already exported it must name an absolute executable file, otherwise `up.sh` stops before starting anything and names the path. If it is unset, `up.sh` uses the build production runs, `${XDG_STATE_HOME:-$HOME/.local/state}/legion/sjawhar-legion/omp/omp-18.1.15-sami.9bff2014-rpcfix`, when that file is executable. If neither exists, `up.sh` stops naming the default path and both remedies (export `LEGION_OMP_PATH`, or retire the default as described below). The generated `legion.yaml` keeps `omp_invocation: mise x <pin> -- omp` because the daemon's loader requires that form; its `environment.ts` honours `LEGION_OMP_PATH` over it, so every controller, root, and worker pane runs the selected binary, which `up.sh` prints as `GREEN OMP build: <path>`. The default is a stop-gap: the pinned release predates oh-my-pi `fix/rpc-extension-send-rejection` (commit `0e57a6ca`), without which a headless controller crashes on its first prompt with `send did not invoke the agent`. Retiring it takes two edits once a tagged release carries the fix: raise `omp_pin` and remove the default branch of `resolve_omp_path`, leaving only the `LEGION_OMP_PATH` override. `resolve_omp_path` never consults the pin, so raising it alone changes nothing — `up.sh` would still demand the file at the default path.

The generated configuration also sets `omp_launch_prefix` from `SMOKE_OMP_LAUNCH_PREFIX`
(default `secrets ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY --`): the daemon prepends this
argv to every OMP invocation it builds — root, worker, and controller panes, and the two startup
capability probes above — so provider credentials come from `secretsd` inside the pane process
rather than the daemon's own environment (a daemon-spawned architect otherwise has no Anthropic
key and silently falls back to a different, possibly quota-exhausted model).

### Fail-closed OMP probe

To prove the daemon rejects an OMP runtime without `pi.agents`, point `LEGION_OMP_PATH` at an **absolute path** to an older OMP binary and run the rig with `LEGION_OMP_AGENTS=missing`. The daemon must refuse before state, NATS, or its API starts.

Do not use `LEGION_OMP_INVOCATION=omp` as this negative test: the daemon rejects an unpinned invocation. Use the explicit `LEGION_OMP_PATH` override above.

The daemon health check is `http://127.0.0.1:19370/legion/v1/state`. Its state, generated configuration, process IDs, and logs live in `/tmp/legion-smoke` by default; set `SMOKE_DIR` to use another location. `NATS_PORT`, `ENVOY_PORT`, and `LEGION_DAEMON_PORT` override the scratch defaults. `up.sh` refuses to start when any configured port is already occupied, except for a live process recorded in its own PID file and matching Linux `/proc/<pid>/stat` start time. Re-running `up.sh` reuses only those verified rig processes and the `legion-smoke-nats` container. In `envoy` mode, `envoy-bridge.log` records readiness, the first-envelope validation verdict, every forwarded subject, and byte size.

`SMOKE_WEBHOOK_EVENTS` overrides the supported repository-webhook event list in `forward` mode. The default includes `issues`, `issue_comment`, `sub_issues`, `pull_request`, `pull_request_review`, and `check_run`.

When `SMOKE_BRANCH_PROTECTION=1` is set, the rig configures `main` to require one approving review, using the user-authenticated `gh` identity described above. Legion itself never reads or writes a human-approval signal: whether a human must approve before merge is the sandbox repository's own rule, and checkpoint 8 verifies that the merge queue respected it.

Tear down the processes, the private tmux server (`tmux -L legion-<slug>`), and NATS container with:

```sh
bash scripts/smoke/down.sh
```

Keep `SMOKE_REPO` and `SMOKE_PROJECT` exported for teardown so it can close the named tmux session.

Legion panes never appear in your own `tmux list-sessions`; attach with `tmux -L legion-<slug> attach -t legion-<slug>` where `<slug>` is `SMOKE_PROJECT` lower-cased with every non-alphanumeric character removed (`example-org/24` → `exampleorg24`).

`down.sh` also succeeds in `none` mode: absent forwarder process and hook records are ignored while the listener, daemon, tmux session, and NATS container are stopped. In `envoy` mode it first stops the start-time-validated bridge PID, preventing new upstream envelopes from reaching the local daemon during teardown.


## Checkpoints

Run the numbered assertions during the end-to-end exercise:

```sh
secrets DISPATCH_TOKEN -- bash scripts/smoke/checkpoints.sh <1-13>
```

`DISPATCH_TOKEN` is a secret: the `secrets` wrapper injects it only for this one invocation and never persists it under `SMOKE_DIR` (the same wrapper the start command above uses for `up.sh`). `DISPATCH_URL` is not a secret and must already be exported in the shell, exactly as in "Start and stop" above. Checkpoint 13 needs no `DISPATCH_TOKEN`; run it bare.

Each invocation exits nonzero on a failed observable and prints one `CHECKPOINT <n> OK` line on success. A human-controlled gate that is unavailable prints `CHECKPOINT <n> SKIPPED-BLOCKED` and exits 3 rather than reporting a false green. Checkpoints 1–4, 9, and 12 read the root issue `up.sh` recorded at `${SMOKE_DIR}/root-issue`; checkpoint 5 infers the Legion pull request from `gh pr list` where possible. Set the listed variable when a later exercise has more than one candidate, or when checkpoints run against a `SMOKE_DIR` `up.sh` never populated. Such a directory also has no recorded webhook mode: `checkpoints.sh` reads `${SMOKE_DIR}/webhook-mode`, falls back to an exported `SMOKE_WEBHOOK_MODE` (`envoy`, `forward`, or `none`), and otherwise stops naming the missing file — it never guesses a mode, because the mode decides which checkpoints are reachable at all.

With a recorded `SMOKE_WEBHOOK_MODE=none`, checkpoints 1–4 and 12 are blocked: no Dispatch issue event reaches the rig NATS, so the daemon never admits the root issue (`resync.ts`'s `healStatusDrift` skips issue keys it has never ingested). Checkpoints 5–7 and 9–11 are blocked because they assert pull-request, check-run, issue-comment, or issue-event delivery. With a recorded `forward`, checkpoints 1–4 and 12 are blocked for the same reason — `gh webhook forward` provides live GitHub events only, never Dispatch issue events — while 5–7 and 9–11 are available. Checkpoints 8 and 13 are not gated by the mode. `envoy` is the only mode for checkpoints 1–4 and 12; `envoy` and `forward` both provide the live GitHub ingress that checkpoints 5–7 and 9–11 need.

### Required checkpoint sequence

After the reviewer cleanup, retro, and reviewer approval complete, run checkpoint 7 **before** asking for the human merge approval. It captures `main`'s base SHA under `SMOKE_DIR`; checkpoint 8 consumes that recorded value to prove the resulting merge is a squash onto that base.

Immediately before posting the revival-triggering PR comment, run:

```sh
bash scripts/smoke/checkpoints.sh arm-revival
```

Then post the comment and run checkpoint 10. The command captures the daemon-log offset at the trigger boundary, so older same-run events cannot satisfy the assertion.


| Checkpoint | Extra input when needed | Assertion |
| --- | --- | --- |
| 1 | `SMOKE_ROOT_ISSUE` optional | The root Dispatch issue has progressed past `triage`; daemon state records a controller window/pane locator for a live tmux window. |
| 2 | `SMOKE_ROOT_ISSUE` optional | The root Dispatch issue is `in_progress`, is admitted, and has a recorded architect window/pane locator for a live tmux window. |
| 3 | `SMOKE_ROOT_ISSUE` optional | Root has a posted primary `spec.md` artifact, its registered open design-gate ask offers `Approve`, and a Dispatch child issue exists. |
| 4 | `SMOKE_ROOT_ISSUE` optional | A child in a released lifecycle status is tracked in active admission or an active/queued tree. |
| 5 | `SMOKE_PR` optional | A Legion branch has implementation identity and `Legion-Session:` commit attribution. |
| 6 | `SMOKE_ARCHITECT_WINDOW`, `SMOKE_VERDICT_FRAGMENT`, `SMOKE_RAW_CHECK_FRAGMENT` | One architect verdict appears in the pane; raw check noise is absent. |
| 7 | `SMOKE_BRANCH_PROTECTION=1`, `SMOKE_PR`, `SMOKE_RETRO_COMMIT`, `SMOKE_REVIEWER_LOGIN` | Reviewer `.legion` deletion precedes its approval; retro is durable; final PR diff has no `.legion` path; records the pre-merge base for checkpoint 8. |
| 8 | `SMOKE_BRANCH_PROTECTION=1`, `SMOKE_PR`, `SMOKE_HUMAN_LOGIN` | Current head has human approval; merge is squash-shaped onto checkpoint 7's recorded base; main has no `.legion` tree. |
| 9 | `SMOKE_ROOT_ISSUE` optional | Dispatch reports the root issue and all its children as `done`. |
| 10 | `arm-revival`, `SMOKE_WORKER_WINDOW`, `SMOKE_ARCHITECT_WINDOW`, `SMOKE_COMMENT_FRAGMENT` | Daemon logs `no_holder → probe → revive` after the arm point; the worker receives the comment without architect consumption. |
| 11 | `SMOKE_RESURRECTION_ISSUE`, `SMOKE_RESURRECTION_ROLE`, `SMOKE_RESURRECTION_WORKER_SESSION`, `SMOKE_WORKER_WINDOW`, `SMOKE_CATCHUP_FRAGMENT` | Second live issue advances exactly one generation and its specific revived worker receives catch-up. |
| 12 | `SMOKE_ROOT_ISSUE`, `SMOKE_QUEUED_ISSUE` | The lingering root released its slot and the queued issue was promoted. |
| 13 | — | The private tmux server and every recorded pane (controller, roots, workers) carry no `DISPATCH_TOKEN=`, `LEGION_BOOT_TOKEN=`, or `LEGION_CONTROLLER_SECRET=` on argv or in environ; the server's global environment has none; the default server hosts no `legion-<slug>` session. |
