---
title: "A block agents paste runs under the bash tool, whose timeout kills the call's process tree, so detach it and prove it there"
category: legion
tags:
  - legion
  - bash
  - bash-tool
  - trap
  - setsid
  - flock
  - docs
  - verification
date: 2026-10-25
status: active
module: docs
related_issues:
  - "INFRA-215"
  - "sjawhar/legion#2193"
---

# A block agents paste runs under the bash tool, whose timeout kills the call's process tree, so detach it and prove it there

Extends docs/solutions/legion/worker-pane-shell-gotchas.md (§14, the bash tool is not the shell a
script gets) from `jq` to traps, `$!`, `wait`, `tail` and the tool's timeout.

## Rules

1. **A shell block published for agents is a contract with the tool that runs it.** An agent pastes
   it into its bash tool, so the tool's behaviour is part of what the block has to survive. No
   amount of correctness inside the block makes up for a step the tool kills.
2. **Clean-up that rests on a trap cannot run in the tool's foreground.** When a call passes its
   timeout, the tool signals the call and every process under it, then kills them almost at once:
   a `SIGTERM` handler got 70 ms in one probe and nothing in two others. That covers a child in a
   session of its own too (`setsid --wait`), for as long as it descends from the call. Detach with
   `setsid -f`: the process is handed to pid 1, and the tool's kill no longer reaches it.
3. **The tool's shell is not bash: plan for that, and do not wait on a pid.** `$$` is the harness
   process, so a trap typed into the tool belongs to no shell that exits. `$!` is empty, `wait`
   finds no job, and `tail` is a harness builtin. Write the procedure to a file under `$TMPDIR` and
   run it as `bash <file>`. To wait for a detached run, take a lock on that file before detaching
   (`flock "$f" setsid -f bash "$f" > "$f.log" 2>&1 < /dev/null`). The detached bash inherits the
   lock, so `flock "$f" cat "$f.log"` waits for the run to end, with no race and no sleep loop.
   That line is also how a later call re-attaches after a timeout.
4. **A detached run tells the agent everything it needs to recover.** Print these first: the file
   and its log, the process group's stop command (`kill -TERM -- -$$`), and the name of every
   resource the run creates. Print `exit <status>` last, because the call that waits shows output
   and not a status. The resource's name is the only way back after a `SIGKILL` or a lost pod,
   when no trap runs.
5. **Prove the block where agents run it.** Paste the published text into your own bash tool, once
   with a timeout long enough and once with a timeout shorter than the run. Use the previous form
   as the negative control. A driver that starts the block in its own session (Python's
   `start_new_session`, `setsid`) has already escaped the tool's kill, so it cannot see this
   failure.
6. **Signal the group, and leave no background child.** A signal sent to the run's bash alone runs
   the trap only when the step in the foreground returns. A child the work leaves running still
   holds the lock, so the re-attach waits for that child too.

## Evidence

- Planning drove the sequence block of `docs/kubernetes.md` as `bash <file>`, and so did the
  implementer's first round. The first-round driver was `vrun.py`, which used
  `start_new_session=True`. Its signal cases were group and pid signals sent by hand, and all of
  them ran the trap. The tester then pasted the block into its own bash tool with a 40 s timeout.
  The call printed `[Command timed out after 40 seconds]`, and 15 s later the sequence's bash was
  gone. The Job and the Service were left with no `deletionTimestamp`, and the pod was `Running`.
  The implementer saw the same result with the same form (pod `Running` 2m36s later). The recovery
  lines removed the leftovers in 1.6 s.
- Probes through the tool, with call timeouts of 3-4 s:
  - A Python process that logs every 20 ms after a catchable signal logged nothing, both as the
    call's command and under `bash -c`.
  - `setsid --wait bash -c '…'` got as far as its EXIT trap but never reached `finished`.
  - `flock f setsid -f bash f` ran with `ppid 1` to `finished`, and its EXIT trap ran. A later
    `flock f cat f.log` printed the run.
- At the detached head (3fe13ffe), pasted into the tool: with a 40 s call timeout and with a 20 s
  one, the call ended at its timeout. The sequence ran on to `sequence: exit 0` and left nothing.
  A later `flock … cat …` waited 68 s and printed the whole run. `kill -TERM -- -<pid>` gave
  exit 143 with nothing left. `kill -KILL -- -<pid>` left the Job, and the printed `workload:`
  name let the recovery lines remove it. In the tester's fixture, a `sleep 45 &` left behind by
  the work held the re-attach for 44 s after `sequence: exit 0`.
- In the tool, `$!` printed nothing, `wait "$!"` answered `wait: no such job`, and `type tail`
  answered `tail is a shell builtin`. `/usr/bin/tail` is GNU coreutils.
- Another trap of the same kind: the `eval` tool's Python kernel did not inherit the bash tool's
  in-cluster `KUBERNETES_*` environment. `kubectl` run from there fell back to `localhost:8080`,
  and a whole first batch of cluster runs failed before anything was created. Run cluster
  fixtures from the bash tool, or pass its environment through explicitly.
