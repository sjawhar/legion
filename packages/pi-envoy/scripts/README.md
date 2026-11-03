# Pi Envoy diagnostic scripts

## smoke-delivery.sh

Real end-to-end delivery smoke test against the *installed* pi-envoy
plugin. It launches a real, interactive `omp` session (TUI mode, never `-p`)
in a scratch tmux session and a throwaway directory outside any repo
checkout, so the only extension that loads is whatever is materialized at
`~/.omp/plugins/node_modules` — never a local source checkout via a repo's
own `omp.extensions` manifest. The session runs on the smoke's own tmux
server, on a socket in that throwaway directory, and the script prints the
`tmux -S <socket> attach -t <session>` line that watches it. It drives the
session with `tmux send-keys` to
call `envoy_role_set` and wait for a message, publishes to that role's
namespaced topic through the live Envoy HTTP API, asserts the session wrote
the exact payload to disk, then kills the session and asserts teardown: the
tmux session disappears and the role stops resolving (404) within 15 seconds.

### Usage

```bash
cd packages/pi-envoy/scripts && ./smoke-delivery.sh
```

Env overrides:

- `ENVOY_URL` — Envoy listener base URL (default `http://127.0.0.1:9020`).
- `OMP_BIN` — the `omp` binary to run (default `omp`, resolved on `PATH` —
  the dotfiles wrapper that injects provider auth).

Run from a host with:

- A live Envoy listener reachable at `ENVOY_URL`, with NATS behind it.
- The `pi-envoy` plugin installed for every `omp` session on the
  machine, materialized at
  `~/.omp/plugins/node_modules/@sjawhar/pi-envoy`. The script fails
  fast and names the path if that package.json is missing, is not valid
  JSON, or has no `.version` — the smoke is meaningless without the real
  installed artifact.
- A configured model provider: the script makes a real model call inside the
  spawned session, so it costs tokens and needs live credentials.

CI does not run this script — `.github/workflows/ci.yaml` has no
step for it, and none should be added while these prerequisites are unmet in
CI's sandbox.

## smoke-btw.sh

