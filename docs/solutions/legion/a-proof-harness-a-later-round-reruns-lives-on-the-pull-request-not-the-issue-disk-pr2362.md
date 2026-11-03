---
title: "A proof harness a later round reruns lives on the pull request, not on the issue disk"
category: legion
tags:
  - issue-pod
  - scratch
  - proof
  - session-store
date: 2026-10-29
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "sjawhar/legion#2362"
---

# A proof harness a later round reruns lives on the pull request, not on the issue disk

Updates docs/solutions/legion/a-roles-scratch-on-the-container-filesystem-is-what-an-issue-pod-is-replaced-over-INFRA-43.md,
whose "the volume survives a pod replacement" no longer holds: an issue pod's disk is created with
the pod and deleted with it (docs/kubernetes.md, "The disk dies with its pod"). `/legion/scratch`
keeps the container's root filesystem small; it does not keep anything past the pod.

- **What a later round must run again goes where the pod's end cannot take it.** That means the
  proof script, its server configs and the commands that fetch its tools. Post the script on the
  pull request with the proof's first comment (the repository is public, so leave out any secret),
  or commit it under the issue's handoff directory. A forward merge or a review round after a pod
  replacement then reruns the same proof. It does not rebuild a different one.
- **Write each tool the harness needs as a command that fetches it at a pinned version**, such as
  `GOBIN=<dir> go install github.com/nats-io/nats-server/v2@v2.15.1`, never as a binary you
  expect to find. Then a rebuild is one pasted block.
- **When it is already lost, the session still has it.** Under a database session store
  (`OMP_SESSION_STORAGE=sql`), the session's JSONL is in the `omp_session_files_parts` table,
  keyed by `path` and ordered by `start_offset`. Read it with `psql "$(cat
  "$OMP_SESSION_SQL_DSN_FILE")"` (never print the DSN). Each `write` tool call's arguments hold a
  file's full content, and each `bash` call holds the setup commands. An `edit` of the file after
  its last `write` must be replayed too.

## Evidence

This pull request's implementer kept `proof.sh` (real listener and Dispatch binaries against a
three-server nats-server v2.15.1 cluster, a standalone server and Postgres 17) under
`/legion/scratch/tmp/p546b`, and posted only its output on sjawhar/legion#2362. The pod was
replaced between retro and the next forward merge. The workspace came back from the pushed branch,
and `/legion/scratch/tmp` came back empty. The script and the setup commands were recovered from
the session's rows in the session database, and the rebuilt proof ran at the merge head
(sjawhar/legion#2362, comment 6491621803). Without that recovery, the round's proof would have
been a new script that no earlier round had run.
