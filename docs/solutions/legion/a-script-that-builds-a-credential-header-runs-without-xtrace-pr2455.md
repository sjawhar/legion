---
title: "A script that builds a credential header runs without xtrace"
category: legion
tags:
  - credentials
  - shell
  - transcript
  - dispatch-token
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2455"
---

# A script that builds a credential header runs without xtrace

A role's transcript keeps everything a command prints. A shell trace prints each command after
expansion, so a header built from a token file appears in the log with the token in it.

- Never run `set -x` (or `bash -x`) in a script that reads a credential into a command line, such
  as `-H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")"`.
- Prefer passing the header from a file the trace never expands (`curl -H @<file>`), or put the
  credentialed calls in a part of the script with tracing off.
- Never read a trace log of such a script back into your session. Summarise it with `grep` for
  the result lines you need, excluding any line with the credential.
- If a token reached your transcript anyway, delete the file that holds it and tell the architect
  the exact file and role, so the operator can rotate it.

## Evidence

- On sjawhar/legion#2455 the implementer's one-off setup script (corpus and replay-input
  downloads) began with `set -x`. Tailing its log printed the expanded `Authorization: Bearer`
  header of the pod's Dispatch token into the transcript. The log was deleted and the architect
  told, by role topic, which token file it was.
