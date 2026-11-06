---
title: "Answer only the refusal the caller caused as a 4xx, through one sentinel, and name every place one fix must reach"
category: envoy
tags:
  - error-classification
  - api-errors
  - dispatch-uploads
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2480"
---

# Answer only the refusal the caller caused as a 4xx, through one sentinel, and name every place one fix must reach

- When an operation runs on something built only from the caller's input, such as a private
  scratch document, a refusal there can be the caller's fault. Give that one refusal a typed
  sentinel at the place it is raised. Make the type also match the old, broader error
  (`errors.Is(err, ErrSchema)`), so every other caller still sees it.
- Classify in one function at the boundary. The sentinel becomes the caller's 4xx. Every other
  cause, including an unknown kind, stays a plain server fault (500, logged), never wrapped as a
  stored document to repair. Test both outcomes. A branch that only returns nil is untested.
- Before you refuse, try every alternative the operation could have used. Have the finder list
  its candidates in the same order its pick-one call chooses, using one shared comparator. Take
  the first one that is allowed, and refuse only when none is. Taking only the nearest candidate
  refuses inputs that left the original alone and added a copy elsewhere.
- Name every refused place in the message, nearest first, counted by distinct place, not by
  match: "the front matter and two code blocks". Reporting only the last place tried sends the
  author back for a second refusal after the first fix.

## Evidence

sjawhar/legion#2480 took five review rounds to settle this. `pmdoc.MarkNotAllowedError`
(`pmdoc/find.go`), with its `Is` method and `At` place key, is the sentinel. `reanchorError`
(`docs/mutation.go`) is the one decision. `QuoteFinder.NearestFirst` and `nearerFirst` give the
candidate order that `Find` also uses through `slices.MinFunc`. These rounds fixed, in turn:

- a 500 for the caller's markdown;
- a 400 for every other error;
- a 400 for an upload that only copied anchored text;
- a message that named the farthest place;
- a message that counted two code blocks as one.

`TestAnUploadMovingAnAnchorsQuoteWhereItsMarkIsNotAllowedIsRefused` pins each behaviour, with a
negative control for each.
