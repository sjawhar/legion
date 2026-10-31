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
  `scripts/semantic-line-breaks.guides.test.ts` holds five files in two tests:
  `packages/envoy/AGENTS.md` and `packages/envoy/cmd/dispatch/README.md` in one, and `AGENTS.md`,
  `scripts/e2e/README.md` and `docs/kubernetes.md` in the other.
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
first, and a gate only once a measurement shows one is needed." `.github/workflows/ci.yaml:1544-1547`
runs `bun test scripts/semantic-line-breaks.test.ts`, the checker's own unit tests, and nothing
else; its path filter at `:477-478` lists the script and that unit test, not the guides test and not
the guides.

The tester ran the guides test at the first merge head: 1 pass, 0 fail, 2 `expect()` calls, 420 ms.
The reviewer then read the script and found the second half: `const MAX_LINE = 160`
(`scripts/semantic-line-breaks.ts:47`) is applied by `wrapLongLine` under `--write`
(`:771-786`) and by no check. The shipped line, `packages/envoy/AGENTS.md:2607`, is about 460
characters, passes the check, and a later `--write` would split it after `reports it:`.

The first two bullets are wider than this page first stated, and a later forward merge of the same
pull request is what widened them. At `c10800814dec` the guides test held one test over the two
Envoy files; the merge of main at `1a9095e6cf22` brought a second test over `AGENTS.md`,
`scripts/e2e/README.md` and `docs/kubernetes.md`
(`scripts/semantic-line-breaks.guides.test.ts:12`, `:25-40`). The page's own slug still says
"the Envoy guides"; the checker is the repository's.