Real targeted Dispatch smoke against the installed plugin. It starts one isolated OMP TUI session
whose initial message (the prompt is `omp`'s argument, not typed keystrokes) asks for one bash
command that creates a sentinel file and then sleeps 45 seconds, waits for that session's exact
work directory to appear in the live listener registry and for the sentinel to exist, then targets
only that session with a message on an existing open Dispatch issue while the sleep is still
running. A correlated reply must appear on the same issue before the script tears down the session
and confirms its registry entry clears. It never targets a role or an existing session. The
spawned session is pointed at the same Dispatch the script talks to (`DISPATCH_URL`, plus a 0600
token file when the script holds a bearer), so its reply lands where the script looks for it.

### Usage

```bash
cd packages/pi-envoy/scripts && MODE=btw DISPATCH_SMOKE_ISSUE=CORE-1 ./smoke-btw.sh
```

Set `MODE=btw` (the default) for an ephemeral reply, or `MODE=steer` to prove the primary-turn
delivery path. `ENVOY_URL` defaults to `http://127.0.0.1:9020` and `OMP_BIN` defaults to `omp`.
`DISPATCH_SMOKE_ISSUE` names the open issue that will receive the smoke card.

Who asks: by default the script is an agent. It asks the Dispatch a command run from this shell
resolves, with that command's bearer
([`scripts/e2e/lib/operator-dispatch.sh`](../../../scripts/e2e/README.md#liboperator-dispatchsh),
which runs `bun` and needs the checkout's dependencies installed): `DISPATCH_URL`,
`DISPATCH_TOKEN_FILE` and `DISPATCH_TOKEN` over the `dispatch` section of the envoy.json
`envoyConfigPath("command")` chooses, whose token goes only to the server written beside it and
whose `!` token runs. It names itself in the message's `actor` as `SMOKE_ACTOR_SESSION` (default
`smoke-btw-<hex>`; pass your own Envoy session id when you run it from a session), so the card on
the issue is visibly agent-authored. With no Dispatch configured it refuses, saying why; any other
refusal of the client's (a file's token that `DISPATCH_URL` names another server for, a token file
that cannot be read) stops it naming the reason. Set `DISPATCH_AUTH_HEADER` to one
human-authenticated HTTP header (the current Dispatch cookie or a trusted identity header) to ask as
a human instead, exactly as the dashboard would: `actor` is then omitted, and the script asks
`DISPATCH_URL`, default `http://127.0.0.1:8766`.

The script requires live Envoy, Dispatch, and model credentials; it is never a fake-listener test or
a CI step. A 404 from the targeted message path prints `SKIP` and exits zero, meaning the server does
not support targeted messages. A `400 ACTOR_KIND` on a bearer run prints `SKIP` too, meaning
the server requires a human-authenticated targeted message; use a compatible server or pass
`DISPATCH_AUTH_HEADER`.

### Timeouts

Session registration and post-kill teardown bound infrastructure round trips only.
Delivery (90s) is generous by comparison because that window spans real Envoy delivery
plus the time for a live model turn to notice the targeted message and post the correlated
Dispatch reply. Every wait is a wall-clock deadline (not a fixed iteration count), so a
blackholed Envoy fails at its stated timeout instead of quietly stretching it.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | `PASS:` delivery, correlated reply, and teardown verified; or `SKIP:` server lacks this smoke path. |
| 1 | Any assertion failed — see the `FAIL:` line for which one, plus a captured pane or HTTP status. |

Cleanup (`tmux kill-session`, `rm -rf` the temp dir) runs in an exit trap on
every exit path, so a failed run leaves nothing behind either. Both smokes run
their tmux on a private socket in their temp dir, so the kill can end only the
session the smoke started; on the shared default server it could end anyone's.

## smoke-replay.sh

Live proof that a session shows once each Envoy message it missed while its NATS subscription was
down. It runs one real `omp --mode rpc` session with this checkout's plugin
(`extensions/envoy.ts`, loaded with `--no-extensions -e`), against a real Envoy listener and a
real JetStream NATS, and the session reaches NATS only through a `socat` proxy the script owns:

- **Leg 1:** once the session is registered, the proxy is killed, and natsd's monitoring endpoint
  (`/connz?subs=1`) shows the session's connection and its `notifications.agent.<id>` subscription
  gone. A Dispatch-shaped envelope (`event_id` = `dedupe_key` = `dispatch-<n>`, source `dispatch`)
  is published through JetStream to that subject with the outbox's `Nats-Msg-Id`
  (`<dedupe_key>:<topic>`), and `STREAM.MSG.GET` reads it back from `ENVOY_NOTIFICATIONS`. The
  proxy comes back; within two heartbeats the session file holds exactly one `envoy-message` entry
  carrying the event id, and still one three heartbeats later: the replay's card, whose last line
  says the message was published while the session's connection was down.
- **Leg 2:** omp stops, another envelope is published, and omp resumes the same session file: that
  event shows once within two heartbeats of the session's return, still once three heartbeats
  later, and leg 1's is still once.
- **Leg 3, cards held by a streaming turn:** a fresh session starts a long turn (writing 1 to 1200
  in words), and omp's RPC `get_state` reports `isStreaming: true` before anything is published.
  Two envelopes are published live; the proxy is then killed, two more are published and the
  proxy comes back. `get_state` shows omp holding all four cards in memory for the still-streaming
  turn (`queuedMessageCount` 4), the session file holds no `envoy-message` entry for any of them
  (`card=0` each), and omp is SIGKILLed. omp resumes the same session file; once the resumed
  session has gone idle (`isStreaming: false`, nothing queued), each of the four events has exactly
  one entry (`card=1` each), still one three heartbeats later. A card counts as shown only once
  omp writes it, so the cards the kill dropped come back from the stream.
- **Leg 4, cards an interrupt withdrew:** in a fresh session (legs 5 and 6 reuse it), three
  envelopes are published live while a long turn streams, and omp holds their cards
  (`queuedMessageCount` 3, `card=0` each). RPC `abort_and_restore_queue` (what Escape does at the
  terminal) withdraws them: omp is idle with nothing queued and has written none of them. The same
  process shows each once within two heartbeats (the plugin hands a card again once omp is idle,
  holds no message and the card is still unwritten), still once when the session is idle and three
  heartbeats later: no restart is needed to get them back.
- **Leg 5, a plain abort:** the same hold, then RPC `abort`: omp keeps the cards and writes them
  in the run it starts at once. Each shows once within two heartbeats and still once three
  heartbeats on, so the plugin hands none of them a second time.
- **Leg 6, a switch away and back (local mode only):** the same hold, then RPC `new_session`. The
  new session answers a first prompt (so its file exists) and three heartbeats later holds none of
  the three cards. The listener is stopped, RPC `switch_session` goes back to the first session
  (`get_state` confirms it), a long turn starts, and the listener comes back: a heartbeat's sweep
  hands the cards again, and omp holds all three for the streaming turn, unwritten. omp is
  SIGKILLed and resumes the same file; once idle each shows once, still once three heartbeats
  later, and legs 4 and 5's events are still once each. The switch dropped the cards unwritten, so
  the plugin released them rather than recording them shown. The listener is down across the
  switch so that the switch's own sweep cannot hand the cards to the idle session before the turn
  streams; remote mode cannot stop the listener, so it skips this leg and says so.
- **Leg 7, a BTW replayed while a turn streams:** in a fresh session the proxy is killed and a
  Dispatch BTW (a person's message to the session, `message.created` with `delivery.mode: btw`) is
  published to it. A long turn starts and the proxy comes back, so the replay sweep finds the BTW
  mid-turn. omp steers any card handed to a streaming turn into that turn, whatever `triggerTurn`
  says, and runs another model turn for it, so the plugin holds the inert card until omp is idle
  with nothing queued. For three heartbeats of the turn `get_state` shows nothing queued and the
  session file holds no card for the BTW. The turn ends on its own; within two heartbeats exactly
  one entry carries the BTW's body and the inert note ("runs no side turn and needs no answer"),
  still one three heartbeats later. omp's RPC output holds one `agent_start` and one `turn_start`
  since the long turn's prompt, and no assistant message follows the card: no turn ran for it.
  Remote mode sends the BTW through `/v1/messages/send`, which the listener takes only for a
  session that advertises `btw`. The mid-turn claim (the card not handed while the turn streams)
  is checked in local mode only: there natsd's monitoring shows the session resubscribed inside
  the check window, so the replay sweep ran mid-turn. Remote mode cannot see that, so a remote
  pass of leg 7 shows only that one inert card appeared after idle with no turn run for it.
- **Negative control, same run:** leg 1 again with a session running the released plugin
  (`SMOKE_REPLAY_CONTROL_PLUGIN`) ends with no entry for its publish, and a frame published live
  afterwards shows once, so that session was connected and writing cards the whole time.
- **Negative control for leg 3, same run:** the released plugin through leg 3's live half: two
  envelopes published live while a turn streams, held by omp (`queuedMessageCount` 2), SIGKILLed
  unwritten. Five heartbeats after the resume, once the session is idle, at least one of them has
  no entry: that plugin keeps no record of a card omp holds in memory, so the kill loses it.
- **Negative control for leg 4, same run:** the released plugin through leg 4: three envelopes
  published live while a turn streams, held by omp (`queuedMessageCount` 3), then RPC
  `abort_and_restore_queue`. Five heartbeats on, once the session is idle, none of them has an
  entry: omp withdrew them unwritten and that plugin never hands them again in the same process.

Each step prints what it observed, and every matched transcript entry is printed as a
`transcript:` line.

### Usage

```bash
bash packages/pi-envoy/scripts/smoke-replay.sh
```

Local mode (the default) builds natsd (`packages/envoy/internal/testnats/cmd/natsd`, started with
`-js -store_dir <scratch> -m <port>`) and the listener (`packages/envoy/cmd/listener`) from the
checkout and runs them on loopback ports of its own. The session gets `ENVOY_HEARTBEAT_MS=5000`.

Env overrides:

- `SMOKE_REPLAY_ENVOY_URL` and `SMOKE_REPLAY_NATS_URL` (both, `nats://host:port`) — remote mode:
  run against an existing listener and NATS, building nothing. Each envelope goes through the
  listener's `/v1/messages/send`, and the listener's `/v1/replay` shows the stream stored it, so
  the run also proves the route and its grant there. The script cannot stop a remote NATS or read
  its monitoring endpoint: it breaks the session's connection by killing the local proxy that
  fronts the remote NATS (leg 3 included), and leg 2 SIGKILLs omp, since `/v1/messages/send`
  needs the registration a clean exit drops. Legs 4, 5 and 7 and the leg 4 control run as in
  local mode; leg 6 needs the listener stopped and does not run. This is the production check
  after a rollout.
- `SMOKE_REPLAY_ENVOY_TOKEN_FILE` — remote mode only: a file holding the listener's bearer, sent
  from a 0600 header file and handed to the session as `ENVOY_TOKEN_FILE`.
- `SMOKE_REPLAY_CONTROL_PLUGIN` — the released plugin the control runs (default
  `/opt/legion/pi-envoy/dist/envoy.js`).
- `SMOKE_REPLAY_MODEL` — the session's model (default `anthropic/claude-haiku-4-5`).
- `SMOKE_REPLAY_PROVIDER_ENV` — the space-separated variables holding that model's credentials,
  the only ones of the caller's shell the session gets (default `ANTHROPIC_API_KEY`).
- `SMOKE_REPLAY_OMP` — the `omp` binary (default `omp` on `PATH`).

Prerequisites, each checked before anything starts: `go` (local mode), `bun` with the checkout's
dependencies installed, `socat`, `setsid`, `pgrep`, `omp`, `curl`, `jq`, and every variable
`SMOKE_REPLAY_PROVIDER_ENV` names. Every model call is real and costs tokens: one prompt per
session start and one turn per card, and legs 3 to 7 and the controls of legs 3 and 4 each start
a long answer (about a thousand lines, cut short by the kill, interrupt, abort or switch, and
left to finish in leg 7), which a resumed session may write again.

No process it starts inherits the caller's environment: each gets `PATH`, the provider variables
(omp only) and what the script sets — a scratch `HOME` holding the session's `envoy.json` (the
listener, the proxy as its only NATS server, Dispatch off), `TMPDIR`, `OMP_SESSION_STORAGE=file`
— so an operator's `ENVOY_*`, `NATS_*`, `DISPATCH_*`, `LEGION_*`, `OMP_*` or `PI_*` variables
never reach it, and the credentials never appear on a command line. Everything lives in one
scratch directory under `$TMPDIR`; an exit trap stops every process the script started (by pid;
the proxy by the process group it alone runs in) and removes that directory after a pass, keeping
it, with each session's RPC log and session file, after anything else.

### Timing

A local run takes about seven minutes (measured on omp 18.12.2 with the default model, Go modules
cached): about 10 s to build and start the services, 25 s for leg 1, 20 s for leg 2, 35 s for
leg 3, 30 s for leg 4, 20 s for leg 5, 45 s for leg 6, 95 s for leg 7 (most of it the long turn
running to its end), 45 s for each control. Each wait is bounded: registration 90 s, a replayed
card two heartbeats (10 s), the long turn's start 30 s, omp holding the live cards two heartbeats
and the replayed ones (leg 3, and leg 6 once the listener is back) four, a card after leg 4's
interrupt or leg 5's abort two heartbeats, a session going idle after them, and leg 7's long turn
ending, 300 s, and the resumed session of legs 3 and 6 300 s to show every card and 300 s more to
go idle.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | `SMOKE REPLAY PASS:` legs 1 to 6 showed each event exactly once (leg 6 in local mode only), leg 7 showed its BTW once as an inert card after the turn and ran no turn for it, the control showed none of its missed publish, the leg 3 control lost at least one of its live cards, and the leg 4 control showed none of the cards its interrupt withdrew. |
| 1 | `SMOKE REPLAY FAIL (<step>):` an observation failed; the line names the step and what was seen. |
| 2 | Refused: a prerequisite is missing, the configuration is invalid, or the model provider did not answer the session's first prompt (its error is printed). |
| 129, 130, 143 | Interrupted by SIGHUP, SIGINT or SIGTERM; cleanup still ran. |

## smoke-role-takeover.sh

Live proof that a soft role claim naming a predecessor takes the role only from a predecessor
that is gone or is the claimant's own client instance (dispatch://INFRA-404, R8 and A5 of its
spec). It is the only smoke here that needs neither a model provider nor a deployment: `go` and
`bun` alone, so CI runs it as a step of the `envoy-session-stall` job.

