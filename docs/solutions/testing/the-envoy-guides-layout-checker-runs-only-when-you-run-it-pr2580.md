---
title: "The repository's guide layout checker runs only when you run it, and its check mode is weaker than its --write"
category: testing
tags:
  - documentation
  - bun-test
  - ci-coverage
  - envoy
  - daemon
date: 2026-10-31
status: active
module: scripts
related_issues:
  - "INFRA-841"
  - "sjawhar/legion#2580"
---

# The repository's guide layout checker runs only when you run it, and its check mode is weaker than its --write

- Read the checker's own file lists before you call a document gated or ungated.
  `scripts/semantic-line-breaks.guides.test.ts` holds five files across three tests. The test
  titled "the Envoy guide and the Dispatch server README keep the layout" runs the checker over
  `packages/envoy/AGENTS.md` and `packages/envoy/cmd/dispatch/README.md`. The test titled "the
  root guide, the e2e README and the Kubernetes doc keep the layout" runs it over the three names
  of `const GUIDES`: `AGENTS.md`, `scripts/e2e/README.md` and `docs/kubernetes.md`. The test
  titled "the root guide, the e2e README and the Kubernetes doc hold no raw HTML block, which the
  checker skips" reads those same three.
- After you change any of those five, run `bun test scripts/semantic-line-breaks.guides.test.ts`
  yourself. No CI lane runs it, so a line that breaks the one-sentence-per-line layout reaches
  main green.
- A green check mode does not mean the next contributor's `--write` will leave your line alone.
  Check mode enforces the sentence rules; the 160-character limit is applied only by the rewriter.
  A sentence longer than that passes the check and is split the next time somebody reformats the
  file, which puts your text in a diff you did not write.
- So write a long sentence you want kept whole as two sentences, or accept that it will be split
  later. Either way, run the checker before you push.
- One of those five, `docs/kubernetes.md`, is also parsed by a Go test
  (`packages/envoy/internal/broker/oauth/claims_doc_test.go`), so its layout is two gates' business
  at once and neither knows about the other. See
  `docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-is-mains-until-mains-own-run-says-otherwise-pr2580.md`,
  the rule about a red that belongs to the base, for what that cost.

## Evidence

sjawhar/legion#2580, INFRA-841. The branch changed one sentence of `packages/envoy/AGENTS.md`;
main then reflowed both guides to one sentence per line and added `scripts/semantic-line-breaks.ts`
with its own tests. The first line of `scripts/semantic-line-breaks.guides.test.ts` says the guides
test "runs on demand … and is deliberately not in CI: the deployment's rule is an instruction
first, and a gate only once a measurement shows one is needed." The workflow step named
`Test the semantic line-break checker` runs `bun test scripts/semantic-line-breaks.test.ts`, the
checker's own unit tests, and nothing else; the `test:` path filter that selects it lists
`scripts/semantic-line-breaks.ts` and `scripts/semantic-line-breaks.test.ts`, not the guides test
and not the guides.

The tester ran the guides test at the first merge head: 1 pass, 0 fail, 2 `expect()` calls, 420 ms.
The reviewer then read the script and found the second half: `const MAX_LINE = 160` in
`scripts/semantic-line-breaks.ts` is applied by `wrapLongLine`, which returns the line as it is
when the line is no longer than that, and by no check. The shipped line of
`packages/envoy/AGENTS.md`, the one that begins "a run that fails once the client has stopped is
the stop and not a failure, and the path the run was on reports it:", is about 460 characters,
passes the check, and a later `--write` would split it after `reports it:`.

The first two bullets are wider than this page first stated, and a later forward merge of the same
pull request is what widened them. At `c10800814dec` the guides test held one test, over the two
Envoy files; the merge of main at `1a9095e6cf22` brought two more, both over the three names of
`const GUIDES`, so the file holds three. The page's own slug still says "the Envoy guides"; the
checker is the repository's.
