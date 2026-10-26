---
title: "The dispatch CLI prints its dashboard origin, so a proof log is linted before it is posted"
category: legion
tags:
  - private-names
  - public-repository
  - live-proof
  - dispatch-cli
date: 2026-10-26
status: active
module: .github/scripts/check-private-names.sh
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# The dispatch CLI prints its dashboard origin, so a proof log is linted before it is posted

Extends docs/solutions/legion/a-pod-name-copied-from-dispatch-into-a-pull-request-carries-the-deployments-project-key-and-fails-the-private-names-check-LEGION-164.md.

- The `dispatch` CLI ends its `project` and `issue` lines with a dashboard link built from the
  pane's configured Dispatch origin. That origin is a private tailnet host, even when the CLI
  talks to a local test server. A captured proof log therefore carries it.
- Write a comment or PR body to a file, run `.github/scripts/check-private-names.sh <file>`, and
  post it only when that passes. Replace the host with the reserved `example-tailnet` host, and say
  in the comment that you did.
- If one slips through, delete the comment at once (`gh pr comment --delete-last --yes`) and
  repost it after the check passes. An edit leaves the old text in the comment's history; a
  deletion does not.

## Evidence

On sjawhar/legion#2209 the first version-skew proof comment carried
`https://dispatch.<tailnet>.ts.net/projects/…` lines from `dispatch project` and `dispatch issue`.
It was deleted within a minute and reposted with the host replaced
(https://github.com/sjawhar/legion/pull/2209#issuecomment-6405051940). Every later proof comment
ran the check first and passed.