It builds natsd (`packages/envoy/internal/testnats/cmd/natsd`) and the listener
(`packages/envoy/cmd/listener`) from the checkout and runs both on loopback ports of its own, as
[`packages/envoy-client/test/envoy-stack.ts`](../../envoy-client/test/envoy-stack.ts) does, and
drives them with real client processes:

- one Bun process per Oh My Pi Envoy extension instance (`role-takeover/omp-client.ts`), each
  loading this checkout's `extensions/envoy.ts` against the stand-in host the package's own suite
  uses — one session manager whose id and branch the driver moves, and a heartbeat the driver
  ticks rather than a timer. One process is one client instance, with one client instance id;
- the real Claude Code channel server (`packages/claude-envoy/bin/envoy-channel.ts`), spawned and
  driven over MCP by `role-takeover/claude-client.ts`, which is also the parent process whose
  handoff file a `/clear` writes, so the server adopts the new session id on its own.

Every client reaches the listener through a recording proxy of its own
(`role-takeover/listener-proxy.ts`), so each request and the listener's answer to it is read from
a log rather than inferred, and a request can be held: the first scenario below holds a fork's
role claim back until another process has resumed and registered its predecessor.

One line per row of R8's table is printed with its outcome:

- **row 1** — a claim under the id that holds the role keeps it: an Oh My Pi plugin reload, and a
  Claude Code channel server restart, each under the session id that holds the role.
