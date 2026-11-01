---
title: "A recreated workspace is rebuilt from the session store's subagent transcripts, and every verified commit is pushed at once"
category: legion
tags:
  - issue-pod
  - workspace-recreated
  - session-store
  - recovery
  - subagents
date: 2026-10-28
status: active
module: skills/legion-worker
---

# A recreated workspace is rebuilt from the session store's subagent transcripts, and every verified commit is pushed at once

- Push each commit as soon as you have verified it, not at the end of the phase. A recreated
  workspace holds only what is on the issue branch at origin, and the issue volume can be wiped
  with it (`/legion/harness`, `$TMPDIR`).
- When unpushed work is lost, rebuild it before you redo it. Under
  `OMP_SESSION_STORAGE` with a SQL store, each `task` subagent's transcript is a row group in
  `omp_session_files_parts` (path `…/<parent-session-id>/<AgentName>.jsonl`, joined in
  `start_offset` order). Read the DSN from `$OMP_SESSION_SQL_DSN_FILE`.
- Replay only successful `write` and `edit` tool calls: skip any whose `toolResult` has `isError`.
  An edit tool's `PUT N.=M` is relative to the file as it stood then, so apply them in order. The
  edit tool also auto-repairs a duplicated boundary line, which a plain replay does not, so build
  and diff the result and fix such lines by hand.
- Re-extract throwaway scripts (harness setup, crawlers, probes) the same way, from their `write`
  calls, and run them again. Commit only after the checks pass on the rebuilt files.

## Evidence

- Pull request #2403's pod was replaced twice mid-phase. Each time the workspace came back at the
  last pushed commit, and `/legion/harness` and `$TMPDIR` were empty.
- The red tests, the construct (40bdc369), the Go fence fix (fc8278df), the harness `setup.sh` and
  the census crawler were all rebuilt from subagent transcripts. Only the replay of
  `block-schema.ts` needed a fix: one stray brace from a boundary line the edit tool had repaired.
- After the first loss, every verified commit was pushed at once, and the second replacement lost
  nothing.
