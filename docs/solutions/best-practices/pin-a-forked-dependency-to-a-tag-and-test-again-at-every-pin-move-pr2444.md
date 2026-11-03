---
title: "Pin a forked dependency to a tag, and test again at every pin move"
category: best-practices
tags:
  - go-modules
  - dependencies
  - fork
  - pinning
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# Pin a forked dependency to a tag, and test again at every pin move

- Pin a forked dependency to a release tag. A pseudo-version of a release-candidate branch's head can vanish from every branch when that branch is rewritten, and the evidence you took at it then describes no code anyone can fetch.
- When the pin moves, list the dependency's changes between the two pins in code your package calls, and run the tests that depend on that behaviour again at the new pin. Keep one result from the old pin as the negative control.
- Check what a tag lets you check: the `go.sum` hashes against sum.golang.org, and the tag's signature.

## Evidence

- sjawhar/legion#2444 pinned `github.com/sjawhar/ygo` at the release candidate `845ce79c`. The candidate branch was then rewritten, and `845ce79c` was on no branch. The next pin, `a23bc871`, differed from the tag `v1.54.1-sami.1` by 16 commits ahead and 13 behind, among them #356's clock bounds and #357's partial-commit drain.
- At each move (`800ec539`, `be665c15`) the refused-peer, parked-update and refusal tests ran 300 times under `-race` again. Review round 8 checked the tag's signature and its `go.sum` hash against sum.golang.org. The pin then moved once more, to `v1.54.1-sami.3`, through main's #2604, which the branch took at `e22b1dfa`; the tester ran the format-replay and compaction tests again there.