- **row 2** — an Oh My Pi fork gets its parent's role, because the parent's live entry carries the
  claimant's own client instance id.
- **row 3** — an Oh My Pi move, in one process, to a session whose records name the held role gets
  it, by the same case.
- **row 4** — a Claude Code `/clear` handoff gets the role: the deregistration removes the
  process's own entry, and the claim then finds no live predecessor.
- **row 5** — each of those after another process resumed the predecessor id and registered it
  last is refused `409` naming the predecessor, and that process keeps the role. The fork case is
  the spec's A5 scenario 1 and the `/clear` case its A5 scenario 2.
- **A5** — a refused process stays registered, and after the holder exits the refused Oh My Pi
  process takes the role on its next session event.

### Usage

```bash
bash packages/pi-envoy/scripts/smoke-role-takeover.sh
bash packages/pi-envoy/scripts/smoke-role-takeover.sh --base-rev main@origin   # must fail
```

`--base-rev <rev>` builds only the listener from that revision, leaving every client as it is in
the working copy, which is the negative control: against `main@origin` row 5's three cases fail,
because that listener takes a role from any predecessor a claim names, live or not. It reads the
revision without touching the working copy (`jj --ignore-working-copy log` resolves it, `git
archive` extracts `packages/envoy`), moves no bookmark and writes nothing outside its scratch.

