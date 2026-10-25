---
title: "A page that tells a worker where to carry a name ships the grep, not a list, and a skills/ text change is an image change"
category: legion
tags:
  - grep
  - duplication
  - worker-image
  - skills
  - capabilities
  - proof
date: 2026-10-25
status: active
module: packages/daemon/docker
related_issues:
  - "INFRA-221"
  - "sjawhar/legion#2149"
---

# A page that tells a worker where to carry a name ships the grep, not a list, and a skills/ text change is an image change

Extends docs/solutions/legion/find-every-copy-of-a-hand-maintained-list-by-its-last-member-alone-not-by-a-phrase-LEGION-764.md.

That note tells the implementer to find a list's copies by grepping for its last member. The delta
is for the document that hands that job to the next worker, and for proving the result:

- **Give the reader the grep, never a list of places.** In instructions such as "add a tool to the
  image by carrying its name through every place that spells it", write the grep: the command name
  and the package name, over the Dockerfile, the Go sources and `docs/`. Say which hits a test
  checks (here the `onPath` call, the capability `Summary` and the test stubs and expected string)
  and which are hand-kept: the Dockerfile's comments, its notices string and its smoke step. A list
  in prose is a copy that goes wrong at the next site nobody counted.
- **A change to `skills/` text is a worker-image change.** `worker.Dockerfile` copies `skills/` into
  the image, so the image half of a proof holds only at the final head's own tag. When that tag is
  still building at your completion, say it is not proven. Drive it once the build is done.
  Compare the shipped skill file's `sha256sum` with the workspace's file, so the proof is about the
  text that changed.

## Evidence

In sjawhar/legion#2149, the list of places a new image tool must reach was wrong three times in a
row:

- First it named two of the five places.
- Then it missed the Dockerfile's notices string and its smoke step. A worker following it would
  have shipped an image whose licence file does not name the package it installs.
- Then it still missed sites, since the Dockerfile spells the five names in six places. The
  reviewer wrote, "The fix is to stop enumerating, not to extend the enumeration."

The final `skills/legion-worker/references/tools.md`, "When no runner reaches it", gives the grep
instead and says which hits a test holds.

The last two rounds changed only `skills/` and `docs/solutions/`, and both completions left the
image half unproven at the round's own tag. The retro drove it on
`ghcr.io/sjawhar/legion-worker:sha-3061dbb55729` with a Job in the role's namespace. The Job ran
`type -P psql redis-cli sqlite3 nc dig` (five paths, exit 0) and each tool's version, and
`legion probe-image` printed `capability toolchain: present (… /usr/bin/psql, /usr/bin/redis-cli,
/usr/bin/sqlite3, /usr/bin/nc, /usr/bin/dig …)` and `probe-image: OK`. The image's
`/opt/legion/pi-legion/dist/skills/legion-worker/references/tools.md` had the same `sha256sum` as
the workspace's (`c79367b1…`). The namespace's admission policy refused each field the Job lacked
in turn. `kubectl create --dry-run=server` read each refusal without creating anything, and that
was the fast way through.
