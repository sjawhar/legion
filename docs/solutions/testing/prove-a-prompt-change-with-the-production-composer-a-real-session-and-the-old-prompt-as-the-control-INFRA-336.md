---
title: "Prove a prompt change with the production composer, a real session, and the old prompt as the control"
category: testing
tags:
  - role-prompts
  - behavioural-proof
  - negative-control
  - append-system-prompt
  - headless-session
  - issue-pod
date: 2026-10-31
status: active
module: packages/pi-legion/scripts/skill-scenarios
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# Prove a prompt change with the production composer, a real session, and the old prompt as the control

Extends docs/solutions/testing/role-prompt-and-skill-changes-are-proven-on-a-two-leg-scratch-rig.md,
whose two-leg rule still stands and whose full rig needs docker and tmux. An issue pod has
neither. This is the proof an issue pod can run: four steps, no rig, no daemon, no tmux.

- **A grep proves the text; only a session proves the behaviour.** An acceptance grep over the
  prompt files says the words changed. It says nothing about what an agent reading them writes.

- **Compose the prompt with the function the daemon uses, from the checkout under test.** Never
  assemble it by hand: the composition order, and the way the parts join into one
  `--append-system-prompt` value, are part of what a prompt change changes.

- **Run the identical task on the base checkout's composed prompt as the control, and show the
  control reproduces the defect.** A control on the same text proves nothing. The control is what
  turns "the agent behaved well" into "the change caused it".

- **Assert on the artifact the session wrote, not on what it said it did.**

- **Say what such a proof establishes:** a reproduction and a control, not a rate. Three roles and
  two labels is six sessions, not a measurement of how often an agent complies.

## The four steps, as INFRA-336 ran them

1. **Compose.** `workerPane()` in `packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts`
   takes a `daemonModule` — point it at `<checkout>/packages/daemon` — and with
   `systemPrompt: true` returns `systemPromptArgument`, the pane's whole
   `--append-system-prompt` argument as shell text. It runs that checkout's own
   `go run ./cmd/daemon-pane`, which snapshots the module's embedded prompts, so the text is the
   one the daemon would send. Two details the call needs: the project token must be lower case,
   and `ghPath` must name the real `gh` (`process.env.LEGION_GH_PATH`), because the composer
   refuses a first `gh` on `PATH` that is a script and in a pod that `gh` is Legion's wrapper.
   Turn the returned shell text into the prompt with the shell, not a regular expression:

   ```sh
   eval "args=( $(cat arg.txt) )" && printf '%s' "${args[1]}" >prompt.txt
   ```

2. **Run a real session** on it, with the role's own task:

   ```sh
   OMP_SESSION_STORAGE=file omp -p --session-dir "$dir" "<the role's task>" \
     --append-system-prompt "$(cat prompt.txt)"
   ```

   Unset every `LEGION_*` and `DISPATCH_*` variable and `OMP_SESSION_SQL_DSN_FILE` first: that
   keeps the pod's credentials out of a scratch session and its transcript, and keeps the scratch
   session from acting on the real tree. `OMP_SESSION_STORAGE=file` with the SQL pointer unset is
   what writes the transcript under `--session-dir`, which the provenance check then reads
   (`docs/solutions/legion/a-value-an-agent-wrote-is-a-measurement-only-when-the-transcript-holds-it-in-a-tool-result-INFRA-336.md`).

   The task stands in for the phase: name the issue, the head and the result, and tell the session
   to write its artifact to a scratch path, since it has no workspace, no jj and no `legion` tool.

3. **Control on the base checkout.** `git archive <base sha> packages/daemon | tar -x -C "$TMPDIR/..."`
   gives a daemon module to compose from without touching the workspace or making a worktree. Run
   the same tasks on it.

4. **Assert on the artifacts**, with `jq` over each written file, and on the transcripts.

## Evidence

INFRA-336 removed the `completed` time from every Legion handoff. At the branch's head all three
head sessions wrote a handoff with no `completed` key; on `main`'s prompt, composed from the
daemon module of `92619d8df` at the same minute, all three wrote one, two of them a fabricated
midnight and noon. The grep over the prompt files could not have shown either.
