---
title: "A per-item size limit over nested lists gives each line one owner and is tested against what surrounds the item"
category: best-practices
tags:
  - nested-lists
  - invariants
  - property-tests
  - markdown
date: 2026-10-31
status: active
module: scripts
related_issues:
  - "sjawhar/legion#2474"
---
# A per-item size limit over nested lists gives each line one owner and is tested against what surrounds the item

- When a limit is measured per item of a nested structure (rows, bytes, cost), give every line exactly one owner: the innermost item whose span holds it.
  A line no item's own content needs, such as the blank line a separator rule requires before an item, belongs to no item.
- Test two properties, not only examples:
  an item's verdict is the same whether a sibling follows it or the file ends;
  a parent's verdict does not grow with the number of its children.
- A limit that a fixer cannot satisfy (here, blank lines another rule requires) is a bug in the limit, never a reason to split the content.

## Evidence

- `scripts/semantic-line-breaks.ts`, the R3 block of `check()`: `owners.fill(item, item.first, item.last + 1)` in item order, so the innermost item wins, then a loop that clears the owner of the blank run before each item marker.
- Review round 1 of sjawhar/legion#2474 found a nested item's trailing blank line charged to it only when a sibling followed, so a 50-row item passed at the end of a file and failed before a sibling (fixed in 362ba0e9).
  Review round 2 found the fix charged the blank lines R2 requires between children to the parent, so a one-line parent failed at 50 children and `--write` could not repair it (fixed in 7cf8f4e4).
- The tests that pin both properties, in `scripts/semantic-line-breaks.test.ts`: "R3 gives a nested item the same verdict whether a sibling follows it or nothing does" and "R3 does not count the blank lines R2 requires between nested items toward their parent".
