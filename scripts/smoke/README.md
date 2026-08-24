# Legion smoke rig

This rig runs the real NATS broker, Envoy listener (including its GitHub webhook receiver), Envoy Dispatch service, GitHub webhook forwarding, Legion daemon, and the pinned oh-my-pi binary that the daemon places in tmux windows. It targets a dedicated GitHub sandbox repository and Projects V2 board.

## Prerequisites

Install the GitHub CLI webhook extension once:

```sh
gh extension install cli/gh-webhook
```

The active `gh` identity must administer `SMOKE_REPO`, create pull requests, create labels, and change branch protection. Both Legion GitHub Apps must be installed on the sandbox repository.

`up.sh` requires these non-secret inputs:

| Variable | Meaning | Default |
| --- | --- | --- |
| `SMOKE_REPO` | Dedicated repository as `<owner>/<repo>`. | Required. |
| `SMOKE_PROJECT` | Dedicated Projects V2 board as `<owner>/<number>`. | Required. |
| `LEGION_IMPLEMENT_APP_ID` | Numeric implementation App ID. | `3202636` |
| `LEGION_REVIEW_APP_ID` | Numeric reviewer App ID. | `3202653` |
| `LEGION_APP_LOGINS` | Comma-separated GitHub bot logins for both Legion Apps. | `legion-implementer[bot],legion-reviewer[bot]` |
| `LEGION_DISPATCH_BEARER` | Explicit installation token for daemon calls to Dispatch. | A fresh implementation-App installation token minted from `GH_AGENT_APP_PRIVATE_KEY_B64`. |

It also requires these secret inputs:

| Variable | Meaning |
| --- | --- |
| `GITHUB_WEBHOOK_SECRET` | Secret configured for the GitHub webhook forwarded to Envoy. |
| `GH_AGENT_APP_PRIVATE_KEY_B64` | Base64-encoded implementation App private key. |
| `GH_REVIEW_APP_PRIVATE_KEY_B64` | Base64-encoded reviewer App private key. |

Provide secrets with the `secrets` wrapper rather than writing a `.env` file. The private keys stay in the process environment; `up.sh` writes only `private_key_command` references into its generated daemon configuration. The webhook forwarder inherits `GITHUB_WEBHOOK_SECRET` from its environment; the rig never passes that secret through a process argument.
### Human-controlled gates

| Variable | Required for | Behavior when absent |
| --- | --- | --- |
| `SMOKE_PROJECT_ID` | Board ingress | `up.sh` starts the real services with an empty board filter and prints `SKIPPED-BLOCKED`; checkpoints 1–4 exit 3 with the exact missing-Project reason. Set it to the sandbox Project V2 node ID (`PVT_…`) after the board exists. |
| `SMOKE_BOARD_SCOPE` | Project V2 ingress transport | Defaults to `org` for organization owners and starts a second org-scoped `projects_v2_item` forwarder. Set `none` for a personal board; repository hooks cannot carry that event, so checkpoints 1–4 remain blocked. |
| `SMOKE_BRANCH_PROTECTION=1` | Merge gate | `up.sh` configures branch protection and runs the reviewer-App approval measurement only when explicitly armed. Without it, it prints `SKIPPED-BLOCKED`; checkpoints 7–8 exit 3 with the exact missing-ruleset reason. |

The sandbox repository includes the 20-second `ci` check and the `.fail-me`-controlled `fail-on-demand` workflow. Both Legion Apps are installed account-wide for `sjawhar`; no per-repository install step is required.


## Start and stop

```sh
export SMOKE_REPO=sjawhar/legion-smoke
export SMOKE_PROJECT=sjawhar/<board-number>

secrets ENVOY_GITHUB_WEBHOOK_SECRET GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 -- \
  bash -c 'GITHUB_WEBHOOK_SECRET="$ENVOY_GITHUB_WEBHOOK_SECRET" exec bash scripts/smoke/up.sh'
```

The `secrets` command injects `ENVOY_GITHUB_WEBHOOK_SECRET`, so a wrapper must map it to the public rig interface name `GITHUB_WEBHOOK_SECRET` without printing it. `up.sh` builds the Envoy listener and Dispatch binaries, starts an isolated NATS container at `127.0.0.1:14222`, waits for listener health at `127.0.0.1:19020/healthz`, starts Dispatch at `127.0.0.1:18766/healthz`, starts `gh webhook forward` and waits for its `Forwarding Webhook events from GitHub...` tunnel-ready signal, then launches the daemon with its normal Bun command:

```sh
bun run packages/daemon/src/cli/index.ts start <owner>/<board-number> --config /path/to/legion.yaml
```

The generated configuration uses `omp_invocation: mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp`. Before accepting work, the daemon asks `mise env --json` for the complete tool environment, resolves absolute `jj`, `git`, `gh`, and `tmux` paths, and resolves that pinned OMP binary with `mise where`. Its tmux panes receive the resulting full `PATH` and execute the resolved OMP path directly. Startup probes that exact OMP executable for `pi.agents`; it refuses to start before opening NATS or its API if the probe or any required tool fails.

Set `LEGION_MISE_PATH`, `LEGION_JJ_PATH`, `LEGION_GIT_PATH`, `LEGION_GH_PATH`, `LEGION_TMUX_PATH`, or `LEGION_OMP_PATH` to an absolute executable path when a tool cannot be discovered. `omp_invocation` must use the `mise x <tool> -- omp` form; set `LEGION_OMP_PATH` when selecting a direct OMP binary.
### Fail-closed OMP probe

To prove the daemon rejects an OMP runtime without `pi.agents`, point `LEGION_OMP_PATH` at an **absolute path** to an older OMP binary and run the rig with `LEGION_OMP_AGENTS=missing`. The daemon must refuse before state, NATS, or its API starts.

