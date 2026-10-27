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

CI does not run this script — `.github/workflows/pr-and-main.yaml` has no
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
./rig.sh batch ask-on-message 5 head base   # starts and stops the scratch Dispatch, Postgres, NATS, listener
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
route (the gateway or an Anthropic key file) and Docker. `SKILL_SCENARIOS_WORK` must have no `.omp`
in it or above it, so not under your home directory: Oh My Pi would load that directory's plugins
and not the label's. Its bounded waits, port picks and process stops are
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
token, from an owner-only file that is removed when the judge ends.

Two tools read production Dispatch, with GET requests only, as the calling session does (its
envoy.json, or `DISPATCH_URL` with a token): `fetch-spec-sources.ts`, which writes a source's input
into the work directory for the spec scenarios, and `label-comments.ts`, which labels the comments
people wrote in a window and prints the share that complain about writing. Neither writes inside a
checkout. Text either one reads from production stays in the work directory and is never committed.
