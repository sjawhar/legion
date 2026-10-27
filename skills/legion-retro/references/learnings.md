# Writing a retro learning under `docs/solutions/`

Read this at step 3 of `skill://legion-retro`, when you write a learning document. Organize by
reusable topic, not by pull request, so a worker on another issue finds it by its subject.

## Never edit a document you did not write this retro

Not its front matter, not its body. Two issues' retros can land within the same hour, and an
in-place edit (another `related_issues` entry, a sharpened sentence, a `status` flip) conflicts
with the other retro's edit to the same lines.

## Find what already covers the topic

1. Search `docs/solutions/` with the `grep` tool, case-insensitive, for two or three words naming
   the topic, and read each hit's H1 and rule.
2. For each file you found, search `docs/solutions/` for its path (`docs/solutions/<its-path>`).
   A file that lists that path under `supersedes:` (on that line, or as an item of the list below
   it) replaces it: read that file instead, and repeat this search for it. A file whose first line
   under the H1 is `Extends docs/solutions/<its-path>.md.` adds to it: read both. Legion runs no
   pass that reconciles these links, so this search is the only way to reach the newer file.

## Write a new file of your own

Name it `docs/solutions/<category>/<slug>-<LEGION_ISSUE>.md`, so two issues' retros never choose
the same path. `<category>` is an existing subdirectory of `docs/solutions/`; make a new one only
when none fits. `<slug>` is the title in lowercase words joined by `-`.

- **A fresh topic:** state the rule in a few imperative lines. The incident goes under a
  `## Evidence` heading below the rule, never in the rule, because a reader takes a story as an
  instruction.
- **A topic an existing document already covers:** the first line under the H1 reads
  `Extends docs/solutions/<existing-path>.md.`, and the rule states only what is new.
- **A topic an existing document states wrongly:** front matter adds
  `supersedes: docs/solutions/<existing-path>.md`, or a `supersedes:` list with one
  `- docs/solutions/<existing-path>.md` item each, and the first line under the H1 says why. Leave
  the old file untouched. A reader reaches your correction from it only through the search in step
  2 above, so the path in your `supersedes:` must be exact.

Front matter (`status: active` for a new file; `module:` the package or directory the rule is
about, as `packages/daemon` or `skills/legion-worker`):

```yaml
---
title: "Descriptive title matching the H1"
category: subdirectory-name
tags:
  - searchable-topic
date: YYYY-MM-DD
status: active
module: affected-module
related_issues:
  - "LEGION-123"      # the Dispatch issue
  - "owner/repo#456"  # the pull request
---
```

Commit it on the existing issue branch (step 3 of `skill://legion-retro`).