Do not use `LEGION_OMP_INVOCATION=omp` as this negative test: the daemon rejects an unpinned invocation. Use the explicit `LEGION_OMP_PATH` override above.

The daemon health check is `http://127.0.0.1:19370/legion/v1/state`. Its state, generated configuration, process IDs, and logs live in `/tmp/legion-smoke` by default; set `SMOKE_DIR` to use another location. `NATS_PORT`, `ENVOY_PORT`, `DISPATCH_PORT`, and `LEGION_DAEMON_PORT` override the scratch defaults. `up.sh` refuses to start when any configured port is already occupied, except for a live process recorded in its own PID file and matching Linux `/proc/<pid>/stat` start time. Re-running `up.sh` reuses only those verified rig processes and the `legion-smoke-nats` container.
`SMOKE_WEBHOOK_EVENTS` overrides the supported repository-webhook event list. The default includes `issues`, `issue_comment`, `sub_issues`, `pull_request`, `pull_request_review`, and `check_run`; GitHub rejects `projects_v2_item` on repository hooks, so Project V2 ingress remains gated by `SMOKE_PROJECT_ID`.

Before reporting `RIG READY`, the rig creates exactly these sandbox labels:

- `needs-approval`
- `human-approved`
- `legion-child`
- `legion-backlog`

When `SMOKE_BRANCH_PROTECTION=1` is set, the rig configures `main` to require one approving review, opens a disposable pull request, approves it through the reviewer App, and reads `reviewDecision`. If GitHub reports `APPROVED`, the rig adds the existing `legion-human-approval` status check to branch protection; otherwise it leaves that check unrequired. The disposable pull request number and result are recorded under the smoke directory.

Tear down the processes, tmux session, disposable protection probe, and NATS container with:

```sh
bash scripts/smoke/down.sh
```

Keep `SMOKE_REPO` and `SMOKE_PROJECT` exported for teardown so it can close the disposable protection probe and the named tmux session.

## Checkpoints

Run the numbered assertions during the end-to-end exercise:

```sh
bash scripts/smoke/checkpoints.sh <1-12>
```

Each invocation exits nonzero on a failed observable and prints one `CHECKPOINT <n> OK` line on success. A human-controlled gate that is unavailable prints `CHECKPOINT <n> SKIPPED-BLOCKED` and exits 3 rather than reporting a false green. Checkpoints infer the root issue and Legion pull request from daemon state where possible. Set the listed variable when a later exercise has more than one candidate:
### Required checkpoint sequence

After the reviewer cleanup, retro, and reviewer approval complete, run checkpoint 7 **before** asking for the human merge approval. It captures `main`'s base SHA under `SMOKE_DIR`; checkpoint 8 consumes that recorded value to prove the resulting merge is a squash onto that base.

Immediately before posting the revival-triggering PR comment, run:

```sh
bash scripts/smoke/checkpoints.sh arm-revival
```

Then post the comment and run checkpoint 10. The command captures the daemon-log offset at the trigger boundary, so older same-run events cannot satisfy the assertion.


| Checkpoint | Extra input when needed | Assertion |
| --- | --- | --- |
| 1 | `SMOKE_PROJECT_ID`, `SMOKE_ROOT_ISSUE` optional | The daemon records a sandbox issue and the controller tmux window exists. |
| 2 | `SMOKE_PROJECT_ID`, `SMOKE_ROOT_ISSUE` optional | The root is admitted and its architect window exists. |
| 3 | `SMOKE_PROJECT_ID`, `SMOKE_ROOT_ISSUE` optional | Root issue has a posted spec and `needs-approval`; a `legion-child` exists. |
| 4 | `SMOKE_PROJECT_ID`, `SMOKE_ROOT_ISSUE` optional | A child is marked released in daemon state. |
| 5 | `SMOKE_PR` optional | A Legion branch has implementation identity and `Legion-Session:` commit attribution. |
| 6 | `SMOKE_ARCHITECT_WINDOW`, `SMOKE_VERDICT_FRAGMENT`, `SMOKE_RAW_CHECK_FRAGMENT` | One architect verdict appears in the pane; raw check noise is absent. |
| 7 | `SMOKE_BRANCH_PROTECTION=1`, `SMOKE_PR`, `SMOKE_RETRO_COMMIT`, `SMOKE_REVIEWER_LOGIN` | Reviewer `.legion` deletion precedes its approval; retro is durable; final PR diff has no `.legion` path; records the pre-merge base for checkpoint 8. |
| 8 | `SMOKE_BRANCH_PROTECTION=1`, `SMOKE_PR`, `SMOKE_HUMAN_LOGIN` | Current head has human approval; merge is squash-shaped onto checkpoint 7's recorded base; main has no `.legion` tree. |
| 9 | `SMOKE_ROOT_ISSUE`, `SMOKE_ARCHITECT_LOGIN`, `SMOKE_SIGNOFF_FRAGMENT` | Architect sign-off closes a lingering root. |
| 10 | `arm-revival`, `SMOKE_WORKER_WINDOW`, `SMOKE_ARCHITECT_WINDOW`, `SMOKE_COMMENT_FRAGMENT` | Daemon logs `no_holder → probe → revive` after the arm point; the worker receives the comment without architect consumption. |
| 11 | `SMOKE_RESURRECTION_ISSUE`, `SMOKE_RESURRECTION_ROLE`, `SMOKE_RESURRECTION_WORKER_SESSION`, `SMOKE_WORKER_WINDOW`, `SMOKE_CATCHUP_FRAGMENT` | Second live issue advances exactly one generation and its specific revived worker receives catch-up. |
| 12 | `SMOKE_ROOT_ISSUE`, `SMOKE_QUEUED_ISSUE` | The lingering root released its slot and the queued issue was promoted. |
