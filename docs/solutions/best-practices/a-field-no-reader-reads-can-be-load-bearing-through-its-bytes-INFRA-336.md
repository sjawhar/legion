---
title: "A field no reader reads can be load-bearing through its bytes"
category: best-practices
tags:
  - field-removal
  - uniqueness
  - dedupe
  - serialisation
  - residual-risk
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A field no reader reads can be load-bearing through its bytes

"Nothing reads its value" and "nothing depends on it" are different claims. A field with no reader
can still carry a property of the serialisation that something downstream consumes: uniqueness,
non-emptiness, length, ordering.

- **Before you remove a field, ask which two writes would now be identical, and what consumes the
  difference.** List the mechanisms that take the record as bytes rather than as values — a
  commit, a content hash, a dedupe key, a diff, a cache — and check each against the first pair of
  writes that would collide. This is answerable by reading those consumers; it is not a judgement
  call.

- **Prefer closing the requirement where it is enforced over asking every writer to carry a new
  token.** The component that refuses a duplicate knows what it is comparing; a writer asked to
  supply uniqueness can fail to.

- **If the token must sit on the writer, give it a value the writer cannot invent.** A command's
  output, not a remembered string. A writer that can make the value up re-opens the failure one
  layer down — especially when the writer is an agent.

- **Name the residual the replacement leaves, and measure it.** Write the case the new token does
  not cover, and run it, rather than asserting that it is unlikely.

## Evidence

In Legion, every file-backed phase ends with a committed `.legion/<issue>/<phase>.json` handoff.
Nothing has read the `completed` time in that file since the handoff CLI was removed: the daemon
reads no handoff body, and `findHandoffCommit` locates the commit carrying the path without
parsing it.

The field was still load-bearing. It made each round's handoff bytes differ from the round
before's. INFRA-336 removed it, and a second round at an unmoved head that repeats its
predecessor's decision, counts and findings then writes the same bytes: `jj split` produces an
**empty** commit, the revset answers the previous round's commit, and the daemon refuses the
completion `HANDOFF_NOT_NEW`, because it compares the reported commit with the role's last
recorded one (`packages/daemon/internal/workflow/engine.go`).

The replacement, `previousHandoff`, is the newest commit on the branch that changed the handoff's
file (usually the role's previous handoff of the phase; after a retro, retro's removal), read with
a command rather than recalled. It is a writer-side token, so the residual is real and was measured
instead of argued: three rounds in a scratch jj repository showed round 2 with the field set
producing a non-empty commit the revset answers, and a byte-identical round 3 producing an empty
commit and the previous answer — the refusal the field exists to close, still reachable by a writer
that does not run the command.

Related: `docs/solutions/legion/a-round-that-changes-no-code-writes-its-handoff-again-so-its-completion-keys-to-a-fresh-commit-and-strips-the-four-keys-the-cli-stamps-LEGION-629.md`
records the same refusal from the writer's side.