Prerequisites, each checked before anything starts: `go`, `bun` with the checkout's dependencies
installed, `curl`, `jq`, and `jj`, `git` and `tar` for `--base-rev`. Everything the run writes —
the two binaries, the JetStream store, each client's home and state directory, every proxy's
request log — lives in one `mktemp -d` under `$TMPDIR`, which an exit trap removes after a pass
and keeps after anything else. Nothing it starts inherits this shell's environment (`env -i` plus
`PATH` and what the script sets), so an operator's `ENVOY_*`, `NATS_*` or `DISPATCH_*` variables
never reach a client. A local run takes about a minute.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | `SMOKE ROLE TAKEOVER PASS:` every row held. |
| 1 | `SMOKE ROLE TAKEOVER FAIL:` the rows that did not hold are listed, each with the check that failed and what it saw. |
| 2 | Refused: a prerequisite is missing, the configuration is invalid, or a client or service did not start. |
| 129, 130, 143 | Interrupted by SIGHUP, SIGINT or SIGTERM; cleanup still ran. |

## smoke-registration-stall.sh

Manual live smoke for the registration-stall fix (dispatch://INFRA-246): a real headless `omp`
pane against a real Envoy listener, watched through the live registry. `--arm stall` shows that a
pane whose registration post is wedged forever keeps refreshing past one record lifetime and says
so exactly once; `--arm recover` shows that a pane unregistered while its process runs
re-registers itself. `--control` is each arm's negative control (the stall control needs
`--base-rev <rev>`, the plugin as it was then).

