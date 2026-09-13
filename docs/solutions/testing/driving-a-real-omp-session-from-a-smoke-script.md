---
title: "Driving a real omp session from a smoke script: initial-message launch, tmux -e for env, a sentinel file for readiness, never send-keys or a pane regex or a process lookup"
category: testing
tags:
  - smoke-rig
  - tmux
  - omp
  - send-keys
  - pi-builtins
  - dispatch-api
  - btw
  - readiness
date: 2026-09-13
status: active
module: packages/pi-envoy/scripts
related_issues:
  - "sjawhar/legion#1060"
  - "sjawhar/legion#965"
  - "LEGION-2"
symptoms:
  - "tmux send-keys into a fresh omp pane prints nothing in the editor; the transcript has no user turn"
  - "`tmux send-keys -t <session> …` fails with `no current client` right after `new-session -d`"
  - "the target session posts its BTW reply to the wrong Dispatch (404 MESSAGE_NOT_FOUND) although DISPATCH_URL was exported before tmux new-session"
  - "pgrep/ps never see a `sleep 45` process while the omp pane shows the sleep running"
  - "a pane-text readiness grep fires on the typed prompt or the model's narration before the tool runs"
---

# Driving a real omp session from a smoke script

`packages/pi-envoy/scripts/smoke-btw.sh` starts one isolated omp TUI session in tmux and must
(1) put it into a long-running bash tool call, (2) have it talk to a Dispatch server of the
script's choosing, and (3) know the exact moment the tool call is blocked so a targeted
message provably lands mid-turn. Four approaches that look right each failed on this box in
2026-09-13's PR #1060 work; the pattern below is what held up across three consecutive
BTW and steer runs.

## Guidance

**Give omp the prompt as its argument, never as keystrokes.** `omp "<message>"` starts
interactive mode with that message as the first user turn (`omp --help`: `MESSAGES  Messages
to send`). `tmux send-keys` into a pane whose omp is still initialising (MCP servers
connecting, editor not yet mounted) is dropped silently — the transcript shows no user
turn — and on this box `send-keys -t <name>` issued right after `new-session -d` sometimes
failed outright with `no current client`. Neither is worth diagnosing when the argument
form exists.

**Pass environment into the pane with `tmux new-session -e KEY=VALUE`, not by exporting.**
When a tmux server already exists, a new session's environment comes from the server's
global environment, not from the shell that ran `new-session`. An exported `DISPATCH_URL`
never reached the plugin, which fell back to `~/.config/opencode/envoy.json` and posted its
reply to the production Dispatch, where the message id did not exist. Secrets still must
not ride argv (`/proc/<pid>/cmdline` is world-readable, the LEGION-6 rule): write the
token to a 0600 file under the run's workdir and pass `-e DISPATCH_TOKEN_FILE=<path>`;
`resolveDispatchConfig` reads `DISPATCH_TOKEN_FILE` ahead of `DISPATCH_TOKEN`. Pass curl
auth the same way: `-H @<0600 file>`.

**Readiness comes from a side effect of the command itself, not from what you can see.**
Two tempting signals are wrong:

- *Process lookup.* omp's bash tool runs `sleep` (and `touch`, and the rest of coreutils)
  through the in-process `pi-builtins` implementations, so `pgrep`/`ps` never see a
  `sleep 45` under the pane. A `pstree` of the pane while the sleep runs shows only omp and
  its MCP children.
- *Pane text.* A regex for the command matches the echoed prompt, and even with the prompt
  worded to avoid the literal, the model can narrate "I'll run `sleep 45`" before the tool
  starts.

Ask for **one** bash command that creates a sentinel and then blocks —
`touch ${workdir}/sleep-started && sleep 45` — and wait for the file. It exists exactly
while the tool call is in the sleep. The timing then doubles as evidence: BTW answered in
18 s (mid-sleep), steer in 66 s (waited for the 45 s turn boundary), which is the contract
difference between the two modes.

**Run the script from a stable copy while anyone may edit it.** bash reads a script
incrementally; an edit under a running script corrupts a later line (`syntax error near
unexpected token '||'`). A gate agent's `MODE=steer` run died this way while a sibling
agent was simplifying the file.

## Applicability

Any rig that drives a real omp session and needs a deterministic "the model is inside a
tool call now" point: BTW/aside/steer delivery smokes, hub/worker liveness checks, anything
that must interrupt a turn rather than queue behind it. The `-e` rule applies to every
tmux-launched process on a box with a long-lived server (that is every devbox here). The
builtin rule applies to any test that expects omp's bash tool to spawn a visible OS process
for a coreutil.
