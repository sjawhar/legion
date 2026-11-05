# Deploying the Legion daemon on a shared box

This runbook covers a shared-box deployment: the daemon, its tmux server, and every pane it
launches run as the same Unix user on one machine.
This provides no filesystem boundary between the daemon and its panes.
Kubernetes deployments (`docs/kubernetes.md`) get a pod boundary instead.

The daemon gets GitHub App installation tokens from the secrets broker; the App private keys
stay in the broker.
Raw key secrets are not used.
Configure each App with `app_id` and `token_command`, as [the App section](#the-legion-implementer-and-legion-reviewer-apps) shows.

`secrets` and `agent-secrets` are two CLIs over two stores, and a name can exist in both.
`secrets` is secretsd, shipped from `sjawhar/forward`: it reads its agent tier from local sops
files and its human tier through the local secretsd daemon, which asks for a YubiKey touch.
`agent-secrets` reads the secrets broker, which is backed by a cloud Secret Manager and releases
installation tokens for `github-app` secrets.
The App configuration below uses `agent-secrets`; `provider_keys` can still read secretsd keys.

## How the daemon launcher gets tokens

Start the daemon inside an `agent-secrets` session, so every `token_command` it runs inherits that session:

```sh
export AGENT_SECRETS_URL=<the broker URL>
agent-secrets register --wait 10 --exec -- legion start …
```

The `agent-secrets` helper must be running, with the machine login a person approves through `agent-secrets machine login` ([Log a machine in](site/src/content/docs/broker/guides/log-a-machine-in.md)).
That login must be allowed to request both Apps' broker secrets.
A daemon started from a plain shell is no session: each `token_command` then fails with `NOT_A_SESSION` ([Troubleshooting](site/src/content/docs/broker/guides/troubleshooting.md)).

## The `legion-implementer` and `legion-reviewer` Apps

The `sjawhar` account owns both Apps.
The secrets broker holds them as `github-app` secrets, which release installation tokens and never the App's private key:
`LEGION_IMPLEMENT_APP` (agent tier) and `LEGION_REVIEW_APP` (human tier: each new grant waits for a person's approval in Dispatch, and a later release under the same live grant reuses it).
A daemon that holds no App key uses them through `token_command`, as the live proofs do (`scripts/e2e/lib/github-apps.sh`, `app_token_command`):

```yaml
github_apps:
  implement:
    app_id: "5341240"
    token_command: "agent-secrets LEGION_IMPLEMENT_APP --reason=legion-daemon-github-app-token ${LEGION_GITHUB_OWNER:+--github-owner \"$LEGION_GITHUB_OWNER\"} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s \"$LEGION_IMPLEMENT_APP\"'"
  review:
    app_id: "5341243"
    token_command: "agent-secrets LEGION_REVIEW_APP --reason=legion-daemon-github-app-token ${LEGION_GITHUB_OWNER:+--github-owner \"$LEGION_GITHUB_OWNER\"} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s \"$LEGION_REVIEW_APP\"'"
```

The daemon runs the command with `sh -c` whenever it needs a lease: at boot, and again once fewer
than 20 minutes remain on a lease it holds.
Its environment is the daemon's, less the NATS seeds, with two variables the daemon sets:
`LEGION_GITHUB_OWNER`, the account whose installation token the daemon wants (empty for every
installation the command may release, which the boot's read of the App's login asks for), and
`LEGION_GITHUB_READ_ONLY`, `1` for a read-only token and empty otherwise.
Its stdout must be the broker's release,
`{"installations":{"<account login>":{"installation_id":<id>,"token":"ghs_…","expires_at":"<RFC 3339>"}}}`,
and the daemon takes the owner's entry, matched without regard to case.
An owner the command releases nothing for is an App not installed there.
With no key, the daemon reads each App's bot login from GraphQL's `viewer` under one of its
tokens, refuses a token whose `GET /apps/<slug>` names an id other than `app_id`, and reads whether
the App is installed on a repository from that owner's installation's repository list.
A read-only token holds what the command's read-only release gives: the broker's holds
`contents: read`, `metadata: read`, `pull_requests: read` and `actions: read`, the same set a
configured key mints, and the first three alone where the App's installation has not been granted
Actions.
The daemon passes the command's stderr through as it runs, with every GitHub token in it redacted,
so `agent-secrets`' line naming the pending request appears in the daemon's log while the review
App waits for its grant.
A run is not cut off by the boot's 15-second attempt: the boot waits for it, logging that it waits,
and the next attempt joins the same run, so a grant that waits on a person's approval is one
request; `agent-secrets`' exit 75 (still waiting when its own wait ended) is retried.
`legion start --check-config` never runs it, and `installations` cannot be set beside it, since
the release names every installation.

Each App needs its own `app_id` and `token_command`, with no second credential source.
See [Rotating broker-held App keys](#rotating-broker-held-app-keys) for the rotation procedure.

## What a pane's `gh`, `git` and `jj` are

A tmux pane's `PATH` is the daemon's own with the state directory's `bin/` put first: its `legion`
launcher and its `gh`, a script that runs `legion gh`. Nothing else of Legion's is on it, so the
`git` and `jj` a pane runs are whatever the daemon's `PATH` resolves. The daemon resolves at boot a
jj of 0.38 or later — the jj bound it refuses to boot below (`resolveTools`, naming
`LEGION_JJ_PATH`, its own override for the jj it runs) — and the real `gh` (`LEGION_GH_PATH`, or the
first `gh` on its `PATH`, refused when that is a script), which every pane is told as `LEGION_GH_PATH`. A pane's GitHub identity is
a set of files that binary reads, one directory per GitHub owner of the project: the daemon writes
gh's own `hosts.yml` (an App installation token, user `x-access-token`) and `config.yml` under
`<state_dir>/secrets/<claim>-gh/<owner>` (0700, files 0600) when it opens the pane — the role's App
token for the issue's repository's owner, and that App's token narrowed to reading contents, pull
requests, Actions and metadata for every other owner (an owner whose installation has not been
granted Actions gets the other three) — names the issue owner's directory as `GH_CONFIG_DIR`
and the bundle's root as `LEGION_GH_OWNERS_DIR`, with `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` set
empty so nothing outranks the files, and rewrites each owner's `hosts.yml` in place every minute
from its cached leases (the log lines `tmux runtime: github credential written` and `github
credential refreshed`, with the claim, role, App, `expiresAt` and the owners written); a pane it
adopted after a restart is refreshed for the owner the daemon records for its claim, never one read
from the pane's directory. `legion gh` runs the real gh with `GH_CONFIG_DIR` at the directory of the
owner the command targets, read as gh reads it, with pflag's rules, from the real gh's own help of
each command down to its leaf (`-R`/`--repo` before or after the command, a repository or owner
argument, before `--` or as a clone's or fork's first after it, a `github.com` URL argument — for a
`pr` command or `agent-task view` only a pull request's, since gh reads any other as a branch of
the command's own repository — every one of a slot gh's usage takes several of, `agent-task view`'s
`OWNER/REPO#N`, a `repos/<owner>/…` endpoint of `gh api` or a search endpoint's `q`, a search
query's plain `repo:`/`owner:`, the options naming a repository, an owner, a team or an issue by
URL). The command's own repository counts too whenever gh resolves it, because no argument names
it, and for `repo set-default`, which reads every remote's network, whatever names it: `GH_REPO`
for a command that reads it (`gh api`'s `{owner}` and `:owner` placeholders among them), and
otherwise every `github.com` remote and `gh-resolved` setting of the working directory, read with
gh's own URL parser, whatever the scheme. A command naming no owner, in a directory whose remotes
name none, runs at the issue owner's. A command whose last `--help` is true, and `gh --version`
alone, only print. It fails closed: a command it cannot read for certain (an option the leaf's
help does not list or names a repository with in a way it does not know, a subcommand gh does not
know, a first word gh's own help does not list, anything before the command but `-R` and
`--help`, an argument the leaf's usage has no place for or names in a way it does not know, a
search qualifier gh or GitHub could read otherwise, a gh alias or an extension, run or named as
`gh help`'s target, whose help it never reads since reading it runs them, `gh copilot` and `gh
extension exec`), one naming two owners (an option or a second repository naming one owner while
gh acts on another's, or a workspace whose remotes name two), one naming an owner the bundle
holds no directory for, every command where `PATH`'s first `gh` is not Legion's wrapper (a script
that execs `legion gh` by its absolute path), and every command under a `GH_HOST` other than
`github.com` or a set `GH_TOKEN` or `GITHUB_TOKEN` are refused with exit 2, naming the argument,
and send nothing. The real gh runs with `GH_PATH`
naming the pane's wrapper, so the git gh runs itself (a clone, the fetch of a fork's parent, `pr
checkout`) authenticates through the helper gh names after it, `legion gh auth git-credential`,
which answers as `legion git-credential` does, with `credential.useHttpPath` on for `github.com`;
and with gh's update check and prompts off. The saved help its tests read is the image's gh's,
with that gh's whole command reference, every argument and every repository-naming option of
which the wrapper's tables know (`packages/daemon/cmd/legion/testdata`, with the script that
regenerates it when that pin moves; `.github/scripts/check-gh-version.sh` fails the build until
the saved help is the pin's).
The shared clone's `credential.helper` is `!legion git-credential` with `credential.useHttpPath`
on, so a pane's `git` and `jj git push` read the hosts.yml of the owner in the path git asks about
and nothing else. The real gh is a compiled program, so when the first `gh` on the daemon's `PATH`
is a script (a `gh` wrapper of the box's own, as the devbox's login `PATH` heads with, or asdf's
shim), boot refuses it, naming it and `LEGION_GH_PATH` (`RefuseScriptGH`): run as the real gh, a
wrapper would answer in place of the owner's App token. Set `LEGION_GH_PATH` to the real gh's
install path (`mise which gh` or `asdf which gh` prints it). The check reads scripts only: mise's
shim is compiled and passes, and where mise has no gh for the directory it runs the next `gh` on
`PATH`, which in a pane is the pane's own `legion gh`, so a shims directory is no real gh either.

The daemon runs tmux itself: the first `tmux` on its `PATH`, with the pane environment, whose XDG
directories are under the state directory. A mise shim, or a wrapper script that hands the call to
one, then does not read the operator's mise config as its global config. Started from a directory
under the operator's `HOME`, it finds that config as a parent directory's config with no trust
record and exits 1, and boot refuses with `scrub the private tmux server's environment: … not
trusted`. Put a real tmux binary first on the daemon's `PATH`
(`mise which tmux` prints its install path), as the live proofs do with `real_bin_path`
(`scripts/e2e/README.md`).

## What a pane's Dispatch credential is

A pane's Dispatch bearer is the daemon's own. The daemon reads `dispatch_token_file` once at boot
(`readBoot`, `packages/daemon/internal/daemon/bootreads.go`) and writes the value to
`<state_dir>/secrets/dispatch-token`, a 0600 file in a 0700 directory, with
`WriteDispatchTokenFile` (`packages/daemon/internal/runtime/secretfiles.go`, called from
`prepareTmux` in `packages/daemon/internal/daemon/daemon.go`). It then tells every pane
`DISPATCH_TOKEN_FILE` naming that file (`panePairs`,
`packages/daemon/internal/runtime/tmux/spawn.go`). The value never enters tmux's argv or a pane's
environment. So a pane holds the daemon's own credential, not one of its own.

The Envoy listener bearer reaches a pane the same way. It is a launch secret, `ENVOY_TOKEN`
(`launchSecrets`, `packages/daemon/internal/daemon/launchsecrets.go`), and the tmux runtime writes
every launch secret to a 0600 file of its own under `<state_dir>/secrets` and tells the pane the
matching `<NAME>_FILE` pointer, here `ENVOY_TOKEN_FILE` (`secretFiles`, `WriteSecretFiles` and
`panePairs`, `packages/daemon/internal/runtime/tmux/spawn.go`).

When `dispatch_token_file` holds one of the deployment's shared agent tokens
(`DISPATCH_AGENT_TOKEN`), Dispatch matches the bearer against them (`MatchesSharedAgentToken`,
`packages/envoy/internal/dispatch/auth/agent_token.go`) and reads no person, no service and no
project from it. Whatever the bearer, the acting session is the one the request body names
(`bearerSessionActor`, `packages/envoy/internal/dispatch/api/server.go`). Dispatch takes any other
bearer as a service token or a personal token (`optionalActor`, the same file), and every pane's
session then carries that token's service or owner as well. `dispatch whoami` in a pane tells the
two apart: for a shared agent token it answers
`Session <id> runs under the shared token: no owner, …`
(`packages/envoy-client/src/dispatch-execute.ts`). A pod's side of this is in `docs/kubernetes.md`,
"Anatomy of a Sandbox pod". A person's own session on the same box uses a personal token instead,
minted from Dispatch's Settings → Agent tokens
(`docs/site/src/content/docs/dispatch/for-agents.md`).

The agent-secrets helper holds a credential of its own: the machine login a person approves for
`agent-secrets machine login`. The helper spends that credential on the broker's own routes
(`packages/envoy/internal/broker/helper/broker.go`) and asks the broker for no Dispatch token. The
broker can exchange such a login's access token for one (`docs/kubernetes.md`, "The Dispatch
identity the broker can issue"), and nothing on this box does that at this release.
`dispatch://INFRA-267` is where the daemon and its workers get Dispatch tokens of their own, a dev
box session writing under an approved machine login included, and `dispatch://INFRA-264` is where
Dispatch accepts a broker token.

## Phase workers on Claude Code

`worker_backends` in `legion.yaml` runs a role on another agent harness than Oh My Pi:

```yaml
worker_backends:
  implementer: claude
```

Each key is an issue role (`architect`, `planner`, `implementer`, `tester`, `reviewer`, `merger`)
and each value `omp` or `claude`; a role left out runs on Oh My Pi. What each harness may run is
one table, `runtime.Backends` (`packages/daemon/internal/runtime/backend.go`): Claude Code runs the
phase workers alone, since an architect is woken by notices on its Envoy role topic and Claude
Code's print mode delivers no channel notification, and it runs them under either runtime — a tmux
pane here, and an issue pod under `runtime: kubernetes`, whose worker image carries the
claude-envoy plugin and Claude Code's pin, from which the pod installs and verifies the pinned
Claude Code at every launch (`docs/kubernetes.md`). The loader refuses a role on a harness that does
not run it, naming both, a role named twice, and any other value. The
controller always runs on Oh My Pi.

For such a role the pane's shim runs `legion claude-bridge -- claude -p --input-format stream-json
--output-format stream-json --verbose --replay-user-messages --permission-mode bypassPermissions
--strict-mcp-config …`. With `--replay-user-messages` Claude Code re-emits each user message it
takes, and the bridge answers an Envoy delivery only then, so claude-envoy counts the delivery
taken only once Claude Code has it.
The claim's own Claude Code directory, `<state_dir>/claude/<claim token>`, is the pane's
`CLAUDE_CONFIG_DIR`: it holds the transcript, the MCP configuration the daemon writes (one server,
`legion`, running claude-envoy's `dist/envoy-channel.js` on bun) and that server's phase-reminder
state file, one per session,
and it outlives every launch of the claim. A fresh launch passes `--session-id <new id>`; a
relaunch passes `--resume <id>`, read from the transcript the claim registered, so a killed pane
comes back as the same Claude Code session. A role moved to Claude Code since its session was
recorded is refused rather than resumed, naming the Oh My Pi session file. A role moved back to Oh
My Pi gets its recorded Claude Code transcript passed to `omp --resume`, as every recorded session
file is; the pinned Oh My Pi refuses it, exiting 1 with `Cannot resume session "<file>": the
session header is missing or malformed. The file was not modified.`, and leaves its bytes as they
were, so the launch fails loudly rather than starting a fresh agent
(`packages/daemon/internal/runtime/tmux/resume_omp_test.go` holds the real binary to this).

tmux's boot resolves three things from the daemon's environment, and `legion start --check-config`
makes the same refusals:

- `LEGION_CLAUDE_PATH`, the `claude` executable, else the first `claude` on `PATH`;
- `LEGION_BUN_PATH`, the `bun` the MCP server runs on, else the first `bun` on `PATH`;
- `LEGION_CLAUDE_PLUGIN_DIR`, the absolute path of a built `packages/claude-envoy`: it must hold
  `dist/envoy-channel.js` and the generated Legion plugin's `legion/.claude-plugin/plugin.json`.
  A checkout of a commit on `main`, never of a branch, already holds both (`AGENTS.md`, "Deploys
  come only from main"): main's Rebuild Claude Envoy plugin workflow commits them after each merge.
  `bun run check-dist` in that package confirms them. A checkout taken after a merge and before a
  rebuild of it lands (at most one run of that workflow behind its inputs, its runner queues
  included) holds an earlier build: build the plugin from it with `bun scripts/build.ts --out
  <dir>` in that package, into a directory outside the checkout, and point
  `LEGION_CLAUDE_PLUGIN_DIR` at `<dir>` until the rebuild commit lands. That leaves the checkout's
  tracked files as they are, so its next `git pull` fast-forwards.

A missing one refuses the boot, naming every gap in one line. The three are tmux's alone: a pod
resolves nothing. Its bun and its claude-envoy root are the worker image's, at fixed paths
(`/usr/local/bin/bun`, `/opt/legion/claude-envoy`), which the runtime treats as image-owned so no
operator mount can hide them. Its `claude` is not the image's: the image carries Claude Code's pin,
`/opt/legion/claude-code`, and no Claude Code, since its licence grants no redistribution and the
image is public, so the pod's start script installs the pinned release at every launch into the
generation's own directory on the issue's volume, holds it to the lockfile's integrity and the
pinned version, and fails the launch, loudly, when the fetch fails or does not verify. The launch
shapes both runtimes build from those paths — the agent command, the session id and transcript
rules, the MCP document and Claude Code's own variables — are one package,
`packages/daemon/internal/claudelaunch`; the pod's side of it is in `docs/kubernetes.md`.

The daemon names no model for Claude Code under either runtime. In a pane its key reaches it the
way an Oh My Pi pane's does: `provider_keys` (for example
`ANTHROPIC_API_KEY: <secretsd key>`) is exported by the shim into `legion claude-bridge`'s
environment, which Claude Code inherits, and into every Oh My Pi pane's as well. A key meant for the
Claude Code roles alone goes in the claim's directory instead, before the claim first launches: a
`settings.json` there whose `apiKeyHelper` prints the key, as `scripts/e2e/claude-backend-tmux.sh`,
the live proof, writes it. In a pod the route is the operator's the same way: `provider_keys`
carries `ANTHROPIC_API_KEY`, and `ANTHROPIC_BASE_URL` and `ANTHROPIC_MODEL`, which are not
credentials, go in `runtime.kubernetes.pod.env` (`docs/kubernetes.md`, "Configuration").

### How a harness plugs in

Every harness runs behind `legion worker-shim`, which speaks Oh My Pi's RPC frames to it
(`legion claude-bridge` translates them for Claude Code), so the worker stream and supervision do
not know which harness a process runs. A harness is four pieces in the daemon:

- its row in `runtime.Backends`: the roles it runs, the runtimes that launch it, the daemon prompt
  part its launches are told after their role's parts (Claude Code's is
  `internal/prompts/go/claude-mechanics.md`);
- a `Launcher` in `internal/runtime/tmux`: the command the shim runs, the extra `-e` pairs of its
  pane, and the marker its pane's command line holds;
- a boot resolver in `internal/daemon/launchers.go`, which finds what its launch needs on the host;
- the Kubernetes runtime's own branch in `internal/runtime/sandbox`, which builds the same launch
  from the worker image's paths instead: the pod's agent command, the files its launcher writes,
  the variables its start command tells the role, and the rule for the session a replacement pod
  resumes.

Boot resolves a launch for each harness `worker_backends` uses (`legion start --check-config` makes
the same refusals), and the tmux runtime refuses a launch on a harness it has no launcher for. A
pane is its recorded process only while its root process's command line, `sh -c` on the whole pane
command, holds a configured harness's marker: `omp` for Oh My Pi, ` claude-bridge -- ` for Claude
Code. A daemon with no `worker_backends` has Oh My Pi's launcher alone and checks for `omp` alone.

## The `runtime.tmux` block

`runtime: tmux` (or no `runtime` key) runs every pane with what the runtime tells it and the
allow-listed part of the daemon's environment (`HOME`, `OMP_PROFILE`, `PATH`, locale, proxies). The
mapping form adds the host's own needs:

```yaml
runtime:
  tmux:
    pane_env:                       # plain variables every pane gets, after the runtime's own
      ELAN_HOME: /opt/elan
      DISPATCH_PUBLIC_URL: https://dispatch.example.com
      LEDGER_DSN_FILE: /etc/ledger/dsn   # a _FILE pointer names a file and is allowed
    pi_config_files:                # Oh My Pi overlays, in order, in every pane's PI_CONFIG_FILES
      - /etc/legion-operator/overlay.yml
    take_over: kubernetes           # only while moving a project onto tmux (below)
provider_keys:
  ANTHROPIC_API_KEY: file:/etc/legion/providers/ANTHROPIC_API_KEY
  OPENAI_API_KEY: OPENAI_API_KEY_PROD    # a secretsd key, as before
```

- **`pane_env`** refuses `PATH` (the runtime composes it), every name the runtime sets on a pane
  (its own `LEGION_*` names, the XDG directories, the gh variables, `PI_CONFIG_FILES`,
  `LEGION_CONTROLLER`, `LEGION_SLACK_CHANNELS`), the variables it sets on every Claude Code pane
  (`CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_DISABLE_CRON`), the git
  identity a launch sets (`JJ_USER`, `GIT_AUTHOR_NAME`, …), the pointers to the secrets every launch
  gets as a file of its own (`ENVOY_TOKEN_FILE`, `NATS_NKEY_SEED_FILE`), the variables that place
  the session a resume reads (`OMP_SESSION_STORAGE`, `OMP_SESSION_SQL_DSN`,
  `OMP_SESSION_SQL_DSN_FILE`, `PI_CODING_AGENT_DIR`, `PI_CONFIG_DIR`), a provider key or its
  `_FILE` pointer, any credential-shaped name (`…_TOKEN`, `…_KEY`, `…_SECRET`, `…_SEED`,
  `…_PASSWORD`, `…_PAT`, `…_GRANT`, `…_CREDENTIALS`, `PRIVATE_KEY`), and any value holding a URL
  with a password in it (`scheme://user:password@…`). A value on tmux's command line is readable by
  every process of the host, so a secret reaches a pane as a file. The loader refuses these at load
  and the runtime again at boot, naming the variable and never its value. A launch whose own
  variable or secret pointer collides with a `pane_env` name is refused.
- **`pi_config_files`** paths resolve against `legion.yaml`'s directory and may not hold `:`.
  Legion adds no overlay of its own to the list: it writes none, so the operator's are the whole of
  it, with a role's own (`role_config_files`) after them. The
  boot plugin gate probes Oh My Pi under the same overlays and variables.
- **`role_config_files`**, a top-level key beside the block, gives one role's panes overlays no
  other role's get (`role_config_files: {implementer: [/etc/legion-operator/fallback.yml]}`, the
  controller's pane under `controller: daemon` too): such a pane's `PI_CONFIG_FILES` is
  `pi_config_files`, then the role's own in order, and nothing after them. Each
  path must be absolute; boot and `legion start --check-config` refuse one the host does not hold
  as a file the daemon's user can read, naming the role and the path, and a pane of a role the key
  leaves out is told what it was told without it. After the plugin gate passes, the boot gate runs
  `omp config get retry.modelFallback --json` under each named role's list and on each of its files
  alone, and refuses the boot when Oh My Pi exits non-zero (`boot gate: role_config_files.<role>:
  the <role>'s Oh My Pi would not start: Oh My Pi refuses <file>: error: …`), rather than letting
  every launch of that role fail. `docs/kubernetes.md` ("Operator configuration") has the rest of
  the key's rules. Every pane runs as the daemon's Unix user, so each role can read every role's
  overlay file; only loading is per role, so never put a secret in one.
- **`take_over: kubernetes`** lets this daemon take over the claims a daemon running the project
  under `runtime: kubernetes` recorded on the same store ([Moving a project onto
  tmux](#moving-a-project-onto-tmux)). `kubernetes` is the only accepted value. Without the key, a
  claim whose recorded process is a pod is refused, naming the claim and the key, and the boot does
  not start: two daemons must never drive one store's claims at once.
- **`provider_keys`** values of the form `file:/abs/path` are read once at boot, as the daemon,
  into the same daemon-held 0600 files a secretsd key lands in. The file is read by the rules every
  other secret file the daemon reads follows: one that cannot be read, is not a regular file, holds
  nothing, or is readable by its group or others refuses the boot, naming the variable and the
  file. Kubernetes refuses the form: a file on the daemon's disk reaches no pod.
- **`controller: daemon`** works under tmux too: the daemon launches the controller in a pane of
  its own window, `controller`, working in `<state_dir>/controller`, told `LEGION_CONTROLLER=1` and
  an empty `LEGION_SLACK_CHANNELS`, and supervises it like a root architect. It needs
  `dispatch_url`. A project with no GitHub owner to mint for runs it with gh acting as nobody, its
  gh directory empty.

## How workspaces are provisioned on the host

Each repository has one shared jj clone at `<state_dir>/repos/github.com/<owner>/<repo>`, and each
issue a jj workspace of it at `<state_dir>/workspaces/<owner>/<repo>/<issue, lowercased>`, on the
bookmark `legion/<ISSUE>`. The first provision of a repository clones it with the implement App's
installation token, handed to git through a one-shot 0600 credential helper under the state
directory. Every launch first makes sure its issue's workspace is there: a claim resumed,
relaunched or recovered, as well as a new one, is provisioned when its directory is missing or its
checkout never finished (it holds no sparse pattern, read with `jj sparse list
--ignore-working-copy`; a workspace an agent narrowed keeps its patterns), checking out
`legion/<ISSUE>` when GitHub has it. A launch waits for a provision or removal of the same issue
that is under way.

Provisions of different issues run side by side. Everything a provision does to the shared clone
runs under the clone's one store lock: a fetch of the default branch and the issue's branch alone,
the workspace's registration with no files (`jj workspace add --sparse-patterns empty`), its
bookmark, and the clone's git configuration. The checkout of the working copy (`jj sparse reset`
in the new workspace) writes only that workspace and runs with the lock released; at most 16
checkouts run at once, and a provision waiting on its repository's store lock holds no place
among them. A workspace whose checkout never finished is checked out by its next provision, which
its next launch runs. A removal takes the issue's turn and holds the store lock only for its jj
commands on the clone and the move of the workspace aside to `<dir>.removing`; the directory is
deleted after the lock is released, and a `<dir>.removing` a crash left is cleared by the issue's
next removal.

A store may be made before the daemon's first boot, with the same jj the daemon uses, as the
daemon's user: a colocated `jj git clone https://github.com/<owner>/<repo>` into the clone path,
then `jj -R <clone> --ignore-working-copy config set --repo git.abandon-unreachable-commits false`.
A directory there without `.jj` is removed and cloned again; one without `.git` is not a store.
Boot rewrites every store's credential helper to `legion git-credential`.

## Moving a project onto tmux

A tmux daemon can take over the store of a daemon that ran the project under `runtime: kubernetes`,
when its `legion.yaml` sets `runtime.tmux.take_over: kubernetes`. Without that key it refuses every
claim whose recorded process is a pod, and does not start. Nothing else stops two daemons from
driving one store at once — the instance lock is a file on each machine, and claim records are
written by whichever daemon wrote last — so the order matters:

1. Stop the Kubernetes daemon. Its issue pods outlive it: push every issue's unpushed work, then
   delete the project's Sandboxes (`kubectl -n <namespace> delete sandbox -l
   'legion.dev/project=<project>'`), which takes each pod and each pod's disk, and wait until none
   of the project's Sandboxes or pods is left. A pod still running beside the tmux daemon is a
   second agent on the same claim and branch.
2. Put each claim's session file where its pane's Oh My Pi keeps sessions, in a folder one level
   down, under its recorded file name (`<timestamp>_<session id>.jsonl`): either at the recorded
   path itself, or in the pane's own sessions directory. That directory is
   `<HOME>/.omp/agent/sessions`, or `<HOME>/.omp/profiles/<profile>/agent/sessions` when the
   daemon's environment or `pane_env` sets `OMP_PROFILE` or `PI_PROFILE`, or
   `<state_dir>/home/.local/share/omp[/profiles/<profile>]/sessions` when that directory already
   exists; a profile named only inside `omp_launch_prefix` is not read. A session the Kubernetes
   runtime kept in its session database (`runtime.kubernetes.session_store`) is not carried: no
   command writes its row out as a file, so its claim starts a fresh agent from the issue's pushed
   branch and committed handoffs.
3. Set `runtime.tmux.take_over: kubernetes` and boot the tmux daemon with the same Postgres. Each
   claim whose recorded process is a pod is reported gone, displaced, and relaunched in a pane,
   charged nothing, on its session when step 2 put its file in one of those places, searched by
   file name or session id. While `take_over` is set, a session found nowhere starts a fresh agent,
   uncharged, which registers a session of its own, whatever the claim's last process was and
   wherever the session was recorded: a claim the Kubernetes daemon left suspended carries no
   process, and a host built from the worker image records its own sessions at the paths a pod
   used, so neither tells the two apart. That is what the key buys and what it costs: in this
   window a session of this host's own that has gone missing starts fresh too. Each workspace is
   provisioned before its first launch.
4. Retry what failed to launch meanwhile, once the cause is gone: `legion claims retry --failed`
   (an architect relaunches on its session; a phase worker whose issue is held for its own role
   retries that phase).
5. Remove `take_over` once no claim can still wake on a session a pod recorded: every claim that
   was running has relaunched in a pane, and no claim the Kubernetes daemon suspended (a
   production check waiting for its deploy among them) is left to wake. From then on a session
   found nowhere is refused again, and a tmux daemon pointed at a store a Kubernetes daemon still
   drives refuses to start rather than take its claims.

## Rotating broker-held App keys

Rotate the keys in the broker's `github-app` secrets, not on the daemon's machine:

1. In each GitHub App's settings, generate a new private key and leave the old key active.

2. Update the matching broker secret, `LEGION_IMPLEMENT_APP` or `LEGION_REVIEW_APP`, as [Store the App key](site/src/content/docs/broker/guides/github-app-for-a-dev-box.md) shows:
   its value is the JSON document `{"app_id": …, "private_key": "<the new PEM>"}`, set with `agent-secrets secret set <NAME> < app-key.json`, never the bare PEM.
   Keep the secret's `github-app` kind and its GitHub owners.

3. Open a registered shell of your own (`agent-secrets register --wait 10 --exec -- sh`; the daemon's session cannot be entered from another terminal), then run each role's `token_command` from [the App section](#the-legion-implementer-and-legion-reviewer-apps) there, with `LEGION_GITHUB_OWNER` and `LEGION_GITHUB_READ_ONLY` unset, and confirm it prints the installations' tokens (`LEGION_REVIEW_APP` waits for a person's approval).
   Then confirm a fresh worker pane's `gh auth status` succeeds with account `x-access-token`.
   The daemon needs no restart: `token_command` gets fresh tokens as its cached leases near expiry.

4. Only then revoke the old private keys in GitHub App settings.
   Revoking them before the broker can mint with the new keys would break token refresh and,
   once existing tokens expire, the panes' `gh` and `jj git push` credentials.

## Serving on a socket the harness holds

`legion start` binds legion.yaml's `bind` and `port` for its API. With `LEGION_LISTEN_FD=<n>` in
its environment, it serves the API on descriptor `<n>` instead, a listening TCP socket it inherited
from whatever started it, and binds nothing. A test harness opens that socket on port 0, so the
kernel chooses the port, and holds it until the daemon serves on it; no other program can take the
port in between. A shared-box deployment leaves the variable unset.

- The file's `bind` and `port` must name the socket's address: `bind` an IP literal equal to the
  socket's IP (`0.0.0.0` matches only a socket on every IPv4 interface, and a host name matches
  nothing), `port` its port. The registry entry, the default `daemon_url` and `legion state` dial
  them. A socket they do not name, or a descriptor that is not a listening TCP socket, refuses the
  start before it takes the team or opens the store, naming `LEGION_LISTEN_FD` and the setting:
  `legion start: legion.yaml: port=8081 does not match LEGION_LISTEN_FD=3, a socket listening on 127.0.0.1:8080`.
- The daemon takes the socket over before it runs anything, the configuration's token commands
  included, and serves on a close-on-exec copy, then unsets the variable: no process it starts, its
  panes and tmux server among them, holds the socket or inherits the variable.
- The boot line `legion daemon started` names the address the daemon serves on, as it does when it
  binds.
- `legion start --check-config` binds nothing and never reads the variable.