```bash
# against a deployment
ENVOY_URL=<listener> ENVOY_TOKEN_FILE=<0600 file> \
  bash packages/pi-envoy/scripts/smoke-registration-stall.sh --arm recover
# against a listener this run starts itself
bash packages/pi-envoy/scripts/smoke-registration-stall.sh --arm recover --local-stack
```

`--local-stack` builds natsd and the listener from the checkout and runs both on loopback ports of
its own, as `smoke-replay.sh` does, so the run needs no deployment and no bearer: it ignores
`ENVOY_URL` and `ENVOY_TOKEN_FILE`, says so, and keeps both out of the pane it spawns. Its one
remaining live dependency is the model provider the pane's turn needs, which the pane inherits
from the caller's environment. `--heartbeat-ms <n>` shortens the pane's refresh interval, which
every budget the run prints is measured in. Its scratch is a `mktemp -d` under `$TMPDIR` that an
exit trap removes. Exit 0 is `SMOKE <arm> PASS:`, 1 is `SMOKE <arm> FAIL:` naming the observation
that failed, and 2 is a refusal.

## local-stack.sh

The loopback Envoy stack the three smokes above bring up, in one sourced file: a free loopback
port below the kernel's ephemeral range (`port_range`, `port_open`, `pick_port`), the natsd and
listener builds (`build_natsd`, `build_listener`, each taking the `packages/envoy` directory to
build from, which is how `smoke-role-takeover.sh --base-rev` builds the listener from another
revision), the argv and assignments each service is run with (`set_natsd_argv`, `set_listener_env`)
and when it is up (`natsd_serving`, `listener_healthy`, polled with `poll_until`). It is sourced,
never run: it starts no process, so every script keeps its own isolation, logs, traps, timeouts,
messages and exit codes. A caller defines `refuse` and sets `local_stack_fail` to the function an
exhausted port range is reported through (`refuse`, or `smoke-replay.sh`'s `fail`).
`packages/envoy-client/test/envoy-stack.ts` is the same stack for the TypeScript suite.

## skill-scenarios/rig.sh

Replays skill scenarios on real agents, to compare two versions of a skill. Each label is one
checkout's packed plugin in an isolated Oh My Pi home (`rig.sh profile <label> <checkout>`); each
run is one fresh `omp -p` agent given one task a skill rule governs, scored afterwards from what the
stand-ins and the scratch Dispatch recorded (`rig.sh score`). `rig.sh live-read <label> <skill>...`
has one session read every file of a skill and reports whether each arrived whole.

```bash
cd packages/pi-legion/scripts/skill-scenarios
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic ./rig.sh profile head <checkout>
# or, with no gateway: LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file>
# or, through a provider block of your own models.yml whose apiKey is `!cat <absolute file>`:
#   LEGION_E2E_MODEL_ROUTE=provider LEGION_E2E_MODEL_PROVIDER_FILE=<models.yml> LEGION_E2E_MODEL_PROVIDER=<block>
./rig.sh batch ask-on-message 5 head base   # starts and stops Postgres, NATS, the listener, and each run's own scratch Dispatch
./rig.sh batch tester-proof 5 head base
./rig.sh score
```

The spec-writing and spec-rewrite scenarios measure how an agent writes a Dispatch spec:

```bash
bun fetch-spec-sources.ts --out "$SKILL_SCENARIOS_WORK/sources" <issue key | dispatch://PROJECT/artifact/slug>
SKILL_SCENARIOS_SUPERPOWERS=<superpowers plugin> ./rig.sh profile main <checkout>
SKILL_SCENARIOS_SOURCE=<directory under sources/> ./rig.sh batch spec-writing 1 main
./rig.sh judge "$SKILL_SCENARIOS_WORK"/runs/spec-writing-main-1
./rig.sh score spec-writing
```

The scenarios and every input are in the script's header (`./rig.sh` with no arguments prints it),
and each scenario's scoring rule is on the `score.ts` function that scores it. It needs a model
route (the gateway, an Anthropic key file, or a provider block of the operator's own models.yml)
and Docker; every container a batch or a judge starts has CPU and memory limits.
`SKILL_SCENARIOS_WORK` must have no `.omp` in it or above it, so not under your home directory: Oh
My Pi would load that directory's plugins and not the label's. Its bounded waits, port picks and
process stops are
[`scripts/e2e/lib/rig.sh`](../../../scripts/e2e/README.md#librigsh)'s. Nothing a batch runs reaches
production Dispatch, GitHub, Envoy or NATS: it starts nothing with the caller's `DISPATCH_*`,
`ENVOY_*` or `NATS_*` variables, and `seed.ts` refuses to run while `PLAYWRIGHT_BASE_URL` or
`E2E_AGENT_TOKEN` is set. That is the environment only: an agent reads the whole filesystem, the
other label's checkout included, so `score.ts` does not score a run that read outside its own
label. A judge is held tighter: `rig.sh judge-omp` runs each one in a Docker container that sees
only its own directory, read-only but for its one answer file and its transcript, so a judge
cannot read the run's transcript or another judge's input. On the key-file route the container
reads the key file itself. On the gateway route the gateway's key command runs on the host, once
for each judge, because it needs the operator's home and session bus. The container gets only the
token, from an owner-only file that is removed when the judge ends. On the provider route the
profile's models.yml is the block as the operator wrote it, `!cat` command included, so no key is
copied anywhere: the container mounts the directory of the file that command reads, read-only.

Two tools read production Dispatch, with GET requests only, as the calling session does (its
envoy.json, or `DISPATCH_URL` with a token): `fetch-spec-sources.ts`, which writes a source's input
into the work directory for the spec scenarios, and `label-comments.ts`, which labels the comments
people wrote in a window and prints the share that complain about writing. Neither writes inside a
checkout. Text either one reads from production stays in the work directory and is never committed.
